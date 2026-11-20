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
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"path"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/a2aproject/a2a-serve/internal/boundary"
	"github.com/a2aproject/a2a-serve/internal/egressrules"
	"github.com/a2aproject/a2a-serve/internal/sandboxrt"
)

// DockerOptions configure the Docker runtime.
type DockerOptions struct {
	// Launcher is the host path of the binary that a container with an
	// egress endpoint runs as PID 1: a statically linked Linux binary
	// whose main calls [Main]. Empty uses a copy of this binary.
	Launcher string
	// Progress receives the output of pulling an image. Discarded if nil.
	Progress io.Writer
	// StateDir keeps the files of the containers: their env files, which
	// hold the values of Command.Env, and the sockets of their egress
	// endpoints. No container may see it. Empty uses a temporary
	// directory, removed when the last sandbox closes.
	StateDir string
	// ServedAppArmor is the AppArmor profile of the containers with a
	// [Served] mount, whose launcher mounts it with FUSE. Docker's default
	// profile denies the mount, so where Docker applies AppArmor, such as
	// rootful Docker on Ubuntu, a Served mount needs a profile that allows
	// it. "a2a-serve-live" is Docker's default profile with only that
	// mount allowed; `a2a-serve-sidecar apparmor-profile` prints it, to
	// load with apparmor_parser on the machine of the Docker daemon.
	// "unconfined" runs the container without AppArmor. Empty keeps
	// Docker's default and refuses Served mounts where it applies.
	ServedAppArmor string
}

// Docker returns the runtime that runs commands in Docker containers.
func Docker(opts DockerOptions) Runtime {
	return &docker{opts: opts, shared: shared[*sandboxrt.Docker]{
		stateDir: opts.StateDir,
		launcher: opts.Launcher,
		newBackend: func(dir string, launcher func() string) *sandboxrt.Docker {
			return sandboxrt.NewDocker(filepath.Join(dir, "env"), launcher, opts.ServedAppArmor)
		},
	}}
}

type docker struct {
	opts   DockerOptions
	shared shared[*sandboxrt.Docker]
}

func (d *docker) Probe(ctx context.Context) (Capabilities, error) {
	c, err := sandboxrt.NewDocker("", func() string { return d.opts.Launcher }, d.opts.ServedAppArmor).Probe(ctx)
	if err != nil {
		return Capabilities{}, err
	}
	return capabilities(c), nil
}

func (d *docker) Create(ctx context.Context, spec Spec) (Sandbox, error) {
	sb := sandboxrt.Spec{
		Name:     spec.Name,
		Image:    spec.Image,
		Command:  command(spec.Command),
		Home:     spec.Home,
		Limits:   sandboxrt.Limits(spec.Limits),
		Labels:   spec.Labels,
		Stderr:   spec.Stderr,
		Progress: d.opts.Progress,
	}
	ep, behind, err := isolate(&sb, spec, "docker")
	if err != nil {
		return nil, err
	}
	if err := expose(&sb, spec.Sockets, behind); err != nil {
		return nil, err
	}
	backend, boundaries, err := d.shared.acquire()
	if err != nil {
		return nil, err
	}
	if behind {
		sb.Network = sandboxrt.Network{Boundary: boundaries.OpenerOf(ep.Open), CA: ep.CA}
	}
	s, err := backend.Prepare(ctx, sb)
	if err != nil {
		return nil, errors.Join(unsupportedMount("docker", err), d.shared.release(ctx))
	}
	return &releasing{Sandbox: s, release: d.shared.release}, nil
}

// isolate gives sb the mounts of spec, the masks of its hidden paths and
// the boundary of its egress endpoint, which the sandbox is behind unless
// there is none or its policy is unrestricted. backend names the runtime
// in errors.
func isolate(sb *sandboxrt.Spec, spec Spec, backend string) (ep boundary.Endpoint, behind bool, err error) {
	if err := backendMounts(sb, spec.Mounts); err != nil {
		return boundary.Endpoint{}, false, err
	}
	visible := sb.Mounts
	if spec.Home != "" {
		visible = append(slices.Clone(visible), sandboxrt.Mount{Src: spec.Home, Dst: sandboxrt.ContainerHome})
	}
	for _, dst := range sandboxrt.ContainerPaths(visible, spec.Hide) {
		sb.Masks = append(sb.Masks, sandboxrt.Mask{Dst: dst})
	}
	if spec.Egress != nil {
		if ep, err = boundary.EndpointOf(spec.Egress); err != nil {
			return boundary.Endpoint{}, false, err
		}
		if ep.Mode() == egressrules.ModeUnrestricted && ep.Credentials {
			return boundary.Endpoint{}, false, errors.New("the egress endpoint adds credentials, which needs the boundary, and its policy is unrestricted")
		}
	}
	behind = spec.Egress != nil && ep.Mode() != egressrules.ModeUnrestricted
	if len(sb.Served) > 0 && !behind {
		return boundary.Endpoint{}, false, &UnsupportedMountError{Backend: backend, Dst: sb.Served[0].Dst, Kind: MountServed, Reason: "a live workspace needs the egress boundary, whose launcher mounts it: give the sandbox an egress endpoint with a policy other than unrestricted"}
	}
	return ep, behind, nil
}

// backendMounts gives the sandbox of sb the mounts: a host directory as
// a bind mount, a tmpfs in memory, read-only as an empty mask, and a
// served tree as a live workspace.
func backendMounts(sb *sandboxrt.Spec, mounts []Mount) error {
	for _, m := range mounts {
		if err := checkSource(m); err != nil {
			return err
		}
		if !path.IsAbs(m.Dst) {
			return fmt.Errorf("the mount of %s has the destination %q, which isn't an absolute path", m.Src.kind, m.Dst)
		}
		switch {
		case m.Src.kind == MountHostDir:
			sb.Mounts = append(sb.Mounts, sandboxrt.Mount{Src: m.Src.path, Dst: m.Dst, ReadOnly: m.ReadOnly})
		case m.Src.kind == MountServed:
			sb.Served = append(sb.Served, sandboxrt.ServedMount{Dst: m.Dst, Tree: m.Src.tree, ReadOnly: m.ReadOnly})
		case m.ReadOnly:
			sb.Masks = append(sb.Masks, sandboxrt.Mask{Dst: m.Dst})
		default:
			sb.Scratch = append(sb.Scratch, m.Dst)
		}
	}
	return nil
}

// expose gives the container of sb the sockets: a Unix listener mounted
// by its path, and a TCP listener reached at host.docker.internal on
// Docker Desktop, through the egress container when the container is
// behind the boundary.
func expose(sb *sandboxrt.Spec, sockets []Socket, behind bool) error {
	for _, s := range sockets {
		if s.Listener == nil {
			return errors.New("a socket of the sandbox has no listener")
		}
		switch addr := s.Listener.Addr().(type) {
		case *net.UnixAddr:
			sb.Expose = append(sb.Expose, sandboxrt.Socket{Path: addr.Name, Env: s.Env})
		case *net.TCPAddr:
			_, dial, tcp := sandboxrt.DockerTCPBridge()
			if !tcp {
				return errors.New("a Docker sandbox on Linux reaches only Unix listeners")
			}
			hostPort := net.JoinHostPort(dial, strconv.Itoa(addr.Port))
			if behind {
				sb.Expose = append(sb.Expose, sandboxrt.Socket{Addr: hostPort, Env: s.Env})
			} else if s.Env != "" {
				sb.Command.Env = append(slices.Clone(sb.Command.Env), s.Env+"="+hostPort)
			}
		default:
			return fmt.Errorf("a Docker sandbox can't reach a listener on %s", addr.Network())
		}
	}
	return nil
}

// capabilities converts what the runtime found on this machine.
func capabilities(c sandboxrt.Capabilities) Capabilities {
	caps := Capabilities{Boundary: true, Mounts: []MountKind{MountHostDir, MountTmpfs}}
	switch {
	case c.Boundary != nil:
		caps.Boundary, caps.Problem = false, c.Boundary.Error()
	case c.RelayImage != nil:
		_, missing := errors.AsType[*sandboxrt.MissingRelayImage](c.RelayImage)
		caps.Boundary, caps.Problem = missing, c.RelayImage.Error()
	}
	if c.Served != nil {
		caps.MountProblems = map[MountKind]string{MountServed: c.Served.Error()}
	} else {
		caps.Mounts = append(caps.Mounts, MountServed)
	}
	return caps
}
