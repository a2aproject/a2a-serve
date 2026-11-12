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

// Package access turns the access settings of an agent into the policy in
// force, and describes who may call the agent under it.
package access

import (
	"cmp"
	"fmt"
	"strings"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/harness"
	"github.com/a2aproject/a2a-serve/internal/network"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// Network is what the policy and its description depend on in the
// networks of an agent.
type Network struct {
	// Private is the provider of the private network, or empty without one.
	Private string
	// Public reports whether the agent has a public URL.
	Public bool
	// Messenger reports whether the agent has a messenger.
	Messenger bool
}

// NetworkOf returns the networks cfg configures.
func NetworkOf(cfg *agentdir.Config) Network {
	n := Network{Public: cfg.Network.Public != "", Messenger: cfg.HasMessengers()}
	switch p := cfg.Network.Private; p {
	case "":
	case agentdir.NetworkTailscale:
		n.Private = network.ProviderTailscale
	default:
		n.Private = network.ProviderAgentMesh
	}
	return n
}

// PrivateAllowIncoming is the --private-allow-incoming in force: the setting, or
// everyone by default.
func PrivateAllowIncoming(p agentdir.PolicyConfig) string {
	return cmp.Or(p.PrivateAllowIncoming, agentdir.AccessEveryone)
}

// Policy is the policy of the settings p: the private network with
// --private-allow-incoming everyone and --allow-email are members; with
// --public-allow-incoming anyone, anyone is a guest.
func Policy(p agentdir.PolicyConfig) auth.Policy {
	var out auth.Policy
	if PrivateAllowIncoming(p) == agentdir.AccessEveryone {
		out.Members = append(out.Members, auth.MemberPrivate)
	}
	for _, e := range p.AllowEmails {
		if e = strings.TrimSpace(e); e != "" {
			out.Members = append(out.Members, auth.MemberEmailPrefix+e)
		}
	}
	if p.PublicAllowIncoming == agentdir.AccessAnyone {
		out.Guests = []string{auth.MemberAnyone}
	}
	return out
}

// Pairings counts the pairings of an agent besides the local one.
type Pairings struct {
	All, Owners int
	// Names holds the names of the pairings and webhook nodes, or is nil
	// when they aren't known.
	Names map[string]bool
}

// CountPairings counts the pairings of ps besides the local one, and
// names them and the webhook nodes.
func CountPairings(ps []store.Pairing, nodes []store.Node) Pairings {
	n := Pairings{Names: map[string]bool{}}
	for _, p := range ps {
		if p.Name == auth.LocalPairing {
			continue
		}
		n.All++
		n.Names[p.Name] = true
		if p.Role == auth.RoleOwner {
			n.Owners++
		}
	}
	for _, node := range nodes {
		n.Names[node.ID] = true
	}
	return n
}

// Description says who may call an agent.
type Description struct {
	// PrivateAllowIncoming is everyone or paired, set with a private network;
	// PublicAllowIncoming is paired or anyone, set with a public URL.
	PrivateAllowIncoming string
	PublicAllowIncoming  string
	// Owner, Member and Guest are the callers each role holds.
	Owner  []string
	Member []string
	Guest  []string
	// Pairings counts the pairings besides the local one, and OwnerPairings
	// those of them that are owners.
	Pairings      int
	OwnerPairings int
	// Others describes who may call without a pairing, such as everyone on
	// the tailnet.
	Others []string
	// Summary says who may call, one line each for you, everyone else and
	// the networks that need a pairing.
	Summary []string
	// Sandbox says what the agent containers of callers get, one line each
	// for you, the sandbox rules and everyone else.
	Sandbox []string
	// SandboxNotes are what the sandbox rules don't do, such as callers
	// whose container gets no model key.
	SandboxNotes []string
	// GuestNote says that the agent can't reach its model for guests, who
	// get no secrets, if it needs a key.
	GuestNote string
}

// DescribeConfig is [Describe] of the settings of cfg, with what the agent
// containers of callers get when sandbox rules or guests make
// it differ between them.
func DescribeConfig(cfg *agentdir.Config, net Network, n Pairings) *Description {
	a := Describe(cfg.Policy, net, n)
	sp := SandboxPolicyOf(cfg)
	guests := len(a.Guest) > 0
	for _, l := range sp.SandboxLines(n.Owners, guests, net.Private != "") {
		if len(cfg.Policy.Sandbox) > 0 || guests {
			a.Sandbox = append(a.Sandbox, l.Who+": "+sp.Describe(l.Sandbox))
		}
		note := ModelKeyNote(cfg, l)
		switch {
		case note == "":
		case l.Guest:
			a.GuestNote = note
		default:
			a.SandboxNotes = append(a.SandboxNotes, note)
		}
	}
	if sp.Local && (n.All > n.Owners || len(a.Others) > 0) {
		a.SandboxNotes = append(a.SandboxNotes, LocalSandboxNote)
	}
	a.SandboxNotes = append(a.SandboxNotes, unknownPairings(cfg.Policy.Sandbox, n.Names)...)
	return a
}

// unknownPairings says which pairing matchers of rules name no pairing or
// webhook node in names.
func unknownPairings(rules []agentdir.SandboxRule, names map[string]bool) []string {
	if names == nil {
		return nil
	}
	var notes []string
	for _, r := range rules {
		for _, m := range r.Match {
			name, ok := strings.CutPrefix(m, agentdir.MatchPairingPrefix)
			if !ok || names[name] {
				continue
			}
			notes = append(notes, fmt.Sprintf("no pairing or webhook node is named %s, so the sandbox rule of %s matches no caller", name, m))
		}
	}
	return notes
}

// LocalSandboxNote says what sandbox rules don't do without the Docker
// sandbox.
const LocalSandboxNote = "the agent runs without the Docker sandbox: storage rules don't apply, and a caller's agent can read the secrets that rules keep out of its environment in .a2a/state/secrets"

// ModelKeyNote warns that the containers of the callers of l have no key
// of the agent's model provider, so the agent can't reach its model for
// them, and says how to give one, or returns "".
func ModelKeyNote(cfg *agentdir.Config, l SandboxLine) string {
	var names []string
	for _, g := range l.Sandbox.Secrets {
		names = append(names, g.Name)
	}
	provider, key := harness.ModelKeyWithheld(cfg, names)
	if provider == "" {
		return ""
	}
	name := harness.ProviderName(provider)
	switch {
	case l.Guest:
		return fmt.Sprintf("Guests' agent gets no %s key and can't reach its model. Give it a key of its own with `a2a-serve config set --secret GUEST_KEY --guest-secrets %s=secret:GUEST_KEY --restart`, or share yours with `a2a-serve config set --guest-secrets %s`.", name, key, key)
	case l.Pairing != "":
		return fmt.Sprintf("%s's agent gets no %s key and can't reach its model. Give one with `a2a-serve pair set %s --secrets %s`, or a key of its own with `--secrets %s=secret:<STORED_KEY>`.", l.Pairing, name, l.Pairing, key, key)
	}
	return fmt.Sprintf("The agents of %s get no %s key and can't reach their model. Give them one in their rule with `a2a-serve config edit`.", l.Who, name)
}

// Describe says who may call an agent with the settings p on the networks
// net, which has the pairings n.
func Describe(p agentdir.PolicyConfig, net Network, n Pairings) *Description {
	pol := Policy(p)
	a := &Description{
		Owner: []string{auth.MemberSelf}, Member: nonNil(pol.Members), Guest: nonNil(pol.Guests),
		Pairings: n.All, OwnerPairings: n.Owners, Others: pol.Others(net.Private, net.Public, net.Messenger),
	}
	if net.Private != "" {
		a.PrivateAllowIncoming = PrivateAllowIncoming(p)
	}
	if net.Public {
		a.PublicAllowIncoming = cmp.Or(p.PublicAllowIncoming, agentdir.AccessPaired)
	}
	a.Summary = []string{"you: " + yourDevices(net.Private), others(a.Others, n)}
	if a.PrivateAllowIncoming == agentdir.AccessPaired {
		a.Summary = append(a.Summary, networkName(net.Private)+" needs a pairing")
	}
	switch {
	case !net.Public:
	case a.PublicAllowIncoming == agentdir.AccessAnyone:
		a.Summary = append(a.Summary, "the public URL is open to anyone, as guests")
	case len(p.AllowEmails) > 0:
		a.Summary = append(a.Summary, "the public URL needs a pairing or Google sign-in")
	default:
		a.Summary = append(a.Summary, "the public URL needs a pairing")
	}
	return a
}

// yourDevices names where the user running the agent is an owner.
func yourDevices(provider string) string {
	switch provider {
	case network.ProviderTailscale:
		return "this machine and your tailnet devices"
	case network.ProviderAgentMesh:
		return "this machine and your devices on the relay"
	}
	return "this machine"
}

func others(who []string, n Pairings) string {
	if n.All > 0 {
		p := fmt.Sprintf("%d %s", n.All, plural(n.All, "pairing"))
		if n.Owners > 0 {
			p += fmt.Sprintf(" (%d %s)", n.Owners, plural(n.Owners, "owner"))
		}
		who = append(who, p)
	}
	if len(who) == 0 {
		return "nobody else"
	}
	return strings.Join(who, ", ")
}

func networkName(provider string) string {
	switch provider {
	case network.ProviderTailscale:
		return "the tailnet"
	case network.ProviderAgentMesh:
		return "the relay"
	}
	return "the private network"
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
