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
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/auth"
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

func invite(t *testing.T, s *Service, req InviteRequest) *Code {
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
		req      InviteRequest
		wantKind errKind
	}{
		{name: "new caller", req: InviteRequest{Name: "bob"}},
		{name: "another device of a caller", req: InviteRequest{Name: "anna"}},
		{name: "new owner", req: InviteRequest{Name: "bob", Owner: true}},
		{name: "owner for a member pairing", req: InviteRequest{Name: "anna", Owner: true}, wantKind: Invalid},
		{name: "node code", req: InviteRequest{Name: "grok"}},
		{name: "webhook of an existing node", req: InviteRequest{Name: "grok", Webhook: "https://example.com/new"}, wantKind: Invalid},
		{name: "webhook for a person's name", req: InviteRequest{Name: "anna", Webhook: "https://example.com/hook"}, wantKind: Conflict},
		{name: "owner of a node", req: InviteRequest{Name: "grok", Owner: true}, wantKind: Invalid},
		{name: "description without a webhook", req: InviteRequest{Name: "bob", Description: "x"}, wantKind: Invalid},
		{name: "local pairing", req: InviteRequest{Name: LocalOwner}, wantKind: Invalid},
		{name: "owner", req: InviteRequest{Name: auth.OwnerName}, wantKind: Invalid},
		{name: "invalid name", req: InviteRequest{Name: "a b"}, wantKind: Invalid},
		{name: "local pairing in capitals", req: InviteRequest{Name: "LOCAL"}, wantKind: Invalid},
		{name: "owner capitalized", req: InviteRequest{Name: "Owner"}, wantKind: Invalid},
		{name: "name differing only by case", req: InviteRequest{Name: "Anna"}, wantKind: Conflict},
		{name: "node name differing only by case", req: InviteRequest{Name: "GROK", Webhook: "https://example.com/hook"}, wantKind: Conflict},
		{name: "leading hyphen", req: InviteRequest{Name: "-x"}, wantKind: Invalid},
		{name: "at sign", req: InviteRequest{Name: "bob@coder"}, wantKind: Invalid},
		{name: "too long", req: InviteRequest{Name: strings.Repeat("a", 65)}, wantKind: Invalid},
		{name: "longest name", req: InviteRequest{Name: strings.Repeat("a", 64)}},
		{name: "dots, hyphens and underscores", req: InviteRequest{Name: "ci_bot-2.prod"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newService(t)
			redeem(t, s, invite(t, s, InviteRequest{Name: "anna"}).Code, pairapi.PairingPerson)
			invite(t, s, InviteRequest{Name: "grok", Webhook: "https://example.com/hook"})

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
	redeem(t, s, invite(t, s, InviteRequest{Name: "anna", Owner: true}).Code, pairapi.PairingPerson)
	invite(t, s, InviteRequest{Name: "anna"})
	redeem(t, s, invite(t, s, InviteRequest{Name: "anna"}).Code, pairapi.PairingAgent)
	redeem(t, s, invite(t, s, InviteRequest{Name: "reviewer"}).Code, pairapi.PairingAgent)
	on := true
	invite(t, s, InviteRequest{Name: "grok", Webhook: "https://example.com/hook?key=secret", Headers: map[string]string{"Authorization": "Bearer x"}, Description: "searches", Advertised: &on})

	all, err := s.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	type summary struct {
		Name, Kind, Role, Principal string
		Tokens, Codes               int
		Node                        *NodeDetails
	}
	var got []summary
	for _, p := range all {
		got = append(got, summary{p.Name, p.Kind, p.Role, p.Principal, len(p.Tokens), len(p.PendingCodes), p.Node})
	}
	want := []summary{
		{Name: "anna", Kind: pairapi.PairingPerson, Role: auth.RoleOwner, Principal: "pair:anna", Tokens: 2, Codes: 1},
		{Name: "grok", Kind: pairapi.PairingWebhook, Role: auth.RoleMember, Principal: "node:grok", Codes: 1, Node: &NodeDetails{
			URL: "https://coder.example.com/n/grok", Webhook: "https://example.com/hook?…", Description: "searches",
			HeaderNames: []string{"Authorization"}, Advertised: true,
		}},
		{Name: LocalOwner, Kind: pairapi.PairingPerson, Role: auth.RoleOwner, Principal: auth.OwnerID},
		{Name: "reviewer", Kind: pairapi.PairingAgent, Role: auth.RoleMember, Principal: "pair:reviewer", Tokens: 1},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("List() wrong result (-want +got) diff = %s", diff)
	}
	invite(t, s, InviteRequest{Name: "bob"})
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
	redeem(t, s, invite(t, s, InviteRequest{Name: "anna"}).Code, pairapi.PairingPerson)
	on, off := true, false

	resp, err := s.Update(ctx, "anna", UpdateRequest{Owner: &on, Kind: pairapi.PairingDevice})
	if err != nil {
		t.Fatalf("Update(owner, kind) error = %v", err)
	}
	if p := resp.Pairing; p.Role != auth.RoleOwner || p.Kind != pairapi.PairingDevice || p.Principal != "pair:anna" || len(p.Tokens) != 1 || resp.Code != nil {
		t.Fatalf("Update(owner, kind) = %+v, want anna as an owner device with her token", resp)
	}
	if resp, err = s.Update(ctx, "anna", UpdateRequest{Owner: &off}); err != nil || resp.Pairing.Role != auth.RoleMember {
		t.Fatalf("Update(owner=false) = %+v, %v, want anna member", resp, err)
	}

	resp, err = s.Update(ctx, "anna", UpdateRequest{Rotate: true})
	if err != nil {
		t.Fatalf("Update(rotate) error = %v", err)
	}
	if resp.Code == nil || len(resp.Pairing.Tokens) != 0 || len(resp.Pairing.PendingCodes) != 1 {
		t.Fatalf("Update(rotate) = %+v, want no tokens and one new code", resp)
	}

	for _, req := range []UpdateRequest{{Webhook: "https://example.com/hook"}, {Kind: "robot"}, {Kind: pairapi.PairingWebhook}} {
		if _, err := s.Update(ctx, "anna", req); kindOf(err) != Invalid {
			t.Fatalf("Update(%+v) error = %v, want invalid", req, err)
		}
	}
	if _, err := s.Update(ctx, LocalOwner, UpdateRequest{Owner: &off}); kindOf(err) != Invalid {
		t.Fatalf("Update(local) error = %v, want invalid", err)
	}
	if _, err := s.Update(ctx, "nobody", UpdateRequest{Rotate: true}); kindOf(err) != NotFound {
		t.Fatalf("Update(nobody) error = %v, want not found", err)
	}
}

func TestUpdateNode(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ads := &flakyAds{Advertiser: fakeAds{}}
	s := newService(t, withAds(ads))
	invite(t, s, InviteRequest{Name: "grok", Webhook: "https://example.com/hook"})
	node, err := s.deps.Store.Node(ctx, "grok")
	if err != nil {
		t.Fatalf("Node() error = %v", err)
	}
	node.TokenHash = "paired"
	if err := s.deps.Store.PutNode(ctx, node); err != nil {
		t.Fatalf("PutNode() error = %v", err)
	}

	desc, on := "searches", true
	resp, err := s.Update(ctx, "grok", UpdateRequest{Webhook: "https://example.com/new", Description: &desc, Advertised: &on})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if n := resp.Pairing.Node; n.Webhook != "https://example.com/new" || n.Description != desc || !n.Advertised || !n.Paired || resp.Code != nil {
		t.Fatalf("Update() = %+v, want the node changed in place and still paired", resp)
	}

	resp, err = s.Update(ctx, "grok", UpdateRequest{Rotate: true})
	if err != nil {
		t.Fatalf("Update(rotate) error = %v", err)
	}
	if resp.Pairing.Node.Paired || resp.Code == nil || resp.Code.Node == nil {
		t.Fatalf("Update(rotate) = %+v, want the token revoked and a node code", resp)
	}
	if _, err := s.Update(ctx, "grok", UpdateRequest{Owner: &on}); kindOf(err) != Invalid {
		t.Fatalf("Update(owner) of a node error = %v, want invalid", err)
	}
}

func TestRemove(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var forgotten []string
	s := newService(t, func(d *Deps) {
		d.Forget = func(id string) { forgotten = append(forgotten, id) }
	})
	redeem(t, s, invite(t, s, InviteRequest{Name: "anna"}).Code, pairapi.PairingPerson)
	on := true
	invite(t, s, InviteRequest{Name: "grok", Webhook: "https://example.com/hook", Advertised: &on})
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
		want *RemoveResult
	}{
		{name: "anna", want: &RemoveResult{Kind: pairapi.PairingPerson}},
		{name: "grok", want: &RemoveResult{Kind: pairapi.PairingWebhook}},
		{name: LocalOwner, want: &RemoveResult{Kind: pairapi.PairingPerson, Kept: true}},
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

func TestPreviewRemoveListsHomesAndWorkspaces(t *testing.T) {
	t.Parallel()
	d, err := agentdir.Open(t.TempDir())
	if err != nil {
		t.Fatalf("agentdir.Open() error = %v", err)
	}
	users := &fakeUsers{t: t, homes: d}
	s := newService(t, withUsers(users, d))
	users.s = s
	redeem(t, s, invite(t, s, InviteRequest{Name: "anna"}).Code, pairapi.PairingPerson)
	for _, dir := range []string{d.UserHomeDir("pair:anna"), d.UserWorkspacesDir("pair:anna")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("MkdirAll(%s) error = %v", dir, err)
		}
	}

	got, err := s.PreviewRemove(context.Background(), "anna")
	if err != nil {
		t.Fatalf("PreviewRemove() error = %v", err)
	}

	want := struct{ AgentHomes, Workspaces []string }{[]string{d.UserHomeDir("pair:anna")}, []string{d.UserWorkspacesDir("pair:anna")}}
	if diff := cmp.Diff(want, struct{ AgentHomes, Workspaces []string }{got.AgentHomes, got.Workspaces}); diff != "" {
		t.Fatalf("PreviewRemove() wrong result (-want +got) diff = %s", diff)
	}
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
		redeem(t, s, invite(t, s, InviteRequest{Name: name}).Code, pairapi.PairingPerson)
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
	redeem(t, s, invite(t, s, InviteRequest{Name: "anna"}).Code, pairapi.PairingPerson)
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
		if _, err = s.Invite(ctx, InviteRequest{Name: fmt.Sprintf("p%d", i)}); err != nil {
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

	_, err := s.Invite(ctx, InviteRequest{Name: "grok", Webhook: "https://example.com/hook", Advertised: &on})

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

	invite(t, s, InviteRequest{Name: "grok", Webhook: "https://example.com/hook"})

	if *changes != 1 {
		t.Fatalf("Changed called %d times after the invitation, want 1", *changes)
	}
}

func TestUpdateNodeRotateWithFailingAdvertisementKeepsToken(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ads := &flakyAds{Advertiser: fakeAds{}}
	s := newService(t, withAds(ads))
	invite(t, s, InviteRequest{Name: "grok", Webhook: "https://example.com/hook"})
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

	_, err = s.Update(ctx, "grok", UpdateRequest{Rotate: true, Advertised: &on})

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
	invite(t, s, InviteRequest{Name: "grok", Webhook: "https://example.com/hook"})
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

	_, err := s.Update(ctx, "grok", UpdateRequest{Advertised: &on})

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
	invite(t, s, InviteRequest{Name: "grok", Webhook: "https://example.com/hook"})
	*changes = 0
	desc := "searches"

	if _, err := s.Update(ctx, "grok", UpdateRequest{Description: &desc}); err != nil {
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
	invite(t, s, InviteRequest{Name: "grok", Webhook: "https://example.com/hook", Advertised: &on})
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

func TestDirectConvWithoutChatHints(t *testing.T) {
	t.Parallel()
	s := newService(t)
	tests := []struct {
		user string
		want string
	}{
		{user: auth.OwnerID, want: "owner has no chat on telegram: link it with the owner link that `a2a-serve restart` prints"},
		{user: auth.PairPrincipal("anna"), want: "anna has no chat on telegram: link it with a code from `a2a-serve pair invite anna`"},
		{user: auth.NodePrincipal("grok"), want: "grok has no chat on telegram"},
	}
	for _, tc := range tests {
		t.Run(tc.user, func(t *testing.T) {
			t.Parallel()
			_, err := DirectConv(context.Background(), s.deps.Store, "telegram", tc.user)
			if got := agenterr.Text(err); got != tc.want {
				t.Fatalf("DirectConv(%q) error = %q, want %q", tc.user, got, tc.want)
			}
		})
	}
}
