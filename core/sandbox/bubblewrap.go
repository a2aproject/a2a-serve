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
	"net"
	"path/filepath"

	"github.com/a2aproject/a2a-serve/internal/sandboxrt"
)

// BubblewrapOptions configure the bubblewrap runtime.
type BubblewrapOptions struct {
	// Launcher is the host path of the binary that a sandbox with an
	// egress endpoint runs first: a statically linked Linux binary whose
	// main calls [Main]. Empty uses a copy of this binary.
	Launcher string
	// StateDir keeps the files of the sandboxes, such as the sockets of
	// their egress endpoints. No sandbox may see it. Empty uses a
	// temporary directory, removed when the last sandbox closes.
	StateDir string
	// Programs name the programs of this machine that the command runs,
	// besides its own, such as "a2a". Create finds each on PATH and shows
	// its directory read-only.
	Programs []string
	// HostNetwork shares this machine's network with a sandbox without an
	// Egress endpoint, or with an unrestricted policy, which otherwise has
	// only loopback. The command then reaches every service of this
	// machine, on loopback and on abstract Unix sockets, such as the X11
	// display, which can read and send keystrokes.
	HostNetwork bool
}

// Bubblewrap returns the runtime that runs commands on this machine with
// bubblewrap (bwrap), in new user, PID, IPC, UTS, cgroup, mount and network
// namespaces. It needs Linux with unprivileged user namespaces, and ignores
// the Image, Labels and Limits of a Spec.
//
// The command and the Programs come from this machine: Create finds the
// command on PATH, unless its path is in the sandbox, and shows the
// directory where it found each program, and the directory of its file with
// links resolved, read-only at their paths. An agent that needs more files,
// such as one that npm installed under a version manager, needs them in
// Mounts.
//
// The command sees a filesystem of its own: /usr and the other system
// directories read-only; of /etc only the users and groups, name
// resolution, the dynamic linker's files, the alternatives, the CA
// certificates and the time zone; private /tmp, /var/tmp and /run; /proc;
// a minimal /dev; the Mounts; and Home as $HOME. Hide works as with
// Docker.
//
// With an Egress endpoint, the sandbox's network has only loopback and the
// egress boundary, and Create fails with an error that wraps ErrNoBoundary
// where the boundary can't work, such as without /dev/net/tun. The
// launcher, which runs as PID 1 of the sandbox, sets up the network, mounts
// the [Served] trees with FUSE, drops every capability and installs a
// seccomp filter that refuses new namespaces, mounts and ptrace, and then
// runs the command as the user who started the sandbox. Without an Egress endpoint,
// or with an unrestricted policy, the sandbox has only loopback, or the
// network of this machine with HostNetwork, and Served mounts fail with an
// [UnsupportedMountError].
//
// The sandbox reaches Unix listeners among Sockets.
func Bubblewrap(opts BubblewrapOptions) Runtime {
	return &bubblewrap{opts: opts, shared: shared[*sandboxrt.Bubblewrap]{
		stateDir: opts.StateDir,
		launcher: opts.Launcher,
		newBackend: func(dir string, launcher func() string) *sandboxrt.Bubblewrap {
			return sandboxrt.NewBubblewrap(sandboxrt.BubblewrapConfig{Dir: filepath.Join(dir, "bwrap"), Launcher: launcher, Programs: opts.Programs, HostNetwork: opts.HostNetwork})
		},
	}}
}

type bubblewrap struct {
	opts   BubblewrapOptions
	shared shared[*sandboxrt.Bubblewrap]
}

// bubblewrapName names the runtime in errors.
const bubblewrapName = "bwrap"

func (b *bubblewrap) Probe(ctx context.Context) (Capabilities, error) {
	c, err := sandboxrt.NewBubblewrap(sandboxrt.BubblewrapConfig{Launcher: func() string { return b.opts.Launcher }, Programs: b.opts.Programs}).Probe(ctx)
	if err != nil {
		return Capabilities{}, err
	}
	return capabilities(c), nil
}

func (b *bubblewrap) Create(ctx context.Context, spec Spec) (Sandbox, error) {
	sb := sandboxrt.Spec{
		Name:    spec.Name,
		Command: command(spec.Command),
		Home:    spec.Home,
		Stderr:  spec.Stderr,
	}
	ep, behind, err := isolate(&sb, spec, bubblewrapName)
	if err != nil {
		return nil, err
	}
	for _, s := range spec.Sockets {
		if s.Listener == nil {
			return nil, errors.New("a socket of the sandbox has no listener")
		}
		addr, ok := s.Listener.Addr().(*net.UnixAddr)
		if !ok {
			return nil, fmt.Errorf("a bubblewrap sandbox reaches only Unix listeners, not one on %s", s.Listener.Addr().Network())
		}
		sb.Expose = append(sb.Expose, sandboxrt.Socket{Path: addr.Name, Env: s.Env})
	}
	backend, boundaries, err := b.shared.acquire()
	if err != nil {
		return nil, err
	}
	if behind {
		sb.Network = sandboxrt.Network{Boundary: boundaries.OpenerOf(ep.Open), CA: ep.CA}
	}
	s, err := backend.Prepare(ctx, sb)
	if err != nil {
		return nil, errors.Join(unsupportedMount(bubblewrapName, err), b.shared.release(ctx))
	}
	return &releasing{Sandbox: s, release: b.shared.release}, nil
}
