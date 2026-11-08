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
	"maps"
	"os"
	"os/exec"
	"regexp"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"github.com/a2aproject/a2a-serve/internal/sandboxnet"
)

// With Docker Desktop, which can't mount the server's sockets, a relay
// container next to each agent's container serves the agent's sockets in
// volumes the agent's container takes with --volumes-from, and forwards
// their connections to the server. Its image also holds the Linux
// a2a-serve that the agent's container runs as its launcher.
const (
	// RelayImageRepo is where the relay image is published, tagged with
	// the version of a2a-serve, or sha-<commit> for builds of main.
	RelayImageRepo = "ghcr.io/a2aproject/a2a-serve"
	// EnvRelayImage replaces the relay image, such as with one built from
	// a work tree.
	EnvRelayImage = "A2A_SERVE_SANDBOX_IMAGE"
	// EnvForceRelay set to 1 gives Linux containers a relay as with Docker
	// Desktop, to test it there.
	EnvForceRelay = "A2A_SERVE_SANDBOX_RELAY"
	// relaySource is the repository the relay image is built from when it
	// can't be pulled.
	relaySource = "https://github.com/a2aproject/a2a-serve.git"
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

// relayImageRef returns the relay image of this a2a-serve, and the git
// ref to build it from when it can't be pulled, or "" when there is none.
func relayImageRef() (image, ref string, err error) {
	if v := os.Getenv(EnvRelayImage); v != "" {
		return v, "", nil
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "", "", errors.New("this a2a-serve doesn't know its version; set " + EnvRelayImage + " to an image built with " + relayDockerfile)
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
	if len(rev) < 12 {
		return "", "", errors.New("this a2a-serve doesn't know its commit; set " + EnvRelayImage + " to an image built with " + relayDockerfile)
	}
	return RelayImageRepo + ":sha-" + rev[:12], rev, nil
}

// EnsureRelayImage makes the relay image of this a2a-serve available: it
// uses a local copy, pulls it, or builds it from the repository at this
// version.
func EnsureRelayImage(ctx context.Context, progress io.Writer) (string, error) {
	image, ref, err := relayImageRef()
	if err != nil {
		return "", err
	}
	if imageExists(ctx, image) {
		return image, nil
	}
	pullErr := pullImage(ctx, image, progress)
	if pullErr == nil {
		return image, nil
	}
	if ref == "" {
		return "", fmt.Errorf("docker pull %s: %w", image, pullErr)
	}
	build := exec.CommandContext(ctx, "docker", "build", "-t", image, "-f", relayDockerfile, "--build-arg", "VERSION="+ref, relaySource+"#"+ref)
	build.Stdout, build.Stderr = progress, progress
	if err := build.Run(); err != nil {
		return "", fmt.Errorf("docker pull %s failed (%v), and building it from %s#%s failed: %w", image, pullErr, relaySource, ref, err)
	}
	return image, nil
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
	args := []string{"run", "-d", "--rm", "--name", spec.name,
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
		return nil, fmt.Errorf("starting the relay container: %w: %s", err, bytes.TrimSpace(out))
	}
	if err := waitForRelay(ctx, spec.name); err != nil {
		return nil, errors.Join(err, stop(ctx))
	}
	return stop, nil
}

// waitForRelay waits until the relay container name prints that it
// listens.
func waitForRelay(ctx context.Context, name string) error {
	deadline := time.Now().Add(relayReadyTimeout)
	for {
		out, err := exec.CommandContext(ctx, "docker", "logs", name).CombinedOutput()
		if err == nil && bytes.Contains(out, []byte(sandboxnet.RelayReady)) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the relay container %s didn't start within %s: %s", name, relayReadyTimeout, bytes.TrimSpace(out))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
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
