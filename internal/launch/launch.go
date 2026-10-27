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

// Package launch runs an agent server in the foreground, in the background or
// as a user service, and follows its log.
package launch

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agentsrv"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/credstore"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
	"github.com/a2aproject/a2a-serve/internal/service"
)

// maxLogSize is the size at which the server log is rotated on start.
const maxLogSize = 10 << 20

// Foreground runs the agent of dir until ctx ends. Progress, including the
// output of pulling or building the harness image, goes to out; logs and the
// harness's stderr go to state/server.log, and also to stderr when verbose.
// quiet keeps the image output in the log only. creds, if set, receives an
// owner token for the local URL. ready, if set, is called once the agent
// serves.
func Foreground(ctx context.Context, dir agentdir.Dir, out io.Writer, verbose, quiet bool, creds *credstore.Store, ready func()) error {
	cfg, err := dir.LoadConfig()
	if err != nil {
		return err
	}
	secrets, err := dir.LoadSecrets()
	if err != nil {
		return err
	}
	if err := dir.CheckConfig(cfg, secrets); err != nil {
		return err
	}
	if err := dir.Init(); err != nil {
		return err
	}
	// A second start must fail before it touches the running agent's log.
	if err := checkNotRunning(dir); err != nil {
		return err
	}
	if err := rotateLog(dir.LogPath(), maxLogSize, os.Stdout, os.Stderr); err != nil {
		return err
	}
	logFile, err := os.OpenFile(dir.LogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if err := logFile.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "closing log: %v\n", err)
		}
	}()
	var logOut io.Writer = logFile
	level := slog.LevelInfo
	if verbose {
		logOut = io.MultiWriter(logFile, os.Stderr)
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(logOut, &slog.HandlerOptions{Level: level})))
	slog.Info("starting agent server", "name", cfg.Name, "harness", cfg.Harness, "dir", dir.Root)
	opts := agentsrv.Options{Dir: dir, Config: cfg, Secrets: secrets, Out: out, HarnessLog: logOut, ImageProgress: out, Credentials: creds}
	if quiet {
		lw := &logLines{msg: "harness image"}
		defer lw.flush()
		opts.ImageProgress = lw
	}
	if ready != nil {
		opts.Ready = func(*agentsrv.Server) { ready() }
	}
	err = agentsrv.Run(ctx, opts)
	if err != nil {
		slog.Error("agent server stopped", "error", err)
	}
	return err
}

// checkNotRunning fails if an agent server holds the lock of dir. The lock
// is released at once: the server takes it again when it starts.
func checkNotRunning(dir agentdir.Dir) error {
	release, err := fsutil.TryLock(dir.LockPath())
	if errors.Is(err, fsutil.ErrLocked) {
		return fmt.Errorf("an agent server is already running in %s (a2a serve status)", dir.Root)
	}
	if err != nil {
		return fmt.Errorf("locking the agent directory: %w", err)
	}
	return release()
}

// rotateLog moves the log at path to path.1 once it is larger than limit. A
// log this process already writes to through one of writers, as the stdout
// a detached start or the service manager opened, is copied and truncated
// instead, since a rename would leave those writes in path.1.
func rotateLog(path string, limit int64, writers ...*os.File) error {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Size() <= limit {
		return nil
	}
	if !slices.ContainsFunc(writers, func(f *os.File) bool { return sameFile(f, info) }) {
		return os.Rename(path, path+".1")
	}
	return copyTruncate(path, path+".1")
}

func sameFile(f *os.File, info os.FileInfo) bool {
	if f == nil {
		return false
	}
	fi, err := f.Stat()
	return err == nil && os.SameFile(fi, info)
}

func copyTruncate(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, in.Close()) }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		return errors.Join(err, out.Close())
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Truncate(src, 0)
}

// ServiceName is the name of the agent's background service. Services
// installed before the name was derived from the directory alone also had
// the agent name in it; while only such a unit is installed, its name is
// returned, so that `a2a serve stop --uninstall` removes it.
func ServiceName(dir agentdir.Dir, _ *agentdir.Config) string {
	name := serviceName(dir)
	if service.Installed(name) {
		return name
	}
	if legacy := legacyServices(dir); len(legacy) > 0 {
		return legacy[0]
	}
	return name
}

// InstalledServices returns the installed services of the agent, legacy
// ones included.
func InstalledServices(dir agentdir.Dir) []string {
	var out []string
	if name := serviceName(dir); service.Installed(name) {
		out = append(out, name)
	}
	return append(out, legacyServices(dir)...)
}

func serviceName(dir agentdir.Dir) string { return "a2a-" + dir.ID() }

// legacyServices returns the installed services of dir named
// a2a-<agent name>-<id>.
func legacyServices(dir agentdir.Dir) []string {
	names, err := service.Glob("a2a-*-" + dir.ID())
	if err != nil {
		slog.Debug("listing services", "error", err)
		return nil
	}
	return names
}

// Background starts the agent detached from the terminal, or as a user
// service when asService, and relays its progress to out until it serves.
// quiet keeps the output of pulling or building the harness image out of it.
func Background(ctx context.Context, dir agentdir.Dir, asService, quiet bool, out io.Writer) error {
	cfg, err := dir.LoadConfig()
	if err != nil {
		return err
	}
	secrets, err := dir.LoadSecrets()
	if err != nil {
		return err
	}
	if err := dir.CheckConfig(cfg, secrets); err != nil {
		return err
	}
	if err := dir.Init(); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if _, err := controlapi.NewClient(dir).Status(ctx); err == nil {
		return fmt.Errorf("the agent is already running (a2a serve status); stop it first with `a2a serve stop`")
	}
	if err := checkNotRunning(dir); err != nil {
		return err
	}
	// The child's stdout is the log, so it is rotated before the child opens it.
	if err := rotateLog(dir.LogPath(), maxLogSize); err != nil {
		return err
	}
	var offset int64
	if info, err := os.Stat(dir.LogPath()); err == nil {
		offset = info.Size()
	}
	args := []string{"--dir", dir.Root}
	if quiet {
		args = append(args, "--quiet")
	}
	u := service.Unit{
		Name:        serviceName(dir),
		Description: fmt.Sprintf("a2a serve %s (%s)", cfg.Name, dir.Root),
		Exe:         exe,
		Args:        args,
		Dir:         dir.Root,
		LogPath:     dir.LogPath(),
		Env:         []string{"PATH=" + os.Getenv("PATH")},
	}
	var note string
	alive := func() bool { return true }
	if asService {
		// A unit named after the agent's former name would keep starting and
		// failing on the lock.
		for _, name := range legacyServices(dir) {
			if err := service.Uninstall(name); err != nil {
				return fmt.Errorf("removing the old service %s: %w", name, err)
			}
		}
		if note, err = service.Install(u); err != nil {
			return err
		}
	} else {
		exited, err := service.Detach(u)
		if err != nil {
			return err
		}
		alive = func() bool {
			select {
			case <-exited:
				return false
			default:
				return true
			}
		}
	}
	if err := waitReady(ctx, dir, offset, alive, out); err != nil {
		return err
	}
	msg := "✓ running in the background (a2a serve logs, a2a serve stop)\n"
	if asService {
		msg = "✓ running as a user service (a2a serve logs)\n"
		if note != "" {
			msg += "  " + note + "\n"
		}
	}
	_, err = io.WriteString(out, msg)
	return err
}

// waitReady relays progress lines from the log until the control socket
// answers or the process dies.
func waitReady(ctx context.Context, dir agentdir.Dir, offset int64, alive func() bool, out io.Writer) error {
	deadline := time.Now().Add(15 * time.Minute)
	client := controlapi.NewClient(dir)
	var tail []string
	for {
		lines, next, err := readFrom(dir.LogPath(), offset)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		offset = next
		for _, l := range lines {
			if isProgress(l) {
				if _, err := fmt.Fprintln(out, l); err != nil {
					return err
				}
			}
			tail = append(tail, l)
			if len(tail) > 20 {
				tail = tail[1:]
			}
		}
		sctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, statusErr := client.Status(sctx)
		cancel()
		if statusErr == nil {
			// Print what the server logged while becoming ready.
			lines, _, err := readFrom(dir.LogPath(), offset)
			if err != nil {
				slog.Debug("reading the startup log", "error", err)
				return nil
			}
			for _, l := range lines {
				if !isProgress(l) {
					continue
				}
				if _, err := fmt.Fprintln(out, l); err != nil {
					return err
				}
			}
			return nil
		}
		if !alive() {
			return fmt.Errorf("the agent server exited:\n%s", strings.Join(tail, "\n"))
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the agent server did not start in time; see %s", dir.LogPath())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// isProgress reports whether a log line is a progress line for the user
// rather than a structured log record.
func isProgress(line string) bool {
	return line != "" && !strings.HasPrefix(line, "time=")
}

// readFrom returns the complete lines of the file at path after offset, and
// the offset after them. A file shorter than offset was replaced, as on a
// restart, and is read from the start.
func readFrom(path string, offset int64) ([]string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, offset, err
	}
	defer func() {
		if err := f.Close(); err != nil {
			slog.Debug("closing log", "error", err)
		}
	}()
	fi, err := f.Stat()
	if err != nil {
		return nil, offset, err
	}
	if fi.Size() < offset {
		offset = 0
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, offset, err
	}
	var lines []string
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadString('\n')
		if errors.Is(err, io.EOF) {
			// A partial line is read again once it is complete.
			return lines, offset, nil
		}
		if err != nil {
			return lines, offset, err
		}
		offset += int64(len(line))
		lines = append(lines, strings.TrimRight(line, "\r\n"))
	}
}

// FollowLog prints the last n lines of the log, then new lines until ctx
// ends if follow is set.
func FollowLog(ctx context.Context, dir agentdir.Dir, n int, follow bool, out io.Writer) error {
	data, err := os.ReadFile(dir.LogPath())
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("no log yet at %s", dir.LogPath())
	}
	if err != nil {
		return err
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	if err := printLines(out, lines); err != nil {
		return err
	}
	if !follow {
		return nil
	}
	offset := int64(len(data))
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(500 * time.Millisecond):
		}
		lines, next, err := readFrom(dir.LogPath(), offset)
		if err != nil {
			return err
		}
		offset = next
		if err := printLines(out, lines); err != nil {
			return err
		}
	}
}

func printLines(out io.Writer, lines []string) error {
	for _, l := range lines {
		if _, err := fmt.Fprintln(out, l); err != nil {
			return err
		}
	}
	return nil
}
