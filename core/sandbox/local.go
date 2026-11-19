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
	"path/filepath"
	"slices"

	"github.com/a2aproject/a2a-serve/internal/sandboxrt"
)

// Local returns the runtime that runs commands on the host, with no
// isolation and no boundary. It uses only the Command, Mounts, Sockets and
// Stderr of a Spec. The processes a command starts are killed once it
// exits.
//
// The command sees the host's files in place, so Local takes a [HostDir]
// mount whose Dst is its own host path, and does nothing for it. It
// confines nothing, so it ignores ReadOnly and Hide. Create refuses other
// mounts with an [UnsupportedMountError]: a host directory at another path,
// a [Tmpfs] and a [Served] tree, which need a mount namespace.
func Local() Runtime { return local{} }

type local struct{}

func (local) Probe(ctx context.Context) (Capabilities, error) {
	c, err := sandboxrt.Local{}.Probe(ctx)
	if err != nil {
		return Capabilities{}, err
	}
	caps := capabilities(c)
	caps.Mounts = []MountKind{MountHostDir}
	caps.MountProblems = map[MountKind]string{MountTmpfs: localNoNamespace, MountServed: localNoNamespace}
	return caps, nil
}

func (local) Create(ctx context.Context, spec Spec) (Sandbox, error) {
	for _, m := range spec.Mounts {
		if err := checkLocalMount(m); err != nil {
			return nil, err
		}
	}
	sb := sandboxrt.Spec{Name: spec.Name, Command: command(spec.Command), Stderr: spec.Stderr}
	sb.Command.Env = slices.Clone(sb.Command.Env)
	for _, s := range spec.Sockets {
		if s.Env != "" && s.Listener != nil {
			sb.Command.Env = append(sb.Command.Env, s.Env+"="+s.Listener.Addr().String())
		}
	}
	if spec.Egress != nil {
		sb.Network.Boundary = func(sandboxrt.HostGatewayFunc, bool) (sandboxrt.EgressEndpoint, error) {
			return sandboxrt.EgressEndpoint{}, ErrNoBoundary
		}
	}
	return sandboxrt.Local{}.Prepare(ctx, sb)
}

// localNoNamespace is why Local doesn't mount a Tmpfs or a Served tree.
const localNoNamespace = "the command runs on the host, with no mount namespace"

// checkLocalMount refuses a mount that the host can't show as asked.
func checkLocalMount(m Mount) error {
	if err := checkSource(m); err != nil {
		return err
	}
	switch {
	case m.Src.kind == MountTmpfs || m.Src.kind == MountServed:
		return &UnsupportedMountError{Backend: "local", Dst: m.Dst, Kind: m.Src.kind, Reason: localNoNamespace}
	case filepath.Clean(m.Dst) != filepath.Clean(m.Src.path):
		return &UnsupportedMountError{Backend: "local", Dst: m.Dst, Kind: m.Src.kind, Reason: "the command runs on the host, where " + m.Src.path + " stays at its own path"}
	}
	return nil
}
