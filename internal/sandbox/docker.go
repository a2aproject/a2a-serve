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
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/a2aproject/a2a-serve/internal/fsutil"
	"github.com/a2aproject/a2a-serve/internal/sandboxnet"
)

// DockerRunner runs a process in a container.
type DockerRunner struct {
	// Name is the container name.
	Name   string
	Image  string
	Mounts []Mount
	// WorkspaceSrc is the host directory mounted at the destination of the
	// first mount in its place. Empty mounts the first mount's source.
	WorkspaceSrc string
	// Hidden are host paths which appear as empty read-only directories
	// wherever a mount exposes them.
	Hidden []string
	// Scratch are container paths which appear as empty directories, whose
	// changes stay in the container.
	Scratch []string
	// Home is the host directory mounted at /home/agent.
	Home string
	// EnvDir is the host directory for the env files, which hold the secrets
	// in Env until [DockerRunner.Stop]. A server that is killed leaves them
	// there, for [PruneEnvFiles]. It must not be visible in the container.
	EnvDir string
	// Env holds NAME=value pairs passed into the container. Values are passed
	// in a private env file, not in the arguments or the environment of the
	// docker CLI, so they neither show up in process listings nor change
	// which docker the host runs.
	Env    []string
	Limits Limits
	Labels map[string]string
	// Rootless is set for rootless Docker, where container root is the host
	// user, so files the process writes stay owned by the user.
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
	// Ensure starts the relay container again when it doesn't run.
	Ensure func() error
}

// DefaultLimits are the limits of a container that sets none.
var DefaultLimits = Limits{Memory: "4g", CPUs: "2", PIDs: 512}

// LineBreakError reports an environment variable whose value has a line
// break.
type LineBreakError struct{ Name string }

func (e *LineBreakError) Error() string {
	return fmt.Sprintf("the value of %s has a line break, which docker can't pass to the container", e.Name)
}

// CheckEnvValue returns a [*LineBreakError] if docker can't pass value as
// the environment variable name, since its env file holds one per line.
func CheckEnvValue(name, value string) error {
	if strings.ContainsAny(value, "\r\n") {
		return &LineBreakError{Name: name}
	}
	return nil
}

// ContainerHome is where a container mounts its home directory.
const ContainerHome = "/home/agent"

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
	return r.startCommand(ctx, r.runArgs(c, envFile, false)), nil
}

func (r *DockerRunner) startCommand(ctx context.Context, runArgs []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "sh", append([]string{"-c", startScript, r.Name}, runArgs...)...)
	cmd.Env = os.Environ()
	cmd.Dir = os.TempDir()
	return cmd
}

// Sandbox returns the [Sandbox] that runs c in the container of r. With
// checkBoundary, every start first runs the check of the launcher's setup,
// and fails with an [*EgressSetupError] when the setup can't work on this
// host. Close calls [DockerRunner.Stop].
func (r *DockerRunner) Sandbox(c Command, checkBoundary bool, opts Options) (Sandbox, error) {
	envFile, err := r.writeEnvFile(append(slices.Clone(r.Env), c.Env...))
	if err != nil {
		return nil, err
	}
	return &dockerSandbox{r: r, runArgs: r.runArgs(c, envFile, false), envFile: envFile, checkBoundary: checkBoundary && r.Egress != nil, opts: opts}, nil
}

type dockerSandbox struct {
	r             *DockerRunner
	runArgs       []string
	envFile       string
	checkBoundary bool
	opts          Options
}

func (s *dockerSandbox) Start(ctx context.Context) (io.ReadWriteCloser, error) {
	if err := s.opts.check(); err != nil {
		return nil, err
	}
	if s.checkBoundary {
		if err := s.r.probe(ctx, s.envFile); err != nil {
			if errors.Is(err, ErrNoBoundary) {
				return nil, err
			}
			return nil, fmt.Errorf("checking the network setup of the container: %w", err)
		}
	}
	return startProcess(s.r.startCommand(context.WithoutCancel(ctx), s.runArgs), s.opts.Stderr)
}

func (s *dockerSandbox) Close(ctx context.Context) error { return s.r.Stop(ctx) }

// EgressSetupError is the failure of a container to set up its network on
// this host, which the server answers by giving the container Docker's
// own network.
type EgressSetupError struct{ Reason string }

func (e *EgressSetupError) Error() string { return e.Reason }

// Unwrap returns [ErrNoBoundary].
func (e *EgressSetupError) Unwrap() error { return ErrNoBoundary }

// runtimeFailedExits are the exit codes of docker run when the container's
// command didn't run, and setupRefusals what its output names when the
// runtime refuses the device or the capability that the boundary takes.
var (
	runtimeFailedExits = []int{125, 126, 127}
	setupRefusals      = []string{"/dev/net/tun", "NET_ADMIN"}
)

// setupFailed reports whether a check that exited with code and printed
// out found that the container can't set up its network on this host: the
// launcher says so, or the runtime refuses what the boundary takes.
func setupFailed(code int, out string) bool {
	if code == sandboxnet.SetupFailedExit {
		return true
	}
	if !slices.Contains(runtimeFailedExits, code) {
		return false
	}
	return slices.ContainsFunc(setupRefusals, func(s string) bool { return strings.Contains(out, s) })
}

// reasonOf returns the launcher's setup error in the output of a check, or
// its first line.
func reasonOf(out string) string {
	for line := range strings.Lines(out) {
		if reason, ok := strings.CutPrefix(strings.TrimSpace(line), "a2a-init: setting up the sandbox's network: "); ok {
			return reason
		}
	}
	return cmp.Or(firstLine(strings.TrimSpace(out)), "it failed without output")
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// probeTimeout bounds each check of a container's egress setup, and
// probeAttempts the checks that fail for another reason, such as an error
// of the Docker daemon, probeRetryDelay apart.
const (
	probeTimeout    = time.Minute
	probeAttempts   = 3
	probeRetryDelay = time.Second
)

// Probe runs a container with the flags of r's container whose launcher
// only sets up the network and drops its capabilities. It returns an
// [*EgressSetupError] when the setup can't work on this host, and another
// error when the check kept failing for another reason. A check that runs
// out of time isn't run again, which would only multiply the wait.
func (r *DockerRunner) Probe(ctx context.Context) error {
	envFile, err := r.writeEnvFile(slices.Clone(r.Env))
	if err != nil {
		return err
	}
	for attempt := 1; ; attempt++ {
		err := r.probe(ctx, envFile)
		if err == nil || errors.Is(err, ErrNoBoundary) {
			return err
		}
		if attempt == probeAttempts || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
			return err
		}
		slog.Warn("checking the egress setup of the agent's container failed; trying again", "error", err)
		select {
		case <-ctx.Done():
			return err
		case <-time.After(probeRetryDelay):
		}
	}
}

func (r *DockerRunner) probe(ctx context.Context, envFile string) error {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	name := r.Name + "-check"
	if err := RemoveContainer(ctx, name); err != nil {
		return err
	}
	out, err := exec.CommandContext(ctx, "docker", append([]string{"run", "--name", name}, r.runArgs(Command{}, envFile, true)...)...).CombinedOutput()
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return fmt.Errorf("the check didn't finish: %w", ctx.Err())
	}
	if ee, ok := errors.AsType[*exec.ExitError](err); ok && setupFailed(ee.ExitCode(), string(out)) {
		return &EgressSetupError{Reason: reasonOf(string(out))}
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return err
	}
	return fmt.Errorf("%w: %s", err, reasonOf(string(out)))
}

// envFilePrefix starts the names of the env files.
const envFilePrefix = "a2a-env-"

// PruneEnvFiles removes the env files that an earlier server left in dir,
// such as after a crash or a kill, and returns how many it removed. Call it
// only while no container that writes to dir starts.
func PruneEnvFiles(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var errs []error
	n := 0
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), envFilePrefix) || e.IsDir() {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
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
		if err := CheckEnvValue(name, value); err != nil {
			return "", err
		}
		buf.WriteString(name + "=" + value + "\n")
	}
	if buf.Len() == 0 {
		return "", nil
	}
	if r.EnvDir == "" {
		return "", errors.New("creating the container env file: no directory for it")
	}
	if err := fsutil.MkdirReal(r.EnvDir); err != nil {
		return "", fmt.Errorf("creating the container env file: %w", err)
	}
	f, err := os.CreateTemp(r.EnvDir, envFilePrefix+"*")
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
		limits.Memory = DefaultLimits.Memory
	}
	if limits.CPUs == "" {
		limits.CPUs = DefaultLimits.CPUs
	}
	if limits.PIDs == 0 {
		limits.PIDs = DefaultLimits.PIDs
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
		// switching to uid:gid take, and drops it all before the command starts.
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
	for _, dst := range ContainerPaths(r.Mounts, r.Hidden) {
		args = append(args, "--mount", "type=tmpfs,dst="+dst+",readonly")
	}
	for _, dst := range r.Scratch {
		args = append(args, "--mount", "type=tmpfs,dst="+dst)
	}
	if r.Home != "" {
		args = append(args, "-v", r.Home+":"+ContainerHome)
	}
	// -e takes precedence over --env-file, so the process keeps its home.
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

// ContainerPaths returns the container paths at which mounts expose the
// host paths. Paths are compared with symlinks resolved, as docker resolves
// them in bind sources.
func ContainerPaths(mounts []Mount, paths []string) []string {
	var out []string
	for _, h := range paths {
		h = fsutil.ResolvePath(h)
		for _, m := range mounts {
			rel, err := filepath.Rel(fsutil.ResolvePath(m.Src), h)
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

// RemoveContainer removes the container name, if any, with its volumes.
func RemoveContainer(ctx context.Context, name string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), containerStopTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "rm", "-f", "-v", name).CombinedOutput()
	if err != nil && !bytes.Contains(out, []byte("No such container")) {
		return fmt.Errorf("docker rm -f %s: %w: %s", name, err, bytes.TrimSpace(out))
	}
	return nil
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
	return pruneContainers(ctx, labels, DockerTimeout)
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
	return checkDocker(ctx, DockerTimeout)
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

// DockerTimeout bounds the docker calls that a daemon answers at once.
const DockerTimeout = 10 * time.Second

// ErrDockerNoAnswer is returned when the docker daemon takes a call and
// doesn't answer it, as an endpoint that accepts connections and hangs does.
var ErrDockerNoAnswer = errors.New("docker does not answer")

// DockerQuery runs a docker command that the daemon answers at once, and
// fails with [ErrDockerNoAnswer] after [DockerTimeout].
func DockerQuery(ctx context.Context, args ...string) ([]byte, error) {
	return dockerQuery(ctx, DockerTimeout, args...)
}

// dockerQuery is [DockerQuery] with its own timeout.
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
