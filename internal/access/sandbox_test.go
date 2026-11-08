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
		{Match: []string{"guests"}, Storage: agentdir.StorageReadOnly, Secrets: []string{"ANTHROPIC_API_KEY=secret:GUEST_KEY"}},
		{Match: []string{"pairing:ben", "email:ann@example.com"}, Storage: agentdir.StorageShared, Secrets: []string{"all"}},
		{Match: []string{"private"}, Storage: agentdir.StorageEmpty, Secrets: []string{"none"}},
	}
	user := func(id auth.Identity) auth.Principal {
		return auth.Principal{ID: id.PrincipalID(), Role: auth.RoleMember, Identity: id}
	}
	guest := func(id auth.Identity) auth.Principal {
		return auth.Principal{ID: auth.GuestID, Role: auth.RoleGuest, Identity: id}
	}
	all := []SecretGrant{{"ANTHROPIC_API_KEY", "ANTHROPIC_API_KEY"}, {"GITHUB_TOKEN", "GITHUB_TOKEN"}, {"GUEST_KEY", "GUEST_KEY"}}
	shared := Sandbox{Storage: agentdir.StorageShared, Secrets: all}
	readOnly := Sandbox{Storage: agentdir.StorageReadOnly}
	tests := []struct {
		name   string
		rules  []agentdir.SandboxRule
		caller auth.Principal
		want   Sandbox
	}{
		{name: "owner by default", caller: auth.Owner(), want: shared},
		{name: "owner pairing by default", caller: auth.Principal{ID: "pair:ann", Role: auth.RoleOwner, Identity: auth.Identity{Pairing: "ann", PairingRole: auth.RoleOwner}}, want: shared},
		{name: "pairing by default", caller: user(auth.Identity{Pairing: "ben"}), want: shared},
		{name: "private network member by default", caller: user(auth.Identity{Network: network.ListenerPrivate, Node: "n1"}), want: shared},
		{name: "guest by default", caller: guest(auth.Identity{}), want: readOnly},
		{name: "owner ignores rules for others", rules: rules, caller: auth.Owner(), want: shared},
		{name: "self rule narrows the owner", rules: []agentdir.SandboxRule{{Match: []string{"self"}, Secrets: []string{"GITHUB_TOKEN"}}}, caller: auth.Owner(), want: Sandbox{Storage: agentdir.StorageShared, Secrets: []SecretGrant{{"GITHUB_TOKEN", "GITHUB_TOKEN"}}}},
		{name: "pairing beats domain", rules: rules, caller: user(auth.Identity{Pairing: "ben", Email: "ben@example.com", Domain: "example.com"}), want: shared},
		{name: "webhook node by pairing", rules: rules, caller: user(auth.Identity{WebhookNode: "ben"}), want: shared},
		{name: "address beats domain", rules: rules, caller: user(auth.Identity{Email: "Ann@example.com", Domain: "example.com", EmailSource: auth.EmailFromGoogle}), want: shared},
		{name: "domain", rules: rules, caller: user(auth.Identity{Email: "cy@example.com", Domain: "example.com", Network: network.ListenerPrivate}), want: Sandbox{Storage: agentdir.StorageOwn, Secrets: all[:2]}},
		{name: "domain needs a proven domain", rules: rules, caller: user(auth.Identity{Email: "cy@example.com", Network: network.ListenerPrivate}), want: Sandbox{Storage: agentdir.StorageEmpty}},
		{name: "anyone doesn't match pairings", rules: rules, caller: user(auth.Identity{Network: network.ListenerPublic, Pairing: "dan"}), want: shared},
		{name: "guests get the rule of anyone", rules: rules, caller: guest(auth.Identity{Email: "cy@example.com", Domain: "example.com"}), want: Sandbox{Storage: agentdir.StorageReadOnly, Secrets: []SecretGrant{{"ANTHROPIC_API_KEY", "GUEST_KEY"}}}},
		{name: "a rule widens guests", rules: []agentdir.SandboxRule{{Match: []string{"guests"}, Secrets: []string{"GITHUB_TOKEN"}}}, caller: guest(auth.Identity{}), want: Sandbox{Storage: agentdir.StorageReadOnly, Secrets: []SecretGrant{{"GITHUB_TOKEN", "GITHUB_TOKEN"}}}},
		{name: "guests narrowed to empty", rules: []agentdir.SandboxRule{{Match: []string{"guests"}, Storage: agentdir.StorageEmpty}}, caller: guest(auth.Identity{}), want: Sandbox{Storage: agentdir.StorageEmpty}},
		{name: "a rule without storage keeps the default", rules: []agentdir.SandboxRule{{Match: []string{"pairing:ben"}, Secrets: []string{"GITHUB_TOKEN"}}}, caller: user(auth.Identity{Pairing: "ben"}), want: Sandbox{Storage: agentdir.StorageShared, Secrets: []SecretGrant{{"GITHUB_TOKEN", "GITHUB_TOKEN"}}}},
		{name: "a rule without secrets keeps the default", rules: []agentdir.SandboxRule{{Match: []string{"pairing:ben"}, Storage: agentdir.StorageOwn}}, caller: user(auth.Identity{Pairing: "ben"}), want: Sandbox{Storage: agentdir.StorageOwn, Secrets: all}},
		{name: "first rule of the same specificity wins", rules: []agentdir.SandboxRule{{Match: []string{"email:*@example.com"}, Storage: agentdir.StorageOwn}, {Match: []string{"email:*@Example.com"}, Storage: agentdir.StorageEmpty}}, caller: user(auth.Identity{Email: "cy@example.com", Domain: "example.com"}), want: Sandbox{Storage: agentdir.StorageOwn, Secrets: all}},
		{name: "a pairing rule falls back to broader rules for what it leaves out", rules: []agentdir.SandboxRule{
			{Match: []string{"private"}, Storage: agentdir.StorageReadOnly, Secrets: []string{"ANTHROPIC_API_KEY=secret:GUEST_KEY"}},
			{Match: []string{"pairing:ben"}, Storage: agentdir.StorageOwn},
		}, caller: user(auth.Identity{Pairing: "ben", Network: network.ListenerPrivate}), want: Sandbox{Storage: agentdir.StorageOwn, Secrets: []SecretGrant{{"ANTHROPIC_API_KEY", "GUEST_KEY"}}}},
		{name: "empty secrets keep the default", rules: []agentdir.SandboxRule{{Match: []string{"pairing:ben"}, Storage: agentdir.StorageOwn, Secrets: []string{}}}, caller: user(auth.Identity{Pairing: "ben"}), want: Sandbox{Storage: agentdir.StorageOwn, Secrets: all}},
		{name: "all with a renamed secret", rules: []agentdir.SandboxRule{{Match: []string{"pairing:ben"}, Secrets: []string{"GITHUB_TOKEN=secret:GUEST_KEY"}}}, caller: user(auth.Identity{Pairing: "ben"}), want: Sandbox{Storage: agentdir.StorageShared, Secrets: []SecretGrant{{"GITHUB_TOKEN", "GUEST_KEY"}}}},
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
	rules := []agentdir.SandboxRule{
		{Match: []string{"pairing:ben", "email:*@example.com"}, Storage: agentdir.StorageOwn, Secrets: []string{"GUEST"}},
		{Match: []string{"guests"}, Storage: agentdir.StorageReadOnly},
	}
	gemini := func(rules ...agentdir.SandboxRule) *agentdir.Config {
		return &agentdir.Config{Harness: "opencode", Model: "google/gemini", Secrets: []string{"GEMINI_API_KEY"}, Policy: agentdir.PolicyConfig{PublicAllowIncoming: agentdir.AccessAnyone, Sandbox: rules}}
	}
	tests := []struct {
		name      string
		cfg       *agentdir.Config
		pairings  Pairings
		wantLines []string
		wantNotes []string
		wantGuest string
	}{
		{name: "defaults", cfg: &agentdir.Config{Secrets: []string{"KEY"}}},
		{
			name: "guests", cfg: &agentdir.Config{Secrets: []string{"KEY"}, Policy: agentdir.PolicyConfig{PublicAllowIncoming: agentdir.AccessAnyone}},
			wantLines: []string{"you: shared workspace, secrets KEY", "members: shared workspace, secrets KEY", "guests: read-only workspace, no secrets"},
		},
		{
			name: "rules", cfg: &agentdir.Config{Secrets: []string{"KEY", "GUEST"}, Policy: agentdir.PolicyConfig{Sandbox: rules}}, pairings: Pairings{All: 2, Owners: 1},
			wantLines: []string{
				"you and owner pairings: shared workspace, secrets KEY, GUEST",
				"the pairing ben, emails matching *@example.com: own copy of the workspace, secrets GUEST",
				"other members: shared workspace, secrets KEY, GUEST",
			},
		},
		{
			name: "pairing without its model key", pairings: Pairings{All: 1},
			cfg:       gemini(agentdir.SandboxRule{Match: []string{"pairing:ben"}, Secrets: []string{"none"}}),
			wantLines: []string{"you: shared workspace, secrets GEMINI_API_KEY", "the pairing ben: shared workspace, no secrets", "other members: shared workspace, secrets GEMINI_API_KEY", "guests: read-only workspace, no secrets"},
			wantNotes: []string{"ben's agent gets no Google key and can't reach its model. Give one with `a2a-serve pair set ben --secrets GEMINI_API_KEY`, or a key of its own with `--secrets GEMINI_API_KEY=secret:<STORED_KEY>`."},
			wantGuest: "Guests' agent gets no Google key and can't reach its model. Give it a key of its own with `a2a-serve config set --secret GUEST_KEY --guest-secrets GEMINI_API_KEY=secret:GUEST_KEY --restart`, or share yours with `a2a-serve config set --guest-secrets GEMINI_API_KEY`.",
		},
		{
			name: "guests with a guest key", pairings: Pairings{All: 1},
			cfg: func() *agentdir.Config {
				c := gemini(agentdir.SandboxRule{Match: []string{"guests"}, Secrets: []string{"GEMINI_API_KEY=secret:GUEST"}})
				c.Secrets = append(c.Secrets, "GUEST")
				return c
			}(),
			wantLines: []string{"you: shared workspace, secrets GEMINI_API_KEY, GUEST", "members: shared workspace, secrets GEMINI_API_KEY, GUEST", "guests: read-only workspace, secrets GEMINI_API_KEY (from GUEST)"},
		},
		{
			name: "private rule without a private network", pairings: Pairings{All: 1},
			cfg: &agentdir.Config{Secrets: []string{"KEY"}, Policy: agentdir.PolicyConfig{Sandbox: []agentdir.SandboxRule{
				{Match: []string{"private"}, Storage: agentdir.StorageReadOnly},
				{Match: []string{"pairing:ben", "private"}, Secrets: []string{"none"}},
			}}},
			wantLines: []string{"you: shared workspace, secrets KEY", "the pairing ben: shared workspace, no secrets", "other members: shared workspace, secrets KEY"},
		},
		{
			name: "rule of an unknown pairing", pairings: Pairings{All: 1, Names: map[string]bool{"ben": true}},
			cfg:       &agentdir.Config{Secrets: []string{"KEY"}, Policy: agentdir.PolicyConfig{Sandbox: []agentdir.SandboxRule{{Match: []string{"pairing:ben", "pairing:nobody"}, Storage: agentdir.StorageReadOnly}}}},
			wantLines: []string{"you: shared workspace, secrets KEY", "the pairing ben, the pairing nobody: read-only workspace, secrets KEY", "other members: shared workspace, secrets KEY"},
			wantNotes: []string{"no pairing or webhook node is named nobody, so the sandbox rule of pairing:nobody matches no caller"},
		},
		{
			name: "no sandbox", cfg: &agentdir.Config{Sandbox: agentdir.SandboxLocal, Secrets: []string{"KEY"}, Policy: agentdir.PolicyConfig{Sandbox: rules}}, pairings: Pairings{All: 1},
			wantNotes: []string{LocalSandboxNote},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := DescribeConfig(tt.cfg, Network{Public: true}, tt.pairings)
			if diff := cmp.Diff(tt.wantLines, got.Sandbox); diff != "" {
				t.Fatalf("DescribeConfig().Sandbox wrong result (-want +got) diff = %s", diff)
			}
			if diff := cmp.Diff(tt.wantNotes, got.SandboxNotes); diff != "" {
				t.Fatalf("DescribeConfig().SandboxNotes wrong result (-want +got) diff = %s", diff)
			}
			if got.GuestNote != tt.wantGuest {
				t.Fatalf("DescribeConfig().GuestNote = %q, want %q", got.GuestNote, tt.wantGuest)
			}
		})
	}
}

func TestSandboxPolicyResolveUnverified(t *testing.T) {
	t.Parallel()
	sp := SandboxPolicy{Secrets: []string{"KEY"}, Rules: []agentdir.SandboxRule{
		{Match: []string{"email:*@example.com"}, Storage: agentdir.StorageReadOnly},
		{Match: []string{"private"}, Secrets: []string{"none"}},
	}}
	ann := auth.Principal{ID: "email:ann@example.com", Role: auth.RoleMember, Identity: auth.IdentityOf("email:ann@example.com")}

	got := sp.ResolveUnverified(ann)

	if diff := cmp.Diff(Sandbox{Storage: agentdir.StorageReadOnly}, got); diff != "" {
		t.Fatalf("ResolveUnverified() wrong result (-want +got) diff = %s", diff)
	}
}

func TestSandboxPolicyResolveUnverifiedPairing(t *testing.T) {
	t.Parallel()
	sp := SandboxPolicy{Secrets: []string{"KEY"}, Rules: []agentdir.SandboxRule{
		{Match: []string{"pairing:ben"}, Storage: agentdir.StorageOwn},
		{Match: []string{"private"}, Secrets: []string{"none"}},
	}}
	ben := auth.Principal{ID: "pair:ben", Role: auth.RoleMember, Identity: auth.IdentityOf("pair:ben")}

	got := sp.ResolveUnverified(ben)

	if diff := cmp.Diff(Sandbox{Storage: agentdir.StorageOwn}, got); diff != "" {
		t.Fatalf("ResolveUnverified() wrong result (-want +got) diff = %s", diff)
	}
}
