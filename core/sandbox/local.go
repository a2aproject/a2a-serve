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
	"slices"

	"github.com/a2aproject/a2a-serve/internal/sandboxrt"
)

// Local returns the runtime that runs commands on the host, with no
// isolation and no boundary. It uses only the Command, Sockets and Stderr
// of a Spec. The processes a command starts are killed once it exits.
func Local() Runtime { return local{} }

type local struct{}

func (local) Probe(ctx context.Context) (Capabilities, error) {
	c, err := sandboxrt.Local{}.Probe(ctx)
	if err != nil {
		return Capabilities{}, err
	}
	return capabilities(c), nil
}

func (local) Create(ctx context.Context, spec Spec) (Sandbox, error) {
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
