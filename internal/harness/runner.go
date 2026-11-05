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

package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
)

// Runner decides where a harness process runs.
type Runner interface {
	// Command returns the process which runs c.
	Command(ctx context.Context, c Command) (*exec.Cmd, error)
}

// LocalRunner runs the harness directly on the host, for trusted setups or
// when Docker isn't available.
type LocalRunner struct{}

var _ Runner = LocalRunner{}

// Command implements [Runner].
func (LocalRunner) Command(ctx context.Context, c Command) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, c.Path, c.Args...)
	cmd.Dir = c.Dir
	cmd.Env = append(os.Environ(), c.Env...)
	return cmd, nil
}

// Limits are container resource limits.
type Limits struct {
	Memory string
	CPUs   string
	PIDs   int
}

// DockerRunner runs the harness in a container with the workspace mounted.
type DockerRunner struct {
	// Name is the container name.
	Name   string
	Image  string
	Mounts []agentdir.Mount
	// Hidden are host paths which appear as empty read-only directories
	// wherever a mount exposes them, such as the agent's .a2a directory.
	Hidden []string
	// Home is the host directory mounted at /home/agent.
	Home string
	// Env holds NAME=value pairs passed into the container. Values are passed
	// in a private env file, not in the arguments or the environment of the
	// docker CLI, so they neither show up in process listings nor change
	// which docker the host runs.
	Env    []string
	Limits Limits
	Labels map[string]string
	// Rootless is set for rootless Docker, where container root is the host
	// user, so files the agent writes stay owned by you.
	Rootless bool

	envFiles []string
}

var _ Runner = (*DockerRunner)(nil)

// ContainerHome is the home directory of the harness in the container.
const ContainerHome = "/home/agent"

// SandboxDir is where the directory of the socket the harness reaches the
// server through is mounted in the container.
const SandboxDir = "/run/a2a"

// startScript removes a container left behind by a crash, which would keep
// the name taken, and replaces itself with docker run. Signals sent to the
// process group reach the docker CLI, which forwards them to the container.
const startScript = `docker rm -f "$0" >/dev/null 2>&1; exec docker run --name "$0" "$@"`

// Command implements [Runner].
func (r *DockerRunner) Command(ctx context.Context, c Command) (*exec.Cmd, error) {
	envFile, err := r.writeEnvFile(append(slices.Clone(r.Env), c.Env...))
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "sh", append([]string{"-c", startScript, r.Name}, r.runArgs(c, envFile)...)...)
	cmd.Env = os.Environ()
	cmd.Dir = os.TempDir()
	return cmd, nil
}

// writeEnvFile writes env to a file only the user can read, for docker run
// --env-file. It returns "" when env is empty.
func (r *DockerRunner) writeEnvFile(env []string) (string, error) {
	var buf strings.Builder
	for _, kv := range env {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || name == "" {
			continue
		}
		if strings.ContainsAny(value, "\r\n") {
			return "", fmt.Errorf("the value of %s has a line break, which docker can't pass to the container", name)
		}
		buf.WriteString(name + "=" + value + "\n")
	}
	if buf.Len() == 0 {
		return "", nil
	}
	f, err := os.CreateTemp("", "a2a-env-*")
	if err != nil {
		return "", fmt.Errorf("creating the container env file: %w", err)
	}
	if _, err := f.WriteString(buf.String()); err != nil {
		return "", errors.Join(fmt.Errorf("writing the container env file: %w", err), f.Close(), os.Remove(f.Name()))
	}
	if err := f.Close(); err != nil {
		return "", errors.Join(fmt.Errorf("writing the container env file: %w", err), os.Remove(f.Name()))
	}
	r.envFiles = append(r.envFiles, f.Name())
	return f.Name(), nil
}

// runArgs returns the arguments of docker run after --name.
func (r *DockerRunner) runArgs(c Command, envFile string) []string {
	limits := r.Limits
	if limits.Memory == "" {
		limits.Memory = agentdir.DefaultLimits.Memory
	}
	if limits.CPUs == "" {
		limits.CPUs = agentdir.DefaultLimits.CPUs
	}
	if limits.PIDs == 0 {
		limits.PIDs = agentdir.DefaultLimits.PIDs
	}
	user := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	if r.Rootless {
		user = "0:0"
	}
	args := []string{
		"--rm", "-i", "--init",
		"--user", user,
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--memory", limits.Memory, "--cpus", limits.CPUs, "--pids-limit", strconv.Itoa(limits.PIDs),
		"--add-host", "host.docker.internal:host-gateway",
	}
	for _, k := range slices.Sorted(maps.Keys(r.Labels)) {
		args = append(args, "--label", k+"="+r.Labels[k])
	}
	for _, m := range r.Mounts {
		spec := m.Src + ":" + m.Dst
		if m.ReadOnly {
			spec += ":ro"
		}
		args = append(args, "-v", spec)
	}
	for _, dst := range hiddenDsts(r.Mounts, r.Hidden) {
		args = append(args, "--mount", "type=tmpfs,dst="+dst+",readonly")
	}
	if r.Home != "" {
		args = append(args, "-v", r.Home+":"+ContainerHome)
	}
	// -e takes precedence over --env-file, so the harness keeps its home.
	if envFile != "" {
		args = append(args, "--env-file", envFile)
	}
	args = append(args, "-e", "HOME="+ContainerHome)
	if c.Dir != "" {
		args = append(args, "-w", c.Dir)
	}
	args = append(args, r.Image, c.Path)
	return append(args, c.Args...)
}

// hiddenDsts returns the container paths at which mounts expose the hidden
// host paths. Paths are compared with symlinks resolved, as docker resolves
// them in bind sources.
func hiddenDsts(mounts []agentdir.Mount, hidden []string) []string {
	var out []string
	for _, h := range hidden {
		h = resolvePath(h)
		for _, m := range mounts {
			rel, err := filepath.Rel(resolvePath(m.Src), h)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				continue
			}
			dst := filepath.Join(m.Dst, rel)
			if !slices.Contains(out, dst) {
				out = append(out, dst)
			}
		}
	}
	return out
}

// resolvePath returns p with symlinks resolved. Of a path that doesn't
// exist, it resolves the part that does, dangling symlinks included.
func resolvePath(p string) string {
	return resolveFrom(filepath.Clean(p), 0)
}

// maxSymlinks bounds the symlinks resolvePath follows by hand.
const maxSymlinks = 40

func resolveFrom(p string, hops int) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	if target, err := os.Readlink(p); err == nil && hops < maxSymlinks {
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(p), target)
		}
		return resolveFrom(filepath.Clean(target), hops+1)
	}
	parent := filepath.Dir(p)
	if parent == p {
		return p
	}
	return filepath.Join(resolveFrom(parent, hops), filepath.Base(p))
}

// Pins are the mount sources and hidden paths of an agent with symlinks
// resolved, as they were when its server started.
type Pins map[string]string

// Pin resolves the sources of mounts and the hidden paths.
func Pin(mounts []agentdir.Mount, hidden []string) Pins {
	p := Pins{}
	for _, m := range mounts {
		p[m.Src] = resolvePath(m.Src)
	}
	for _, h := range hidden {
		p[h] = resolvePath(h)
	}
	return p
}

// check refuses a pinned mount source or hidden path that lies inside
// another writable mount and resolves elsewhere now. The harness can
// replace a directory in a writable mount with a symlink, which docker
// would follow at the next start.
func (p Pins) check(mounts []agentdir.Mount, hidden []string) error {
	paths := slices.Clone(hidden)
	for _, m := range mounts {
		paths = append(paths, m.Src)
	}
	for _, path := range paths {
		pinned, ok := p[path]
		if !ok || !p.inWritableMount(path, mounts) {
			continue
		}
		if now := resolvePath(path); now != pinned {
			return fmt.Errorf("%s resolved to %s when the server started and resolves to %s now; the agent may have replaced a directory with a symlink, so check it and restart the server", path, pinned, now)
		}
	}
	return nil
}

// inWritableMount reports whether the pinned path lies strictly inside the
// pinned source of a writable mount.
func (p Pins) inWritableMount(path string, mounts []agentdir.Mount) bool {
	for _, m := range mounts {
		src, ok := p[m.Src]
		if m.ReadOnly || !ok || m.Src == path {
			continue
		}
		rel, err := filepath.Rel(src, p[path])
		if err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// containerStopTimeout bounds removing a container, which gets its own
// budget so that a slow shutdown of other components doesn't leave it running.
const containerStopTimeout = 30 * time.Second

// Stop removes the container if it is still running, and the env files.
func (r *DockerRunner) Stop(ctx context.Context) error {
	var errs []error
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), containerStopTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "rm", "-f", r.Name).CombinedOutput()
	if err != nil && !bytes.Contains(out, []byte("No such container")) {
		errs = append(errs, fmt.Errorf("docker rm -f %s: %w: %s", r.Name, err, bytes.TrimSpace(out)))
	}
	for _, f := range r.envFiles {
		if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	r.envFiles = nil
	return errors.Join(errs...)
}

// PruneContainers removes the containers, running or not, that carry all
// of labels, and returns how many it found. It cleans up after a server
// that crashed before it could remove its containers.
func PruneContainers(ctx context.Context, labels map[string]string) (int, error) {
	args := []string{"ps", "-aq"}
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		args = append(args, "--filter", "label="+k+"="+labels[k])
	}
	out, err := exec.CommandContext(ctx, "docker", args...).Output()
	if err != nil {
		return 0, fmt.Errorf("docker ps: %w", exitErr(err))
	}
	ids := strings.Fields(string(out))
	if len(ids) == 0 {
		return 0, nil
	}
	rm, err := exec.CommandContext(ctx, "docker", append([]string{"rm", "-f"}, ids...)...).CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("docker rm -f: %w: %s", err, bytes.TrimSpace(rm))
	}
	return len(ids), nil
}

// CheckDocker reports whether the docker daemon is reachable and whether it
// runs rootless.
func CheckDocker(ctx context.Context) (rootless bool, err error) {
	if _, err := exec.LookPath("docker"); err != nil {
		return false, fmt.Errorf("docker is not installed; install Docker or run a2a-serve config set --sandbox local, or run a2a-serve doctor: %w", err)
	}
	out, err := exec.CommandContext(ctx, "docker", "info", "--format", "{{.SecurityOptions}}").Output()
	if err != nil {
		return false, fmt.Errorf("docker is not running; start Docker or run a2a-serve config set --sandbox local, or run a2a-serve doctor: %w", exitErr(err))
	}
	return bytes.Contains(out, []byte("rootless")), nil
}

func exitErr(err error) error {
	if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
		return fmt.Errorf("%w: %s", err, bytes.TrimSpace(ee.Stderr))
	}
	return err
}
