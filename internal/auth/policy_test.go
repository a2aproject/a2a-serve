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

// testPolicy is the policy where the user running the agent is owner, and
// everyone on the private network, if private, and emails are authenticated.
func testPolicy(private bool, emails []string) Policy {
	p := Policy{Owner: []string{MemberSelf}}
	if private {
		p.Authenticated = append(p.Authenticated, MemberPrivate)
	}
	for _, e := range emails {
		p.Authenticated = append(p.Authenticated, MemberEmailPrefix+e)
	}
	return p
}

func TestPolicyResolve(t *testing.T) {
	t.Parallel()
	private := Identity{Network: network.ListenerPrivate, Provider: network.ProviderTailscale, Email: "anna@example.com", EmailSource: EmailFromNetwork}
	open := Policy{Owner: []string{MemberSelf}, Anonymous: []string{MemberAnyone}}
	tests := []struct {
		name   string
		policy Policy
		id     Identity
		want   string
	}{
		{name: "self", policy: testPolicy(true, nil), id: Identity{Network: network.ListenerPrivate, Self: true}, want: RoleOwner},
		{name: "self through another agent", policy: testPolicy(true, nil), id: Identity{Network: network.ListenerPrivate, Self: true, Hops: 1}, want: RoleAuthenticated},
		{name: "self never on the public URL", policy: testPolicy(false, nil), id: Identity{Network: network.ListenerPublic, Self: true}},
		{name: "private network", policy: testPolicy(true, nil), id: private, want: RoleAuthenticated},
		{name: "closed private network", policy: testPolicy(false, nil), id: private},
		{name: "allowed email", policy: testPolicy(false, []string{"Anna@Example.com"}), id: private, want: RoleAuthenticated},
		{
			name:   "owner email",
			policy: Policy{Owner: []string{"email:anna@example.com"}},
			id:     private,
			want:   RoleOwner,
		},
		{
			name:   "domain pattern with a proven domain",
			policy: testPolicy(false, []string{"*@example.com"}),
			id:     Identity{Network: network.ListenerPublic, Email: "anna@example.com", EmailSource: EmailFromGoogle, Domain: "example.com"},
			want:   RoleAuthenticated,
		},
		{
			name:   "domain pattern with another proven domain",
			policy: testPolicy(false, []string{"*@example.com"}),
			id:     Identity{Network: network.ListenerPublic, Email: "anna@example.com", EmailSource: EmailFromGoogle, Domain: "other.com"},
		},
		{name: "domain pattern without a proven domain", policy: testPolicy(false, []string{"*@example.com"}), id: private},
		{name: "full address from the relay", policy: testPolicy(false, []string{"anna@example.com"}), id: Identity{Network: network.ListenerPrivate, Provider: network.ProviderRelay, Email: "anna@example.com", EmailSource: EmailFromNetwork}, want: RoleAuthenticated},
		{name: "owner pairing", policy: testPolicy(false, nil), id: Identity{Pairing: "phone", PairingRole: RoleOwner}, want: RoleOwner},
		{name: "highest of pairing and bindings", policy: Policy{Owner: []string{MemberPrivate}}, id: Identity{Network: network.ListenerPrivate, Pairing: "anna", PairingRole: RoleAuthenticated}, want: RoleOwner},
		{name: "webhook node", policy: testPolicy(false, nil), id: Identity{Network: network.ListenerPublic, WebhookNode: "grok"}, want: RoleAuthenticated},
		{name: "anyone on the public URL", policy: open, id: Identity{Network: network.ListenerPublic}, want: RoleAnonymous},
		{name: "anyone in a direct chat", policy: open, id: Identity{Network: NetworkChat, ChatAccount: "telegram:1", Direct: true}, want: RoleAnonymous},
		{name: "identified caller that matches nothing", policy: open, id: private, want: RoleAnonymous},
		{name: "unidentified caller on the private network", policy: open, id: Identity{Network: network.ListenerPrivate}},
		{name: "anonymous without members", policy: testPolicy(false, nil), id: Identity{Network: network.ListenerPublic}},
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
		{name: "network node", id: Identity{Provider: network.ProviderLAN, Node: "192.168.1.5"}, want: "node:lan:192.168.1.5"},
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
		want     []string
	}{
		{name: "private network", policy: testPolicy(true, nil), provider: network.ProviderTailscale, want: []string{"everyone on the tailnet"}},
		{name: "no private network", policy: testPolicy(true, nil)},
		{name: "closed private network", policy: testPolicy(false, nil), provider: network.ProviderRelay},
		{
			name: "emails", policy: testPolicy(true, []string{"*@example.com", "anna@example.org"}), provider: network.ProviderLAN,
			want: []string{"everyone on the LAN", "emails matching *@example.com", "emails matching anna@example.org"},
		},
		{name: "anyone", policy: Policy{Owner: []string{MemberSelf}, Anonymous: []string{MemberAnyone}}, want: []string{"anyone on the public URL or in a direct chat"}},
		{
			name: "owners", policy: Policy{Owner: []string{MemberSelf, "email:me@example.com", MemberPrivate}, Authenticated: []string{MemberPrivate}}, provider: network.ProviderTailscale,
			want: []string{"emails matching me@example.com as owners", "everyone on the tailnet as owners"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(tc.want, tc.policy.Others(tc.provider)); diff != "" {
				t.Fatalf("Policy.Others() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}
