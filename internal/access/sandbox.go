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
		storage = "no sandbox"
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
	// Local is set for an agent without a sandbox, where only the secrets
	// of a rule apply.
	Local bool
}

// SandboxPolicyOf returns the sandbox policy of cfg.
func SandboxPolicyOf(cfg *agentdir.Config) SandboxPolicy {
	return SandboxPolicy{Rules: cfg.Policy.Sandbox, Secrets: cfg.Secrets, Local: cfg.Sandbox == agentdir.SandboxLocal}
}

// Describe says what sb gives under the policy, such as "read-only
// workspace, no secrets", or "no sandbox, no secrets" without a sandbox.
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
//   - anonymous callers share one container, which the rule of anyone sets;
//   - every other caller gets a container of their own, which the most
//     specific rule that matches them sets: pairing:<name>, then
//     email:<address>, then email:*@<domain>, then private, then anyone.
//     Between rules of the same specificity, the first one wins.
//
// With no rule, owners get the shared workspace and every secret, and
// everyone else a read-only workspace and no secrets.
func (sp SandboxPolicy) Resolve(p auth.Principal) Sandbox {
	if i := sp.match(p); i >= 0 {
		return sp.sandboxOf(sp.Rules[i])
	}
	if p.Role == auth.RoleOwner {
		return Sandbox{Storage: agentdir.StorageShared, Secrets: sp.grants([]string{agentdir.SecretsAll})}
	}
	return Sandbox{Storage: agentdir.StorageReadOnly}
}

// match returns the index of the rule of p, or -1.
func (sp SandboxPolicy) match(p auth.Principal) int {
	best, bestRank := -1, 0
	for i, r := range sp.Rules {
		for _, m := range r.Match {
			if rank := matchRank(m, p); rank > bestRank {
				best, bestRank = i, rank
			}
		}
	}
	return best
}

func matchRank(m string, p auth.Principal) int {
	switch p.Role {
	case auth.RoleOwner:
		if m == agentdir.MatchSelf {
			return rankPairing
		}
		return 0
	case auth.RoleAnonymous:
		if m == agentdir.MatchAnyone {
			return rankAnyone
		}
		return 0
	}
	id := p.Identity
	switch m {
	case agentdir.MatchAnyone:
		return rankAnyone
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

func (sp SandboxPolicy) sandboxOf(r agentdir.SandboxRule) Sandbox {
	storage := r.Storage
	if storage == "" {
		storage = agentdir.StorageReadOnly
	}
	return Sandbox{Storage: storage, Secrets: sp.grants(r.Secrets)}
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

// SandboxLines describe what the containers of each kind of caller get: you
// and the owner pairings, each rule, and everyone else.
func (sp SandboxPolicy) SandboxLines(ownerPairings int) []string {
	you := "you"
	if ownerPairings > 0 {
		you = "you and owner pairings"
	}
	lines := []string{you + ": " + sp.Describe(sp.Resolve(auth.Owner()))}
	for _, r := range sp.Rules {
		var who []string
		for _, m := range r.Match {
			if m != agentdir.MatchSelf && m != agentdir.MatchAnyone {
				who = append(who, describeMatcher(m))
			}
		}
		if len(who) > 0 {
			lines = append(lines, strings.Join(who, ", ")+": "+sp.Describe(sp.sandboxOf(r)))
		}
	}
	return append(lines, "everyone else: "+sp.Describe(sp.Resolve(auth.Principal{Role: auth.RoleAnonymous})))
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
