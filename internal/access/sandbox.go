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

package access

import (
	"cmp"
	"slices"
	"strings"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/network"
)

// Sandbox is what the agent container of a caller gets.
type Sandbox struct {
	// Storage is one of the agentdir.Storage* modes.
	Storage string
	// Secrets are the variables the agent gets, in the order of the agent's
	// secrets.
	Secrets []SecretGrant
}

// SecretGrant passes the stored secret From to the agent as Name.
type SecretGrant struct {
	Name, From string
}

// Equal reports whether s and o give the same storage and secrets.
func (s Sandbox) Equal(o Sandbox) bool {
	return s.Storage == o.Storage && slices.Equal(s.Secrets, o.Secrets)
}

// Covers reports whether s gives at least what o gives: storage with as much
// access, and every secret of o.
func (s Sandbox) Covers(o Sandbox) bool {
	if storageRank(s.Storage) > storageRank(o.Storage) {
		return false
	}
	for _, g := range o.Secrets {
		if !slices.Contains(s.Secrets, g) {
			return false
		}
	}
	return true
}

func storageRank(storage string) int {
	if i := slices.Index(agentdir.StorageModes, storage); i >= 0 {
		return i
	}
	return len(agentdir.StorageModes)
}

// Describe says what s gives, such as "read-only workspace, no secrets".
func (s Sandbox) Describe() string { return s.describe(false) }

// describe is [Sandbox.Describe]; without a sandbox, the storage isn't
// limited.
func (s Sandbox) describe(local bool) string {
	var storage string
	switch s.Storage {
	case agentdir.StorageShared:
		storage = "shared workspace"
	case agentdir.StorageReadOnly:
		storage = "read-only workspace"
	case agentdir.StorageOwn:
		storage = "own copy of the workspace"
	case agentdir.StorageEmpty:
		storage = "empty workspace"
	}
	if local {
		storage = "no sandbox, so storage rules don't apply"
	}
	if len(s.Secrets) == 0 {
		return storage + ", no secrets"
	}
	names := make([]string, len(s.Secrets))
	for i, g := range s.Secrets {
		names[i] = g.Name
		if g.From != g.Name {
			names[i] += " (from " + g.From + ")"
		}
	}
	return storage + ", secrets " + strings.Join(names, ", ")
}

// SandboxPolicy resolves the sandbox rules of an agent.
type SandboxPolicy struct {
	Rules []agentdir.SandboxRule
	// Secrets are the names of the agent's secrets.
	Secrets []string
	// Local is set for an agent without a sandbox, where the rules don't
	// apply.
	Local bool
}

// SandboxPolicyOf returns the sandbox policy of cfg.
func SandboxPolicyOf(cfg *agentdir.Config) SandboxPolicy {
	return SandboxPolicy{Rules: cfg.Policy.Sandbox, Secrets: cfg.Secrets, Local: cfg.Sandbox == agentdir.SandboxLocal}
}

// Describe says what sb gives under the policy, such as "read-only
// workspace, no secrets".
func (sp SandboxPolicy) Describe(sb Sandbox) string { return sb.describe(sp.Local) }

// Specificity of the matchers; the most specific match wins.
const (
	rankAnyone = iota + 1
	rankPrivate
	rankDomain
	rankEmail
	rankPairing
)

// Resolve returns the sandbox of the container that the conversations
// started by p run in:
//
//   - owners share the container of the user running the agent, which the
//     rule of self sets;
//   - guests, the anonymous callers, share one container, which the rule of
//     guests sets;
//   - every other caller gets a container of their own, which the most
//     specific rule that matches them sets: pairing:<name>, then
//     email:<address>, then email:*@<domain>, then private.
//     Between rules of the same specificity, the first one wins.
//
// A rule sets what the callers it matches get. What no matching rule sets
// comes from the default of the caller: guests get a read-only
// workspace and no secrets, and every other caller the shared workspace and
// every secret.
func (sp SandboxPolicy) Resolve(p auth.Principal) Sandbox {
	return sp.apply(sp.defaultOf(p.Role == auth.RoleAnonymous), sp.ruleOf(p))
}

// ResolveUnverified is [SandboxPolicy.Resolve] for a user whose identity
// the server hasn't verified, rebuilt from the principal ID alone. Its
// email might come with a proven domain and from the private network, so
// it gets the least of what each of those identities would get. Any
// principal might call from the private network.
func (sp SandboxPolicy) ResolveUnverified(p auth.Principal) Sandbox {
	variants := []auth.Identity{{Network: network.ListenerPrivate}}
	if _, domain, ok := strings.Cut(p.Email, "@"); ok {
		variants = append(variants, auth.Identity{Domain: domain}, auth.Identity{Domain: domain, Network: network.ListenerPrivate})
	}
	out := sp.Resolve(p)
	for _, v := range variants {
		q := p
		q.Domain, q.Network = v.Domain, cmp.Or(v.Network, p.Network)
		out = out.Meet(sp.Resolve(q))
	}
	return out
}

// Meet returns what both s and o give: the storage with less access, and
// the secrets in both.
func (s Sandbox) Meet(o Sandbox) Sandbox {
	out := Sandbox{Storage: s.Storage}
	if storageRank(o.Storage) > storageRank(s.Storage) {
		out.Storage = o.Storage
	}
	for _, g := range s.Secrets {
		if slices.Contains(o.Secrets, g) {
			out.Secrets = append(out.Secrets, g)
		}
	}
	return out
}

// ruleOf returns the rule of p: the storage and the secrets of the most
// specific matching rule that sets each.
func (sp SandboxPolicy) ruleOf(p auth.Principal) agentdir.SandboxRule {
	var out agentdir.SandboxRule
	for _, i := range sp.matches(p) {
		r := sp.Rules[i]
		if out.Storage == "" {
			out.Storage = r.Storage
		}
		if len(out.Secrets) == 0 {
			out.Secrets = r.Secrets
		}
	}
	return out
}

// Default is the sandbox of the callers who aren't owners or anonymous,
// without a rule.
func (sp SandboxPolicy) Default() Sandbox { return sp.defaultOf(false) }

// defaultOf is the sandbox of guests, or of every other caller,
// without a rule.
func (sp SandboxPolicy) defaultOf(anonymous bool) Sandbox {
	if anonymous {
		return Sandbox{Storage: agentdir.StorageReadOnly}
	}
	return Sandbox{Storage: agentdir.StorageShared, Secrets: sp.grants([]string{agentdir.SecretsAll})}
}

// apply returns def with the storage and the secrets that r sets. A part
// r leaves out keeps def.
func (sp SandboxPolicy) apply(def Sandbox, r agentdir.SandboxRule) Sandbox {
	out := def
	if r.Storage != "" {
		out.Storage = r.Storage
	}
	if len(r.Secrets) > 0 {
		out.Secrets = sp.grants(r.Secrets)
	}
	return out
}

// matches returns the indexes of the rules that match p, the most specific
// first; rules of the same specificity keep their order.
func (sp SandboxPolicy) matches(p auth.Principal) []int {
	type match struct{ rule, rank int }
	var out []match
	for i, r := range sp.Rules {
		best := 0
		for _, m := range r.Match {
			best = max(best, matchRank(m, p))
		}
		if best > 0 {
			out = append(out, match{i, best})
		}
	}
	slices.SortStableFunc(out, func(a, b match) int { return b.rank - a.rank })
	idx := make([]int, len(out))
	for i, m := range out {
		idx[i] = m.rule
	}
	return idx
}

func matchRank(m string, p auth.Principal) int {
	switch p.Role {
	case auth.RoleOwner:
		if m == agentdir.MatchSelf {
			return rankPairing
		}
		return 0
	case auth.RoleAnonymous:
		if m == agentdir.MatchGuests {
			return rankAnyone
		}
		return 0
	}
	id := p.Identity
	switch m {
	case agentdir.MatchGuests:
		return 0
	case auth.MemberPrivate:
		if id.Network == network.ListenerPrivate {
			return rankPrivate
		}
		return 0
	}
	if name, ok := strings.CutPrefix(m, agentdir.MatchPairingPrefix); ok {
		if name == id.Pairing || name == id.WebhookNode {
			return rankPairing
		}
		return 0
	}
	pattern, ok := strings.CutPrefix(m, auth.MemberEmailPrefix)
	if !ok || id.Email == "" || !auth.EmailMatches(pattern, id.Email, id.Domain) {
		return 0
	}
	if strings.HasPrefix(pattern, "*@") {
		return rankDomain
	}
	return rankEmail
}

// grants returns the secrets of a rule, in the order of the agent's
// secrets and then of the rule. A rule names only the agent's secrets, as
// config validation checks.
func (sp SandboxPolicy) grants(entries []string) []SecretGrant {
	byName := map[string]string{}
	var order []string
	add := func(name, from string) {
		if !slices.Contains(sp.Secrets, from) {
			return
		}
		if _, ok := byName[name]; !ok {
			order = append(order, name)
		}
		byName[name] = from
	}
	if slices.Contains(entries, agentdir.SecretsAll) {
		for _, name := range sp.Secrets {
			add(name, name)
		}
	}
	for _, e := range entries {
		if e == agentdir.SecretsNone {
			continue
		}
		if name, from, ok := agentdir.ParseSecretGrant(e); ok {
			add(name, from)
		}
	}
	var out []SecretGrant
	for _, name := range sp.Secrets {
		if from, ok := byName[name]; ok {
			out = append(out, SecretGrant{Name: name, From: from})
		}
	}
	for _, name := range order {
		if !slices.Contains(sp.Secrets, name) {
			out = append(out, SecretGrant{Name: name, From: byName[name]})
		}
	}
	return out
}

// Values returns the NAME=value pairs of the secrets of s, from stored.
// It reports the names of the secrets that have no stored value.
func (s Sandbox) Values(stored agentdir.Secrets) (env, missing []string) {
	for _, g := range s.Secrets {
		v, ok := stored[g.From]
		if !ok {
			missing = append(missing, g.From)
			continue
		}
		env = append(env, g.Name+"="+v)
	}
	return env, missing
}

// SandboxLines describe what the containers of each kind of caller get:
// you and the owner pairings, each rule, other callers, and anonymous
// callers if they may call. Without a sandbox the rules don't apply, so
// there are none.
func (sp SandboxPolicy) SandboxLines(ownerPairings int, anonymous bool) []SandboxLine {
	if sp.Local {
		return nil
	}
	you := "you"
	if ownerPairings > 0 {
		you = "you and owner pairings"
	}
	lines := []SandboxLine{{Who: you, Sandbox: sp.Resolve(auth.Owner())}}
	other := sp.defaultOf(false)
	for _, r := range sp.Rules {
		var who []string
		for _, m := range r.Match {
			if m != agentdir.MatchSelf && m != agentdir.MatchGuests {
				who = append(who, describeMatcher(m))
			}
		}
		if len(who) == 0 {
			continue
		}
		line := SandboxLine{Who: strings.Join(who, ", "), Sandbox: sp.apply(other, r)}
		if len(r.Match) == 1 {
			line.Pairing, _ = strings.CutPrefix(r.Match[0], agentdir.MatchPairingPrefix)
		}
		lines = append(lines, line)
	}
	lines = append(lines, SandboxLine{Who: OtherCallers, Sandbox: other})
	if anonymous {
		lines = append(lines, SandboxLine{Who: Guests, Sandbox: sp.Resolve(auth.Principal{Role: auth.RoleAnonymous}), Anonymous: true})
	}
	return lines
}

// OtherCallers names the callers who aren't owners or anonymous and whom
// no rule matches, who get the default.
const OtherCallers = "other callers"

// Guests names the guests in the lines: the callers without a token or
// account, which the access policy calls anonymous.
const Guests = "guests"

// SandboxLine is what the containers of one kind of caller get.
type SandboxLine struct {
	Who     string
	Sandbox Sandbox
	// Pairing names the pairing of a rule of one pairing, and Anonymous is
	// set for guests.
	Pairing   string
	Anonymous bool
}

func describeMatcher(m string) string {
	if name, ok := strings.CutPrefix(m, agentdir.MatchPairingPrefix); ok {
		return "the pairing " + name
	}
	if pattern, ok := strings.CutPrefix(m, auth.MemberEmailPrefix); ok {
		if strings.HasPrefix(pattern, "*@") {
			return "emails matching " + pattern
		}
		return pattern
	}
	if m == auth.MemberPrivate {
		return "the private network"
	}
	return m
}
