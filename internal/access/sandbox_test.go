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
		{Match: []string{"private"}, Storage: agentdir.StorageEmpty, Secrets: []string{"none"}},
	}
	user := func(id auth.Identity) auth.Principal {
		return auth.Principal{ID: id.PrincipalID(), Role: auth.RoleAuthenticated, Identity: id}
	}
	anonymous := func(id auth.Identity) auth.Principal {
		return auth.Principal{ID: auth.AnonymousID, Role: auth.RoleAnonymous, Identity: id}
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
		{name: "anonymous by default", caller: anonymous(auth.Identity{}), want: readOnly},
		{name: "owner ignores rules for others", rules: rules, caller: auth.Owner(), want: shared},
		{name: "self rule narrows the owner", rules: []agentdir.SandboxRule{{Match: []string{"self"}, Secrets: []string{"GITHUB_TOKEN"}}}, caller: auth.Owner(), want: Sandbox{Storage: agentdir.StorageShared, Secrets: []SecretGrant{{"GITHUB_TOKEN", "GITHUB_TOKEN"}}}},
		{name: "pairing beats domain", rules: rules, caller: user(auth.Identity{Pairing: "ben", Email: "ben@example.com", Domain: "example.com"}), want: shared},
		{name: "webhook node by pairing", rules: rules, caller: user(auth.Identity{WebhookNode: "ben"}), want: shared},
		{name: "address beats domain", rules: rules, caller: user(auth.Identity{Email: "Ann@example.com", Domain: "example.com", EmailSource: auth.EmailFromGoogle}), want: shared},
		{name: "domain", rules: rules, caller: user(auth.Identity{Email: "cy@example.com", Domain: "example.com", Network: network.ListenerPrivate}), want: Sandbox{Storage: agentdir.StorageOwn, Secrets: all[:2]}},
		{name: "domain needs a proven domain", rules: rules, caller: user(auth.Identity{Email: "cy@example.com", Network: network.ListenerPrivate}), want: Sandbox{Storage: agentdir.StorageEmpty}},
		{name: "anyone doesn't match pairings", rules: rules, caller: user(auth.Identity{Network: network.ListenerPublic, Pairing: "dan"}), want: shared},
		{name: "anonymous callers get the rule of anyone", rules: rules, caller: anonymous(auth.Identity{Email: "cy@example.com", Domain: "example.com"}), want: Sandbox{Storage: agentdir.StorageReadOnly, Secrets: []SecretGrant{{"ANTHROPIC_API_KEY", "GUEST_KEY"}}}},
		{name: "a rule widens anonymous callers", rules: []agentdir.SandboxRule{{Match: []string{"anyone"}, Secrets: []string{"GITHUB_TOKEN"}}}, caller: anonymous(auth.Identity{}), want: Sandbox{Storage: agentdir.StorageReadOnly, Secrets: []SecretGrant{{"GITHUB_TOKEN", "GITHUB_TOKEN"}}}},
		{name: "anonymous callers narrowed to empty", rules: []agentdir.SandboxRule{{Match: []string{"anyone"}, Storage: agentdir.StorageEmpty}}, caller: anonymous(auth.Identity{}), want: Sandbox{Storage: agentdir.StorageEmpty}},
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
		{Match: []string{"anyone"}, Storage: agentdir.StorageReadOnly},
	}
	gemini := func(rules ...agentdir.SandboxRule) *agentdir.Config {
		return &agentdir.Config{Harness: "opencode", Model: "google/gemini", Secrets: []string{"GEMINI_API_KEY"}, Policy: agentdir.PolicyConfig{PublicAllowIncoming: agentdir.AccessAnyone, Sandbox: rules}}
	}
	tests := []struct {
		name          string
		cfg           *agentdir.Config
		pairings      Pairings
		wantLines     []string
		wantNotes     []string
		wantAnonymous string
	}{
		{name: "defaults", cfg: &agentdir.Config{Secrets: []string{"KEY"}}},
		{
			name: "anonymous callers", cfg: &agentdir.Config{Secrets: []string{"KEY"}, Policy: agentdir.PolicyConfig{PublicAllowIncoming: agentdir.AccessAnyone}},
			wantLines: []string{"you: shared workspace, secrets KEY", "other callers: shared workspace, secrets KEY", "anonymous callers: read-only workspace, no secrets"},
		},
		{
			name: "rules", cfg: &agentdir.Config{Secrets: []string{"KEY", "GUEST"}, Policy: agentdir.PolicyConfig{Sandbox: rules}}, pairings: Pairings{All: 2, Owners: 1},
			wantLines: []string{
				"you and owner pairings: shared workspace, secrets KEY, GUEST",
				"the pairing ben, emails matching *@example.com: own copy of the workspace, secrets GUEST",
				"other callers: shared workspace, secrets KEY, GUEST",
			},
		},
		{
			name: "pairing without its model key", pairings: Pairings{All: 1},
			cfg:           gemini(agentdir.SandboxRule{Match: []string{"pairing:ben"}, Secrets: []string{"none"}}),
			wantLines:     []string{"you: shared workspace, secrets GEMINI_API_KEY", "the pairing ben: shared workspace, no secrets", "other callers: shared workspace, secrets GEMINI_API_KEY", "anonymous callers: read-only workspace, no secrets"},
			wantNotes:     []string{"ben's agent gets no Google key and can't reach its model. Give one with `a2a-serve pair set ben --secrets GEMINI_API_KEY`, or a key of its own with `--secrets GEMINI_API_KEY=secret:<STORED_KEY>`."},
			wantAnonymous: "Anonymous callers' agent gets no Google key and can't reach its model. Give it a key of its own with `a2a-serve config set --secret GUEST_KEY --anonymous-secrets GEMINI_API_KEY=secret:GUEST_KEY --restart`, or share yours with `a2a-serve config set --anonymous-secrets GEMINI_API_KEY`.",
		},
		{
			name: "anonymous callers with a guest key", pairings: Pairings{All: 1},
			cfg: func() *agentdir.Config {
				c := gemini(agentdir.SandboxRule{Match: []string{"anyone"}, Secrets: []string{"GEMINI_API_KEY=secret:GUEST"}})
				c.Secrets = append(c.Secrets, "GUEST")
				return c
			}(),
			wantLines: []string{"you: shared workspace, secrets GEMINI_API_KEY, GUEST", "other callers: shared workspace, secrets GEMINI_API_KEY, GUEST", "anonymous callers: read-only workspace, secrets GEMINI_API_KEY (from GUEST)"},
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
			if got.AnonymousNote != tt.wantAnonymous {
				t.Fatalf("DescribeConfig().AnonymousNote = %q, want %q", got.AnonymousNote, tt.wantAnonymous)
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
	ann := auth.Principal{ID: "email:ann@example.com", Role: auth.RoleAuthenticated, Identity: auth.IdentityOf("email:ann@example.com")}

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
	ben := auth.Principal{ID: "pair:ben", Role: auth.RoleAuthenticated, Identity: auth.IdentityOf("pair:ben")}

	got := sp.ResolveUnverified(ben)

	if diff := cmp.Diff(Sandbox{Storage: agentdir.StorageOwn}, got); diff != "" {
		t.Fatalf("ResolveUnverified() wrong result (-want +got) diff = %s", diff)
	}
}
