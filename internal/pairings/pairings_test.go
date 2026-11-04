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
	"fmt"
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

// redeem exchanges code for a token as a redeemer of kind, as POST /pair
// does.
func redeem(t *testing.T, s *Service, code, kind string) {
	t.Helper()
	c, err := s.Store.TakeCode(context.Background(), auth.NormalizeCode(code), store.AnyCode)
	if err != nil {
		t.Fatalf("TakeCode() error = %v", err)
	}
	if _, _, err := auth.IssueToken(context.Background(), s.Store, c.Pairing, s.BaseURL(), kind); err != nil {
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
		{name: "new owner", req: controlapi.PairCodeRequest{Name: "bob", Owner: true}},
		{name: "owner for an authenticated pairing", req: controlapi.PairCodeRequest{Name: "anna", Owner: true}, wantKind: Invalid},
		{name: "node code", req: controlapi.PairCodeRequest{Name: "grok"}},
		{name: "webhook of an existing node", req: controlapi.PairCodeRequest{Name: "grok", Webhook: "https://example.com/new"}, wantKind: Invalid},
		{name: "webhook for a person's name", req: controlapi.PairCodeRequest{Name: "anna", Webhook: "https://example.com/hook"}, wantKind: Conflict},
		{name: "owner of a node", req: controlapi.PairCodeRequest{Name: "grok", Owner: true}, wantKind: Invalid},
		{name: "description without a webhook", req: controlapi.PairCodeRequest{Name: "bob", Description: "x"}, wantKind: Invalid},
		{name: "local pairing", req: controlapi.PairCodeRequest{Name: LocalOwner}, wantKind: Invalid},
		{name: "owner", req: controlapi.PairCodeRequest{Name: auth.OwnerName}, wantKind: Invalid},
		{name: "invalid name", req: controlapi.PairCodeRequest{Name: "a b"}, wantKind: Invalid},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newService(t)
			redeem(t, s, invite(t, s, controlapi.PairCodeRequest{Name: "anna"}).Code, pairapi.PairingPerson)
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
	redeem(t, s, invite(t, s, controlapi.PairCodeRequest{Name: "anna", Owner: true}).Code, pairapi.PairingPerson)
	invite(t, s, controlapi.PairCodeRequest{Name: "anna"})
	redeem(t, s, invite(t, s, controlapi.PairCodeRequest{Name: "anna"}).Code, pairapi.PairingAgent)
	redeem(t, s, invite(t, s, controlapi.PairCodeRequest{Name: "reviewer"}).Code, pairapi.PairingAgent)
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
		{Name: "anna", Kind: pairapi.PairingPerson, Role: auth.RoleOwner, Principal: "pair:anna", Tokens: 2, Codes: 1},
		{Name: "grok", Kind: pairapi.PairingWebhook, Role: auth.RoleAuthenticated, Principal: "node:grok", Codes: 1, Node: &controlapi.NodeDetails{
			URL: "https://coder.example.com/n/grok", Webhook: "https://example.com/hook?…", Description: "searches",
			HeaderNames: []string{"Authorization"}, Advertised: true,
		}},
		{Name: LocalOwner, Kind: pairapi.PairingPerson, Role: auth.RoleOwner, Principal: auth.OwnerID},
		{Name: "reviewer", Kind: pairapi.PairingAgent, Role: auth.RoleAuthenticated, Principal: "pair:reviewer", Tokens: 1},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("List() wrong result (-want +got) diff = %s", diff)
	}
	invite(t, s, controlapi.PairCodeRequest{Name: "bob"})
	pending, err := s.Get(context.Background(), "bob")
	if err != nil || pending.Created.IsZero() || pending.Kind != "" || len(pending.Tokens) != 0 {
		t.Fatalf("Get(bob) = %+v, %v, want a pairing without a kind until its code is used", pending, err)
	}
	if _, err := s.Get(context.Background(), "nobody"); kindOf(err) != NotFound {
		t.Fatalf("Get(nobody) error = %v, want not found", err)
	}
}

func TestUpdate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newService(t)
	redeem(t, s, invite(t, s, controlapi.PairCodeRequest{Name: "anna"}).Code, pairapi.PairingPerson)
	on, off := true, false

	resp, err := s.Update(ctx, "anna", controlapi.PairingUpdate{Owner: &on, Kind: pairapi.PairingDevice})
	if err != nil {
		t.Fatalf("Update(owner, kind) error = %v", err)
	}
	if p := resp.Pairing; p.Role != auth.RoleOwner || p.Kind != pairapi.PairingDevice || p.Principal != "pair:anna" || len(p.Tokens) != 1 || resp.Code != nil {
		t.Fatalf("Update(owner, kind) = %+v, want anna as an owner device with her token", resp)
	}
	if resp, err = s.Update(ctx, "anna", controlapi.PairingUpdate{Owner: &off}); err != nil || resp.Pairing.Role != auth.RoleAuthenticated {
		t.Fatalf("Update(owner=false) = %+v, %v, want anna authenticated", resp, err)
	}

	resp, err = s.Update(ctx, "anna", controlapi.PairingUpdate{Rotate: true})
	if err != nil {
		t.Fatalf("Update(rotate) error = %v", err)
	}
	if resp.Code == nil || len(resp.Pairing.Tokens) != 0 || len(resp.Pairing.PendingCodes) != 1 {
		t.Fatalf("Update(rotate) = %+v, want no tokens and one new code", resp)
	}

	for _, req := range []controlapi.PairingUpdate{{Webhook: "https://example.com/hook"}, {Kind: "robot"}, {Kind: pairapi.PairingWebhook}} {
		if _, err := s.Update(ctx, "anna", req); kindOf(err) != Invalid {
			t.Fatalf("Update(%+v) error = %v, want invalid", req, err)
		}
	}
	if _, err := s.Update(ctx, LocalOwner, controlapi.PairingUpdate{Owner: &off}); kindOf(err) != Invalid {
		t.Fatalf("Update(local) error = %v, want invalid", err)
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
	if _, err := s.Update(ctx, "grok", controlapi.PairingUpdate{Owner: &on}); kindOf(err) != Invalid {
		t.Fatalf("Update(owner) of a node error = %v, want invalid", err)
	}
}

func TestRemove(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newService(t)
	everyone := &controlapi.Access{Others: []string{"everyone on the tailnet"}, Summary: []string{"you: this machine and your tailnet devices", "everyone on the tailnet"}}
	s.Access = func(context.Context) (*controlapi.Access, error) { return everyone, nil }
	redeem(t, s, invite(t, s, controlapi.PairCodeRequest{Name: "anna"}).Code, pairapi.PairingPerson)
	on := true
	invite(t, s, controlapi.PairCodeRequest{Name: "grok", Webhook: "https://example.com/hook", Advertised: &on})
	if _, _, err := auth.IssueToken(ctx, s.Store, LocalOwner, "http://127.0.0.1:4000", ""); err != nil {
		t.Fatalf("IssueToken(local) error = %v", err)
	}

	tests := []struct {
		name string
		want *controlapi.RevokeResponse
	}{
		{name: "anna", want: &controlapi.RevokeResponse{Kind: pairapi.PairingPerson, StillAllowed: []string{"everyone on the tailnet"}, Access: everyone}},
		{name: "grok", want: &controlapi.RevokeResponse{Kind: pairapi.PairingWebhook, Access: everyone}},
		{name: LocalOwner, want: &controlapi.RevokeResponse{Kind: pairapi.PairingPerson, Kept: true}},
	}
	for _, tc := range tests {
		got, err := s.Remove(ctx, tc.name)
		if err != nil {
			t.Fatalf("Remove(%q) error = %v", tc.name, err)
		}
		if diff := cmp.Diff(tc.want, got); diff != "" {
			t.Fatalf("Remove(%q) wrong result (-want +got) diff = %s", tc.name, diff)
		}
	}
	if s.Ads.Advertised("grok") {
		t.Fatalf("Remove(grok) kept its advertisement")
	}
	all, err := s.List(ctx)
	if err != nil || len(all) != 1 || all[0].Name != LocalOwner || len(all[0].Tokens) != 0 {
		t.Fatalf("List() after Remove() = %+v, %v, want only the local pairing, without tokens", all, err)
	}
	if _, err := s.Remove(ctx, "anna"); kindOf(err) != NotFound {
		t.Fatalf("Remove(anna) again error = %v, want not found", err)
	}
}

func TestInviteRacesPrune(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newService(t)
	done := make(chan struct{})
	pruned := make(chan error, 1)
	go func() {
		for {
			select {
			case <-done:
				pruned <- nil
				return
			default:
			}
			if err := s.Store.Prune(ctx); err != nil {
				pruned <- err
				return
			}
		}
	}()

	var err error
	for i := range 200 {
		if _, err = s.Invite(ctx, controlapi.PairCodeRequest{Name: fmt.Sprintf("p%d", i)}); err != nil {
			break
		}
	}
	close(done)
	if pruneErr := <-pruned; pruneErr != nil {
		t.Fatalf("Prune() error = %v", pruneErr)
	}
	if err != nil {
		t.Fatalf("Invite() while pruning error = %v, want nil", err)
	}
}
