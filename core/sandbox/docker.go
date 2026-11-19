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
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"sync"

	"github.com/a2aproject/a2a-serve/internal/boundary"
	"github.com/a2aproject/a2a-serve/internal/cryptoutil"
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
}

// Docker returns the runtime that runs commands in Docker containers.
func Docker(opts DockerOptions) Runtime { return &docker{opts: opts} }

type docker struct {
	opts DockerOptions

	mu         sync.Mutex
	dir        string
	backend    *sandboxrt.Docker
	boundaries *sandboxrt.Boundaries
	// users counts the sandboxes that are being created or not yet
	// closed, which use backend and boundaries.
	users int
}

// acquire returns the backend and the boundaries of d, setting them up for
// the first user. The caller calls release when done.
func (d *docker) acquire() (*sandboxrt.Docker, *sandboxrt.Boundaries, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.backend == nil {
		dir := d.opts.StateDir
		if dir == "" {
			tmp, err := os.MkdirTemp("", "a2a-sandbox-")
			if err != nil {
				return nil, nil, fmt.Errorf("creating the directory of the containers' files: %w", err)
			}
			dir = tmp
		}
		d.dir = dir
		d.boundaries = sandboxrt.NewBoundaries(filepath.Join(dir, "egress"), sandboxrt.SocketIDLen, cryptoutil.Unambiguous, nil)
		launcher := d.boundaries.Launcher
		if d.opts.Launcher != "" {
			launcher = func() string { return d.opts.Launcher }
		}
		d.backend = sandboxrt.NewDocker(filepath.Join(dir, "env"), launcher)
	}
	d.users++
	return d.backend, d.boundaries, nil
}

// release ends a use of acquire. The last one closes the listener of the
// egress containers and removes a temporary state directory.
func (d *docker) release(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.users--
	if d.users > 0 {
		return nil
	}
	err := d.boundaries.Close(ctx)
	if d.opts.StateDir == "" {
		err = errors.Join(err, os.RemoveAll(d.dir))
	}
	d.backend, d.boundaries, d.dir = nil, nil, ""
	return err
}

func (d *docker) Probe(ctx context.Context) (Capabilities, error) {
	c, err := sandboxrt.NewDocker("", func() string { return d.opts.Launcher }).Probe(ctx)
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
	if err := dockerMounts(&sb, spec.Mounts); err != nil {
		return nil, err
	}
	visible := sb.Mounts
	if spec.Home != "" {
		visible = append(slices.Clone(visible), sandboxrt.Mount{Src: spec.Home, Dst: sandboxrt.ContainerHome})
	}
	for _, dst := range sandboxrt.ContainerPaths(visible, spec.Hide) {
		sb.Masks = append(sb.Masks, sandboxrt.Mask{Dst: dst})
	}
	var ep boundary.Endpoint
	var err error
	if spec.Egress != nil {
		if ep, err = boundary.EndpointOf(spec.Egress); err != nil {
			return nil, err
		}
		if ep.Mode() == egressrules.ModeUnrestricted && ep.Credentials {
			return nil, errors.New("the egress endpoint adds credentials, which needs the boundary, and its policy is unrestricted")
		}
	}
	behind := spec.Egress != nil && ep.Mode() != egressrules.ModeUnrestricted
	if err := expose(&sb, spec.Sockets, behind); err != nil {
		return nil, err
	}
	backend, boundaries, err := d.acquire()
	if err != nil {
		return nil, err
	}
	if behind {
		sb.Network = sandboxrt.Network{Boundary: boundaries.OpenerOf(ep.Open), CA: ep.CA}
	}
	s, err := backend.Prepare(ctx, sb)
	if err != nil {
		return nil, errors.Join(err, d.release(ctx))
	}
	return &dockerSandbox{Sandbox: s, d: d}, nil
}

// dockerMounts gives the container of sb the mounts: a host directory as
// a bind mount, and a tmpfs in memory, read-only as an empty mask.
func dockerMounts(sb *sandboxrt.Spec, mounts []Mount) error {
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

type dockerSandbox struct {
	sandboxrt.Sandbox
	d    *docker
	once sync.Once
}

func (s *dockerSandbox) Close(ctx context.Context) error {
	err := s.Sandbox.Close(ctx)
	s.once.Do(func() { err = errors.Join(err, s.d.release(ctx)) })
	return err
}

// capabilities converts what the runtime found on this machine.
func capabilities(c sandboxrt.Capabilities) Capabilities {
	if c.Boundary != nil {
		return Capabilities{Problem: c.Boundary.Error(), Mounts: dockerMountKinds()}
	}
	if c.RelayImage != nil {
		if _, missing := errors.AsType[*sandboxrt.MissingRelayImage](c.RelayImage); !missing {
			return Capabilities{Problem: c.RelayImage.Error(), Mounts: dockerMountKinds()}
		}
		return Capabilities{Boundary: true, Problem: c.RelayImage.Error(), Mounts: dockerMountKinds()}
	}
	return Capabilities{Boundary: true, Mounts: dockerMountKinds()}
}

func dockerMountKinds() []MountKind { return []MountKind{MountHostDir, MountTmpfs} }
