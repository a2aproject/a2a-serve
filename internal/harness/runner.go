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
	"github.com/a2aproject/a2a-serve/internal/sandboxnet"
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
	// WorkspaceSrc is the host directory mounted at the destination of the
	// first mount in its place. Empty mounts the first mount's source.
	WorkspaceSrc string
	// Hidden are host paths which appear as empty read-only directories
	// wherever a mount exposes them, such as the agent's .a2a directory.
	Hidden []string
	// Scratch are container paths which appear as empty directories, whose
	// changes stay in the container.
	Scratch []string
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
	// Egress is set when the container's only network path is the egress
	// socket. The launcher starts the command after the image's entrypoint.
	Egress *DockerEgress

	envFiles []string
}

var _ Runner = (*DockerRunner)(nil)

// DockerEgress gives a container no network but the egress socket.
type DockerEgress struct {
	// Socket is the host path of the egress socket, alone in its directory,
	// and Launcher the host path of the binary run as PID 1.
	Socket, Launcher string
	// Relay is the name of the relay container whose volumes have the
	// egress socket and the launcher, in place of Socket and Launcher.
	Relay string
	// Entrypoint is the image's entrypoint, which the launcher replaces.
	Entrypoint []string
	// ensure starts the relay container again when it doesn't run.
	ensure func() error
}

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
	cmd := exec.CommandContext(ctx, "sh", append([]string{"-c", startScript, r.Name}, r.runArgs(c, envFile, false)...)...)
	cmd.Env = os.Environ()
	cmd.Dir = os.TempDir()
	return cmd, nil
}

// fallbackScript starts a container as startScript does, after a check
// of its launcher's setup. $2, $3 and $4 are the quoted arguments of
// docker run for the check, for the container with the egress boundary,
// and for the fallback, which has Docker's own network. A failed check
// writes its output to marker $1, and that and every later start get the
// fallback. The check reads no stdin, which the container gets.
const fallbackScript = `name=$0 marker=$1
docker rm -f "$name" "$name-check" >/dev/null 2>&1
if [ ! -e "$marker" ] && ! out=$(eval "docker run --name \"\$name-check\" $2" </dev/null 2>&1); then
  printf '%s\n' "$out" >"$marker"
  printf 'a2a-serve: egress control is off, as the container can'"'"'t set up its network: %s\n' "$out" >&2
fi
if [ -e "$marker" ]; then eval "exec docker run --name \"\$name\" $4"; fi
eval "exec docker run --name \"\$name\" $3"`

// CommandWithFallback returns the process which runs c in the container of
// r once a check of its launcher's setup passes, or else in the container
// of fallback, from then on. The failed check leaves its output in marker.
func (r *DockerRunner) CommandWithFallback(ctx context.Context, c Command, fallback *DockerRunner, marker string) (*exec.Cmd, error) {
	envFile, err := r.writeEnvFile(append(slices.Clone(r.Env), c.Env...))
	if err != nil {
		return nil, err
	}
	fbEnvFile, err := fallback.writeEnvFile(append(slices.Clone(fallback.Env), c.Env...))
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", fallbackScript, r.Name, marker,
		shellQuote(r.runArgs(c, envFile, true)), shellQuote(r.runArgs(c, envFile, false)), shellQuote(fallback.runArgs(c, fbEnvFile, false)))
	cmd.Env = os.Environ()
	cmd.Dir = os.TempDir()
	return cmd, nil
}

// shellQuote joins args, each in single quotes, for sh to eval.
func shellQuote(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(quoted, " ")
}

// probeTimeout bounds the check of a container's egress setup.
const probeTimeout = time.Minute

// Probe runs a container with the flags of r's container whose launcher
// only sets up the network and drops its capabilities, and returns why
// that failed.
func (r *DockerRunner) Probe(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	envFile, err := r.writeEnvFile(slices.Clone(r.Env))
	if err != nil {
		return err
	}
	name := r.Name + "-check"
	if err := removeContainer(ctx, name); err != nil {
		return err
	}
	out, err := exec.CommandContext(ctx, "docker", append([]string{"run", "--name", name}, r.runArgs(Command{}, envFile, true)...)...).CombinedOutput()
	if err == nil {
		return nil
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return err
	}
	return errors.New(reasonOf(string(out)))
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
		if err := agentdir.CheckEnvValue(name, value); err != nil {
			return "", err
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
// With check, the launcher only checks its setup.
func (r *DockerRunner) runArgs(c Command, envFile string, check bool) []string {
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
	uid, gid := os.Getuid(), os.Getgid()
	if r.Rootless {
		uid, gid = 0, 0
	}
	args := []string{"--rm", "-i"}
	if r.Egress == nil {
		args = append(args,
			"--init",
			"--user", fmt.Sprintf("%d:%d", uid, gid),
			"--cap-drop", "ALL",
		)
	} else {
		// The launcher starts as root with what building the network and
		// switching to uid:gid take, and drops it all before the agent starts.
		args = append(args,
			"--user", "0:0",
			"--cap-drop", "ALL",
			"--cap-add", "NET_ADMIN", "--cap-add", "SETPCAP", "--cap-add", "SETUID", "--cap-add", "SETGID",
			"--network", "none", "--device", "/dev/net/tun",
		)
		if r.Egress.Relay != "" {
			args = append(args, "--entrypoint", sandboxnet.RelayLauncher, "--volumes-from", r.Egress.Relay+":ro")
		} else {
			args = append(args,
				"--entrypoint", sandboxnet.ContainerLauncher,
				"-v", r.Egress.Launcher+":"+sandboxnet.ContainerLauncher+":ro",
				"-v", filepath.Dir(r.Egress.Socket)+":"+sandboxnet.ContainerSocketDir+":ro",
			)
		}
	}
	args = append(args,
		"--security-opt", "no-new-privileges",
		"--memory", limits.Memory, "--cpus", limits.CPUs, "--pids-limit", strconv.Itoa(limits.PIDs),
	)
	if r.Egress == nil {
		args = append(args, "--add-host", "host.docker.internal:host-gateway")
	}
	for _, k := range slices.Sorted(maps.Keys(r.Labels)) {
		args = append(args, "--label", k+"="+r.Labels[k])
	}
	for i, m := range r.Mounts {
		if i == 0 && r.WorkspaceSrc != "" {
			m.Src = r.WorkspaceSrc
		}
		spec := m.Src + ":" + m.Dst
		if m.ReadOnly {
			spec += ":ro"
		}
		args = append(args, "-v", spec)
	}
	for _, dst := range hiddenDsts(r.Mounts, r.Hidden) {
		args = append(args, "--mount", "type=tmpfs,dst="+dst+",readonly")
	}
	for _, dst := range r.Scratch {
		args = append(args, "--mount", "type=tmpfs,dst="+dst)
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
	args = append(args, r.Image)
	if r.Egress != nil && check {
		return append(args, "sandbox-init", "--check", sandboxnet.ContainerSocketDir+"/"+sandboxnet.SocketName, strconv.Itoa(uid), strconv.Itoa(gid))
	}
	if r.Egress != nil {
		args = append(args, "sandbox-init", sandboxnet.ContainerSocketDir+"/"+sandboxnet.SocketName, strconv.Itoa(uid), strconv.Itoa(gid))
		args = append(args, r.Egress.Entrypoint...)
	}
	args = append(args, c.Path)
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
	return pruneContainers(ctx, labels, dockerTimeout)
}

func pruneContainers(ctx context.Context, labels map[string]string, timeout time.Duration) (int, error) {
	args := []string{"ps", "-aq"}
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		args = append(args, "--filter", "label="+k+"="+labels[k])
	}
	out, err := dockerQuery(ctx, timeout, args...)
	if err != nil {
		return 0, fmt.Errorf("docker ps: %w", err)
	}
	ids := strings.Fields(string(out))
	if len(ids) == 0 {
		return 0, nil
	}
	// -v removes the volumes of relay containers.
	rm, err := exec.CommandContext(ctx, "docker", append([]string{"rm", "-f", "-v"}, ids...)...).CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("docker rm -f: %w: %s", err, bytes.TrimSpace(rm))
	}
	return len(ids), nil
}

// CheckDocker reports whether the docker daemon is reachable and whether it
// runs rootless.
func CheckDocker(ctx context.Context) (rootless bool, err error) {
	return checkDocker(ctx, dockerTimeout)
}

func checkDocker(ctx context.Context, timeout time.Duration) (rootless bool, err error) {
	if _, err := exec.LookPath("docker"); err != nil {
		return false, fmt.Errorf("docker is not installed; install Docker or run a2a-serve config set --sandbox local, or run a2a-serve doctor: %w", err)
	}
	out, err := dockerQuery(ctx, timeout, "info", "--format", "{{.SecurityOptions}}")
	if errors.Is(err, ErrDockerNoAnswer) {
		return false, err
	}
	if host := os.Getenv("DOCKER_HOST"); err != nil && host != "" {
		return false, fmt.Errorf("docker is not reachable at %s from DOCKER_HOST; fix or unset DOCKER_HOST, or run a2a-serve config set --sandbox local, or run a2a-serve doctor: %w", host, err)
	}
	if err != nil {
		return false, fmt.Errorf("docker is not running; start Docker or run a2a-serve config set --sandbox local, or run a2a-serve doctor: %w", err)
	}
	return bytes.Contains(out, []byte("rootless")), nil
}

// dockerTimeout bounds the docker calls that a daemon answers at once.
const dockerTimeout = 10 * time.Second

// ErrDockerNoAnswer is returned when the docker daemon takes a call and
// doesn't answer it, as an endpoint that accepts connections and hangs does.
var ErrDockerNoAnswer = errors.New("docker does not answer")

// dockerQuery runs a docker command that the daemon answers at once, and
// fails with [ErrDockerNoAnswer] after timeout.
func dockerQuery(ctx context.Context, timeout time.Duration, args ...string) ([]byte, error) {
	qctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(qctx, "docker", args...)
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err == nil {
		return out, nil
	}
	if ctx.Err() != nil {
		return nil, fmt.Errorf("docker %s: %w", args[0], ctx.Err())
	}
	if qctx.Err() != nil {
		return nil, noAnswer(timeout)
	}
	return nil, exitErr(err)
}

func noAnswer(timeout time.Duration) error {
	if host := os.Getenv("DOCKER_HOST"); host != "" {
		return fmt.Errorf("%w within %s at %s from DOCKER_HOST; fix or unset DOCKER_HOST, or run a2a-serve config set --sandbox local, or run a2a-serve doctor", ErrDockerNoAnswer, timeout, host)
	}
	return fmt.Errorf("%w within %s; restart Docker or run a2a-serve config set --sandbox local, or run a2a-serve doctor", ErrDockerNoAnswer, timeout)
}

func exitErr(err error) error {
	if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
		return fmt.Errorf("%w: %s", err, bytes.TrimSpace(ee.Stderr))
	}
	return err
}
