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
// changed by ch.
func withSandboxRule(p agentdir.PolicyConfig, name string, ch controlapi.SandboxChange) agentdir.PolicyConfig {
	m := agentdir.PairingMatcher(name)
	r, _ := p.SandboxRuleOf(m)
	r.Match = []string{m}
	if ch.Storage != "" {
		r.Storage = ch.Storage
	}
	if ch.Secrets != nil {
		r.Secrets = slices.Clone(*ch.Secrets)
	}
	p = p.Clone()
	p.SetSandboxRule(r)
	return p
}

// checkSandbox refuses a change of the sandbox rule of the pairing name that
// would make the config invalid, before the pairing changes.
func (s *Service) checkSandbox(name string, ch controlapi.SandboxChange) error {
	cfg := s.deps.Runtime.Config().Clone()
	cfg.Policy = withSandboxRule(cfg.Policy, name, ch)
	if err := cfg.Validate(); err != nil {
		return agenterr.Invalidf("%v", err)
	}
	return nil
}

// setSandbox saves the change of the sandbox rule of the pairing name.
func (s *Service) setSandbox(name string, ch controlapi.SandboxChange) error {
	return s.deps.Runtime.Update(func(c *agentdir.Config) error {
		c.Policy = withSandboxRule(c.Policy, name, ch)
		return nil
	})
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

// sandboxOf describes what the agent container of the pairing p gets.
func (s *Service) sandboxOf(p pairings.Pairing) *controlapi.PairingSandbox {
	id := auth.Identity{Pairing: p.Name, PairingRole: p.Role}
	if p.Kind == pairapi.PairingWebhook {
		id = auth.Identity{WebhookNode: p.Name}
	}
	role := auth.RoleAuthenticated
	if p.Role == auth.RoleOwner {
		role = auth.RoleOwner
	}
	sp := access.SandboxPolicyOf(s.deps.Runtime.Config())
	sb := sp.Resolve(auth.Principal{ID: p.Principal, Role: role, Identity: id})
	out := &controlapi.PairingSandbox{Storage: sb.Storage, Secrets: []string{}, Summary: sp.Describe(sb)}
	for _, g := range sb.Secrets {
		e := g.Name
		if g.From != g.Name {
			e += "=secret:" + g.From
		}
		out.Secrets = append(out.Secrets, e)
	}
	return out
}
