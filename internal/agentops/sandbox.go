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

package agentops

import (
	"context"
	"errors"
	"slices"

	"github.com/a2aproject/a2a-serve/internal/access"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/pairapi"
	"github.com/a2aproject/a2a-serve/internal/pairings"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// errOwnerSandbox refuses a sandbox rule for an owner pairing.
var errOwnerSandbox = agenterr.Invalidf("owners share your agent container, so --storage and --secrets don't apply to them; the sandbox rule of self in config.yaml (`a2a-serve config edit`) sets what it gets")

// withSandboxRule returns cfg's policy with the rule of the pairing name
// changed by ch. A pairing in a rule with other matchers moves to a rule of
// its own, which starts from that rule.
func withSandboxRule(p agentdir.PolicyConfig, name string, ch controlapi.SandboxChange) agentdir.PolicyConfig {
	p = p.Clone()
	p.SetSandboxRule(p.OwnRule(agentdir.PairingMatcher(name), func(r *agentdir.SandboxRule) {
		if ch.Storage != "" {
			r.Storage = ch.Storage
		}
		if ch.Secrets != nil {
			r.Secrets = slices.Clone(*ch.Secrets)
		}
	}))
	return p
}

// setSandbox saves the change of the sandbox rule of the pairing name, and
// returns the function that puts the pairing back in the rules it was in,
// leaving other rules as they are then. It checks the config in use and
// config.yaml, which may hold a change waiting for a restart, such as a
// removed secret.
func (s *Service) setSandbox(name string, ch controlapi.SandboxChange) (undo func() error, err error) {
	m := agentdir.PairingMatcher(name)
	var before []agentdir.SandboxRule
	for _, r := range s.deps.Runtime.Config().Policy.Sandbox {
		if slices.Contains(r.Match, m) {
			before = append(before, agentdir.SandboxRule{Match: slices.Clone(r.Match), Storage: r.Storage, Secrets: slices.Clone(r.Secrets)})
		}
	}
	err = s.deps.Runtime.Update(func(c *agentdir.Config) error {
		c.Policy = withSandboxRule(c.Policy, name, ch)
		if err := c.Validate(); err != nil {
			return agenterr.Invalidf("%v", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return func() error {
		return s.deps.Runtime.Update(func(c *agentdir.Config) error {
			c.Policy = c.Policy.Clone()
			c.Policy.RemoveSandboxMatcher(m)
			for _, r := range before {
				others := slices.DeleteFunc(slices.Clone(r.Match), func(e string) bool { return e == m })
				i := slices.IndexFunc(c.Policy.Sandbox, func(o agentdir.SandboxRule) bool { return len(others) > 0 && slices.Equal(o.Match, others) })
				if i >= 0 {
					c.Policy.Sandbox[i].Match = r.Match
					continue
				}
				c.Policy.Sandbox = append(c.Policy.Sandbox, r)
			}
			return nil
		})
	}, nil
}

// removeSandbox deletes the pairing name from the sandbox rules.
func (s *Service) removeSandbox(name string) error {
	m := agentdir.PairingMatcher(name)
	p := s.deps.Runtime.Config().Policy.Clone()
	if !p.RemoveSandboxMatcher(m) {
		return nil
	}
	return s.deps.Runtime.Update(func(c *agentdir.Config) error {
		c.Policy.RemoveSandboxMatcher(m)
		return nil
	})
}

// pairingRole returns the role of the pairing name, or "" if there is none.
func (s *Service) pairingRole(ctx context.Context, name string) (string, error) {
	p, err := s.deps.Store.Pairing(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	return p.Role, err
}

// narrowedSandbox describes what the agent container of the pairing p gets
// when its rules narrow the default, or returns nil: owners share your
// container, and without the Docker sandbox the rules don't apply.
func (s *Service) narrowedSandbox(p pairings.Pairing) *controlapi.PairingSandbox {
	sp := access.SandboxPolicyOf(s.deps.Runtime.Config())
	if p.Role == auth.RoleOwner || sp.Local || s.resolvePairing(p).Equal(sp.Default()) {
		return nil
	}
	return s.sandboxOf(p)
}

// resolvePairing returns the sandbox of the agent container of the pairing
// p.
func (s *Service) resolvePairing(p pairings.Pairing) access.Sandbox {
	id := auth.Identity{Pairing: p.Name, PairingRole: p.Role}
	if p.Kind == pairapi.PairingWebhook {
		id = auth.Identity{WebhookNode: p.Name}
	}
	role := auth.RoleAuthenticated
	if p.Role == auth.RoleOwner {
		role = auth.RoleOwner
	}
	return access.SandboxPolicyOf(s.deps.Runtime.Config()).Resolve(auth.Principal{ID: p.Principal, Role: role, Identity: id})
}

// sandboxHints says what a change of a sandbox rule doesn't do.
func (s *Service) sandboxHints(ch *controlapi.SandboxChange) []string {
	if ch == nil || ch.Storage == "" || !access.SandboxPolicyOf(s.deps.Runtime.Config()).Local {
		return nil
	}
	return []string{"--storage has no effect without the Docker sandbox; set one with `a2a-serve config set --sandbox docker`"}
}

// sandboxOf describes what the agent container of the pairing p gets.
func (s *Service) sandboxOf(p pairings.Pairing) *controlapi.PairingSandbox {
	cfg := s.deps.Runtime.Config()
	sp := access.SandboxPolicyOf(cfg)
	sb := s.resolvePairing(p)
	out := &controlapi.PairingSandbox{Storage: sb.Storage, Secrets: []string{}, Summary: sp.Describe(sb)}
	if p.Role != auth.RoleOwner {
		out.Warning = access.ModelKeyNote(cfg, access.SandboxLine{Who: "the pairing " + p.Name, Sandbox: sb, Pairing: p.Name})
	}
	for _, g := range sb.Secrets {
		e := g.Name
		if g.From != g.Name {
			e += "=secret:" + g.From
		}
		out.Secrets = append(out.Secrets, e)
	}
	return out
}
