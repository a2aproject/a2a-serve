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
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/egress"
	"github.com/a2aproject/a2a-serve/internal/sandbox"
)

// UnrestrictedEffect says what a sandbox without the egress boundary reaches.
const UnrestrictedEffect = "the agent reaches every network this machine reaches, the LAN and the tailnet included"

// EgressDecision is whether an agent's containers get the egress boundary.
type EgressDecision struct {
	// Boundary is set when the containers' connections go through the
	// server, and Relay when a relay container next to each forwards them.
	Boundary, Relay bool
	// Problem is why they don't although the config asks for it, or "".
	Problem string
	// Warning is what the user should know, or "".
	Warning string
}

// DecideEgress decides whether the containers of cfg get the egress
// boundary on a machine that probe checks, which it calls only when cfg
// asks for the boundary. It is the one place that decides it, for starting
// the containers, doctor, status and the agent's instructions.
func DecideEgress(cfg *agentdir.Config, probe func() sandbox.Capabilities) EgressDecision {
	if cfg.Sandbox == SandboxLocal {
		return EgressDecision{Warning: localEgressWarning(cfg)}
	}
	if cfg.Egress.Mode == egress.ModeUnrestricted {
		return EgressDecision{}
	}
	caps := probe()
	if caps.EgressRelay {
		var warnings []string
		if _, missing := errors.AsType[*sandbox.MissingRelayImage](caps.RelayImage); missing {
			warnings = append(warnings, caps.RelayImage.Error())
		} else if caps.RelayImage != nil {
			return problemDecision(cfg, caps.RelayImage.Error())
		}
		if cfg.Egress.Mode == egress.ModeAllowlist && len(cfg.Egress.Allow) == 0 && len(cfg.Inject) == 0 {
			warnings = append(warnings, noAllowlistRules)
		}
		return EgressDecision{Boundary: true, Relay: true, Warning: strings.Join(warnings, "; ")}
	}
	if caps.Boundary != nil {
		return problemDecision(cfg, caps.Boundary.Error())
	}
	if cfg.Egress.Mode == egress.ModeAllowlist && len(cfg.Egress.Allow) == 0 && len(cfg.Inject) == 0 {
		return EgressDecision{Boundary: true, Warning: noAllowlistRules}
	}
	return EgressDecision{Boundary: true}
}

// HostEgress decides about the egress boundary of cfg on this machine.
func HostEgress(cfg *agentdir.Config) EgressDecision {
	return DecideEgress(cfg, sandbox.HostEgressEnv().Probe)
}

// localEgressWarning says which egress settings of cfg have no effect with
// --sandbox local, and how to remove them, or "".
func localEgressWarning(cfg *agentdir.Config) string {
	mode, allow := cfg.Egress.Mode != "", len(cfg.Egress.Allow) > 0
	effect := " with --sandbox local: " + UnrestrictedEffect + "; use --sandbox docker, or remove "
	switch {
	case mode && allow:
		return "--egress and --egress-allow have no effect" + effect + "the settings with `a2a-serve config unset egress` and `a2a-serve config unset egress-allow`"
	case mode:
		return "--egress has no effect" + effect + "the setting with `a2a-serve config unset egress`"
	case allow:
		return "--egress-allow has no effect" + effect + "the rules with `a2a-serve config unset egress-allow`"
	}
	return ""
}

// problemDecision is the decision for cfg when problem keeps the boundary
// from applying.
func problemDecision(cfg *agentdir.Config, problem string) EgressDecision {
	switch {
	case cfg.Egress.Mode == egress.ModeAllowlist:
		return EgressDecision{Problem: problem, Warning: noAllowlist(problem)}
	case len(cfg.Inject) > 0:
		return EgressDecision{Problem: problem, Warning: noInject(cfg, problem)}
	}
	return EgressDecision{Problem: problem, Warning: noBoundary(problem)}
}

// StrictEgress reports whether the containers of cfg don't start without
// the egress boundary: with --egress allowlist, or with secrets that the
// boundary adds to requests, which would otherwise be lost or exposed.
func StrictEgress(cfg *agentdir.Config) bool {
	return cfg.Egress.Mode == egress.ModeAllowlist || len(cfg.Inject) > 0
}

// noInject says why a container whose secrets the boundary adds can't
// start, and the ways out: fix the cause, or move the keys back into the
// agent's environment.
func noInject(cfg *agentdir.Config, why string) string {
	var flags, unset []string
	for _, r := range cfg.Inject {
		if f := "--" + r.Setting(); !slices.Contains(flags, f) {
			flags = append(flags, f)
			unset = append(unset, "`a2a-serve config unset "+r.Setting()+"`")
		}
	}
	secrets := cfg.InjectedSecrets()
	// One flag per command can read its secret from a prompt, so each key
	// gets a command of its own.
	store := make([]string, len(secrets))
	for i, name := range secrets {
		store[i] = "`a2a-serve config set --secret " + name + "=-`"
	}
	return fmt.Sprintf("%s can't apply: %s. The agent doesn't start, so that it can't read %s. Fix that, or put the keys in the agent's environment: remove the rules with %s, then store the keys again with %s", strings.Join(flags, " and "), why, strings.Join(secrets, ", "), strings.Join(unset, " and "), strings.Join(store, " and "))
}

// noAllowlistRules warns about an allowlist that allows nothing.
const noAllowlistRules = "--egress allowlist with no --egress-allow rules: the agent can't reach its model provider"

// noBoundary says that a container gets Docker's network, and why.
func noBoundary(why string) string {
	return "egress control is off: " + why + ". " + capitalize(UnrestrictedEffect)
}

// noAllowlist says why a container with --egress allowlist can't start.
func noAllowlist(why string) string {
	return "--egress allowlist can't apply: " + why + "; run the server on Linux with a release build, or use --egress internet"
}

func capitalize(s string) string { return strings.ToUpper(s[:1]) + s[1:] }
