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

package agentcfg

import (
	"fmt"
	"slices"
	"strings"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agentprofile"
	"github.com/a2aproject/a2a-serve/internal/egressrules"
	"github.com/a2aproject/a2a-serve/internal/sandboxrt"
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
// asks for the boundary, for starting the containers, doctor, status and
// the agent's instructions.
func DecideEgress(cfg *agentdir.Config, probe func() sandboxrt.Capabilities) EgressDecision {
	e := Egress(cfg)
	d := agentprofile.DecideEgress(e, probe)
	return EgressDecision{Boundary: d.Boundary, Relay: d.Relay, Problem: d.Problem, Warning: Warning(agentprofile.EgressWarnings(d.Warnings(e)))}
}

// HostEgress decides about the egress boundary of cfg on this machine.
func HostEgress(cfg *agentdir.Config) EgressDecision {
	return DecideEgress(cfg, sandboxrt.HostEgressEnv().Probe)
}

// StrictEgress reports whether the containers of cfg don't start without
// the egress boundary: with --egress allowlist, or with secrets that the
// boundary adds to requests, which would otherwise be lost or exposed.
func StrictEgress(cfg *agentdir.Config) bool { return Egress(cfg).Strict() }

// NoBoundary says that a container gets Docker's network, and why.
func NoBoundary(why string) string {
	return "egress control is off: " + why + ". " + capitalize(UnrestrictedEffect)
}

// egressIgnored says which egress settings have no effect with --sandbox
// local, and how to remove them.
func egressIgnored(w *agentprofile.EgressIgnoredWarning) string {
	effect := " with --sandbox local: " + UnrestrictedEffect + "; use --sandbox docker, or remove "
	switch {
	case w.Mode && w.Allow:
		return "--egress and --egress-allow have no effect" + effect + "the settings with `a2a-serve config unset egress` and `a2a-serve config unset egress-allow`"
	case w.Mode:
		return "--egress has no effect" + effect + "the setting with `a2a-serve config unset egress`"
	}
	return "--egress-allow has no effect" + effect + "the rules with `a2a-serve config unset egress-allow`"
}

// egressProblem says why the containers with the settings of e can't start
// or reach the network as the settings ask, for why.
func egressProblem(e agentprofile.Egress, why string) string {
	switch {
	case e.Mode == egressrules.ModeAllowlist:
		return noAllowlist(why)
	case len(e.Inject) > 0:
		return noInject(e.Inject, why)
	}
	return NoBoundary(why)
}

// noInject says why a container whose secrets the boundary adds can't
// start, and the ways out: fix the cause, or move the keys back into the
// agent's environment.
func noInject(rules []agentprofile.InjectRule, why string) string {
	var flags, unset []string
	for _, r := range rules {
		setting := agentdir.InjectSetting(r)
		if f := "--" + setting; !slices.Contains(flags, f) {
			flags = append(flags, f)
			unset = append(unset, "`a2a-serve config unset "+setting+"`")
		}
	}
	secrets := agentprofile.InjectedSecrets(rules)
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

// noAllowlist says why a container with --egress allowlist can't start.
func noAllowlist(why string) string {
	return "--egress allowlist can't apply: " + why + "; run the server on Linux with a release build, or use --egress internet"
}

func capitalize(s string) string { return strings.ToUpper(s[:1]) + s[1:] }
