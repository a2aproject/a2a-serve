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

package pairings

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/pairapi"
	"github.com/a2aproject/a2a-serve/internal/store"
)

type fakeAds map[string]string

func (a fakeAds) Advertise(name, description string) error { a[name] = description; return nil }
func (a fakeAds) Withdraw(name string) error               { delete(a, name); return nil }
func (a fakeAds) Advertised(name string) bool {
	_, ok := a[name]
	return ok
}

func newService(t *testing.T) *Service {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	return &Service{Store: st, BaseURL: func() string { return "https://coder.example.com" }, Ads: fakeAds{}}
}

// redeem exchanges code for a token, as POST /pair does.
func redeem(t *testing.T, s *Service, code string) {
	t.Helper()
	c, err := s.Store.TakeCode(context.Background(), auth.NormalizeCode(code), auth.PurposePair)
	if err != nil {
		t.Fatalf("TakeCode() error = %v", err)
	}
	if _, _, err := auth.IssueToken(context.Background(), s.Store, c, s.BaseURL()); err != nil {
		t.Fatalf("IssueToken() error = %v", err)
	}
}

func invite(t *testing.T, s *Service, req controlapi.PairCodeRequest) *controlapi.PairCodeResponse {
	t.Helper()
	resp, err := s.Invite(context.Background(), req)
	if err != nil {
		t.Fatalf("Invite(%+v) error = %v", req, err)
	}
	return resp
}

func kindOf(err error) Kind {
	var pe *Error
	if errors.As(err, &pe) {
		return pe.Kind
	}
	return 0
}

func TestInvite(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		req      controlapi.PairCodeRequest
		wantKind Kind
	}{
		{name: "new caller", req: controlapi.PairCodeRequest{Name: "bob"}},
		{name: "another device of a caller", req: controlapi.PairCodeRequest{Name: "anna"}},
		{name: "same role", req: controlapi.PairCodeRequest{Name: "anna", Role: auth.RoleAuthenticated}},
		{name: "another role", req: controlapi.PairCodeRequest{Name: "anna", Role: auth.RoleOwner}, wantKind: Invalid},
		{name: "node code", req: controlapi.PairCodeRequest{Name: "grok"}},
		{name: "webhook of an existing node", req: controlapi.PairCodeRequest{Name: "grok", Webhook: "https://example.com/new"}, wantKind: Invalid},
		{name: "webhook for a caller's name", req: controlapi.PairCodeRequest{Name: "anna", Webhook: "https://example.com/hook"}, wantKind: Conflict},
		{name: "role of a node", req: controlapi.PairCodeRequest{Name: "grok", Role: auth.RoleOwner}, wantKind: Invalid},
		{name: "description without a webhook", req: controlapi.PairCodeRequest{Name: "bob", Description: "x"}, wantKind: Invalid},
		{name: "reserved name", req: controlapi.PairCodeRequest{Name: LocalOwner}, wantKind: Invalid},
		{name: "invalid name", req: controlapi.PairCodeRequest{Name: "a b"}, wantKind: Invalid},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newService(t)
			redeem(t, s, invite(t, s, controlapi.PairCodeRequest{Name: "anna"}).Code)
			invite(t, s, controlapi.PairCodeRequest{Name: "grok", Webhook: "https://example.com/hook"})

			_, err := s.Invite(context.Background(), tc.req)

			if got := kindOf(err); got != tc.wantKind || (tc.wantKind == 0 && err != nil) {
				t.Fatalf("Invite(%+v) error = %v, want kind %v", tc.req, err, tc.wantKind)
			}
			if tc.wantKind != 0 {
				return
			}
			anna, err := s.Get(context.Background(), "anna")
			if err != nil {
				t.Fatalf("Get(anna) error = %v", err)
			}
			if len(anna.Tokens) != 1 {
				t.Fatalf("Get(anna).Tokens = %v, want the token issued before the invitation kept", anna.Tokens)
			}
		})
	}
}

func TestGetAndList(t *testing.T) {
	t.Parallel()
	s := newService(t)
	redeem(t, s, invite(t, s, controlapi.PairCodeRequest{Name: "anna", Role: auth.RoleOwner}).Code)
	invite(t, s, controlapi.PairCodeRequest{Name: "anna"})
	on := true
	invite(t, s, controlapi.PairCodeRequest{Name: "grok", Webhook: "https://example.com/hook?key=secret", Headers: map[string]string{"Authorization": "Bearer x"}, Description: "searches", Advertised: &on})

	all, err := s.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	type summary struct {
		Name, Kind, Role, Principal string
		Tokens, Codes               int
		Node                        *controlapi.NodeDetails
	}
	var got []summary
	for _, p := range all {
		got = append(got, summary{p.Name, p.Kind, p.Role, p.Principal, len(p.Tokens), len(p.PendingCodes), p.Node})
	}
	want := []summary{
		{Name: "anna", Kind: pairapi.KindCaller, Role: auth.RoleOwner, Principal: "pair:anna", Tokens: 1, Codes: 1},
		{Name: "grok", Kind: pairapi.KindNode, Principal: "node:grok", Codes: 1, Node: &controlapi.NodeDetails{
			URL: "https://coder.example.com/n/grok", Webhook: "https://example.com/hook?…", Description: "searches",
			HeaderNames: []string{"Authorization"}, Advertised: true,
		}},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("List() wrong result (-want +got) diff = %s", diff)
	}
	invite(t, s, controlapi.PairCodeRequest{Name: "bob"})
	pending, err := s.Get(context.Background(), "bob")
	if err != nil || pending.Created.IsZero() || len(pending.Tokens) != 0 {
		t.Fatalf("Get(grok) = %+v, %v, want the creation time of a pairing whose code isn't used yet", pending, err)
	}
	if _, err := s.Get(context.Background(), "nobody"); kindOf(err) != NotFound {
		t.Fatalf("Get(nobody) error = %v, want not found", err)
	}
}

func TestUpdateCaller(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newService(t)
	redeem(t, s, invite(t, s, controlapi.PairCodeRequest{Name: "anna"}).Code)

	resp, err := s.Update(ctx, "anna", controlapi.PairingUpdate{Role: auth.RoleOwner})
	if err != nil {
		t.Fatalf("Update(role owner) error = %v", err)
	}
	if resp.Pairing.Role != auth.RoleOwner || resp.Pairing.Principal != "pair:anna" || len(resp.Pairing.Tokens) != 1 || resp.Code != nil {
		t.Fatalf("Update(role owner) = %+v, want anna as an owner with her token", resp)
	}
	toks, err := s.Store.Tokens(ctx)
	if err != nil || len(toks) != 1 || toks[0].Principal != "pair:anna" || toks[0].Role != auth.RoleOwner {
		t.Fatalf("Tokens() after Update(role owner) = %+v, %v, want anna's token as an owner", toks, err)
	}
	u, err := s.Store.User(ctx, "pair:anna")
	if err != nil || u.Role != auth.RoleOwner {
		t.Fatalf("User(pair:anna) after Update(role owner) = %+v, %v, want an owner", u, err)
	}

	resp, err = s.Update(ctx, "anna", controlapi.PairingUpdate{Rotate: true})
	if err != nil {
		t.Fatalf("Update(rotate) error = %v", err)
	}
	if resp.Code == nil || len(resp.Pairing.Tokens) != 0 || len(resp.Pairing.PendingCodes) != 1 {
		t.Fatalf("Update(rotate) = %+v, want no tokens and one new code", resp)
	}

	for _, req := range []controlapi.PairingUpdate{{Webhook: "https://example.com/hook"}, {Role: "admin"}} {
		if _, err := s.Update(ctx, "anna", req); kindOf(err) != Invalid {
			t.Fatalf("Update(%+v) error = %v, want invalid", req, err)
		}
	}
	if _, err := s.Update(ctx, "nobody", controlapi.PairingUpdate{Rotate: true}); kindOf(err) != NotFound {
		t.Fatalf("Update(nobody) error = %v, want not found", err)
	}
}

func TestUpdateNode(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newService(t)
	invite(t, s, controlapi.PairCodeRequest{Name: "grok", Webhook: "https://example.com/hook"})
	node, err := s.Store.Node(ctx, "grok")
	if err != nil {
		t.Fatalf("Node() error = %v", err)
	}
	node.TokenHash = "paired"
	if err := s.Store.PutNode(ctx, node); err != nil {
		t.Fatalf("PutNode() error = %v", err)
	}

	desc, on := "searches", true
	resp, err := s.Update(ctx, "grok", controlapi.PairingUpdate{Webhook: "https://example.com/new", Description: &desc, Advertised: &on})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if n := resp.Pairing.Node; n.Webhook != "https://example.com/new" || n.Description != desc || !n.Advertised || !n.Paired || resp.Code != nil {
		t.Fatalf("Update() = %+v, want the node changed in place and still paired", resp)
	}

	resp, err = s.Update(ctx, "grok", controlapi.PairingUpdate{Rotate: true})
	if err != nil {
		t.Fatalf("Update(rotate) error = %v", err)
	}
	if resp.Pairing.Node.Paired || resp.Code == nil || resp.Code.Node == nil {
		t.Fatalf("Update(rotate) = %+v, want the token revoked and a node code", resp)
	}
	if _, err := s.Update(ctx, "grok", controlapi.PairingUpdate{Role: auth.RoleOwner}); kindOf(err) != Invalid {
		t.Fatalf("Update(role) of a node error = %v, want invalid", err)
	}
}

func TestRemove(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newService(t)
	invite(t, s, controlapi.PairCodeRequest{Name: "anna"})
	on := true
	invite(t, s, controlapi.PairCodeRequest{Name: "grok", Webhook: "https://example.com/hook", Advertised: &on})

	for name, want := range map[string]string{"anna": pairapi.KindCaller, "grok": pairapi.KindNode} {
		got, err := s.Remove(ctx, name)
		if err != nil || got != want {
			t.Fatalf("Remove(%q) = %q, %v, want %q", name, got, err, want)
		}
	}
	if s.Ads.Advertised("grok") {
		t.Fatalf("Remove(grok) kept its advertisement")
	}
	if all, err := s.List(ctx); err != nil || len(all) != 0 {
		t.Fatalf("List() after Remove() = %v, %v, want none", all, err)
	}
	if _, err := s.Remove(ctx, "anna"); kindOf(err) != NotFound {
		t.Fatalf("Remove(anna) again error = %v, want not found", err)
	}
}
