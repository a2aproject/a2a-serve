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

package agent

import (
	"errors"

	"github.com/a2aproject/a2a-serve/core/egress"
	"github.com/a2aproject/a2a-serve/core/sandbox"
)

// Egress are the settings that decide the egress of an agent's containers.
type Egress struct {
	// Sandbox is SandboxDocker or SandboxLocal.
	Sandbox string
	// Mode is an egress mode, and Allow its allow rules.
	Mode  string
	Allow []string
	// Inject are the headers the boundary adds.
	Inject []InjectRule
}

// Strict reports whether the containers don't start without the egress
// boundary: with the allowlist mode, or with secrets that the boundary adds
// to requests, which would otherwise be lost or exposed.
func (e Egress) Strict() bool {
	return e.Mode == egress.ModeAllowlist || len(e.Inject) > 0
}

// EgressDecision is whether an agent's containers get the egress boundary.
type EgressDecision struct {
	// Boundary is set when the containers' connections go through the
	// server, and Relay when a relay container next to each forwards them.
	Boundary, Relay bool
	// Problem is why they don't although the settings ask for it, or "".
	Problem string
	// RelayImage is the relay image that the next start fetches, if any.
	RelayImage *sandbox.MissingRelayImage
	// NoAllowRules is set for an allowlist that allows nothing.
	NoAllowRules bool
	// Ignored is set for an agent without a sandbox whose egress settings
	// have no effect.
	Ignored bool
}

// DecideEgress decides whether the containers with the settings e get the
// egress boundary on a machine that probe checks, which it calls only when
// e asks for the boundary. It is the one place that decides it.
func DecideEgress(e Egress, probe func() sandbox.Capabilities) EgressDecision {
	if e.Sandbox == SandboxLocal {
		return EgressDecision{Ignored: e.Mode != "" || len(e.Allow) > 0}
	}
	if e.Mode == egress.ModeUnrestricted {
		return EgressDecision{}
	}
	noRules := e.Mode == egress.ModeAllowlist && len(e.Allow) == 0 && len(e.Inject) == 0
	caps := probe()
	if caps.EgressRelay {
		d := EgressDecision{Boundary: true, Relay: true, NoAllowRules: noRules}
		if missing, ok := errors.AsType[*sandbox.MissingRelayImage](caps.RelayImage); ok {
			d.RelayImage = missing
		} else if caps.RelayImage != nil {
			return EgressDecision{Problem: caps.RelayImage.Error()}
		}
		return d
	}
	if caps.Boundary != nil {
		return EgressDecision{Problem: caps.Boundary.Error()}
	}
	return EgressDecision{Boundary: true, NoAllowRules: noRules}
}

// HostEgress decides about the egress boundary of e on this machine.
func HostEgress(e Egress) EgressDecision {
	return DecideEgress(e, sandbox.HostEgressEnv().Probe)
}

// Warnings returns what the user should know of d, for the settings e.
func (d EgressDecision) Warnings(e Egress) []error {
	var out []error
	if d.Ignored {
		out = append(out, &EgressIgnoredWarning{Mode: e.Mode != "", Allow: len(e.Allow) > 0})
	}
	if d.Problem != "" {
		if e.Strict() {
			out = append(out, &EgressError{Egress: e, Reason: d.Problem})
		} else {
			out = append(out, &NoBoundaryWarning{Reason: d.Problem})
		}
	}
	if d.RelayImage != nil {
		out = append(out, d.RelayImage)
	}
	if d.NoAllowRules {
		out = append(out, ErrNoAllowRules)
	}
	return out
}
