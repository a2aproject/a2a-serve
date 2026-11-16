// Copyright 2026 The A2A Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sandbox

import (
	"archive/tar"
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os/exec"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/a2aproject/a2a-serve/sandbox/sandboxnet"
)

// Memory is a file system in memory that a sandbox mounts: a tmpfs volume
// of the container runtime, which lives until the sandbox closes.
type Memory struct {
	// Dir is where the sandbox mounts it, such as /workspace.
	Dir string
	// Size bounds it, such as "256m"; empty is [DefaultMemorySize].
	Size string
	// Files are written into it before the command first starts, by their
	// paths relative to Dir.
	Files map[string][]byte
}

// DefaultMemorySize bounds a file system in memory that sets no size.
const DefaultMemorySize = "256m"

// Files reads and writes the file system in memory of a sandbox, and runs
// commands next to its command.
type Files interface {
	// ReadFile returns the file at name, relative to the directory of the
	// file system or absolute in the sandbox.
	ReadFile(ctx context.Context, name string) ([]byte, error)
	// WriteFile writes data to the file at name, creating its directories.
	WriteFile(ctx context.Context, name string, data []byte) error
	// List returns the paths of the files under dir, relative to it.
	List(ctx context.Context, dir string) ([]string, error)
	// Exec runs argv in the sandbox, in the directory of the file system,
	// as the command's user and with its network, and returns its output.
	// It needs the command to run.
	Exec(ctx context.Context, argv []string, stdin []byte) (ExecResult, error)
}

// ExecResult is the result of [Files.Exec].
type ExecResult struct {
	// Stdout and Stderr hold up to [MaxExecOutput] bytes each, and
	// Truncated is set when a stream had more.
	Stdout, Stderr []byte
	ExitCode       int
	Truncated      bool
}

// MaxExecOutput bounds each output stream that [Files.Exec] keeps.
const MaxExecOutput = 8 << 20

// capped keeps the first max bytes written to it, and takes the rest
// without keeping it, so that the command doesn't block.
type capped struct {
	max       int
	buf       bytes.Buffer
	truncated bool
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room < len(p) {
		c.truncated = true
		c.buf.Write(p[:max(room, 0)])
		return len(p), nil
	}
	return c.buf.Write(p)
}

// memoryVolume returns the name of the volume of the sandbox name.
func memoryVolume(name string) string { return name + "-memory" }

// maxMemoryRead bounds a file that [Files.ReadFile] returns.
const maxMemoryRead = 32 << 20

// createVolume creates the tmpfs volume of m.
func createVolume(ctx context.Context, name string, m *Memory, labels map[string]string) error {
	size := cmp.Or(m.Size, DefaultMemorySize)
	args := []string{"volume", "create", "--driver", "local", "--opt", "type=tmpfs", "--opt", "device=tmpfs", "--opt", "o=size=" + size + ",mode=1777,nosuid,nodev"}
	for k, v := range labels {
		args = append(args, "--label", k+"="+v)
	}
	if out, err := exec.CommandContext(ctx, "docker", append(args, name)...).CombinedOutput(); err != nil {
		return fmt.Errorf("docker volume create %s: %w: %s", name, err, bytes.TrimSpace(out))
	}
	return nil
}

// removeVolume removes the volume name, if any. A container that Docker
// still removes keeps it in use for a moment, so it tries again then.
func removeVolume(ctx context.Context, name string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), containerStopTimeout)
	defer cancel()
	for {
		out, err := exec.CommandContext(ctx, "docker", "volume", "rm", "-f", name).CombinedOutput()
		if err == nil {
			return nil
		}
		if !bytes.Contains(out, []byte("volume is in use")) || ctx.Err() != nil {
			return fmt.Errorf("docker volume rm %s: %w: %s", name, err, bytes.TrimSpace(out))
		}
		// A container that Docker failed to remove after it exited, which
		// leaves it dead, keeps the volume in use for good.
		users, err := exec.CommandContext(ctx, "docker", "ps", "-aq", "--filter", "volume="+name, "--filter", "status=dead", "--filter", "status=exited").Output()
		if err != nil {
			slog.Warn("listing the stopped containers that use the file system in memory", "volume", name, "error", exitErr(err))
		} else {
			for _, id := range strings.Fields(string(users)) {
				if err := RemoveContainer(ctx, id); err != nil {
					slog.Warn("removing a stopped container that uses the file system in memory", "container", id, "error", err)
				}
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// memoryFiles implements [Files] for a Docker sandbox: holder is the
// container that keeps the volume mounted, and agent the container of the
// command.
type memoryFiles struct {
	holder, agent string
	dir           string
	user          string
	// launcher, when set, is the launcher in the agent's container, which
	// runs the commands of Exec without the capabilities it keeps.
	launcher string
	// exited is closed once the process of the command's last start
	// exits, or nil before the first start.
	exited atomic.Pointer[chan struct{}]
}

// abs returns name in the sandbox, which must be within the file system.
func (f *memoryFiles) abs(name string) (string, error) {
	p := path.Clean(name)
	if !path.IsAbs(p) {
		p = path.Join(f.dir, p)
	}
	if p != f.dir && !strings.HasPrefix(p, f.dir+"/") {
		return "", fmt.Errorf("%s is outside %s", name, f.dir)
	}
	return p, nil
}

func (f *memoryFiles) ReadFile(ctx context.Context, name string) ([]byte, error) {
	p, err := f.abs(name)
	if err != nil {
		return nil, err
	}
	var data []byte
	err = f.copyOut(ctx, p, func(tr *tar.Reader) error {
		h, err := tr.Next()
		if err != nil {
			return err
		}
		if h.Typeflag != tar.TypeReg {
			return fmt.Errorf("%s is not a regular file", p)
		}
		if h.Size > maxMemoryRead {
			return fmt.Errorf("%s has %d bytes, more than %d", p, h.Size, maxMemoryRead)
		}
		data, err = io.ReadAll(tr)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", p, err)
	}
	return data, nil
}

// copyOut streams the tar of the path p of the holder into read, which
// may stop early.
func (f *memoryFiles) copyOut(ctx context.Context, p string, read func(*tar.Reader) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "cp", f.holder+":"+p, "-")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	rerr := read(tar.NewReader(out))
	// A reader that stopped early leaves docker cp blocked on its output.
	cancel()
	werr := cmd.Wait()
	if rerr != nil {
		if stderr.Len() > 0 {
			return fmt.Errorf("%w: %s", rerr, bytes.TrimSpace(stderr.Bytes()))
		}
		return rerr
	}
	if werr != nil && ctx.Err() == nil {
		return fmt.Errorf("%w: %s", werr, bytes.TrimSpace(stderr.Bytes()))
	}
	return nil
}

func (f *memoryFiles) WriteFile(ctx context.Context, name string, data []byte) error {
	p, err := f.abs(name)
	if err != nil {
		return err
	}
	return f.copyIn(ctx, map[string][]byte{p: data})
}

// copyIn writes files, by absolute path, owned by the command's user.
func (f *memoryFiles) copyIn(ctx context.Context, files map[string][]byte) error {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	uid, gid := userIDs(f.user)
	now := time.Now()
	dirs := map[string]bool{}
	for p := range files {
		for d := path.Dir(p); d != f.dir && strings.HasPrefix(d, f.dir+"/"); d = path.Dir(d) {
			dirs[d] = true
		}
	}
	for _, d := range slices.Sorted(maps.Keys(dirs)) {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: strings.TrimPrefix(d, "/") + "/", Mode: 0o755, Uid: uid, Gid: gid, ModTime: now}); err != nil {
			return err
		}
	}
	for p, data := range files {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: strings.TrimPrefix(p, "/"), Mode: 0o644, Size: int64(len(data)), Uid: uid, Gid: gid, ModTime: now}); err != nil {
			return err
		}
		if _, err := tw.Write(data); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "docker", "cp", "-", f.holder+":/")
	cmd.Stdin = &buf
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("writing files into the sandbox: %w: %s", err, bytes.TrimSpace(out))
	}
	return nil
}

func (f *memoryFiles) List(ctx context.Context, dir string) ([]string, error) {
	p, err := f.abs(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	err = f.copyOut(ctx, p+"/.", func(tr *tar.Reader) error {
		for {
			h, err := tr.Next()
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
			if h.Typeflag != tar.TypeReg {
				continue
			}
			if len(names) == maxListed {
				return fmt.Errorf("%s holds more than %d files", p, maxListed)
			}
			names = append(names, strings.TrimPrefix(h.Name, "./"))
		}
	})
	if err != nil {
		return nil, fmt.Errorf("listing %s: %w", p, err)
	}
	return names, nil
}

// maxListed bounds the files that [Files.List] returns.
const maxListed = 10000

func (f *memoryFiles) Exec(ctx context.Context, argv []string, stdin []byte) (ExecResult, error) {
	if len(argv) == 0 {
		return ExecResult{}, errors.New("no command to run")
	}
	if err := f.waitRunning(ctx); err != nil {
		return ExecResult{}, err
	}
	args := []string{"exec", "-i", "-u", f.user, "-w", f.dir, f.agent}
	runCtx := ctx
	deadline, timed := ctx.Deadline()
	if f.launcher != "" {
		uid, gid := userIDs(f.user)
		args = []string{"exec", "-i", "-u", "0:0", "-w", f.dir}
		if timed {
			// The launcher kills the command once its time runs out, which
			// killing docker exec wouldn't; docker exec gets a little more.
			secs := max(int(time.Until(deadline).Seconds()), 1)
			args = append(args, "-e", fmt.Sprintf("%s=%d", sandboxnet.EnvExecTimeout, secs))
			var cancel context.CancelFunc
			runCtx, cancel = context.WithDeadline(context.WithoutCancel(ctx), deadline.Add(execKillGrace))
			defer cancel()
			stop := context.AfterFunc(ctx, func() {
				if ctx.Err() == context.Canceled {
					cancel()
				}
			})
			defer stop()
		}
		args = append(args, f.agent, f.launcher, "sandbox-init", "--exec", "-", strconv.Itoa(uid), strconv.Itoa(gid))
	}
	cmd := exec.CommandContext(runCtx, "docker", append(args, argv...)...)
	cmd.Stdin = bytes.NewReader(stdin)
	stdout, stderr := &capped{max: MaxExecOutput}, &capped{max: MaxExecOutput}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	res := ExecResult{Stdout: stdout.buf.Bytes(), Stderr: stderr.buf.Bytes(), Truncated: stdout.truncated || stderr.truncated}
	if runCtx.Err() != nil {
		res.ExitCode = -1
		if f.launcher == "" {
			return res, fmt.Errorf("the command ran out of time; it runs until it exits or the sandbox closes: %w", runCtx.Err())
		}
		return res, fmt.Errorf("the command ran out of time: %w", runCtx.Err())
	}
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		res.ExitCode = ee.ExitCode()
		// docker exec fails with 1 when the container doesn't run, and 125
		// to 127 when the command can't start.
		if (res.ExitCode == 1 || res.ExitCode >= 125) && bytes.HasPrefix(res.Stderr, []byte("Error response from daemon")) {
			return res, fmt.Errorf("docker exec: %s", bytes.TrimSpace(res.Stderr))
		}
		if f.launcher != "" && timed && res.ExitCode == sandboxnet.ExecTimedOutExit && bytes.Contains(res.Stderr, []byte("a2a-init: ")) {
			return res, errors.New("the command ran out of time, and the sandbox stopped it")
		}
		return res, nil
	}
	return res, err
}

// execKillGrace is how long docker exec outlives the timeout of a command,
// which the launcher kills first.
const execKillGrace = 5 * time.Second

// execStartWait bounds the wait of Exec for a command that just started.
const execStartWait = 30 * time.Second

// waitRunning waits for the container of the command to run, which it
// doesn't yet right after docker run starts, while the process of docker
// run lives.
func (f *memoryFiles) waitRunning(ctx context.Context) error {
	exited := f.exited.Load()
	if exited == nil {
		return errors.New("the command of the sandbox doesn't run; start it first")
	}
	deadline := time.Now().Add(execStartWait)
	for {
		out, err := exec.CommandContext(ctx, "docker", "inspect", "-f", "{{.State.Status}}", f.agent).Output()
		state := strings.TrimSpace(string(out))
		switch {
		case err == nil && state == "running":
			return nil
		case err == nil && state != "created":
			return fmt.Errorf("the command of the sandbox doesn't run: its container is %s", state)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the container of the command didn't start within %s", execStartWait)
		}
		select {
		case <-*exited:
			return errors.New("the command of the sandbox doesn't run; start it first")
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// checkHolder fails when the container that keeps the file system runs no
// longer, with what Docker knows about its end. The file system is gone
// then: Docker unmounts a tmpfs volume that no container uses.
func (f *memoryFiles) checkHolder(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "docker", "inspect", "-f", "{{.State.Status}} {{.State.ExitCode}} {{.State.OOMKilled}} {{.State.Error}}", f.holder)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil && bytes.Contains(stderr.Bytes(), []byte("No such object")) {
		return fmt.Errorf("the file system in memory is gone: its container %s doesn't exist", f.holder)
	}
	if err != nil {
		return fmt.Errorf("checking the container of the file system in memory: %w: %s", err, bytes.TrimSpace(stderr.Bytes()))
	}
	state := strings.TrimSpace(string(out))
	if strings.HasPrefix(state, "running ") {
		return nil
	}
	// A Go runtime that crashes says why first, before its stack.
	logs, err := exec.CommandContext(ctx, "docker", "logs", f.holder).CombinedOutput()
	if err != nil {
		logs = []byte("reading its log: " + err.Error())
	}
	if len(logs) > maxHolderLog {
		logs = logs[:maxHolderLog]
	}
	return fmt.Errorf("the file system in memory is gone: its container %s stopped (status, exit code, OOM killed, error: %s): %s", f.holder, state, bytes.TrimSpace(logs))
}

// maxHolderLog bounds the log of a stopped holder in an error.
const maxHolderLog = 1024

// writeAll writes files, by their paths relative to the file system.
func (f *memoryFiles) writeAll(ctx context.Context, files map[string][]byte) error {
	if len(files) == 0 {
		return nil
	}
	abs := map[string][]byte{}
	for name, data := range files {
		p, err := f.abs(name)
		if err != nil {
			return err
		}
		abs[p] = data
	}
	return f.copyIn(ctx, abs)
}

// startHolder starts the container that keeps the volume of the sandbox
// name mounted at dir while its command restarts, running the launcher at
// launcher, a host path, in image.
func startHolder(ctx context.Context, name, image, volume, dir, launcher string, labels map[string]string) error {
	if err := RemoveContainer(ctx, name); err != nil {
		return err
	}
	// Without --rm, a holder that dies keeps its exit code and its log,
	// which checkHolder reports; closing the sandbox removes it.
	args := []string{"run", "-d", "--name", name,
		"--user", "0:0", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		// One P keeps the launcher's threads few, and the PID limit leaves
		// it room: a Go runtime that can't start a thread exits.
		"--network", "none", "--memory", "32m", "--pids-limit", "32", "-e", "GOMAXPROCS=1",
		"--entrypoint", sandboxnet.ContainerLauncher,
		"-v", launcher + ":" + sandboxnet.ContainerLauncher + ":ro",
		"-v", volume + ":" + dir,
	}
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		args = append(args, "--label", k+"="+labels[k])
	}
	args = append(args, image, "sandbox-init", "--hold")
	if out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput(); err != nil {
		// docker run leaves a container that it created but couldn't start.
		return errors.Join(fmt.Errorf("starting the container of the file system in memory: %w: %s", err, bytes.TrimSpace(out)), RemoveContainer(ctx, name))
	}
	return nil
}

// userIDs parses uid:gid.
func userIDs(user string) (int, int) {
	var uid, gid int
	if _, err := fmt.Sscanf(user, "%d:%d", &uid, &gid); err != nil {
		return 0, 0
	}
	return uid, gid
}
