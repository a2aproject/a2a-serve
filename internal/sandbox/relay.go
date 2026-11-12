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
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"golang.org/x/mod/module"

	"github.com/a2aproject/a2a-serve/internal/sandboxnet"
)

// With Docker Desktop, which can't mount the server's sockets, a relay
// container next to each agent's container serves the agent's sockets in
// volumes the agent's container takes with --volumes-from, and forwards
// their connections to the server. Its image also holds the Linux
// a2a-serve that the agent's container runs as its launcher. Users know
// it as the egress container, as "relay" names Agent Mesh relays.
const (
	// RelayImageRepo is where releases publish the relay image, tagged
	// with their version. Other builds name it sha-<commit>, and build it
	// when nobody published it.
	RelayImageRepo = "ghcr.io/a2aproject/a2a-serve"
	// EnvRelayImage replaces the image of the egress containers, such as
	// with one built from a work tree.
	EnvRelayImage = "A2A_SERVE_EGRESS_IMAGE"
	// EnvForceRelay set to 1 gives Linux containers a relay as with Docker
	// Desktop, to test it there.
	EnvForceRelay = "A2A_SERVE_SANDBOX_RELAY"
	// relaySource is the repository the relay image is built from when it
	// can't be pulled, and relayCommits its commits API, which gives the
	// full commit of a pseudo-version's short one.
	relaySource  = "https://github.com/a2aproject/a2a-serve.git"
	relayCommits = "https://api.github.com/repos/a2aproject/a2a-serve/commits/"
	// relayDockerfile is the Dockerfile of the relay image in the repository.
	relayDockerfile = "internal/sandboxnet/Dockerfile"
	// relayServerDir and RelayEgressDir are where a relay on Linux gets
	// the server's sockets.
	relayServerDir = "/run/a2a-server"
	RelayEgressDir = "/run/a2a-relay"
	// relayReadyTimeout bounds waiting for a relay to listen.
	relayReadyTimeout = 30 * time.Second
)

var releaseVersion = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

// relayImageRef returns the relay image of the a2a-serve of info, and the
// git ref to build it from when it can't be pulled, or "" when there is
// none. A build from a work tree records its commit, and one with go
// install the commit in its pseudo-version.
func relayImageRef(info *debug.BuildInfo) (image, ref string, err error) {
	unknown := errors.New("this a2a-serve doesn't know its commit; set " + EnvRelayImage + " to an image built from a2a-serve's repository with docker build -f " + relayDockerfile)
	if info == nil {
		return "", "", unknown
	}
	if releaseVersion.MatchString(info.Main.Version) {
		return RelayImageRepo + ":" + info.Main.Version, info.Main.Version, nil
	}
	var rev string
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" {
			rev = s.Value
		}
	}
	if rev == "" && module.IsPseudoVersion(info.Main.Version) {
		rev, _ = module.PseudoVersionRev(info.Main.Version)
	}
	if len(rev) < 12 {
		return "", "", unknown
	}
	return RelayImageRepo + ":sha-" + rev[:12], rev, nil
}

// thisRelayImage returns the relay image of this a2a-serve, or the one in
// $A2A_SERVE_EGRESS_IMAGE.
func thisRelayImage() (image, ref string, err error) {
	if v := os.Getenv(EnvRelayImage); v != "" {
		return v, "", nil
	}
	info, _ := debug.ReadBuildInfo()
	return relayImageRef(info)
}

// CheckRelayImage checks that the relay image of this a2a-serve is on
// this machine, or with pull that it can be pulled. An image that the next
// start of a container can pull or build is a [MissingRelayImage].
func CheckRelayImage(ctx context.Context, run func(ctx context.Context, name string, args ...string) ([]byte, error), pull bool) error {
	image, ref, err := thisRelayImage()
	if err != nil {
		return err
	}
	if _, err := run(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", image); err == nil {
		return nil
	}
	missing := &MissingRelayImage{Image: image, Pull: !pull, Build: ref != ""}
	if pull {
		if _, err := run(ctx, "docker", "manifest", "inspect", image); err == nil {
			return nil
		}
		if ref == "" {
			return fmt.Errorf("the egress container's image %s is neither on this machine nor in the registry", image)
		}
	}
	return missing
}

// MissingRelayImage is a relay image that isn't on this machine, which the
// next start of a container pulls or builds.
type MissingRelayImage struct {
	Image       string
	Pull, Build bool
}

func (e *MissingRelayImage) Error() string {
	const next = "the next start of the agent's container"
	switch {
	case e.Pull && e.Build:
		return fmt.Sprintf("the egress container's image %s isn't on this machine; %s pulls it, or builds it from the a2a-serve repository", e.Image, next)
	case e.Pull:
		return fmt.Sprintf("the egress container's image %s isn't on this machine; %s pulls it", e.Image, next)
	}
	return fmt.Sprintf("the egress container's image %s is neither on this machine nor in the registry; %s builds it from the a2a-serve repository, which takes a few minutes", e.Image, next)
}

// EnsureRelayImage makes the relay image of this a2a-serve available: it
// uses a local copy, pulls it, or builds it from the repository at this
// version. Docker's output goes to log.
func EnsureRelayImage(ctx context.Context, log io.Writer) (string, error) {
	image, ref, err := thisRelayImage()
	if err != nil {
		return "", err
	}
	resolve := func(ctx context.Context, short string) (string, error) {
		return resolveCommit(ctx, http.DefaultClient, relayCommits, short)
	}
	return ensureRelayImage(ctx, log, image, ref, resolve)
}

// ensureRelayImage makes image available, building it from the git ref
// when it can't be pulled. resolve returns the full commit of a short one.
func ensureRelayImage(ctx context.Context, log io.Writer, image, ref string, resolve func(ctx context.Context, short string) (string, error)) (string, error) {
	if ImageExists(ctx, image) {
		return image, nil
	}
	pullErr := PullImage(ctx, image, log)
	if pullErr == nil {
		return image, nil
	}
	if ref == "" {
		return "", fmt.Errorf("docker pull %s: %s", image, firstLine(pullErr.Error()))
	}
	if !releaseVersion.MatchString(ref) && !fullCommit.MatchString(ref) {
		full, err := resolve(ctx, ref)
		if err != nil {
			return "", fmt.Errorf("docker pull %s failed (%s), and finding the commit %s to build it from failed: %w", image, firstLine(pullErr.Error()), ref, err)
		}
		ref = full
	}
	build := exec.CommandContext(ctx, "docker", "build", "-t", image, "-f", relayDockerfile, "--build-arg", "VERSION="+strings.TrimPrefix(ref, "v"), relaySource+"#"+ref)
	build.Stdout, build.Stderr = log, log
	if err := build.Run(); err != nil {
		return "", fmt.Errorf("docker pull %s failed (%s), and building it from %s#%s failed: %w", image, firstLine(pullErr.Error()), relaySource, ref, err)
	}
	return image, nil
}

// fullCommit matches the commit IDs that BuildKit's git source checks out;
// it reads a shorter ID as the name of a branch or a tag.
var fullCommit = regexp.MustCompile(`^[0-9a-f]{40}$`)

// resolveCommit returns the full ID of the commit short in the GitHub
// repository whose commits API is api.
func resolveCommit(ctx context.Context, client *http.Client, api, short string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, DockerTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, api+url.PathEscape(short), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github.sha")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			slog.Debug("closing the response of "+api, "error", err)
		}
	}()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s answered %s", api+short, resp.Status)
	}
	full := strings.TrimSpace(string(body))
	if !fullCommit.MatchString(full) || !strings.HasPrefix(full, short) {
		return "", fmt.Errorf("%s answered %q, which isn't the commit %s", api+short, firstLine(full), short)
	}
	return full, nil
}

// LogLines writes each line of Docker's output to the server log.
type LogLines struct{ Msg string }

func (l LogLines) Write(p []byte) (int, error) {
	for line := range strings.Lines(string(p)) {
		if line = strings.TrimSpace(line); line != "" {
			slog.Info(l.Msg, "output", line)
		}
	}
	return len(p), nil
}

// RelaySpec configures a relay container.
type RelaySpec struct {
	Name, Image string
	Labels      map[string]string
	// Rootless is set for rootless Docker, as in [DockerRunner].
	Rootless bool
	// Egress is the endpoint of the container's boundary.
	Egress EgressEndpoint
	// ServerSocket is the host path of the socket through which the agent
	// reaches the server on Linux, and ServerAddr the host:port of the
	// server's TCP listener with Docker Desktop. Both are empty for a
	// container that doesn't reach the server.
	ServerSocket, ServerAddr string
}

// StartRelay starts the relay container of spec, and returns the egress of
// the containers that use it and a function that removes it.
func StartRelay(ctx context.Context, spec RelaySpec, entrypoint []string) (*DockerEgress, func(context.Context) error, error) {
	stop, err := startRelay(ctx, spec)
	if err != nil {
		return nil, nil, err
	}
	ensure := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), relayReadyTimeout+DockerTimeout)
		defer cancel()
		return ensureRelay(ctx, spec)
	}
	return &DockerEgress{Relay: spec.Name, Entrypoint: entrypoint, Ensure: ensure}, stop, nil
}

// startRelay starts the relay container of spec and waits until it
// listens. stop removes it with its volumes.
func startRelay(ctx context.Context, spec RelaySpec) (stop func(context.Context) error, err error) {
	stop = func(ctx context.Context) error { return RemoveContainer(ctx, spec.Name) }
	if err := stop(ctx); err != nil {
		return nil, err
	}
	keyFile, err := os.CreateTemp("", "a2a-relay-*")
	if err != nil {
		return nil, err
	}
	defer func() {
		if rerr := os.Remove(keyFile.Name()); rerr != nil {
			err = errors.Join(err, rerr)
		}
	}()
	if _, err := keyFile.WriteString("A2A_SERVE_RELAY_KEY=" + spec.Egress.RelayKey + "\n"); err != nil {
		return nil, errors.Join(err, keyFile.Close())
	}
	if err := keyFile.Close(); err != nil {
		return nil, err
	}
	// A relay that crashes or outlives a restart of Docker restarts with
	// its volumes, where the agent's container keeps its sockets.
	args := []string{"run", "-d", "--restart", "unless-stopped", "--name", spec.Name,
		"--user", containerUser(spec.Rootless),
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--add-host", "host.docker.internal:host-gateway",
		"-v", sandboxnet.ContainerSocketDir, "-v", "/opt/a2a",
		"--env-file", keyFile.Name(),
	}
	for _, k := range slices.Sorted(maps.Keys(spec.Labels)) {
		args = append(args, "--label", k+"="+spec.Labels[k])
	}
	egressTo := spec.Egress.RelayAddr
	if spec.Egress.RelayMount != "" {
		args = append(args, "-v", spec.Egress.RelayMount+":"+RelayEgressDir+":ro")
	}
	relayArgs := []string{"sandbox-relay",
		"--egress-listen", sandboxnet.ContainerSocketDir + "/" + sandboxnet.SocketName,
		"--egress-to", egressTo,
	}
	serverListen := sandboxnet.ContainerSocketDir + "/" + sandboxnet.ServerSocketName
	switch {
	case spec.ServerSocket != "":
		dir, base := splitSocket(spec.ServerSocket)
		args = append(args, "-v", dir+":"+relayServerDir+":ro")
		relayArgs = append(relayArgs, "--server-listen", serverListen, "--server-to", "unix://"+relayServerDir+"/"+base)
	case spec.ServerAddr != "":
		relayArgs = append(relayArgs, "--server-listen", serverListen, "--server-to", "tcp://"+spec.ServerAddr)
	}
	args = append(append(args, spec.Image), relayArgs...)
	if out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("starting the egress container: %w: %s", err, firstLine(string(bytes.TrimSpace(out))))
	}
	if err := waitForRelay(ctx, spec.Name); err != nil {
		return nil, errors.Join(err, stop(ctx))
	}
	return stop, nil
}

// waitForRelay waits until the relay container name prints that it
// listens, and fails at once with its output when it stops or restarts.
func waitForRelay(ctx context.Context, name string) error {
	deadline := time.Now().Add(relayReadyTimeout)
	for {
		out, err := exec.CommandContext(ctx, "docker", "logs", name).CombinedOutput()
		if err == nil && bytes.Contains(out, []byte(sandboxnet.RelayReady)) {
			return nil
		}
		state, err := DockerQuery(ctx, "inspect", "--format", "{{.State.Running}} {{.RestartCount}} {{.State.ExitCode}}", name)
		if err != nil {
			return fmt.Errorf("the egress container %s is gone: %w", name, err)
		}
		if f := strings.Fields(string(state)); len(f) == 3 && (f[0] != "true" || f[1] != "0") {
			return fmt.Errorf("the egress container %s exited with status %s: %s", name, f[2], firstLine(string(bytes.TrimSpace(out))))
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the egress container %s didn't start within %s: %s", name, relayReadyTimeout, firstLine(string(bytes.TrimSpace(out))))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// ensureRelay starts the relay container of spec again when it doesn't
// run, such as after it was removed.
func ensureRelay(ctx context.Context, spec RelaySpec) error {
	state, err := DockerQuery(ctx, "inspect", "--format", "{{.State.Running}}", spec.Name)
	if err == nil && strings.TrimSpace(string(state)) == "true" {
		return nil
	}
	slog.Warn("starting the egress container again", "container", spec.Name)
	_, err = startRelay(ctx, spec)
	return err
}

func splitSocket(path string) (dir, base string) {
	i := strings.LastIndexByte(path, '/')
	return path[:i], path[i+1:]
}
