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

func pair(t *testing.T, s *store.Store, name, role string) {
	t.Helper()
	if _, err := s.EnsurePairing(context.Background(), store.Pairing{Name: name, Role: role}); err != nil {
		t.Fatalf("EnsurePairing(%s) error = %v", name, err)
	}
}

func issueToken(t *testing.T, s *store.Store, name, kind string) string {
	t.Helper()
	tok, _, err := IssueToken(context.Background(), s, name, "https://agent", kind)
	if err != nil {
		t.Fatalf("IssueToken(%s) error = %v", name, err)
	}
	return tok
}

func TestChainAuthenticate(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	pair(t, s, "grok", RoleAuthenticated)
	code, _, err := MintCode(ctx, s, "grok")
	if err != nil {
		t.Fatalf("MintCode() error = %v", err)
	}
	c, err := s.TakeCode(ctx, NormalizeCode(strings.ToLower(code)), store.AnyCode)
	if err != nil {
		t.Fatalf("TakeCode() error = %v", err)
	}
	token := issueToken(t, s, c.Pairing, "agent")
	pair(t, s, "phone", RoleOwner)
	ownerToken := issueToken(t, s, "phone", "device")
	localToken := issueToken(t, s, LocalPairing, "")
	nodeToken := mailboxapi.NewNodeToken("https://agent/mailbox", "grok-node", "secret")
	if err := s.PutNode(ctx, store.Node{ID: "grok-node", Name: "Grok", TokenHash: cryptoutil.Hash(nodeToken)}); err != nil {
		t.Fatalf("PutNode() error = %v", err)
	}
	googleToken := putGoogleToken(t, s, "anna@example.com", "example.com")

	net := fakeNet{name: network.ProviderTailscale, peers: map[string]*network.Peer{
		"100.1.1.1:5000": {Email: "me@example.com", Node: "nMe", Self: true},
		"100.1.1.2:5000": {Email: "anna@example.com", Node: "nAnna"},
		"100.1.1.3:5000": {Node: "nTagged"},
		"100.1.1.4:5000": {Email: "me@example.com", Node: "nHost", Self: true, ThisMachine: true},
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
			policy: testPolicy(true, nil),
			req:    Request{Listener: network.ListenerPrivate, RemoteAddr: "100.1.1.1:5000", Header: http.Header{}},
			want: Principal{ID: OwnerID, Role: RoleOwner, Identity: Identity{
				Network: network.ListenerPrivate, Provider: network.ProviderTailscale, Email: "me@example.com", EmailSource: EmailFromNetwork, Node: "nMe", Self: true,
			}},
		},
		{
			name:   "tailnet self through another agent",
			policy: testPolicy(true, nil),
			req:    Request{Listener: network.ListenerPrivate, RemoteAddr: "100.1.1.1:5000", Header: header(HeaderHops, "1", "X-A2A-Serve-On-Behalf-Of", OwnerID)},
			want: Principal{ID: "email:me@example.com", Role: RoleAuthenticated, Identity: Identity{
				Network: network.ListenerPrivate, Provider: network.ProviderTailscale, Email: "me@example.com", EmailSource: EmailFromNetwork, Node: "nMe", Self: true, Hops: 1,
			}},
		},
		{
			name:   "tailnet self from the machine running the agent",
			policy: testPolicy(true, nil),
			req:    Request{Listener: network.ListenerPrivate, RemoteAddr: "100.1.1.4:5000", Header: http.Header{}},
			want: Principal{ID: "email:me@example.com", Role: RoleAuthenticated, Identity: Identity{
				Network: network.ListenerPrivate, Provider: network.ProviderTailscale, Email: "me@example.com", EmailSource: EmailFromNetwork, Node: "nHost",
			}},
		},
		{
			name:        "tailnet self from the machine running the agent when the private network is closed",
			policy:      testPolicy(false, nil),
			req:         Request{Listener: network.ListenerPrivate, RemoteAddr: "100.1.1.4:5000", Header: http.Header{}},
			wantRefused: "me@example.com on the tailnet",
		},
		{
			name:   "everyone on the tailnet",
			policy: testPolicy(true, nil),
			req:    Request{Listener: network.ListenerPrivate, RemoteAddr: "100.1.1.2:5000", Header: http.Header{}},
			want: Principal{ID: "email:anna@example.com", Role: RoleAuthenticated, Identity: Identity{
				Network: network.ListenerPrivate, Provider: network.ProviderTailscale, Email: "anna@example.com", EmailSource: EmailFromNetwork, Node: "nAnna",
			}},
		},
		{
			name:   "tagged tailnet node",
			policy: testPolicy(true, nil),
			req:    Request{Listener: network.ListenerPrivate, RemoteAddr: "100.1.1.3:5000", Header: http.Header{}},
			want: Principal{ID: "node:tailscale:nTagged", Role: RoleAuthenticated, Identity: Identity{
				Network: network.ListenerPrivate, Provider: network.ProviderTailscale, Node: "nTagged",
			}},
		},
		{
			name:        "tailnet user without a pairing when the private network is closed",
			policy:      testPolicy(false, nil),
			req:         Request{Listener: network.ListenerPrivate, RemoteAddr: "100.1.1.2:5000", Header: http.Header{}},
			wantRefused: "anna@example.com on the tailnet",
		},
		{
			name:   "allowed tailnet email when the private network is closed",
			policy: testPolicy(false, []string{"anna@example.com"}),
			req:    Request{Listener: network.ListenerPrivate, RemoteAddr: "100.1.1.2:5000", Header: http.Header{}},
			want: Principal{ID: "email:anna@example.com", Role: RoleAuthenticated, Identity: Identity{
				Network: network.ListenerPrivate, Provider: network.ProviderTailscale, Email: "anna@example.com", EmailSource: EmailFromNetwork, Node: "nAnna",
			}},
		},
		{
			name:        "domain pattern doesn't match a tailnet email",
			policy:      testPolicy(false, []string{"*@example.com"}),
			req:         Request{Listener: network.ListenerPrivate, RemoteAddr: "100.1.1.2:5000", Header: http.Header{}},
			wantRefused: "anna@example.com on the tailnet",
		},
		{
			name:    "network identity ignored on public listener",
			policy:  testPolicy(true, nil),
			req:     Request{Listener: network.ListenerPublic, RemoteAddr: "100.1.1.1:5000", Header: http.Header{}},
			wantErr: ErrNoCredential,
		},
		{
			name:   "anyone on the public listener",
			policy: Policy{Anonymous: []string{MemberAnyone}},
			req:    Request{Listener: network.ListenerPublic, Header: http.Header{}},
			want:   Principal{ID: AnonymousID, Role: RoleAnonymous, Identity: Identity{Network: network.ListenerPublic}},
		},
		{
			name:    "anyone on the local listener",
			policy:  Policy{Anonymous: []string{MemberAnyone}},
			req:     Request{Listener: network.ListenerLocal, Header: http.Header{}},
			wantErr: ErrNoCredential,
		},
		{
			name:   "pairing token",
			policy: testPolicy(true, nil),
			req:    Request{Listener: network.ListenerPublic, Header: header("Authorization", "Bearer "+token)},
			want: Principal{ID: "pair:grok", Role: RoleAuthenticated, Identity: Identity{
				Network: network.ListenerPublic, Pairing: "grok", PairingRole: RoleAuthenticated,
			}},
		},
		{
			name:   "owner pairing token keeps its own principal",
			policy: testPolicy(true, nil),
			req:    Request{Listener: network.ListenerPublic, Header: header("Authorization", "Bearer "+ownerToken)},
			want: Principal{ID: "pair:phone", Role: RoleOwner, Identity: Identity{
				Network: network.ListenerPublic, Pairing: "phone", PairingRole: RoleOwner,
			}},
		},
		{
			name:   "owner pairing token through another agent",
			policy: testPolicy(true, nil),
			req:    Request{Listener: network.ListenerPublic, Header: header("Authorization", "Bearer "+ownerToken, HeaderHops, "2")},
			want: Principal{ID: "pair:phone", Role: RoleAuthenticated, Identity: Identity{
				Network: network.ListenerPublic, Pairing: "phone", PairingRole: RoleOwner, Hops: 2,
			}},
		},
		{
			name:   "pairing token on the tailnet takes the higher role",
			policy: testPolicy(true, nil),
			req:    Request{Listener: network.ListenerPrivate, RemoteAddr: "100.1.1.2:5000", Header: header("Authorization", "Bearer "+ownerToken)},
			want: Principal{ID: "pair:phone", Role: RoleOwner, Identity: Identity{
				Network: network.ListenerPrivate, Provider: network.ProviderTailscale, Email: "anna@example.com", EmailSource: EmailFromNetwork, Node: "nAnna",
				Pairing: "phone", PairingRole: RoleOwner,
			}},
		},
		{
			name:   "negative hops ignored",
			policy: testPolicy(true, nil),
			req:    Request{Listener: network.ListenerPublic, Header: header("Authorization", "Bearer "+ownerToken, HeaderHops, "-1000")},
			want: Principal{ID: "pair:phone", Role: RoleOwner, Identity: Identity{
				Network: network.ListenerPublic, Pairing: "phone", PairingRole: RoleOwner,
			}},
		},
		{
			name:   "local token on the local listener",
			policy: testPolicy(true, nil),
			req:    Request{Listener: network.ListenerLocal, Header: header("Authorization", "Bearer "+localToken)},
			want:   Principal{ID: OwnerID, Role: RoleOwner, Identity: Identity{Network: network.ListenerLocal, Self: true}},
		},
		{
			name:    "local token on the public listener",
			policy:  testPolicy(true, nil),
			req:     Request{Listener: network.ListenerPublic, Header: header("Authorization", "Bearer "+localToken)},
			wantErr: ErrNoCredential,
		},
		{
			name:        "local token on the private listener",
			policy:      testPolicy(false, nil),
			req:         Request{Listener: network.ListenerPrivate, RemoteAddr: "100.1.1.3:5000", Header: header("Authorization", "Bearer "+localToken)},
			wantRefused: "the machine nTagged on the tailnet",
		},
		{
			name:    "unknown token",
			policy:  testPolicy(true, nil),
			req:     Request{Listener: network.ListenerLocal, Header: http.Header{"Authorization": {"Bearer a2p_nope"}}},
			wantErr: ErrNoCredential,
		},
		{
			name:   "allowed Google sign-in",
			policy: testPolicy(true, []string{"*@example.com"}),
			req:    Request{Listener: network.ListenerPublic, Header: header("Authorization", "Bearer "+googleToken)},
			want: Principal{ID: "email:anna@example.com", Role: RoleAuthenticated, Identity: Identity{
				Network: network.ListenerPublic, Email: "anna@example.com", EmailSource: EmailFromGoogle, Domain: "example.com",
			}},
		},
		{
			name:        "Google sign-in no longer allowed",
			policy:      testPolicy(true, []string{"bob@example.com"}),
			req:         Request{Listener: network.ListenerPublic, Header: header("Authorization", "Bearer "+googleToken)},
			wantRefused: "anna@example.com with Google sign-in",
		},
		{
			name:   "node token",
			policy: testPolicy(true, nil),
			req:    Request{Listener: network.ListenerPublic, Header: header("Authorization", "Bearer "+nodeToken, HeaderHops, "1")},
			want: Principal{ID: "node:grok-node", Role: RoleAuthenticated, Identity: Identity{
				Network: network.ListenerPublic, WebhookNode: "grok-node", Hops: 1,
			}},
		},
		{
			name:    "node token with a wrong secret",
			policy:  testPolicy(true, nil),
			req:     Request{Listener: network.ListenerPublic, Header: header("Authorization", "Bearer "+mailboxapi.NewNodeToken("https://agent/mailbox", "grok-node", "stale"))},
			wantErr: ErrNoCredential,
		},
		{
			name:    "token of an unknown node",
			policy:  testPolicy(true, nil),
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
	if err := s.PutToken(context.Background(), tok, store.Token{Email: email, Domain: domain}, ""); err != nil {
		t.Fatalf("PutToken() error = %v", err)
	}
	return tok
}

func TestChainAuthenticateChannel(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	link := func(identity, name, role string) {
		pair(t, s, name, role)
		code, _, err := MintCode(ctx, s, name)
		if err != nil {
			t.Fatalf("MintCode() error = %v", err)
		}
		if _, err := Link(ctx, s, &channel.Inbound{Channel: "telegram", Identity: identity, Authentic: true}, code); err != nil {
			t.Fatalf("Link() error = %v", err)
		}
	}
	link("telegram:1", LocalPairing, RoleOwner)
	link("telegram:2", "anna", RoleAuthenticated)
	link("telegram:3", "phone", RoleOwner)

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
			policy: testPolicy(true, nil),
			in:     channel.Inbound{Channel: "telegram", Identity: "telegram:1", Direct: true, Authentic: true},
			want: Principal{ID: OwnerID, Role: RoleOwner, Identity: Identity{
				Network: NetworkChat, Provider: "telegram", ChatAccount: "telegram:1", Direct: true, Self: true,
			}},
		},
		{
			name:   "linked pairing",
			policy: testPolicy(true, nil),
			in:     channel.Inbound{Channel: "telegram", Identity: "telegram:2", Authentic: true},
			want: Principal{ID: "pair:anna", Role: RoleAuthenticated, Identity: Identity{
				Network: NetworkChat, Provider: "telegram", ChatAccount: "telegram:2", Pairing: "anna", PairingRole: RoleAuthenticated,
			}},
		},
		{
			name:   "linked owner pairing",
			policy: testPolicy(true, nil),
			in:     channel.Inbound{Channel: "telegram", Identity: "telegram:3", Authentic: true},
			want: Principal{ID: "pair:phone", Role: RoleOwner, Identity: Identity{
				Network: NetworkChat, Provider: "telegram", ChatAccount: "telegram:3", Pairing: "phone", PairingRole: RoleOwner,
			}},
		},
		{
			name:        "unlinked account",
			policy:      testPolicy(true, nil),
			in:          channel.Inbound{Channel: "telegram", Identity: "telegram:4", Direct: true, Authentic: true},
			wantRefused: "telegram:4 on telegram",
		},
		{
			name:   "allowed email sender that passed DMARC",
			policy: testPolicy(true, []string{"*@example.com"}),
			in:     channel.Inbound{Channel: "email", Identity: "anna@example.com", Direct: true, Authentic: true},
			want: Principal{ID: "email:anna@example.com", Role: RoleAuthenticated, Identity: Identity{
				Network: NetworkChat, Provider: "email", ChatAccount: "anna@example.com", Direct: true,
				Email: "anna@example.com", EmailSource: EmailFromMail, Domain: "example.com",
			}},
		},
		{
			name:        "email sender not allowed",
			policy:      testPolicy(true, []string{"bob@example.com"}),
			in:          channel.Inbound{Channel: "email", Identity: "anna@example.com", Direct: true, Authentic: true},
			wantRefused: "anna@example.com by email",
		},
		{
			name:    "unverified sender",
			policy:  testPolicy(true, []string{"anna@example.com"}),
			in:      channel.Inbound{Channel: "email", Identity: "anna@example.com", Direct: true},
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
	pair(t, s, "phone", RoleAuthenticated)
	issueToken(t, s, "phone", "device")
	if err := s.UpdatePairing(ctx, store.Pairing{Name: "phone", Kind: "device", Role: RoleOwner}); err != nil {
		t.Fatalf("UpdatePairing() error = %v", err)
	}
	if err := s.PutNode(ctx, store.Node{ID: "grok", Name: "grok", Webhook: "https://example.com/hook", Secret: "s"}); err != nil {
		t.Fatalf("PutNode() error = %v", err)
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
			policy: testPolicy(true, nil),
			id:     OwnerID,
			want:   Principal{ID: OwnerID, Role: RoleOwner, Identity: Identity{Self: true}},
		},
		{
			name:   "pairing with its current role",
			policy: testPolicy(true, nil),
			id:     "pair:phone",
			want:   Principal{ID: "pair:phone", Role: RoleOwner, Identity: Identity{Pairing: "phone", PairingRole: RoleOwner}},
		},
		{
			name:    "removed pairing",
			policy:  testPolicy(true, nil),
			id:      "pair:gone",
			wantErr: ErrNoCredential,
		},
		{
			name:   "webhook node",
			policy: testPolicy(true, nil),
			id:     "node:grok",
			want:   Principal{ID: "node:grok", Role: RoleAuthenticated, Identity: Identity{WebhookNode: "grok"}},
		},
		{
			name:    "removed webhook node",
			policy:  testPolicy(true, nil),
			id:      "node:gone",
			wantErr: ErrNoCredential,
		},
		{
			name:     "tailnet user while the private network is open",
			policy:   testPolicy(true, nil),
			id:       "email:anna@example.com",
			identity: tailnet,
			want:     Principal{ID: "email:anna@example.com", Role: RoleAuthenticated, Identity: tailnet},
		},
		{
			name:        "tailnet user once the private network is closed",
			policy:      testPolicy(false, nil),
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
	chain := &Chain{Store: s, Policy: func() Policy { return testPolicy(true, nil) }}
	in := &channel.Inbound{Channel: "telegram", Identity: "telegram:42", Conv: "42", Direct: true, Authentic: true}

	if _, err := chain.Authenticate(ctx, Request{Channel: in}); err == nil {
		t.Fatalf("Authenticate() before link error = nil, want a refusal")
	}
	code, _, err := MintCode(ctx, s, LocalPairing)
	if err != nil {
		t.Fatalf("MintCode() error = %v", err)
	}
	if _, err := Link(ctx, s, &channel.Inbound{Channel: "telegram", Identity: "telegram:43"}, code); err == nil {
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
	links, err := s.LinksOf(ctx, LocalPairing)
	if err != nil || len(links) != 1 || links[0].Conv != "42" {
		t.Fatalf("LinksOf() = %v, %v, want one link with conv 42", links, err)
	}
}

func TestRoleChangeAppliesToNextRequest(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	chain := &Chain{Store: s, Policy: func() Policy { return testPolicy(false, nil) }}
	in := &channel.Inbound{Channel: "telegram", Identity: "telegram:42", Conv: "42", Direct: true, Authentic: true}
	pair(t, s, "phone", RoleAuthenticated)
	code, _, err := MintCode(ctx, s, "phone")
	if err != nil {
		t.Fatalf("MintCode() error = %v", err)
	}
	if _, err := Link(ctx, s, in, code); err != nil {
		t.Fatalf("Link() error = %v", err)
	}
	tok := issueToken(t, s, "phone", "device")
	byToken := Request{Listener: network.ListenerPublic, Header: header("Authorization", "Bearer "+tok)}

	if err := s.UpdatePairing(ctx, store.Pairing{Name: "phone", Kind: "person", Role: RoleOwner}); err != nil {
		t.Fatalf("UpdatePairing() error = %v", err)
	}
	for _, r := range []Request{{Channel: in}, byToken} {
		got, err := chain.Authenticate(ctx, r)
		if err != nil || got.Role != RoleOwner {
			t.Fatalf("Authenticate() after the role change = %+v, %v, want owner", got, err)
		}
	}

	if err := s.DeletePairing(ctx, "phone"); err != nil {
		t.Fatalf("DeletePairing() error = %v", err)
	}
	var refused *RefusedError
	if _, err := chain.Authenticate(ctx, Request{Channel: in}); !errors.As(err, &refused) {
		t.Fatalf("Authenticate() of a chat link after DeletePairing() error = %v, want a refusal", err)
	}
	if _, err := chain.Authenticate(ctx, byToken); !errors.Is(err, ErrNoCredential) {
		t.Fatalf("Authenticate() of a token after DeletePairing() error = %v, want %v", err, ErrNoCredential)
	}
}

func TestLinkRefusesWebhookCodes(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	if err := s.PutNode(ctx, store.Node{ID: "grok", Name: "grok"}); err != nil {
		t.Fatalf("PutNode() error = %v", err)
	}
	code, _, err := MintCode(ctx, s, "grok")
	if err != nil {
		t.Fatalf("MintCode() error = %v", err)
	}
	in := &channel.Inbound{Channel: "telegram", Identity: "telegram:42", Authentic: true}
	if _, err := Link(ctx, s, in, code); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("Link() with a webhook code error = %v, want %v", err, ErrInvalidCode)
	}
	if _, err := s.TakeCode(ctx, NormalizeCode(code), store.AnyCode); err != nil {
		t.Fatalf("TakeCode() of the webhook code after Link() error = %v, want it still pending", err)
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
	first, _, err := MintSoleCode(ctx, s, LocalPairing)
	if err != nil {
		t.Fatalf("MintSoleCode() error = %v", err)
	}
	second, _, err := MintSoleCode(ctx, s, LocalPairing)
	if err != nil {
		t.Fatalf("MintSoleCode() error = %v", err)
	}

	if _, err := s.TakeCode(ctx, NormalizeCode(first), store.AnyCode); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("TakeCode(first) error = %v, want %v", err, store.ErrNotFound)
	}
	if _, err := s.TakeCode(ctx, NormalizeCode(second), store.AnyCode); err != nil {
		t.Fatalf("TakeCode(second) error = %v, want nil", err)
	}
}

func TestDisplayPrincipal(t *testing.T) {
	t.Parallel()
	for id, want := range map[string]string{"": "A caller", "pair:anna": "anna", "owner": "owner"} {
		if got := DisplayPrincipal(id); got != want {
			t.Fatalf("DisplayPrincipal(%q) = %q, want %q", id, got, want)
		}
	}
}
