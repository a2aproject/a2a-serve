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
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/network"
)

func TestSandboxPolicyResolve(t *testing.T) {
	t.Parallel()
	secrets := []string{"ANTHROPIC_API_KEY", "GITHUB_TOKEN", "GUEST_KEY"}
	rules := []agentdir.SandboxRule{
		{Match: []string{"email:*@example.com"}, Storage: agentdir.StorageOwn, Secrets: []string{"ANTHROPIC_API_KEY", "GITHUB_TOKEN"}},
		{Match: []string{"anyone"}, Storage: agentdir.StorageReadOnly, Secrets: []string{"ANTHROPIC_API_KEY=secret:GUEST_KEY"}},
		{Match: []string{"pairing:ben", "email:ann@example.com"}, Storage: agentdir.StorageShared, Secrets: []string{"all"}},
		{Match: []string{"private"}, Storage: agentdir.StorageEmpty},
	}
	user := func(id auth.Identity) auth.Principal {
		return auth.Principal{ID: id.PrincipalID(), Role: auth.RoleAuthenticated, Identity: id}
	}
	all := []SecretGrant{{"ANTHROPIC_API_KEY", "ANTHROPIC_API_KEY"}, {"GITHUB_TOKEN", "GITHUB_TOKEN"}, {"GUEST_KEY", "GUEST_KEY"}}
	own := Sandbox{Storage: agentdir.StorageOwn, Secrets: []SecretGrant{{"ANTHROPIC_API_KEY", "ANTHROPIC_API_KEY"}, {"GITHUB_TOKEN", "GITHUB_TOKEN"}}}
	guest := Sandbox{Storage: agentdir.StorageReadOnly, Secrets: []SecretGrant{{"ANTHROPIC_API_KEY", "GUEST_KEY"}}}
	tests := []struct {
		name   string
		rules  []agentdir.SandboxRule
		caller auth.Principal
		want   Sandbox
	}{
		{name: "owner by default", caller: auth.Owner(), want: Sandbox{Storage: agentdir.StorageShared, Secrets: all}},
		{name: "owner pairing by default", caller: auth.Principal{ID: "pair:ann", Role: auth.RoleOwner, Identity: auth.Identity{Pairing: "ann", PairingRole: auth.RoleOwner}}, want: Sandbox{Storage: agentdir.StorageShared, Secrets: all}},
		{name: "user by default", caller: user(auth.Identity{Pairing: "ben"}), want: Sandbox{Storage: agentdir.StorageReadOnly}},
		{name: "anonymous by default", caller: auth.Principal{ID: auth.AnonymousID, Role: auth.RoleAnonymous}, want: Sandbox{Storage: agentdir.StorageReadOnly}},
		{name: "owner ignores rules for others", rules: rules, caller: auth.Owner(), want: Sandbox{Storage: agentdir.StorageShared, Secrets: all}},
		{name: "self rule", rules: []agentdir.SandboxRule{{Match: []string{"self"}, Storage: agentdir.StorageShared, Secrets: []string{"GITHUB_TOKEN"}}}, caller: auth.Owner(), want: Sandbox{Storage: agentdir.StorageShared, Secrets: []SecretGrant{{"GITHUB_TOKEN", "GITHUB_TOKEN"}}}},
		{name: "pairing beats domain", rules: rules, caller: user(auth.Identity{Pairing: "ben", Email: "ben@example.com", Domain: "example.com"}), want: Sandbox{Storage: agentdir.StorageShared, Secrets: all}},
		{name: "webhook node by pairing", rules: rules, caller: user(auth.Identity{WebhookNode: "ben"}), want: Sandbox{Storage: agentdir.StorageShared, Secrets: all}},
		{name: "address beats domain", rules: rules, caller: user(auth.Identity{Email: "Ann@example.com", Domain: "example.com", EmailSource: auth.EmailFromGoogle}), want: Sandbox{Storage: agentdir.StorageShared, Secrets: all}},
		{name: "domain", rules: rules, caller: user(auth.Identity{Email: "cy@example.com", Domain: "example.com", Network: network.ListenerPrivate}), want: own},
		{name: "domain needs a proven domain", rules: rules, caller: user(auth.Identity{Email: "cy@example.com", Network: network.ListenerPrivate}), want: Sandbox{Storage: agentdir.StorageEmpty}},
		{name: "private", rules: rules, caller: user(auth.Identity{Network: network.ListenerPrivate, Node: "n1"}), want: Sandbox{Storage: agentdir.StorageEmpty}},
		{name: "anyone matches other users", rules: rules, caller: user(auth.Identity{Network: network.ListenerPublic, Pairing: "dan"}), want: guest},
		{name: "anonymous gets the rule of anyone", rules: rules, caller: auth.Principal{ID: auth.AnonymousID, Role: auth.RoleAnonymous, Identity: auth.Identity{Email: "cy@example.com", Domain: "example.com"}}, want: guest},
		{name: "a rule without storage is read-only", rules: []agentdir.SandboxRule{{Match: []string{"anyone"}, Secrets: []string{"GITHUB_TOKEN"}}}, caller: user(auth.Identity{Pairing: "ben"}), want: Sandbox{Storage: agentdir.StorageReadOnly, Secrets: []SecretGrant{{"GITHUB_TOKEN", "GITHUB_TOKEN"}}}},
		{name: "first rule of the same specificity wins", rules: []agentdir.SandboxRule{{Match: []string{"email:*@example.com"}, Storage: agentdir.StorageOwn}, {Match: []string{"email:*@Example.com"}, Storage: agentdir.StorageShared}}, caller: user(auth.Identity{Email: "cy@example.com", Domain: "example.com"}), want: Sandbox{Storage: agentdir.StorageOwn}},
		{name: "all with a renamed secret", rules: []agentdir.SandboxRule{{Match: []string{"anyone"}, Secrets: []string{"all", "GITHUB_TOKEN=secret:GUEST_KEY"}}}, caller: user(auth.Identity{Pairing: "ben"}), want: Sandbox{Storage: agentdir.StorageReadOnly, Secrets: []SecretGrant{{"ANTHROPIC_API_KEY", "ANTHROPIC_API_KEY"}, {"GITHUB_TOKEN", "GUEST_KEY"}, {"GUEST_KEY", "GUEST_KEY"}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := SandboxPolicy{Rules: tt.rules, Secrets: secrets}.Resolve(tt.caller)
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Fatalf("Resolve() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestSandboxCovers(t *testing.T) {
	t.Parallel()
	key := SecretGrant{"KEY", "KEY"}
	guest := SecretGrant{"KEY", "GUEST"}
	tests := []struct {
		name string
		s, o Sandbox
		want bool
	}{
		{name: "same", s: Sandbox{Storage: agentdir.StorageShared, Secrets: []SecretGrant{key}}, o: Sandbox{Storage: agentdir.StorageShared, Secrets: []SecretGrant{key}}, want: true},
		{name: "more storage", s: Sandbox{Storage: agentdir.StorageShared}, o: Sandbox{Storage: agentdir.StorageReadOnly}, want: true},
		{name: "own covers read-only", s: Sandbox{Storage: agentdir.StorageOwn}, o: Sandbox{Storage: agentdir.StorageReadOnly}, want: true},
		{name: "less storage", s: Sandbox{Storage: agentdir.StorageEmpty}, o: Sandbox{Storage: agentdir.StorageReadOnly}},
		{name: "missing secret", s: Sandbox{Storage: agentdir.StorageShared}, o: Sandbox{Storage: agentdir.StorageReadOnly, Secrets: []SecretGrant{key}}},
		{name: "other source", s: Sandbox{Storage: agentdir.StorageShared, Secrets: []SecretGrant{guest}}, o: Sandbox{Storage: agentdir.StorageShared, Secrets: []SecretGrant{key}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.s.Covers(tt.o); got != tt.want {
				t.Fatalf("Covers() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDescribeConfigSandbox(t *testing.T) {
	t.Parallel()
	shared := &agentdir.Config{Secrets: []string{"KEY", "GUEST"}, Policy: agentdir.PolicyConfig{Sandbox: []agentdir.SandboxRule{
		{Match: []string{"pairing:ben", "email:*@example.com"}, Storage: agentdir.StorageOwn, Secrets: []string{"KEY"}},
		{Match: []string{"anyone"}, Secrets: []string{"KEY=secret:GUEST"}},
	}}}
	tests := []struct {
		name      string
		cfg       *agentdir.Config
		pairings  Pairings
		wantLines []string
		wantNotes []string
	}{
		{
			name: "default, not shared", cfg: &agentdir.Config{Secrets: []string{"KEY"}},
			wantLines: []string{"you: shared workspace, secrets KEY", "everyone else: read-only workspace, no secrets"},
		},
		{
			name: "default, shared", cfg: &agentdir.Config{Secrets: []string{"KEY"}}, pairings: Pairings{All: 1},
			wantLines: []string{"you: shared workspace, secrets KEY", "everyone else: read-only workspace, no secrets"},
			wantNotes: []string{"callers who aren't owners now get a read-only workspace and no secrets; to give them the shared workspace and every secret as before, run `a2a-serve config set --caller-storage shared --caller-secrets all`; to keep the new default and hide this note, run `a2a-serve config set --caller-storage read-only`"},
		},
		{
			name: "rules", cfg: shared, pairings: Pairings{All: 2, Owners: 1},
			wantLines: []string{
				"you and owner pairings: shared workspace, secrets KEY, GUEST",
				"the pairing ben, emails matching *@example.com: own copy of the workspace, secrets KEY",
				"everyone else: read-only workspace, secrets KEY (from GUEST)",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := DescribeConfig(tt.cfg, Network{}, tt.pairings)
			if diff := cmp.Diff(tt.wantLines, got.Sandbox); diff != "" {
				t.Fatalf("DescribeConfig().Sandbox wrong result (-want +got) diff = %s", diff)
			}
			if diff := cmp.Diff(tt.wantNotes, got.SandboxNotes); diff != "" {
				t.Fatalf("DescribeConfig().SandboxNotes wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}
