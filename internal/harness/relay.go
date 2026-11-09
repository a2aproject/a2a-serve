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
// this machine, or with pull that it can be pulled.
func CheckRelayImage(ctx context.Context, run func(ctx context.Context, name string, args ...string) ([]byte, error), pull bool) error {
	image, _, err := thisRelayImage()
	if err != nil {
		return err
	}
	if _, err := run(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", image); err == nil {
		return nil
	}
	if !pull {
		return &missingRelayImage{image: image, pull: true}
	}
	if _, err := run(ctx, "docker", "manifest", "inspect", image); err == nil {
		return nil
	}
	return &missingRelayImage{image: image}
}

// missingRelayImage is a relay image that the next start pulls, or builds
// when it isn't in the registry.
type missingRelayImage struct {
	image string
	pull  bool
}

func (e *missingRelayImage) Error() string {
	if e.pull {
		return fmt.Sprintf("the egress container's image %s isn't on this machine; the next start pulls it, or builds it from the a2a-serve repository", e.image)
	}
	return fmt.Sprintf("the egress container's image %s is neither on this machine nor in the registry; the next start builds it from the a2a-serve repository, which takes a few minutes", e.image)
}

// EnsureRelayImage makes the relay image of this a2a-serve available: it
// uses a local copy, pulls it, or builds it from the repository at this
// version. Docker's output goes to log.
func EnsureRelayImage(ctx context.Context, log io.Writer) (string, error) {
	image, ref, err := thisRelayImage()
	if err != nil {
		return "", err
	}
	if imageExists(ctx, image) {
		return image, nil
	}
	pullErr := pullImage(ctx, image, log)
	if pullErr == nil {
		return image, nil
	}
	if ref == "" {
		return "", fmt.Errorf("docker pull %s: %s", image, firstLine(pullErr.Error()))
	}
	if !releaseVersion.MatchString(ref) && !fullCommit.MatchString(ref) {
		full, err := resolveCommit(ctx, http.DefaultClient, relayCommits, ref)
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
	ctx, cancel := context.WithTimeout(ctx, dockerTimeout)
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

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// logLines writes each line of Docker's output to the server log.
type logLines struct{ msg string }

func (l logLines) Write(p []byte) (int, error) {
	for line := range strings.Lines(string(p)) {
		if line = strings.TrimSpace(line); line != "" {
			slog.Info(l.msg, "output", line)
		}
	}
	return len(p), nil
}

// relaySpec configures a relay container.
type relaySpec struct {
	name, image string
	labels      map[string]string
	uid, gid    int
	// egress is the endpoint of the container's boundary.
	egress EgressEndpoint
	// serverSocket is the host path of the socket through which the agent
	// reaches the server on Linux, and serverAddr the host:port of the
	// server's TCP listener with Docker Desktop. Both are empty for a
	// container that doesn't reach the server.
	serverSocket, serverAddr string
}

// startRelay starts the relay container of spec and waits until it
// listens. stop removes it with its volumes.
func startRelay(ctx context.Context, spec relaySpec) (stop func(context.Context) error, err error) {
	stop = func(ctx context.Context) error { return removeContainer(ctx, spec.name) }
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
	if _, err := keyFile.WriteString("A2A_SERVE_RELAY_KEY=" + spec.egress.RelayKey + "\n"); err != nil {
		return nil, errors.Join(err, keyFile.Close())
	}
	if err := keyFile.Close(); err != nil {
		return nil, err
	}
	// A relay that crashes or outlives a restart of Docker restarts with
	// its volumes, where the agent's container keeps its sockets.
	args := []string{"run", "-d", "--restart", "unless-stopped", "--name", spec.name,
		"--user", fmt.Sprintf("%d:%d", spec.uid, spec.gid),
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--add-host", "host.docker.internal:host-gateway",
		"-v", sandboxnet.ContainerSocketDir, "-v", "/opt/a2a",
		"--env-file", keyFile.Name(),
	}
	for _, k := range slices.Sorted(maps.Keys(spec.labels)) {
		args = append(args, "--label", k+"="+spec.labels[k])
	}
	egressTo := spec.egress.RelayAddr
	if spec.egress.RelayMount != "" {
		args = append(args, "-v", spec.egress.RelayMount+":"+RelayEgressDir+":ro")
	}
	relayArgs := []string{"sandbox-relay",
		"--egress-listen", sandboxnet.ContainerSocketDir + "/" + sandboxnet.SocketName,
		"--egress-to", egressTo,
	}
	serverListen := sandboxnet.ContainerSocketDir + "/" + sandboxnet.ServerSocketName
	switch {
	case spec.serverSocket != "":
		dir, base := splitSocket(spec.serverSocket)
		args = append(args, "-v", dir+":"+relayServerDir+":ro")
		relayArgs = append(relayArgs, "--server-listen", serverListen, "--server-to", "unix://"+relayServerDir+"/"+base)
	case spec.serverAddr != "":
		relayArgs = append(relayArgs, "--server-listen", serverListen, "--server-to", "tcp://"+spec.serverAddr)
	}
	args = append(append(args, spec.image), relayArgs...)
	if out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("starting the egress container: %w: %s", err, firstLine(string(bytes.TrimSpace(out))))
	}
	if err := waitForRelay(ctx, spec.name); err != nil {
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
		state, err := dockerQuery(ctx, dockerTimeout, "inspect", "--format", "{{.State.Running}} {{.RestartCount}} {{.State.ExitCode}}", name)
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
func ensureRelay(ctx context.Context, spec relaySpec) error {
	state, err := dockerQuery(ctx, dockerTimeout, "inspect", "--format", "{{.State.Running}}", spec.name)
	if err == nil && strings.TrimSpace(string(state)) == "true" {
		return nil
	}
	slog.Warn("starting the egress container again", "container", spec.name)
	_, err = startRelay(ctx, spec)
	return err
}

// removeContainer removes the container name, if any, with its volumes.
func removeContainer(ctx context.Context, name string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), containerStopTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "rm", "-f", "-v", name).CombinedOutput()
	if err != nil && !bytes.Contains(out, []byte("No such container")) {
		return fmt.Errorf("docker rm -f %s: %w: %s", name, err, bytes.TrimSpace(out))
	}
	return nil
}

func splitSocket(path string) (dir, base string) {
	i := strings.LastIndexByte(path, '/')
	return path[:i], path[i+1:]
}
