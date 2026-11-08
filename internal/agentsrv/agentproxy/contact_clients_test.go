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

package agentproxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/a2aproject/a2a-serve/internal/a2atext"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/approvals"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/egress"
	"github.com/a2aproject/a2a-serve/internal/network"
	"github.com/a2aproject/a2a-serve/internal/store"
)

func newTestContactClients(t *testing.T, st *store.Store, private network.Provider) *ContactClients {
	t.Helper()
	return newTestContactClientsOutgoing(t, st, private, "")
}

// newTestContactClientsOutgoing returns contact clients of an agent with
// --private-allow-outgoing outgoing.
func newTestContactClientsOutgoing(t *testing.T, st *store.Store, private network.Provider, outgoing string) *ContactClients {
	t.Helper()
	cfg := &agentdir.Config{Name: "assistant", Policy: agentdir.PolicyConfig{PrivateAllowOutgoing: outgoing}}
	cc := NewContactClients(ContactClientsDeps{Store: st, Private: private, Config: func() *agentdir.Config { return cfg }})
	t.Cleanup(func() {
		if err := cc.Close(context.Background()); err != nil {
			t.Errorf("ContactClients.close() error = %v", err)
		}
	})
	return cc
}

func putContact(t *testing.T, st *store.Store, c store.Contact) {
	t.Helper()
	if c.Via == "" {
		c.Via = ViaDirect
	}
	if err := st.PutContact(context.Background(), c); err != nil {
		t.Fatalf("store.PutContact() error = %v", err)
	}
}

func putNode(t *testing.T, st *store.Store, id string) {
	t.Helper()
	if err := st.PutNode(context.Background(), store.Node{ID: id, Name: id}); err != nil {
		t.Fatalf("store.PutNode() error = %v", err)
	}
}

func TestContactClientsCallable(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	putContact(t, st, store.Contact{Name: "coder", URL: "http://coder.test"})
	putNode(t, st, "grok")
	cc := newTestContactClients(t, st, nil)
	tests := []struct {
		name         string
		agent        string
		wantNode     bool
		wantCallable bool
	}{
		{name: "contact", agent: "coder", wantCallable: true},
		{name: "node", agent: "grok", wantNode: true, wantCallable: true},
		{name: "unknown", agent: "nobody"},
		{name: "empty", agent: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			node, err := cc.isNode(ctx, tc.agent)
			if err != nil || node != tc.wantNode {
				t.Fatalf("ContactClients.isNode(%q) = %v, %v, want %v", tc.agent, node, err, tc.wantNode)
			}
			callable, err := cc.callable(ctx, tc.agent)
			if err != nil || callable != tc.wantCallable {
				t.Fatalf("ContactClients.callable(%q) = %v, %v, want %v", tc.agent, callable, err, tc.wantCallable)
			}
		})
	}
}

func TestContactClientsCallablePeers(t *testing.T) {
	t.Parallel()
	peers := &staticPeers{peers: []network.PeerInfo{{Name: "ben", URL: "http://ben.test", Online: true}}}
	tests := []struct {
		name         string
		private      network.Provider
		outgoing     string
		agent        string
		wantCallable bool
		wantPeers    []string
	}{
		{name: "everyone on the private network", private: peers, outgoing: agentdir.AccessEveryone, agent: "ben", wantCallable: true, wantPeers: []string{"ben"}},
		{name: "peer in other case", private: peers, outgoing: agentdir.AccessEveryone, agent: "BEN", wantCallable: true, wantPeers: []string{"ben"}},
		{name: "unknown peer", private: peers, outgoing: agentdir.AccessEveryone, agent: "nobody", wantPeers: []string{"ben"}},
		{name: "contacts only", private: peers, outgoing: agentdir.AccessContacts, agent: "ben"},
		{name: "default", private: peers, agent: "ben"},
		{name: "no private network", outgoing: agentdir.AccessEveryone, agent: "ben"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			cc := newTestContactClientsOutgoing(t, openTestStore(t), tc.private, tc.outgoing)

			callable, err := cc.callable(ctx, tc.agent)
			if err != nil || callable != tc.wantCallable {
				t.Fatalf("ContactClients.callable(%q) = %v, %v, want %v", tc.agent, callable, err, tc.wantCallable)
			}
			if diff := cmp.Diff(tc.wantPeers, cc.CallablePeers(ctx)); diff != "" {
				t.Fatalf("ContactClients.CallablePeers() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestContactClientsCallsPeer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	remote := newFakeContact(t)
	peers := &staticPeers{peers: []network.PeerInfo{{Name: "ben", URL: remote.URL, Online: true}}}
	cc := newTestContactClientsOutgoing(t, openTestStore(t), peers, agentdir.AccessEveryone)

	if _, err := cc.contactClient(ctx, "ben"); err != nil {
		t.Fatalf("ContactClients.contactClient() error = %v", err)
	}

	if len(remote.received()) == 0 {
		t.Fatalf("the peer got no requests, want its agent card fetched")
	}
}

func TestContactClientsContactWinsOverPeer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	contact, peer := newFakeContact(t), newFakeContact(t)
	st := openTestStore(t)
	putContact(t, st, store.Contact{Name: "ben", URL: contact.URL})
	peers := &staticPeers{peers: []network.PeerInfo{{Name: "ben", URL: peer.URL, Online: true}}}
	cc := newTestContactClientsOutgoing(t, st, peers, agentdir.AccessEveryone)

	if _, err := cc.contactClient(ctx, "ben"); err != nil {
		t.Fatalf("ContactClients.contactClient() error = %v", err)
	}

	if len(contact.received()) == 0 || len(peer.received()) != 0 {
		t.Fatalf("requests to the contact = %d, to the peer = %d, want the contact's URL used", len(contact.received()), len(peer.received()))
	}
}

func TestContactClientsCallableAfterContactRemoved(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTestStore(t)
	putContact(t, st, store.Contact{Name: "coder", URL: "http://coder.test"})
	cc := newTestContactClients(t, st, nil)
	if err := st.DeleteContact(ctx, "coder"); err != nil {
		t.Fatalf("store.DeleteContact() error = %v", err)
	}

	callable, err := cc.callable(ctx, "coder")
	if err != nil || callable {
		t.Fatalf("ContactClients.callable() = %v, %v, want false after the contact was removed", callable, err)
	}
}

func TestContactClientsContactClientIsCached(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	remote := newFakeContact(t)
	st := openTestStore(t)
	putContact(t, st, store.Contact{Name: "coder", URL: remote.URL})
	cc := newTestContactClients(t, st, nil)

	first, err := cc.contactClient(ctx, "coder")
	if err != nil {
		t.Fatalf("ContactClients.contactClient() error = %v", err)
	}
	putContact(t, st, store.Contact{Name: "coder", URL: remote.URL})
	second, err := cc.contactClient(ctx, "coder")
	if err != nil {
		t.Fatalf("ContactClients.contactClient() again error = %v", err)
	}

	if second != first {
		t.Fatalf("ContactClients.contactClient() for an unchanged contact built a new client, want the cached one")
	}
	cards := 0
	for _, r := range remote.received() {
		if r.Path == "/.well-known/agent-card.json" {
			cards++
		}
	}
	if cards != 1 {
		t.Fatalf("agent card requests = %d, want 1", cards)
	}
}

func TestContactClientsContactClientRebuiltWhenContactChanges(t *testing.T) {
	t.Parallel()
	other := newFakeContact(t)
	tests := []struct {
		name   string
		change func(c *store.Contact)
	}{
		{name: "token", change: func(c *store.Contact) { c.Token = "a2p_new" }},
		{name: "URL", change: func(c *store.Contact) { c.URL = other.URL }},
		{name: "route", change: func(c *store.Contact) { c.Via = ViaPrivate }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			remote := newFakeContact(t)
			st := openTestStore(t)
			c := store.Contact{Name: "coder", URL: remote.URL, Token: "a2p_old", Via: ViaDirect}
			putContact(t, st, c)
			cc := newTestContactClients(t, st, nil)
			first, err := cc.contactClient(ctx, "coder")
			if err != nil {
				t.Fatalf("ContactClients.contactClient() error = %v", err)
			}

			tc.change(&c)
			putContact(t, st, c)
			second, err := cc.contactClient(ctx, "coder")
			if err != nil {
				t.Fatalf("ContactClients.contactClient() after the change error = %v", err)
			}
			third, err := cc.contactClient(ctx, "coder")
			if err != nil {
				t.Fatalf("ContactClients.contactClient() after the change again error = %v", err)
			}

			if second == first {
				t.Fatalf("ContactClients.contactClient() after the contact's %s changed = the old client, want a new one", tc.name)
			}
			if third != second {
				t.Fatalf("ContactClients.contactClient() after the rebuild built another client, want the rebuilt one")
			}
		})
	}
}

func TestContactClientsContactClientSendsTokenToContact(t *testing.T) {
	t.Parallel()
	remote := newFakeContact(t)
	st := openTestStore(t)
	putContact(t, st, store.Contact{Name: "coder", URL: remote.URL, Token: "a2p_secret"})
	cc := newTestContactClients(t, st, nil)

	if _, err := cc.contactClient(context.Background(), "coder"); err != nil {
		t.Fatalf("ContactClients.contactClient() error = %v", err)
	}

	got := remote.received()
	if len(got) == 0 || got[0].Authorization != "Bearer a2p_secret" {
		t.Fatalf("ContactClients.contactClient() requests = %+v, want the contact's token on the card request", got)
	}
}

func TestContactClientsContactClientForgetsRemovedContact(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	remote := newFakeContact(t)
	st := openTestStore(t)
	c := store.Contact{Name: "coder", URL: remote.URL, Via: ViaDirect}
	putContact(t, st, c)
	cc := newTestContactClients(t, st, nil)
	first, err := cc.contactClient(ctx, "coder")
	if err != nil {
		t.Fatalf("ContactClients.contactClient() error = %v", err)
	}

	if err := st.DeleteContact(ctx, "coder"); err != nil {
		t.Fatalf("store.DeleteContact() error = %v", err)
	}
	if _, err := cc.contactClient(ctx, "coder"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("ContactClients.contactClient() of a removed contact error = %v, want %v", err, store.ErrNotFound)
	}
	putContact(t, st, c)
	again, err := cc.contactClient(ctx, "coder")
	if err != nil {
		t.Fatalf("ContactClients.contactClient() after re-adding the contact error = %v", err)
	}

	if again == first {
		t.Fatalf("ContactClients.contactClient() after re-adding the contact = the client of the removed one, want a new client")
	}
}

func TestContactClientsContactClientOfUnknownName(t *testing.T) {
	t.Parallel()
	cc := newTestContactClients(t, openTestStore(t), nil)

	_, err := cc.contactClient(context.Background(), "nobody")

	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("ContactClients.contactClient() error = %v, want %v", err, store.ErrNotFound)
	}
}

func TestContactClientsContactClientOffline(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	// The port of a closed server can go to a listener of another test;
	// nothing listens on port 1.
	putContact(t, st, store.Contact{Name: "coder", URL: "http://127.0.0.1:1"})
	cc := newTestContactClients(t, st, nil)

	_, err := cc.contactClient(context.Background(), "coder")

	if !errors.Is(err, ErrOffline) {
		t.Fatalf("ContactClients.contactClient() of an unreachable contact error = %v, want %v", err, ErrOffline)
	}
}

func TestContactClientsContactClientRecoversWhenContactComesBack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	remote := newFakeContact(t)
	var mu sync.Mutex
	up := false
	gate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if !up {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		http.Redirect(w, r, remote.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(gate.Close)
	st := openTestStore(t)
	putContact(t, st, store.Contact{Name: "coder", URL: gate.URL})
	cc := newTestContactClients(t, st, nil)
	if _, err := cc.contactClient(ctx, "coder"); !errors.Is(err, ErrOffline) {
		t.Fatalf("ContactClients.contactClient() while down error = %v, want %v", err, ErrOffline)
	}

	mu.Lock()
	up = true
	mu.Unlock()
	if _, err := cc.contactClient(ctx, "coder"); err != nil {
		t.Fatalf("ContactClients.contactClient() after the contact came back error = %v", err)
	}
}

func TestContactClientsConcurrentContactClientsAreOne(t *testing.T) {
	t.Parallel()
	remote := newFakeContact(t)
	st := openTestStore(t)
	putContact(t, st, store.Contact{Name: "coder", URL: remote.URL})
	cc := newTestContactClients(t, st, nil)

	clients := make([]*a2aclient.Client, 8)
	var wg sync.WaitGroup
	for i := range clients {
		wg.Go(func() {
			c, err := cc.contactClient(context.Background(), "coder")
			if err != nil {
				t.Errorf("ContactClients.contactClient() error = %v", err)
			}
			clients[i] = c
		})
	}
	wg.Wait()

	for _, c := range clients {
		if c != clients[0] {
			t.Fatalf("ContactClients.contactClient() built different clients concurrently, want one")
		}
	}
}

func TestContactClientsTarget(t *testing.T) {
	t.Parallel()
	remote := newFakeContact(t)
	st := openTestStore(t)
	putContact(t, st, store.Contact{Name: "coder", URL: remote.URL})
	cc := newTestContactClients(t, st, nil)
	tests := []struct {
		name    string
		agent   string
		wantErr error
	}{
		{name: "contact", agent: "coder"},
		{name: "unknown", agent: "nobody", wantErr: store.ErrNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := cc.target(context.Background(), tc.agent, auth.Owner(), 0)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ContactClients.target() error = %v, want %v", err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if _, ok := got.(remoteTarget); !ok {
				t.Fatalf("ContactClients.target() = %T, want a remote target", got)
			}
		})
	}
}

func TestContactClientsTargetSharesTheContactClient(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	remote := newFakeContact(t)
	st := openTestStore(t)
	putContact(t, st, store.Contact{Name: "coder", URL: remote.URL})
	cc := newTestContactClients(t, st, nil)
	want, err := cc.contactClient(ctx, "coder")
	if err != nil {
		t.Fatalf("ContactClients.contactClient() error = %v", err)
	}

	for _, user := range []auth.Principal{auth.Owner(), {ID: "pair:anna", Role: auth.RoleAuthenticated}} {
		tg, err := cc.target(ctx, "coder", user, 1)
		if err != nil {
			t.Fatalf("ContactClients.target() error = %v", err)
		}
		if got := tg.(remoteTarget).c; got != want {
			t.Fatalf("ContactClients.target() for %s built a new client, want the contact's client", user.ID)
		}
	}
}

func TestContactClientsTargetCountsHops(t *testing.T) {
	t.Parallel()
	remote := newFakeContact(t)
	st := openTestStore(t)
	putContact(t, st, store.Contact{Name: "coder", URL: remote.URL})
	cc := newTestContactClients(t, st, nil)

	tg, err := cc.target(context.Background(), "coder", auth.Owner(), 1)
	if err != nil {
		t.Fatalf("ContactClients.target() error = %v", err)
	}
	if _, err := tg.send(context.Background(), a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(promptDone))); err != nil {
		t.Fatalf("target.send() error = %v", err)
	}

	var hops []string
	for _, r := range remote.received() {
		if r.Path == "/" {
			hops = append(hops, r.Hops)
		}
	}
	if diff := cmp.Diff([]string{"2"}, hops); diff != "" {
		t.Fatalf("hops header of the calls to the contact wrong result (-want +got) diff = %s", diff)
	}
}

func TestContactClientsNodeCaller(t *testing.T) {
	t.Parallel()
	cc := newTestContactClients(t, openTestStore(t), nil)

	got := cc.nodeCaller(auth.Principal{ID: "pair:anna", Role: auth.RoleOwner}, 1)

	want := auth.Principal{
		ID: "agent:assistant", Role: auth.RoleAuthenticated, OnBehalfOf: "pair:anna",
		Identity: auth.Identity{Network: network.ListenerHarness, Hops: 2},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("ContactClients.nodeCaller() wrong result (-want +got) diff = %s", diff)
	}
}

func TestContactClientsAnswerRemote(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	remote := newFakeContact(t)
	st := openTestStore(t)
	putContact(t, st, store.Contact{Name: "coder", URL: remote.URL})
	cc := newTestContactClients(t, st, nil)
	task := remote.startTask(t, promptAsk)
	remote.waitState(t, task.ID, a2a.TaskStateInputRequired)
	rec := store.Question{Contact: "coder", TaskID: string(task.ID), ContextID: task.ContextID}
	q := &approvals.Question{ID: "tool1"}

	if err := cc.AnswerRemote(ctx, rec, q, approvals.Option{ID: "once"}, auth.Owner()); err != nil {
		t.Fatalf("ContactClients.AnswerRemote() error = %v", err)
	}

	got := remote.waitState(t, task.ID, a2a.TaskStateCompleted)
	if text := a2atext.Result(got); text != fakeContactAnswer {
		t.Fatalf("remote task result = %q, want %q", text, fakeContactAnswer)
	}
	want, err := approvals.AnswerMessage(task.ID, task.ContextID, "tool1", "once")
	if err != nil {
		t.Fatalf("approvals.AnswerMessage() error = %v", err)
	}
	msgs := remote.receivedMessages()
	answer := msgs[len(msgs)-1]
	if diff := cmp.Diff(want, answer, cmpopts.IgnoreFields(a2a.Message{}, "ID"), cmpopts.EquateEmpty()); diff != "" {
		t.Fatalf("answer received by the remote agent wrong result (-want +got) diff = %s", diff)
	}
}

func TestContactClientsAnswerRemoteErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q := &approvals.Question{ID: "tool1"}
	t.Run("unknown contact", func(t *testing.T) {
		t.Parallel()
		cc := newTestContactClients(t, openTestStore(t), nil)

		err := cc.AnswerRemote(ctx, store.Question{Contact: "nobody", TaskID: "t1"}, q, approvals.Option{ID: "once"}, auth.Owner())

		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("ContactClients.AnswerRemote() error = %v, want %v", err, store.ErrNotFound)
		}
	})
	t.Run("unknown task forgets the client", func(t *testing.T) {
		t.Parallel()
		remote := newFakeContact(t)
		st := openTestStore(t)
		putContact(t, st, store.Contact{Name: "coder", URL: remote.URL})
		cc := newTestContactClients(t, st, nil)
		first, err := cc.contactClient(ctx, "coder")
		if err != nil {
			t.Fatalf("ContactClients.contactClient() error = %v", err)
		}

		err = cc.AnswerRemote(ctx, store.Question{Contact: "coder", TaskID: "gone", ContextID: "c"}, q, approvals.Option{ID: "once"}, auth.Owner())
		if err == nil {
			t.Fatalf("ContactClients.AnswerRemote() of an unknown task error = nil, want an error")
		}

		again, err := cc.contactClient(ctx, "coder")
		if err != nil {
			t.Fatalf("ContactClients.contactClient() after the failed answer error = %v", err)
		}
		if again == first {
			t.Fatalf("ContactClients.contactClient() after a failed answer = the old client, want a new one")
		}
	})
}

func TestContactClientsLocate(t *testing.T) {
	t.Parallel()
	peers := &staticPeers{peers: []network.PeerInfo{
		{Name: "Ben", URL: "http://ben.test:4000/", Online: true},
	}}
	tests := []struct {
		name      string
		private   network.Provider
		agent     string
		url       string
		wantURL   string
		wantVia   string
		wantError string
	}{
		{name: "explicit URL without a private network", agent: "coder", url: "https://coder.test", wantURL: "https://coder.test", wantVia: ViaDirect},
		{name: "explicit URL of a peer", private: peers, agent: "ben", url: "http://ben.test:4000", wantURL: "http://ben.test:4000", wantVia: ViaPrivate},
		{name: "explicit URL elsewhere", private: peers, agent: "coder", url: "https://coder.test", wantURL: "https://coder.test", wantVia: ViaDirect},
		{name: "peer by name", private: peers, agent: "ben", wantURL: "http://ben.test:4000/", wantVia: ViaPrivate},
		{name: "peer by name in other case", private: peers, agent: "BEN", wantURL: "http://ben.test:4000/", wantVia: ViaPrivate},
		{name: "unknown peer", private: peers, agent: "nobody", wantError: `no agent named "nobody" on the private network; pass its URL with --url`},
		{name: "name without a private network", agent: "ben", wantError: "this agent has no private network; add one with `a2a-serve config set --private tailscale` (or a relay URL) and `a2a-serve restart`"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cc := newTestContactClients(t, openTestStore(t), tc.private)

			gotURL, gotVia, err := cc.Locate(context.Background(), tc.agent, tc.url)

			if tc.wantError != "" {
				if err == nil || err.Error() != tc.wantError {
					t.Fatalf("ContactClients.locate() error = %v, want %q", err, tc.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("ContactClients.locate() error = %v", err)
			}
			if gotURL != tc.wantURL || gotVia != tc.wantVia {
				t.Fatalf("ContactClients.locate() = %q, %q, want %q, %q", gotURL, gotVia, tc.wantURL, tc.wantVia)
			}
		})
	}
}

func TestContactClientsSlowContactDoesNotStallOthers(t *testing.T) {
	t.Parallel()
	hanging := make(chan struct{})
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(hanging)
		<-release
	}))
	t.Cleanup(slow.Close)
	t.Cleanup(func() { close(release) })
	remote := newFakeContact(t)
	st := openTestStore(t)
	putContact(t, st, store.Contact{Name: "slow", URL: slow.URL})
	putContact(t, st, store.Contact{Name: "coder", URL: remote.URL})
	cc := newTestContactClients(t, st, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		if _, err := cc.contactClient(ctx, "slow"); err == nil {
			t.Errorf("ContactClients.contactClient() of a hanging contact error = nil, want an error")
		}
	}()
	<-hanging

	done := make(chan error, 1)
	go func() {
		_, err := cc.contactClient(context.Background(), "coder")
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ContactClients.contactClient() while another contact hangs error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("ContactClients.contactClient() was blocked by the card fetch of another contact")
	}
}

func TestContactClientsLimitResponseSize(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		pad     int
		wantErr error
	}{
		{name: "small response", pad: 1 << 10},
		{name: "response over the limit", pad: maxContactResponse, wantErr: egress.ErrResponseTooLarge},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mux := http.NewServeMux()
			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)
			card := &a2a.AgentCard{Name: "big", Version: "1.0.0",
				SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface(srv.URL, a2a.TransportProtocolJSONRPC)}}
			mux.Handle("/.well-known/agent-card.json", a2asrv.NewStaticAgentCardHandler(card))
			mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					ID json.RawMessage `json:"id"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Errorf("decoding the JSON-RPC request: %v", err)
				}
				body := `{"jsonrpc":"2.0","id":` + string(req.ID) + `,"result":{"id":"t1","contextId":"c1","status":{"state":"TASK_STATE_COMPLETED"},"metadata":{"pad":"` +
					strings.Repeat("x", tc.pad) + `"}}}`
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Content-Length", strconv.Itoa(len(body)))
				if _, err := io.WriteString(w, body); err != nil {
					t.Logf("writing the response: %v", err)
				}
			})
			st := openTestStore(t)
			putContact(t, st, store.Contact{Name: "big", URL: srv.URL})
			cc := newTestContactClients(t, st, nil)
			tgt, err := cc.target(context.Background(), "big", auth.Owner(), 0)
			if err != nil {
				t.Fatalf("ContactClients.target() error = %v", err)
			}

			_, err = tgt.get(context.Background(), "t1")

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("target.get() error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// blockingPeers is a private network whose discoveries wait for release,
// and counts them.
type blockingPeers struct {
	network.Provider
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func newBlockingPeers() *blockingPeers {
	return &blockingPeers{entered: make(chan struct{}, 1), release: make(chan struct{})}
}

func (p *blockingPeers) Peers(ctx context.Context) ([]network.PeerInfo, error) {
	p.calls.Add(1)
	select {
	case p.entered <- struct{}{}:
	default:
	}
	select {
	case <-p.release:
		return []network.PeerInfo{{Name: "ben", URL: "http://ben.test", Online: true}}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestContactClientsDiscoverIsSingleFlight(t *testing.T) {
	t.Parallel()
	peers := newBlockingPeers()
	cc := newTestContactClients(t, nil, peers)
	results := make([][]network.PeerInfo, 8)
	var wg sync.WaitGroup
	wg.Go(func() {
		got, err := cc.Discover(context.Background())
		if err != nil {
			t.Errorf("ContactClients.Discover() error = %v", err)
		}
		results[0] = got
	})
	<-peers.entered
	for i := 1; i < len(results); i++ {
		wg.Go(func() {
			got, err := cc.Discover(context.Background())
			if err != nil {
				t.Errorf("ContactClients.Discover() error = %v", err)
			}
			results[i] = got
		})
	}
	close(peers.release)
	wg.Wait()

	if got := peers.calls.Load(); got != 1 {
		t.Fatalf("discoveries of the private network = %d, want 1", got)
	}
	for _, got := range results {
		if diff := cmp.Diff([]string{"ben"}, peerNames(got)); diff != "" {
			t.Fatalf("ContactClients.Discover() wrong result (-want +got) diff = %s", diff)
		}
	}
}

func TestContactClientsDiscoverOutlivesCanceledCaller(t *testing.T) {
	t.Parallel()
	peers := newBlockingPeers()
	cc := newTestContactClients(t, nil, peers)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := cc.Discover(ctx)
		done <- err
	}()
	<-peers.entered

	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("ContactClients.Discover() of a canceled caller error = %v, want %v", err, context.Canceled)
	}
	again := make(chan []network.PeerInfo, 1)
	go func() {
		got, err := cc.Discover(context.Background())
		if err != nil {
			t.Errorf("ContactClients.Discover() error = %v", err)
		}
		again <- got
	}()
	close(peers.release)
	got := <-again

	if diff := cmp.Diff([]string{"ben"}, peerNames(got)); diff != "" {
		t.Fatalf("ContactClients.Discover() wrong result (-want +got) diff = %s", diff)
	}
	if calls := peers.calls.Load(); calls != 1 {
		t.Fatalf("discoveries of the private network = %d, want 1", calls)
	}
}

func peerNames(peers []network.PeerInfo) []string {
	var out []string
	for _, p := range peers {
		out = append(out, p.Name)
	}
	return out
}
