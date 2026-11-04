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
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/channel"
	"github.com/a2aproject/a2a-serve/internal/cryptoutil"
	"github.com/a2aproject/a2a-serve/internal/mailboxapi"
	"github.com/a2aproject/a2a-serve/internal/network"
	"github.com/a2aproject/a2a-serve/internal/store"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	return s
}

type fakeNet struct {
	network.Provider
	name  string
	peers map[string]*network.Peer
}

func (f fakeNet) Name() string { return f.name }

func (f fakeNet) WhoIs(_ context.Context, addr string) (*network.Peer, error) {
	if p, ok := f.peers[addr]; ok {
		return p, nil
	}
	return nil, network.ErrNoIdentity
}

func header(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}

func TestChainAuthenticate(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	code, _, err := MintCode(ctx, s, PurposePair, "grok", RoleAuthenticated)
	if err != nil {
		t.Fatalf("MintCode() error = %v", err)
	}
	c, err := s.TakeCode(ctx, NormalizeCode(strings.ToLower(code)), PurposePair)
	if err != nil {
		t.Fatalf("TakeCode() error = %v", err)
	}
	token, _, err := IssueToken(ctx, s, c, "https://agent")
	if err != nil {
		t.Fatalf("IssueToken() error = %v", err)
	}
	ownerToken, _, err := IssueToken(ctx, s, store.Code{Name: "phone", Role: RoleOwner}, "https://agent")
	if err != nil {
		t.Fatalf("IssueToken() error = %v", err)
	}
	localToken, _, err := IssueToken(ctx, s, store.Code{Name: LocalPairing, Role: RoleOwner}, "http://127.0.0.1:4000")
	if err != nil {
		t.Fatalf("IssueToken() error = %v", err)
	}
	nodeToken := mailboxapi.NewNodeToken("https://agent/mailbox", "grok-node", "secret")
	if err := s.PutNode(ctx, store.Node{ID: "grok-node", Name: "Grok", TokenHash: cryptoutil.Hash(nodeToken)}); err != nil {
		t.Fatalf("PutNode() error = %v", err)
	}
	googleToken := putGoogleToken(t, s, "anna@example.com", "example.com")

	net := fakeNet{name: network.ProviderTailscale, peers: map[string]*network.Peer{
		"100.1.1.1:5000": {Email: "me@example.com", Node: "nMe", Self: true},
		"100.1.1.2:5000": {Email: "anna@example.com", Node: "nAnna"},
		"100.1.1.3:5000": {Node: "nTagged"},
	}}
	tests := []struct {
		name        string
		policy      Policy
		req         Request
		want        Principal
		wantErr     error
		wantRefused string
	}{
		{
			name:   "tailnet self",
			policy: DefaultPolicy(true, nil),
			req:    Request{Listener: network.ListenerPrivate, RemoteAddr: "100.1.1.1:5000", Header: http.Header{}},
			want: Principal{ID: OwnerID, Role: RoleOwner, Identity: Identity{
				Network: network.ListenerPrivate, Provider: network.ProviderTailscale, Email: "me@example.com", EmailSource: EmailFromNetwork, Node: "nMe", Self: true,
			}},
		},
		{
			name:   "tailnet self through another agent",
			policy: DefaultPolicy(true, nil),
			req:    Request{Listener: network.ListenerPrivate, RemoteAddr: "100.1.1.1:5000", Header: header(HeaderHops, "1", "X-A2A-Serve-On-Behalf-Of", OwnerID)},
			want: Principal{ID: "email:me@example.com", Role: RoleAuthenticated, Identity: Identity{
				Network: network.ListenerPrivate, Provider: network.ProviderTailscale, Email: "me@example.com", EmailSource: EmailFromNetwork, Node: "nMe", Self: true, Hops: 1,
			}},
		},
		{
			name:   "everyone on the tailnet",
			policy: DefaultPolicy(true, nil),
			req:    Request{Listener: network.ListenerPrivate, RemoteAddr: "100.1.1.2:5000", Header: http.Header{}},
			want: Principal{ID: "email:anna@example.com", Role: RoleAuthenticated, Identity: Identity{
				Network: network.ListenerPrivate, Provider: network.ProviderTailscale, Email: "anna@example.com", EmailSource: EmailFromNetwork, Node: "nAnna",
			}},
		},
		{
			name:   "tagged tailnet node",
			policy: DefaultPolicy(true, nil),
			req:    Request{Listener: network.ListenerPrivate, RemoteAddr: "100.1.1.3:5000", Header: http.Header{}},
			want: Principal{ID: "node:tailscale:nTagged", Role: RoleAuthenticated, Identity: Identity{
				Network: network.ListenerPrivate, Provider: network.ProviderTailscale, Node: "nTagged",
			}},
		},
		{
			name:        "tailnet user without a pairing when the private network is closed",
			policy:      DefaultPolicy(false, nil),
			req:         Request{Listener: network.ListenerPrivate, RemoteAddr: "100.1.1.2:5000", Header: http.Header{}},
			wantRefused: "anna@example.com on the tailnet",
		},
		{
			name:   "allowed tailnet email when the private network is closed",
			policy: DefaultPolicy(false, []string{"anna@example.com"}),
			req:    Request{Listener: network.ListenerPrivate, RemoteAddr: "100.1.1.2:5000", Header: http.Header{}},
			want: Principal{ID: "email:anna@example.com", Role: RoleAuthenticated, Identity: Identity{
				Network: network.ListenerPrivate, Provider: network.ProviderTailscale, Email: "anna@example.com", EmailSource: EmailFromNetwork, Node: "nAnna",
			}},
		},
		{
			name:        "domain pattern doesn't match a tailnet email",
			policy:      DefaultPolicy(false, []string{"*@example.com"}),
			req:         Request{Listener: network.ListenerPrivate, RemoteAddr: "100.1.1.2:5000", Header: http.Header{}},
			wantRefused: "anna@example.com on the tailnet",
		},
		{
			name:    "network identity ignored on public listener",
			policy:  DefaultPolicy(true, nil),
			req:     Request{Listener: network.ListenerPublic, RemoteAddr: "100.1.1.1:5000", Header: http.Header{}},
			wantErr: ErrNoCredential,
		},
		{
			name:   "pairing token",
			policy: DefaultPolicy(true, nil),
			req:    Request{Listener: network.ListenerPublic, Header: header("Authorization", "Bearer "+token)},
			want: Principal{ID: "pair:grok", Role: RoleAuthenticated, Identity: Identity{
				Network: network.ListenerPublic, Pairing: "grok", PairingRole: RoleAuthenticated,
			}},
		},
		{
			name:   "owner pairing token keeps its own principal",
			policy: DefaultPolicy(true, nil),
			req:    Request{Listener: network.ListenerPublic, Header: header("Authorization", "Bearer "+ownerToken)},
			want: Principal{ID: "pair:phone", Role: RoleOwner, Identity: Identity{
				Network: network.ListenerPublic, Pairing: "phone", PairingRole: RoleOwner,
			}},
		},
		{
			name:   "owner pairing token through another agent",
			policy: DefaultPolicy(true, nil),
			req:    Request{Listener: network.ListenerPublic, Header: header("Authorization", "Bearer "+ownerToken, HeaderHops, "2")},
			want: Principal{ID: "pair:phone", Role: RoleAuthenticated, Identity: Identity{
				Network: network.ListenerPublic, Pairing: "phone", PairingRole: RoleOwner, Hops: 2,
			}},
		},
		{
			name:   "pairing token on the tailnet takes the higher role",
			policy: DefaultPolicy(true, nil),
			req:    Request{Listener: network.ListenerPrivate, RemoteAddr: "100.1.1.2:5000", Header: header("Authorization", "Bearer "+ownerToken)},
			want: Principal{ID: "pair:phone", Role: RoleOwner, Identity: Identity{
				Network: network.ListenerPrivate, Provider: network.ProviderTailscale, Email: "anna@example.com", EmailSource: EmailFromNetwork, Node: "nAnna",
				Pairing: "phone", PairingRole: RoleOwner,
			}},
		},
		{
			name:   "negative hops ignored",
			policy: DefaultPolicy(true, nil),
			req:    Request{Listener: network.ListenerPublic, Header: header("Authorization", "Bearer "+ownerToken, HeaderHops, "-1000")},
			want: Principal{ID: "pair:phone", Role: RoleOwner, Identity: Identity{
				Network: network.ListenerPublic, Pairing: "phone", PairingRole: RoleOwner,
			}},
		},
		{
			name:   "local token on the local listener",
			policy: DefaultPolicy(true, nil),
			req:    Request{Listener: network.ListenerLocal, Header: header("Authorization", "Bearer "+localToken)},
			want:   Principal{ID: OwnerID, Role: RoleOwner, Identity: Identity{Network: network.ListenerLocal, Self: true}},
		},
		{
			name:    "local token on the public listener",
			policy:  DefaultPolicy(true, nil),
			req:     Request{Listener: network.ListenerPublic, Header: header("Authorization", "Bearer "+localToken)},
			wantErr: ErrNoCredential,
		},
		{
			name:        "local token on the private listener",
			policy:      DefaultPolicy(false, nil),
			req:         Request{Listener: network.ListenerPrivate, RemoteAddr: "100.1.1.3:5000", Header: header("Authorization", "Bearer "+localToken)},
			wantRefused: "the machine nTagged on the tailnet",
		},
		{
			name:    "unknown token",
			policy:  DefaultPolicy(true, nil),
			req:     Request{Listener: network.ListenerLocal, Header: http.Header{"Authorization": {"Bearer a2p_nope"}}},
			wantErr: ErrNoCredential,
		},
		{
			name:   "allowed Google sign-in",
			policy: DefaultPolicy(true, []string{"*@example.com"}),
			req:    Request{Listener: network.ListenerPublic, Header: header("Authorization", "Bearer "+googleToken)},
			want: Principal{ID: "email:anna@example.com", Role: RoleAuthenticated, Identity: Identity{
				Network: network.ListenerPublic, Email: "anna@example.com", EmailSource: EmailFromGoogle, Domain: "example.com",
			}},
		},
		{
			name:        "Google sign-in no longer allowed",
			policy:      DefaultPolicy(true, []string{"bob@example.com"}),
			req:         Request{Listener: network.ListenerPublic, Header: header("Authorization", "Bearer "+googleToken)},
			wantRefused: "anna@example.com with Google sign-in",
		},
		{
			name:   "node token",
			policy: DefaultPolicy(true, nil),
			req:    Request{Listener: network.ListenerPublic, Header: header("Authorization", "Bearer "+nodeToken, HeaderHops, "1")},
			want: Principal{ID: "node:grok-node", Role: RoleAuthenticated, Identity: Identity{
				Network: network.ListenerPublic, WebhookNode: "grok-node", Hops: 1,
			}},
		},
		{
			name:    "node token with a wrong secret",
			policy:  DefaultPolicy(true, nil),
			req:     Request{Listener: network.ListenerPublic, Header: header("Authorization", "Bearer "+mailboxapi.NewNodeToken("https://agent/mailbox", "grok-node", "stale"))},
			wantErr: ErrNoCredential,
		},
		{
			name:    "token of an unknown node",
			policy:  DefaultPolicy(true, nil),
			req:     Request{Listener: network.ListenerPublic, Header: header("Authorization", "Bearer "+mailboxapi.NewNodeToken("https://agent/mailbox", "nobody", "secret"))},
			wantErr: ErrNoCredential,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			chain := &Chain{Store: s, Net: net, Policy: func() Policy { return tc.policy }}
			got, err := chain.Authenticate(ctx, tc.req)
			if tc.wantRefused != "" {
				var refused *RefusedError
				if !errors.As(err, &refused) {
					t.Fatalf("Authenticate() error = %v, want a refusal", err)
				}
				if d := refused.Identity.Describe(); d != tc.wantRefused {
					t.Fatalf("Authenticate() refused %q, want %q", d, tc.wantRefused)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Authenticate() error = %v, want %v", err, tc.wantErr)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("Authenticate() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func putGoogleToken(t *testing.T, s *store.Store, email, domain string) string {
	t.Helper()
	tok := NewToken()
	if err := s.PutToken(context.Background(), tok, store.Token{Name: GooglePrefix + email, Principal: EmailPrefix + email, Role: RoleAuthenticated, Domain: domain}); err != nil {
		t.Fatalf("PutToken() error = %v", err)
	}
	return tok
}

func TestChainAuthenticateChannel(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	link := func(identity, name, role string) {
		code, _, err := MintCode(ctx, s, PurposePair, name, role)
		if err != nil {
			t.Fatalf("MintCode() error = %v", err)
		}
		if _, err := Link(ctx, s, &channel.Inbound{Channel: "telegram", Identity: identity, Authentic: true}, code); err != nil {
			t.Fatalf("Link() error = %v", err)
		}
	}
	link("tg:1", OwnerLink, RoleOwner)
	link("tg:2", "anna", RoleAuthenticated)
	link("tg:3", "phone", RoleOwner)

	tests := []struct {
		name        string
		policy      Policy
		in          channel.Inbound
		want        Principal
		wantErr     error
		wantRefused string
	}{
		{
			name:   "owner link",
			policy: DefaultPolicy(true, nil),
			in:     channel.Inbound{Channel: "telegram", Identity: "tg:1", Direct: true, Authentic: true},
			want: Principal{ID: OwnerID, Role: RoleOwner, Identity: Identity{
				Network: NetworkChat, Provider: "telegram", ChatAccount: "tg:1", Direct: true, Self: true,
			}},
		},
		{
			name:   "linked pairing",
			policy: DefaultPolicy(true, nil),
			in:     channel.Inbound{Channel: "telegram", Identity: "tg:2", Authentic: true},
			want: Principal{ID: "pair:anna", Role: RoleAuthenticated, Identity: Identity{
				Network: NetworkChat, Provider: "telegram", ChatAccount: "tg:2", Pairing: "anna", PairingRole: RoleAuthenticated,
			}},
		},
		{
			name:   "linked owner pairing",
			policy: DefaultPolicy(true, nil),
			in:     channel.Inbound{Channel: "telegram", Identity: "tg:3", Authentic: true},
			want: Principal{ID: "pair:phone", Role: RoleOwner, Identity: Identity{
				Network: NetworkChat, Provider: "telegram", ChatAccount: "tg:3", Pairing: "phone", PairingRole: RoleOwner,
			}},
		},
		{
			name:        "unlinked account",
			policy:      DefaultPolicy(true, nil),
			in:          channel.Inbound{Channel: "telegram", Identity: "tg:4", Direct: true, Authentic: true},
			wantRefused: "tg:4 on telegram",
		},
		{
			name:   "allowed email sender that passed DMARC",
			policy: DefaultPolicy(true, []string{"*@example.com"}),
			in:     channel.Inbound{Channel: "email", Identity: "mail:anna@example.com", Direct: true, Authentic: true},
			want: Principal{ID: "email:anna@example.com", Role: RoleAuthenticated, Identity: Identity{
				Network: NetworkChat, Provider: "email", ChatAccount: "mail:anna@example.com", Direct: true,
				Email: "anna@example.com", EmailSource: EmailFromMail, Domain: "example.com",
			}},
		},
		{
			name:        "email sender not allowed",
			policy:      DefaultPolicy(true, []string{"bob@example.com"}),
			in:          channel.Inbound{Channel: "email", Identity: "mail:anna@example.com", Direct: true, Authentic: true},
			wantRefused: "anna@example.com by email",
		},
		{
			name:    "unverified sender",
			policy:  DefaultPolicy(true, []string{"anna@example.com"}),
			in:      channel.Inbound{Channel: "email", Identity: "mail:anna@example.com", Direct: true},
			wantErr: ErrNoCredential,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			chain := &Chain{Store: s, Policy: func() Policy { return tc.policy }}
			got, err := chain.Authenticate(ctx, Request{Channel: &tc.in})
			if tc.wantRefused != "" {
				var refused *RefusedError
				if !errors.As(err, &refused) {
					t.Fatalf("Authenticate() error = %v, want a refusal", err)
				}
				if d := refused.Identity.Describe(); d != tc.wantRefused {
					t.Fatalf("Authenticate() refused %q, want %q", d, tc.wantRefused)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Authenticate() error = %v, want %v", err, tc.wantErr)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("Authenticate() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestChainRefresh(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	if _, _, err := IssueToken(ctx, s, store.Code{Name: "phone", Role: RoleAuthenticated}, "https://agent"); err != nil {
		t.Fatalf("IssueToken() error = %v", err)
	}
	if err := s.SetPairingRole(ctx, "phone", RoleOwner, PairPrincipal("phone")); err != nil {
		t.Fatalf("SetPairingRole() error = %v", err)
	}
	tailnet := Identity{Network: network.ListenerPrivate, Provider: network.ProviderTailscale, Email: "anna@example.com", EmailSource: EmailFromNetwork}

	tests := []struct {
		name        string
		policy      Policy
		id          string
		identity    Identity
		want        Principal
		wantErr     error
		wantRefused bool
	}{
		{
			name:   "you",
			policy: DefaultPolicy(true, nil),
			id:     OwnerID,
			want:   Principal{ID: OwnerID, Role: RoleOwner, Identity: Identity{Self: true}},
		},
		{
			name:   "pairing with its current role",
			policy: DefaultPolicy(true, nil),
			id:     "pair:phone",
			want:   Principal{ID: "pair:phone", Role: RoleOwner, Identity: Identity{Pairing: "phone", PairingRole: RoleOwner}},
		},
		{
			name:    "removed pairing",
			policy:  DefaultPolicy(true, nil),
			id:      "pair:gone",
			wantErr: ErrNoCredential,
		},
		{
			name:     "tailnet user while the private network is open",
			policy:   DefaultPolicy(true, nil),
			id:       "email:anna@example.com",
			identity: tailnet,
			want:     Principal{ID: "email:anna@example.com", Role: RoleAuthenticated, Identity: tailnet},
		},
		{
			name:        "tailnet user once the private network is closed",
			policy:      DefaultPolicy(false, nil),
			id:          "email:anna@example.com",
			identity:    tailnet,
			wantRefused: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			chain := &Chain{Store: s, Policy: func() Policy { return tc.policy }}
			got, err := chain.Refresh(ctx, tc.id, tc.identity)
			if tc.wantRefused {
				var refused *RefusedError
				if !errors.As(err, &refused) {
					t.Fatalf("Refresh() error = %v, want a refusal", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Refresh() error = %v, want %v", err, tc.wantErr)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("Refresh() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestLink(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	chain := &Chain{Store: s, Policy: func() Policy { return DefaultPolicy(true, nil) }}
	in := &channel.Inbound{Channel: "telegram", Identity: "tg:42", Conv: "42", Direct: true, Authentic: true}

	if _, err := chain.Authenticate(ctx, Request{Channel: in}); err == nil {
		t.Fatalf("Authenticate() before link error = nil, want a refusal")
	}
	code, _, err := MintCode(ctx, s, PurposePair, OwnerLink, RoleOwner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Link(ctx, s, &channel.Inbound{Channel: "telegram", Identity: "tg:43"}, code); err == nil {
		t.Fatalf("Link() unauthentic sender error = nil, want error")
	}
	p, err := Link(ctx, s, in, code)
	if err != nil {
		t.Fatalf("Link() error = %v", err)
	}
	if p.ID != OwnerID {
		t.Fatalf("Link() = %v, want %v", p.ID, OwnerID)
	}
	got, err := chain.Authenticate(ctx, Request{Channel: in})
	if err != nil {
		t.Fatalf("Authenticate() after link error = %v", err)
	}
	if got.ID != OwnerID || !got.IsOwner() {
		t.Fatalf("Authenticate() after link = %v, want %v as owner", got, OwnerID)
	}
	links, err := s.LinksOf(ctx, OwnerID)
	if err != nil || len(links) != 1 || links[0].Conv != "42" {
		t.Fatalf("LinksOf() = %v, %v, want one link with conv 42", links, err)
	}
}

func TestRevokeRemovesLinksOfPairing(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		role string
	}{
		{name: "authenticated", role: RoleAuthenticated},
		{name: "owner", role: RoleOwner},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := openStore(t)
			ctx := context.Background()
			chain := &Chain{Store: s, Policy: func() Policy { return DefaultPolicy(true, nil) }}
			in := &channel.Inbound{Channel: "telegram", Identity: "tg:42", Conv: "42", Direct: true, Authentic: true}
			code, _, err := MintCode(ctx, s, PurposePair, "phone", tc.role)
			if err != nil {
				t.Fatalf("MintCode() error = %v", err)
			}
			if _, err := Link(ctx, s, in, code); err != nil {
				t.Fatalf("Link() error = %v", err)
			}

			n, err := s.RevokeTokens(ctx, "phone", PrincipalForCode("phone"))
			if err != nil {
				t.Fatalf("RevokeTokens() error = %v", err)
			}
			if n != 1 {
				t.Fatalf("RevokeTokens() = %d, want 1", n)
			}
			var refused *RefusedError
			if _, err := chain.Authenticate(ctx, Request{Channel: in}); !errors.As(err, &refused) {
				t.Fatalf("Authenticate() after revoke error = %v, want a refusal", err)
			}
		})
	}
}

func TestLinkCode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		text   string
		want   string
		wantOK bool
	}{
		{text: "link K7Q2-9XF4", want: "K7Q2-9XF4", wantOK: true},
		{text: "join K7Q2-9XF4", want: "K7Q2-9XF4", wantOK: true},
		{text: "/join k7q29xf4", want: "k7q29xf4", wantOK: true},
		{text: "/start K7Q29XF4", want: "K7Q29XF4", wantOK: true},
		{text: "/link@my_bot k7q2-9xf4", want: "k7q2-9xf4", wantOK: true},
		{text: "please link K7Q2-9XF4 now"},
		{text: "/start"},
	}
	for _, tc := range tests {
		got, ok := LinkCode(tc.text)
		if ok != tc.wantOK || got != tc.want {
			t.Errorf("LinkCode(%q) = %q, %v, want %q, %v", tc.text, got, ok, tc.want, tc.wantOK)
		}
	}
}

func TestMintSoleCodeRevokesPendingCode(t *testing.T) {
	t.Parallel()
	s, ctx := openStore(t), context.Background()
	first, _, err := MintSoleCode(ctx, s, PurposePair, "owner", RoleOwner)
	if err != nil {
		t.Fatalf("MintSoleCode() error = %v", err)
	}
	second, _, err := MintSoleCode(ctx, s, PurposePair, "owner", RoleOwner)
	if err != nil {
		t.Fatalf("MintSoleCode() error = %v", err)
	}

	if _, err := s.TakeCode(ctx, NormalizeCode(first), PurposePair); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("TakeCode(first) error = %v, want %v", err, store.ErrNotFound)
	}
	if _, err := s.TakeCode(ctx, NormalizeCode(second), PurposePair); err != nil {
		t.Fatalf("TakeCode(second) error = %v, want nil", err)
	}
}
