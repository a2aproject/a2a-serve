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
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/a2aproject/a2a-serve/internal/sandboxnet"
)

// exposeDir is where a container mounts the directory of an exposed socket.
const exposeDir = "/run/a2a"

// Docker is the backend that runs commands in Docker containers.
type Docker struct {
	envDir   string
	launcher func() string

	// probeMu serializes the check of the containers' egress setup, which
	// runs once for the backend, and probed and probeErr are its result.
	probeMu  sync.Mutex
	probed   bool
	probeErr error
}

var _ Backend = (*Docker)(nil)

// NewDocker returns the Docker backend, which keeps the env files of its
// containers in envDir, where no container must see them. launcher returns
// the host path of the a2a-serve binary that containers with the egress
// boundary run as their launcher, or "" to use this binary; nil always
// uses this binary.
func NewDocker(envDir string, launcher func() string) *Docker {
	return &Docker{envDir: envDir, launcher: launcher}
}

// Probe implements [Backend]: the docker daemon answers, and whether the
// egress boundary can work with an image of this host's architecture.
func (d *Docker) Probe(ctx context.Context) (Capabilities, error) {
	rootless, err := CheckDocker(ctx)
	if err != nil {
		return Capabilities{}, err
	}
	c := d.egressEnv().Probe()
	c.Rootless = rootless
	return c, nil
}

// egressEnv returns the environment of this process, with the launcher of d.
func (d *Docker) egressEnv() EgressEnv {
	env := HostEgressEnv()
	if d.launcher == nil {
		return env
	}
	if l := d.launcher(); l != "" {
		env.Launcher = func() (string, error) { return CheckLauncher(l) }
	}
	return env
}

// Prepare implements [Backend]. It pulls or builds the image and, with a
// boundary, opens it and checks that the container can set up its network,
// once for the backend.
func (d *Docker) Prepare(ctx context.Context, spec Spec) (Sandbox, error) {
	if len(spec.Expose) > 1 {
		return nil, errors.New("a Docker sandbox exposes at most one socket")
	}
	rootless, err := CheckDocker(ctx)
	if err != nil {
		return nil, err
	}
	image, what, err := EnsureImage(ctx, spec.Image, spec.Build, spec.Progress)
	if err != nil {
		return nil, err
	}
	r := &DockerRunner{
		Name:     spec.Name,
		Image:    image,
		Mounts:   slices.Clone(spec.Mounts),
		Masks:    spec.Masks,
		Scratch:  spec.Scratch,
		Home:     spec.Home,
		EnvDir:   d.envDir,
		Env:      withoutEgressCA(spec.Command.Env),
		Limits:   spec.Limits,
		Labels:   spec.Labels,
		Rootless: rootless,
	}
	info := Info{Summary: "docker: " + what, Image: image}
	closeEgress := func() error { return nil }
	if spec.Network.Boundary != nil {
		eg, closeFn, err := d.openEgress(ctx, spec, image, rootless)
		if err != nil {
			return nil, err
		}
		r.Egress = eg
		failSetup := os.Getenv(sandboxnet.EnvFailSetup)
		if failSetup == "all" {
			r.Env = append(r.Env, sandboxnet.EnvFailSetup+"=1")
		}
		if err := d.probeOnce(func() error { return r.Probe(ctx) }); err != nil {
			cerr := errors.Join(closeFn(), r.Stop(ctx))
			if !errors.Is(err, ErrNoBoundary) {
				return nil, errors.Join(fmt.Errorf("checking the egress setup of the agent's container: %w", err), cerr)
			}
			if cerr != nil {
				slog.Warn("removing the egress boundary of a container that can't use it", "error", cerr)
			}
			return nil, err
		}
		if failSetup == "agent" {
			r.Env = append(r.Env, sandboxnet.EnvFailSetup+"=1")
		}
		if len(spec.Network.CA) > 0 {
			r.Env = append(r.Env, sandboxnet.EgressCAEnv(spec.Network.CA))
		}
		closeEgress = closeFn
		info.Boundary, info.Relay = true, eg.Relay != ""
	}
	expose(r, spec.Expose, info.Relay)
	opts := Options{Stderr: spec.Stderr, Check: spec.Check}
	if r.Egress != nil && r.Egress.Ensure != nil {
		ensure := r.Egress.Ensure
		opts.Check = func() error {
			if spec.Check != nil {
				if err := spec.Check(); err != nil {
					return err
				}
			}
			return ensure()
		}
	}
	sb, err := r.sandbox(Command{Path: spec.Command.Path, Args: spec.Command.Args, Dir: spec.Command.Dir}, spec.Network.Recheck, opts)
	if err != nil {
		return nil, errors.Join(err, r.Stop(ctx), closeEgress())
	}
	sb.info, sb.closeEgress = info, closeEgress
	return sb, nil
}

// expose gives the container of r the sockets: through its egress
// container with relay, or else mounted read-only.
func expose(r *DockerRunner, sockets []Socket, relay bool) {
	for _, s := range sockets {
		var path string
		switch {
		case relay && (s.Path != "" || s.Addr != ""):
			path = sandboxnet.ContainerSocketDir + "/" + sandboxnet.ServerSocketName
		case s.Path != "":
			r.Mounts = append(r.Mounts, Mount{Src: filepath.Dir(s.Path), Dst: exposeDir, ReadOnly: true})
			path = filepath.Join(exposeDir, filepath.Base(s.Path))
		}
		if path != "" && s.Env != "" {
			r.Env = append(r.Env, s.Env+"="+path)
		}
	}
}

// openEgress opens the boundary of a container of image, and with Docker
// Desktop starts its egress container. It fails with an error that wraps
// [ErrNoBoundary] when the boundary can't work on this host.
func (d *Docker) openEgress(ctx context.Context, spec Spec, image string, rootless bool) (*DockerEgress, func() error, error) {
	arch, entrypoint, err := ImageConfig(ctx, image)
	if err != nil {
		return nil, nil, err
	}
	env := d.egressEnv()
	env.ImageArch = cmp.Or(arch, "an unknown CPU architecture")
	// openRelay pulls or builds a missing image of the egress container.
	env.RelayImage = nil
	caps := env.Probe()
	if caps.EgressRelay {
		return openRelay(ctx, spec, entrypoint, rootless, HostGateway(env.GOOS))
	}
	if caps.Boundary != nil {
		return nil, nil, caps.Boundary
	}
	launcher, err := env.Launcher()
	if err != nil {
		return nil, nil, err
	}
	ep, err := spec.Network.Boundary(HostGateway(env.GOOS), false)
	if err != nil {
		return nil, nil, fmt.Errorf("opening the egress socket: %w", err)
	}
	return &DockerEgress{Socket: ep.Socket, Launcher: launcher, Entrypoint: entrypoint}, ep.Close, nil
}

// openRelay opens the boundary of a container that reaches it through a
// relay container, the egress container to users, and starts it. It fails
// with an error that wraps [ErrNoBoundary] when the relay image isn't
// available.
func openRelay(ctx context.Context, spec Spec, entrypoint []string, rootless bool, gateway HostGatewayFunc) (*DockerEgress, func() error, error) {
	image, err := EnsureRelayImage(ctx, LogLines{Msg: "egress container image"})
	if err != nil {
		return nil, nil, &boundaryError{why: "the egress container's image isn't available: " + err.Error()}
	}
	ep, err := spec.Network.Boundary(gateway, true)
	if err != nil {
		return nil, nil, fmt.Errorf("opening the egress boundary: %w", err)
	}
	labels := maps.Clone(spec.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	labels["a2a.relay"] = spec.Name
	rs := RelaySpec{Name: spec.Name + "-egress", Image: image, Labels: labels, Rootless: rootless, Egress: ep}
	if len(spec.Expose) > 0 {
		rs.ServerSocket, rs.ServerAddr = spec.Expose[0].Path, spec.Expose[0].Addr
	}
	eg, stop, err := StartRelay(ctx, rs, entrypoint)
	if err != nil {
		return nil, nil, errors.Join(err, ep.Close())
	}
	return eg, func() error { return errors.Join(stop(context.Background()), ep.Close()) }, nil
}

// probeOnce runs probe, the check of a container's egress setup, and
// returns its result to every later call once it passed or found that the
// setup can't work on this host. Other failures, such as an error of the
// Docker daemon, aren't kept, and the next call checks again.
func (d *Docker) probeOnce(probe func() error) error {
	d.probeMu.Lock()
	defer d.probeMu.Unlock()
	if d.probed {
		return d.probeErr
	}
	err := probe()
	if err == nil || errors.Is(err, ErrNoBoundary) {
		d.probeErr, d.probed = err, true
	}
	return err
}

// withoutEgressCA returns env without the variable that passes the egress
// CA to the launcher, which only the backend sets: a CA from the caller's
// environment would be trusted by everything in the container.
func withoutEgressCA(env []string) []string {
	return slices.DeleteFunc(slices.Clone(env), func(kv string) bool { return strings.HasPrefix(kv, sandboxnet.EnvEgressCA+"=") })
}
