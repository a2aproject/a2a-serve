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

package auth

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/network"
)

// testPolicy is the policy where everyone on the private network, if
// private, and emails are members.
func testPolicy(private bool, emails []string) Policy {
	var p Policy
	if private {
		p.Members = append(p.Members, MemberPrivate)
	}
	for _, e := range emails {
		p.Members = append(p.Members, MemberEmailPrefix+e)
	}
	return p
}

func TestPolicyResolve(t *testing.T) {
	t.Parallel()
	private := Identity{Network: network.ListenerPrivate, Provider: network.ProviderTailscale, Email: "anna@example.com", EmailSource: EmailFromNetwork}
	open := Policy{Guests: []string{MemberAnyone}}
	tests := []struct {
		name   string
		policy Policy
		id     Identity
		want   string
	}{
		{name: "self", policy: testPolicy(true, nil), id: Identity{Network: network.ListenerPrivate, Self: true}, want: RoleOwner},
		{name: "self through another agent", policy: testPolicy(true, nil), id: Identity{Network: network.ListenerPrivate, Self: true, Hops: 1}, want: RoleMember},
		{name: "self never on the public URL", policy: testPolicy(false, nil), id: Identity{Network: network.ListenerPublic, Self: true}},
		{name: "private network", policy: testPolicy(true, nil), id: private, want: RoleMember},
		{name: "closed private network", policy: testPolicy(false, nil), id: private},
		{name: "allowed email", policy: testPolicy(false, []string{"Anna@Example.com"}), id: private, want: RoleMember},
		{
			name:   "domain pattern with a proven domain",
			policy: testPolicy(false, []string{"*@example.com"}),
			id:     Identity{Network: network.ListenerPublic, Email: "anna@example.com", EmailSource: EmailFromGoogle, Domain: "example.com"},
			want:   RoleMember,
		},
		{
			name:   "domain pattern with another proven domain",
			policy: testPolicy(false, []string{"*@example.com"}),
			id:     Identity{Network: network.ListenerPublic, Email: "anna@example.com", EmailSource: EmailFromGoogle, Domain: "other.com"},
		},
		{name: "domain pattern without a proven domain", policy: testPolicy(false, []string{"*@example.com"}), id: private},
		{name: "full address from the relay", policy: testPolicy(false, []string{"anna@example.com"}), id: Identity{Network: network.ListenerPrivate, Provider: network.ProviderAgentMesh, Email: "anna@example.com", EmailSource: EmailFromNetwork}, want: RoleMember},
		{name: "owner pairing", policy: testPolicy(false, nil), id: Identity{Pairing: "phone", PairingRole: RoleOwner}, want: RoleOwner},
		{name: "highest of pairing and bindings", policy: Policy{Members: []string{MemberPrivate}}, id: Identity{Network: network.ListenerPrivate, Pairing: "anna", PairingRole: RoleOwner}, want: RoleOwner},
		{name: "webhook node", policy: testPolicy(false, nil), id: Identity{Network: network.ListenerPublic, WebhookNode: "grok"}, want: RoleMember},
		{name: "anyone on the public URL", policy: open, id: Identity{Network: network.ListenerPublic}, want: RoleGuest},
		{name: "anyone in a direct chat", policy: open, id: Identity{Network: NetworkChat, ChatAccount: "telegram:1", Direct: true}, want: RoleGuest},
		{name: "identified caller that matches nothing", policy: open, id: private, want: RoleGuest},
		{name: "unidentified caller on the private network", policy: open, id: Identity{Network: network.ListenerPrivate}},
		{name: "guest without members", policy: testPolicy(false, nil), id: Identity{Network: network.ListenerPublic}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.policy.Resolve(tc.id); got != tc.want {
				t.Fatalf("Policy.Resolve() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestIdentityPrincipalID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		id   Identity
		want string
	}{
		{name: "you", id: Identity{Self: true, Email: "me@example.com"}, want: OwnerID},
		{name: "you through another agent", id: Identity{Self: true, Email: "me@example.com", Hops: 1}, want: "email:me@example.com"},
		{name: "pairing before email", id: Identity{Pairing: "anna", Email: "anna@example.com"}, want: "pair:anna"},
		{name: "webhook node", id: Identity{WebhookNode: "grok"}, want: "node:grok"},
		{name: "email in lower case", id: Identity{Email: "Anna@Example.com", Node: "n1"}, want: "email:anna@example.com"},
		{name: "network node", id: Identity{Provider: network.ProviderTailscale, Node: "nT5kZ3"}, want: "node:tailscale:nT5kZ3"},
		{name: "nothing", id: Identity{Network: network.ListenerPublic}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tc.id.PrincipalID()
			if got != tc.want {
				t.Fatalf("Identity.PrincipalID() = %q, want %q", got, tc.want)
			}
			if got == "" || tc.id.Hops > 0 {
				return
			}
			if back := IdentityOf(got).PrincipalID(); back != got {
				t.Fatalf("IdentityOf(%q).PrincipalID() = %q, want %q", got, back, got)
			}
		})
	}
}

func TestPolicyOthers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		policy   Policy
		provider string
		public   bool
		chat     bool
		want     []string
	}{
		{name: "private network", policy: testPolicy(true, nil), provider: network.ProviderTailscale, want: []string{"everyone on the tailnet"}},
		{name: "no private network", policy: testPolicy(true, nil)},
		{name: "closed private network", policy: testPolicy(false, nil), provider: network.ProviderAgentMesh},
		{
			name: "emails", policy: testPolicy(true, []string{"*@example.com", "anna@example.org"}), provider: network.ProviderTailscale,
			want: []string{"everyone on the tailnet", "emails matching *@example.com", "emails matching anna@example.org"},
		},
		{name: "anyone on both routes", policy: Policy{Guests: []string{MemberAnyone}}, public: true, chat: true, want: []string{"anyone on the public URL or in a direct chat"}},
		{name: "anyone on the public URL", policy: Policy{Guests: []string{MemberAnyone}}, public: true, want: []string{"anyone on the public URL"}},
		{name: "anyone in a direct chat", policy: Policy{Guests: []string{MemberAnyone}}, chat: true, want: []string{"anyone in a direct chat"}},
		{name: "anyone without a route", policy: Policy{Guests: []string{MemberAnyone}}, want: []string{"anyone (no public URL or messenger is set up yet)"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(tc.want, tc.policy.Others(tc.provider, tc.public, tc.chat)); diff != "" {
				t.Fatalf("Policy.Others() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestPolicyAllowsSignIn(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		policy Policy
		email  string
		domain string
		want   bool
	}{
		{name: "listed email", policy: testPolicy(true, []string{"Anna@Example.com"}), email: "anna@example.com", want: true},
		{name: "workspace domain", policy: testPolicy(false, []string{"*@example.com"}), email: "anna@example.com", domain: "example.com", want: true},
		{name: "personal account in the domain", policy: testPolicy(false, []string{"*@example.com"}), email: "anna@example.com"},
		{name: "removed email", policy: testPolicy(true, []string{"bob@example.com"}), email: "anna@example.com"},
		{name: "anyone on the public URL", policy: Policy{Guests: []string{MemberAnyone}}, email: "anna@example.com"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.policy.AllowsSignIn(tc.email, tc.domain); got != tc.want {
				t.Fatalf("Policy.AllowsSignIn(%q, %q) = %v, want %v", tc.email, tc.domain, got, tc.want)
			}
		})
	}
}
