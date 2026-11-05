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
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/pairapi"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// frozenClock keeps the limiter's backoffs and windows from expiring
// however slowly a test runs.
func frozenClock() time.Time { return time.Unix(1000, 0) }

type fakeAds map[string]string

func (a fakeAds) Advertise(name, description string) error { a[name] = description; return nil }
func (a fakeAds) Withdraw(name string) error               { delete(a, name); return nil }
func (a fakeAds) Advertised(name string) bool {
	_, ok := a[name]
	return ok
}

func newService(t *testing.T, opts ...func(*Deps)) *Service {
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
	deps := Deps{Store: st, BaseURL: func() string { return "https://coder.example.com" }, Ads: fakeAds{}, Limiter: NewLimiter(frozenClock)}
	for _, o := range opts {
		o(&deps)
	}
	return New(deps)
}

// redeem exchanges code for a token as a redeemer of kind, as POST /pair
// does.
func redeem(t *testing.T, s *Service, code, kind string) {
	t.Helper()
	if _, err := s.Redeem(context.Background(), code, Via{URL: s.deps.BaseURL(), Kind: kind}); err != nil {
		t.Fatalf("Redeem() error = %v", err)
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

type errKind string

const (
	Invalid  errKind = "invalid"
	Conflict errKind = "conflict"
	NotFound errKind = "not found"
)

func kindOf(err error) errKind {
	var (
		nf *agenterr.NotFoundError
		in *agenterr.InvalidError
		co *agenterr.ConflictError
	)
	switch {
	case errors.As(err, &nf):
		return NotFound
	case errors.As(err, &in):
		return Invalid
	case errors.As(err, &co):
		return Conflict
	}
	return ""
}

func TestInvite(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		req      controlapi.PairCodeRequest
		wantKind errKind
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

			if got := kindOf(err); got != tc.wantKind || (tc.wantKind == "" && err != nil) {
				t.Fatalf("Invite(%+v) error = %v, want kind %v", tc.req, err, tc.wantKind)
			}
			if tc.wantKind != "" {
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
	ads := &flakyAds{Advertiser: fakeAds{}}
	s := newService(t, withAds(ads))
	invite(t, s, controlapi.PairCodeRequest{Name: "grok", Webhook: "https://example.com/hook"})
	node, err := s.deps.Store.Node(ctx, "grok")
	if err != nil {
		t.Fatalf("Node() error = %v", err)
	}
	node.TokenHash = "paired"
	if err := s.deps.Store.PutNode(ctx, node); err != nil {
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
	everyone := &controlapi.Access{Others: []string{"everyone on the tailnet"}, Summary: []string{"you: this machine and your tailnet devices", "everyone on the tailnet"}}
	var forgotten []string
	s := newService(t, func(d *Deps) {
		d.Access = func(context.Context) (*controlapi.Access, error) { return everyone, nil }
		d.Forget = func(id string) { forgotten = append(forgotten, id) }
	})
	redeem(t, s, invite(t, s, controlapi.PairCodeRequest{Name: "anna"}).Code, pairapi.PairingPerson)
	on := true
	invite(t, s, controlapi.PairCodeRequest{Name: "grok", Webhook: "https://example.com/hook", Advertised: &on})
	if _, _, err := auth.IssueToken(ctx, s.deps.Store, LocalOwner, "http://127.0.0.1:4000", ""); err != nil {
		t.Fatalf("IssueToken(local) error = %v", err)
	}
	for _, user := range []string{auth.PairPrincipal("anna"), auth.NodePrincipal("grok"), auth.PairPrincipal(LocalOwner)} {
		if err := s.deps.Store.AddSchedule(ctx, store.Schedule{Name: user, Cron: "@daily", Prompt: "p", UserID: user}); err != nil {
			t.Fatalf("AddSchedule(%s) error = %v", user, err)
		}
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
	if s.deps.Ads.Advertised("grok") {
		t.Fatalf("Remove(grok) kept its advertisement")
	}
	if diff := cmp.Diff([]string{"grok"}, forgotten); diff != "" {
		t.Fatalf("Remove() forgot wrong nodes (-want +got) diff = %s", diff)
	}
	all, err := s.List(ctx)
	if err != nil || len(all) != 1 || all[0].Name != LocalOwner || len(all[0].Tokens) != 0 {
		t.Fatalf("List() after Remove() = %+v, %v, want only the local pairing, without tokens", all, err)
	}
	schedules, err := s.deps.Store.Schedules(ctx)
	if err != nil {
		t.Fatalf("Schedules() error = %v", err)
	}
	if len(schedules) != 1 || schedules[0].UserID != auth.PairPrincipal(LocalOwner) {
		t.Fatalf("Schedules() after Remove() = %+v, want only the local pairing's", schedules)
	}
	if _, err := s.Remove(ctx, "anna"); kindOf(err) != NotFound {
		t.Fatalf("Remove(anna) again error = %v, want not found", err)
	}
}

// fakeUsers records the users Stop stops, and checks what Remove does
// while they are stopped.
type fakeUsers struct {
	t        *testing.T
	s        *Service
	homes    agentdir.Dir
	err      error
	stopped  []string
	released bool
}

func (u *fakeUsers) Stop(ctx context.Context, principals ...string) (func(), error) {
	if u.err != nil {
		return nil, u.err
	}
	u.stopped = append(u.stopped, principals...)
	return func() {
		if _, err := os.Stat(u.homes.UserHomeDir(principals[0])); !errors.Is(err, os.ErrNotExist) {
			u.t.Errorf("home of %s at release error = %v, want it deleted while its user is stopped", principals[0], err)
		}
		if p, err := u.s.deps.Store.Pairing(ctx, "anna"); !errors.Is(err, store.ErrNotFound) {
			u.t.Errorf("Pairing() at release = %+v, %v, want it deleted while its users are stopped", p, err)
		}
		u.released = true
	}, nil
}

func TestRemoveStopsUsersAndDeletesTheirHomes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d, err := agentdir.Open(t.TempDir())
	if err != nil {
		t.Fatalf("agentdir.Open() error = %v", err)
	}
	users := &fakeUsers{t: t, homes: d}
	s := newService(t, withUsers(users, d))
	users.s = s
	for _, name := range []string{"anna", "bob"} {
		redeem(t, s, invite(t, s, controlapi.PairCodeRequest{Name: name}).Code, pairapi.PairingPerson)
	}
	for _, id := range []string{"pair:anna", "node:anna", "pair:bob"} {
		if err := os.MkdirAll(d.UserHomeDir(id), 0o700); err != nil {
			t.Fatalf("MkdirAll(%s) error = %v", id, err)
		}
	}

	if _, err := s.Remove(ctx, "anna"); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}

	if diff := cmp.Diff([]string{"pair:anna", "node:anna"}, users.stopped); diff != "" {
		t.Fatalf("Remove() stopped wrong users (-want +got) diff = %s", diff)
	}
	if !users.released {
		t.Fatalf("Remove() didn't let anna's users start again")
	}
	for _, id := range []string{"pair:anna", "node:anna"} {
		if _, err := os.Stat(d.UserHomeDir(id)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("Stat() of the home of %s error = %v, want %v", id, err, os.ErrNotExist)
		}
	}
	if _, err := os.Stat(d.UserHomeDir("pair:bob")); err != nil {
		t.Fatalf("Stat() of another user's home error = %v", err)
	}
	if _, err := s.Remove(ctx, LocalOwner); err != nil {
		t.Fatalf("Remove(local) error = %v", err)
	}
	if diff := cmp.Diff([]string{"pair:anna", "node:anna"}, users.stopped); diff != "" {
		t.Fatalf("Remove(local) stopped users (-want +got) diff = %s", diff)
	}
}

func TestRemoveKeepsPairingWhenUsersDontStop(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d, err := agentdir.Open(t.TempDir())
	if err != nil {
		t.Fatalf("agentdir.Open() error = %v", err)
	}
	users := &fakeUsers{t: t, homes: d, err: errors.New("cancel failed")}
	s := newService(t, withUsers(users, d))
	users.s = s
	redeem(t, s, invite(t, s, controlapi.PairCodeRequest{Name: "anna"}).Code, pairapi.PairingPerson)
	if err := os.MkdirAll(d.UserHomeDir("pair:anna"), 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	if _, err := s.Remove(ctx, "anna"); err == nil {
		t.Fatalf("Remove() error = nil, want the failure to stop anna")
	}

	if _, err := s.deps.Store.Pairing(ctx, "anna"); err != nil {
		t.Fatalf("Pairing() after a failed Remove() error = %v, want it kept", err)
	}
	if _, err := os.Stat(d.UserHomeDir("pair:anna")); err != nil {
		t.Fatalf("Stat() of the home after a failed Remove() error = %v, want it kept", err)
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
			if err := s.deps.Store.Prune(ctx); err != nil {
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

// flakyAds wraps an Advertiser and fails or hooks its changes.
type flakyAds struct {
	Advertiser
	advertiseErr error
	withdrawErr  error
	// onAdvertise runs before an advertisement is stored.
	onAdvertise func()
}

func (a *flakyAds) Advertise(name, description string) error {
	if a.onAdvertise != nil {
		a.onAdvertise()
	}
	if a.advertiseErr != nil {
		return a.advertiseErr
	}
	return a.Advertiser.Advertise(name, description)
}

func (a *flakyAds) Withdraw(name string) error {
	if a.withdrawErr != nil {
		return a.withdrawErr
	}
	return a.Advertiser.Withdraw(name)
}

func withUsers(u Users, h Homes) func(*Deps) {
	return func(d *Deps) { d.Users, d.Homes = u, h }
}

func withAds(a Advertiser) func(*Deps) {
	return func(d *Deps) { d.Ads = a }
}

func countChanges(n *int) func(*Deps) {
	return func(d *Deps) { d.Changed = func(context.Context) { *n++ } }
}

func TestInviteNodeWithFailingAdvertisementLeavesNoNode(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newService(t, withAds(&flakyAds{Advertiser: fakeAds{}, advertiseErr: errors.New("saving the table")}))
	on := true

	_, err := s.Invite(ctx, controlapi.PairCodeRequest{Name: "grok", Webhook: "https://example.com/hook", Advertised: &on})

	if err == nil {
		t.Fatalf("s.Invite() with a failing advertisement error = nil, want error")
	}
	if _, err := s.deps.Store.Node(ctx, "grok"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("s.Store.Node() after the failed invitation error = %v, want %v", err, store.ErrNotFound)
	}
	if _, err := s.Get(ctx, "grok"); kindOf(err) != NotFound {
		t.Fatalf("s.Get() after the failed invitation error = %v, want not found", err)
	}
}

func TestInviteNodeNotifiesChange(t *testing.T) {
	t.Parallel()
	changes := new(int)
	s := newService(t, countChanges(changes))

	invite(t, s, controlapi.PairCodeRequest{Name: "grok", Webhook: "https://example.com/hook"})

	if *changes != 1 {
		t.Fatalf("Changed called %d times after the invitation, want 1", *changes)
	}
}

func TestUpdateNodeRotateWithFailingAdvertisementKeepsToken(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ads := &flakyAds{Advertiser: fakeAds{}}
	s := newService(t, withAds(ads))
	invite(t, s, controlapi.PairCodeRequest{Name: "grok", Webhook: "https://example.com/hook"})
	node, err := s.deps.Store.Node(ctx, "grok")
	if err != nil {
		t.Fatalf("Node() error = %v", err)
	}
	next := node
	next.TokenHash = "paired"
	if err := s.deps.Store.UpdateNode(ctx, node, next); err != nil {
		t.Fatalf("UpdateNode() error = %v", err)
	}
	ads.advertiseErr = errors.New("saving the table")
	on := true

	_, err = s.Update(ctx, "grok", controlapi.PairingUpdate{Rotate: true, Advertised: &on})

	if err == nil {
		t.Fatalf("s.Update() with a failing advertisement error = nil, want error")
	}
	got, err := s.deps.Store.Node(ctx, "grok")
	if err != nil {
		t.Fatalf("s.Store.Node() error = %v", err)
	}
	if got.TokenHash != "paired" || got.Secret != next.Secret {
		t.Fatalf("s.Store.Node() after the failed rotation = %+v, want the token and secret kept", got)
	}
}

func TestUpdateNodeRestoresAdvertisementWhenNodeChangedMeanwhile(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ads := &flakyAds{Advertiser: fakeAds{}}
	s := newService(t, withAds(ads))
	invite(t, s, controlapi.PairCodeRequest{Name: "grok", Webhook: "https://example.com/hook"})
	ads.onAdvertise = func() {
		ads.onAdvertise = nil
		node, err := s.deps.Store.Node(ctx, "grok")
		if err != nil {
			t.Errorf("Node() error = %v", err)
			return
		}
		next := node
		next.Webhook = "https://example.com/other"
		if err := s.deps.Store.UpdateNode(ctx, node, next); err != nil {
			t.Errorf("UpdateNode() error = %v", err)
		}
	}
	on := true

	_, err := s.Update(ctx, "grok", controlapi.PairingUpdate{Advertised: &on})

	if kindOf(err) != Conflict {
		t.Fatalf("s.Update() of a node changed meanwhile error = %v, want conflict", err)
	}
	if ads.Advertised("grok") {
		t.Fatalf("s.Update() of a node changed meanwhile left it advertised")
	}
}

func TestUpdateNodeNotifiesChange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	changes := new(int)
	s := newService(t, countChanges(changes))
	invite(t, s, controlapi.PairCodeRequest{Name: "grok", Webhook: "https://example.com/hook"})
	*changes = 0
	desc := "searches"

	if _, err := s.Update(ctx, "grok", controlapi.PairingUpdate{Description: &desc}); err != nil {
		t.Fatalf("s.Update() error = %v", err)
	}

	if *changes != 1 {
		t.Fatalf("Changed called %d times after the update, want 1", *changes)
	}
}

func TestRemoveNodeNotifiesChangeWhenWithdrawFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ads := &flakyAds{Advertiser: fakeAds{}}
	changes := new(int)
	s := newService(t, withAds(ads), countChanges(changes))
	on := true
	invite(t, s, controlapi.PairCodeRequest{Name: "grok", Webhook: "https://example.com/hook", Advertised: &on})
	ads.withdrawErr = errors.New("saving the table")
	*changes = 0

	_, err := s.Remove(ctx, "grok")

	if err == nil {
		t.Fatalf("s.Remove() with a failing withdrawal error = nil, want error")
	}
	if *changes != 1 {
		t.Fatalf("Changed called %d times after the removal, want 1", *changes)
	}
}
