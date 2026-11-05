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

package agentsrv

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aext/structuredinput"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/a2aproject/a2a-serve/internal/a2atext"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/approvals"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/channel"
	"github.com/a2aproject/a2a-serve/internal/network"
	"github.com/a2aproject/a2a-serve/internal/pairapi"
	"github.com/a2aproject/a2a-serve/internal/store"
)

var watchTimings = Timings{WatchInterval: time.Millisecond, WatchMaxInterval: 4 * time.Millisecond}

type watchesFixture struct {
	ws      *Watches
	clients *ContactClients
	store   *store.Store
	chat    *fakeQuestionChat
	turns   *fakeTurnInjector
	remote  *fakeContact
}

func newWatchesFixture(t *testing.T) *watchesFixture {
	t.Helper()
	return newWatchesFixtureWith(t, watchTimings)
}

func newWatchesFixtureWith(t *testing.T, timings Timings) *watchesFixture {
	t.Helper()
	return newWatchesFixtureNet(t, timings, nil)
}

// newWatchesFixtureNet makes a fixture whose principals on the private
// network net refreshes.
func newWatchesFixtureNet(t *testing.T, timings Timings, net network.Provider) *watchesFixture {
	t.Helper()
	st := openTestStore(t)
	remote := newFakeContact(t)
	putContact(t, st, store.Contact{Name: "coder", URL: remote.URL})
	clients := newTestContactClients(t, st, nil)
	chain := &auth.Chain{Store: st, Net: net, Policy: func() auth.Policy { return auth.Policy{} }}
	f := &watchesFixture{store: st, clients: clients, chat: &fakeQuestionChat{}, turns: newFakeTurnInjector(), remote: remote}
	ctx, cancel := context.WithCancel(context.Background())
	f.ws = newWatches(ctx, WatchesDeps{
		Store:   st,
		Clients: clients,
		Records: approvals.NewRecords(approvals.RecordsDeps{Store: st, Chain: chain, BaseURL: func() string { return "http://assistant.test" }}),
		Chat:    f.chat,
		Turns:   f.turns,
		Chain:   chain,
		Timings: timings.withDefaults(),
	})
	t.Cleanup(func() {
		cancel()
		f.waitIdle(t)
	})
	return f
}

func (f *watchesFixture) waitIdle(t *testing.T) {
	t.Helper()
	waitFor(t, "the watches to stop", func() bool {
		f.ws.mu.Lock()
		defer f.ws.mu.Unlock()
		return len(f.ws.watching) == 0
	})
}

func (f *watchesFixture) addWatch(t *testing.T, task *a2a.Task, user string) store.Watch {
	t.Helper()
	return f.addWatchAs(t, task, user, "")
}

func (f *watchesFixture) addWatchAs(t *testing.T, task *a2a.Task, user, identity string) store.Watch {
	t.Helper()
	w := store.Watch{Contact: "coder", TaskID: string(task.ID), RemoteContextID: task.ContextID, OriginContextID: "origin", OnBehalfOf: user, Identity: identity}
	if err := f.store.PutWatch(context.Background(), w); err != nil {
		t.Fatalf("store.PutWatch() error = %v", err)
	}
	return w
}

func (f *watchesFixture) wantWatch(t *testing.T) {
	t.Helper()
	got, err := f.store.Watches(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("store.Watches() = %+v, %v, want the watch kept", got, err)
	}
}

func (f *watchesFixture) nextTurn(t *testing.T) injectedTurn {
	t.Helper()
	select {
	case turn := <-f.turns.turns:
		return turn
	case <-time.After(10 * time.Second):
		t.Fatalf("no turn was injected")
		return injectedTurn{}
	}
}

func (f *watchesFixture) wantNoWatches(t *testing.T) {
	t.Helper()
	got, err := f.store.Watches(context.Background())
	if err != nil {
		t.Fatalf("store.Watches() error = %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("store.Watches() = %+v, want none", got)
	}
}

func (f *watchesFixture) waitPolls(t *testing.T, n int64) {
	t.Helper()
	want := f.remote.gets.Load() + n
	waitFor(t, "more polls of the watched task", func() bool { return f.remote.gets.Load() >= want })
}

func permissionQuestion(t *testing.T, title string) *approvals.Question {
	t.Helper()
	req := permissionRequest()
	req.Schema.Title = title
	ev, err := structuredinput.NewRequestEvent(&a2a.Task{ID: "t1", ContextID: "remote"}, req, a2a.NewTextPart("Run git push?"))
	if err != nil {
		t.Fatalf("structuredinput.NewRequestEvent() error = %v", err)
	}
	q := approvals.FromStatus(ev.Status)
	if q == nil {
		t.Fatalf("approvals.FromStatus() = nil, want a permission question")
	}
	return q
}

func TestWatchesWatchPostsResultAndEnds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		prompt string
		want   func(id a2a.TaskID) string
	}{
		{name: "completed", prompt: promptDone, want: func(id a2a.TaskID) string {
			return fmt.Sprintf("[coder finished task %s: completed]\n%s", id, fakeContactResult)
		}},
		{name: "failed", prompt: promptFail, want: func(id a2a.TaskID) string {
			return fmt.Sprintf("[coder finished task %s: failed]\nboom", id)
		}},
		{name: "plain question", prompt: promptQuestion, want: func(id a2a.TaskID) string {
			return fmt.Sprintf("[coder asks, on task %s]\nWhich branch?\nAnswer with: a2a send -a <agents URL>/coder --task-id %s \"...\"", id, id)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newWatchesFixture(t)
			task := f.remote.startTask(t, tc.prompt)
			w := f.addWatch(t, task, auth.OwnerID)

			f.ws.watch(w)

			got := f.nextTurn(t)
			want := injectedTurn{ContextID: "origin", User: auth.Owner(), Text: tc.want(task.ID)}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Fatalf("injected turn wrong result (-want +got) diff = %s", diff)
			}
			f.wantNoWatches(t)
		})
	}
}

func TestWatchesWatchWaitsForTaskToFinish(t *testing.T) {
	t.Parallel()
	f := newWatchesFixture(t)
	task := f.remote.startTask(t, promptHold)
	f.ws.watch(f.addWatch(t, task, auth.OwnerID))

	f.waitPolls(t, 3)
	if n := len(f.turns.turns); n != 0 {
		t.Fatalf("turns injected while the task runs = %d, want 0", n)
	}
	if got, err := f.store.Watches(context.Background()); err != nil || len(got) != 1 {
		t.Fatalf("store.Watches() while the task runs = %+v, %v, want the watch", got, err)
	}
	f.remote.releaseHeld()

	got := f.nextTurn(t)
	if want := fmt.Sprintf("[coder finished task %s: completed]\n%s", task.ID, fakeContactResult); got.Text != want {
		t.Fatalf("injected text = %q, want %q", got.Text, want)
	}
	f.wantNoWatches(t)
}

func TestWatchesWatchPostsOnceForTheSameTask(t *testing.T) {
	t.Parallel()
	f := newWatchesFixture(t)
	task := f.remote.startTask(t, promptHold)
	w := f.addWatch(t, task, auth.OwnerID)
	f.ws.watch(w)
	f.ws.watch(w)
	f.remote.releaseHeld()

	f.nextTurn(t)
	f.waitIdle(t)

	if n := len(f.turns.turns); n != 0 {
		t.Fatalf("further turns injected for the same task = %d, want 0", n)
	}
}

func TestWatchesWatchKeepsWatchUntilResultIsAccepted(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		err      error
		wantKept bool
	}{
		{name: "accepted"},
		{name: "refused", err: errors.New("the agent is not running"), wantKept: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newWatchesFixture(t)
			f.turns.hold = true
			task := f.remote.startTask(t, promptDone)
			w := f.addWatch(t, task, auth.OwnerID)
			f.ws.watch(w)
			f.nextTurn(t)

			f.ws.watch(w)
			if got, err := f.store.Watches(context.Background()); err != nil || len(got) != 1 {
				t.Fatalf("store.Watches() before the result is accepted = %+v, %v, want the watch", got, err)
			}
			(<-f.turns.accepted)(tc.err)

			f.waitIdle(t)
			got, err := f.store.Watches(context.Background())
			if err != nil {
				t.Fatalf("store.Watches() error = %v", err)
			}
			if kept := len(got) == 1; kept != tc.wantKept {
				t.Fatalf("watch kept = %v, want %v", kept, tc.wantKept)
			}
			if n := len(f.turns.turns); n != 0 {
				t.Fatalf("further turns injected for the same task = %d, want 0", n)
			}
		})
	}
}

func TestWatchesWatchPostsAsItsUser(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		user     string
		pairing  string
		want     auth.Principal
		wantPost bool
	}{
		{
			name: "owner", user: auth.OwnerID, wantPost: true,
			want: auth.Owner(),
		},
		{
			name: "member with the role they have now", user: "pair:anna", pairing: "anna", wantPost: true,
			want: auth.Principal{ID: "pair:anna", Role: auth.RoleAuthenticated, Identity: auth.Identity{Pairing: "anna", PairingRole: auth.RoleAuthenticated}},
		},
		{
			name: "member whose pairing was removed", user: "pair:gone",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newWatchesFixture(t)
			if tc.pairing != "" {
				if _, err := f.store.EnsurePairing(context.Background(), store.Pairing{Name: tc.pairing, Kind: pairapi.PairingPerson, Role: auth.RoleAuthenticated}); err != nil {
					t.Fatalf("store.EnsurePairing() error = %v", err)
				}
			}
			task := f.remote.startTask(t, promptDone)
			f.ws.watch(f.addWatch(t, task, tc.user))

			if !tc.wantPost {
				waitFor(t, "the watch to end", func() bool {
					got, err := f.store.Watches(context.Background())
					return err == nil && len(got) == 0
				})
				f.waitIdle(t)
				if n := len(f.turns.turns); n != 0 {
					t.Fatalf("turns injected for a user who can't call this agent = %d, want 0", n)
				}
				return
			}
			got := f.nextTurn(t)
			if diff := cmp.Diff(tc.want, got.User); diff != "" {
				t.Fatalf("injected turn user wrong result (-want +got) diff = %s", diff)
			}
			f.wantNoWatches(t)
		})
	}
}

// flakyNet is a private network whose lookups of machines fail while it
// is down.
type flakyNet struct {
	network.Provider
	down    atomic.Bool
	lookups atomic.Int64
}

func (n *flakyNet) Name() string { return "flaky" }

func (n *flakyNet) Member(_ context.Context, node string) (*network.Peer, error) {
	n.lookups.Add(1)
	if n.down.Load() {
		return nil, errors.New("tailscaled is not running")
	}
	return &network.Peer{Node: node}, nil
}

func TestWatchesWatchKeepsResultWhileItsUserCantBeResolved(t *testing.T) {
	t.Parallel()
	net := &flakyNet{}
	net.down.Store(true)
	f := newWatchesFixtureNet(t, watchTimings, net)
	task := f.remote.startTask(t, promptDone)
	f.ws.watch(f.addWatchAs(t, task, auth.OwnerID, `{"network":"private","provider":"flaky","node":"laptop"}`))

	waitFor(t, "more lookups of the user's machine", func() bool { return net.lookups.Load() >= 5 })
	f.wantWatch(t)
	if n := len(f.turns.turns); n != 0 {
		t.Fatalf("turns injected while the user can't be resolved = %d, want 0", n)
	}
	net.down.Store(false)

	got := f.nextTurn(t)
	if want := fmt.Sprintf("[coder finished task %s: completed]\n%s", task.ID, fakeContactResult); got.Text != want {
		t.Fatalf("injected text = %q, want %q", got.Text, want)
	}
	f.wantNoWatches(t)
}

func TestWatchesWatchOfUnreadableUser(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		timings  Timings
		wantKept bool
	}{
		{name: "kept while young", timings: watchTimings, wantKept: true},
		{name: "dropped once too old", timings: Timings{WatchInterval: time.Millisecond, WatchMaxInterval: 4 * time.Millisecond, WatchMaxAge: 50 * time.Millisecond}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newWatchesFixtureWith(t, tc.timings)
			task := f.remote.startTask(t, promptDone)
			f.ws.watch(f.addWatchAs(t, task, auth.OwnerID, "{"))

			if tc.wantKept {
				f.waitPolls(t, 5)
				f.wantWatch(t)
			} else {
				waitFor(t, "the watch to end", func() bool {
					got, err := f.store.Watches(context.Background())
					return err == nil && len(got) == 0
				})
				f.waitIdle(t)
			}
			if n := len(f.turns.turns); n != 0 {
				t.Fatalf("turns injected for a user who can't be resolved = %d, want 0", n)
			}
		})
	}
}

func TestWatchesWatchFollowsUpOnTaskContinuedWhileResultWaits(t *testing.T) {
	t.Parallel()
	f := newWatchesFixture(t)
	f.turns.hold = true
	task := f.remote.startTask(t, promptQuestion)
	f.ws.watch(f.addWatch(t, task, auth.OwnerID))
	if got := f.nextTurn(t); !strings.HasPrefix(got.Text, "[coder asks, on task") {
		t.Fatalf("injected text = %q, want the question", got.Text)
	}

	answer := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("main"))
	answer.TaskID, answer.ContextID = task.ID, task.ContextID
	if _, err := f.remote.Handler.SendMessage(context.Background(), &a2a.SendMessageRequest{Message: answer}); err != nil {
		t.Fatalf("SendMessage() of the answer error = %v", err)
	}
	f.ws.watch(f.addWatch(t, task, auth.OwnerID))
	(<-f.turns.accepted)(nil)

	got := f.nextTurn(t)
	if want := fmt.Sprintf("[coder finished task %s: completed]\n%s", task.ID, fakeContactAnswer); got.Text != want {
		t.Fatalf("injected text after the follow-up = %q, want %q", got.Text, want)
	}
	(<-f.turns.accepted)(nil)
	f.waitIdle(t)
	f.wantNoWatches(t)
}

func TestWatchesWatchStopsAtItsLimits(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		prompt     string
		timings    Timings
		wantReason string
	}{
		{
			name: "no state change", prompt: promptHold, wantReason: "it has been working for 50ms",
			timings: Timings{WatchInterval: time.Millisecond, WatchMaxInterval: 4 * time.Millisecond, WatchStale: 50 * time.Millisecond},
		},
		{
			name: "too long in total, also while waiting for the user", prompt: promptAsk, wantReason: "it hasn't finished after 50ms",
			timings: Timings{WatchInterval: time.Millisecond, WatchMaxInterval: 4 * time.Millisecond, WatchMaxAge: 50 * time.Millisecond},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newWatchesFixtureWith(t, tc.timings)
			task := f.remote.startTask(t, tc.prompt)
			f.ws.watch(f.addWatch(t, task, auth.OwnerID))

			got := f.nextTurn(t)
			if want := fmt.Sprintf("[coder: stopped watching task %s: %s]", task.ID, tc.wantReason); got.Text != want {
				t.Fatalf("injected text = %q, want %q", got.Text, want)
			}
			f.wantNoWatches(t)
		})
	}
}

func TestWatchesWatchOfTaskWaitingForTheUserIsNotStale(t *testing.T) {
	t.Parallel()
	f := newWatchesFixtureWith(t, Timings{WatchInterval: time.Millisecond, WatchMaxInterval: 4 * time.Millisecond, WatchStale: 20 * time.Millisecond})
	if err := f.store.SetContext(context.Background(), "fake", "dm1", "origin"); err != nil {
		t.Fatalf("store.SetContext() error = %v", err)
	}
	task := f.remote.startTask(t, promptAsk)
	f.ws.watch(f.addWatch(t, task, auth.OwnerID))

	waitFor(t, "the question to be asked", func() bool { return len(f.chat.questions()) > 0 })
	time.Sleep(100 * time.Millisecond)
	f.waitPolls(t, 3)

	f.wantWatch(t)
	if n := len(f.turns.turns); n != 0 {
		t.Fatalf("turns injected while the task waits for the user = %d, want 0", n)
	}
}

func TestWatchesWatchOfRemovedContactEnds(t *testing.T) {
	t.Parallel()
	f := newWatchesFixture(t)
	w := store.Watch{Contact: "gone", TaskID: "t1", OriginContextID: "origin", OnBehalfOf: auth.OwnerID}
	if err := f.store.PutWatch(context.Background(), w); err != nil {
		t.Fatalf("store.PutWatch() error = %v", err)
	}

	f.ws.watch(w)

	waitFor(t, "the watch to end", func() bool {
		got, err := f.store.Watches(context.Background())
		return err == nil && len(got) == 0
	})
	f.waitIdle(t)
	if n := len(f.turns.turns); n != 0 {
		t.Fatalf("turns injected for a removed contact = %d, want 0", n)
	}
}

func TestWatchesWatchKeepsPollingWhileContactIsUnreachable(t *testing.T) {
	t.Parallel()
	f := newWatchesFixture(t)
	down := newStatusContact(t, http.StatusServiceUnavailable)
	putContact(t, f.store, store.Contact{Name: "coder", URL: down.URL})
	w := f.addWatch(t, &a2a.Task{ID: "t1", ContextID: "remote"}, auth.OwnerID)

	f.ws.watch(w)
	waitFor(t, "the failure to be recorded", func() bool {
		got, err := f.store.Watches(context.Background())
		return err == nil && len(got) == 1 && !got[0].FailingSince.IsZero()
	})
	waitFor(t, "more polls of the unreachable contact", func() bool { return down.requests.Load() >= 6 })

	got, err := f.store.Watches(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("store.Watches() = %+v, %v, want the watch kept", got, err)
	}
	if n := len(f.turns.turns); n != 0 {
		t.Fatalf("turns injected for an unreachable contact = %d, want 0", n)
	}
}

func TestWatchesWatchStopsOnPermanentErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		status      int
		unknownTask bool
		wantReason  string
	}{
		{name: "task unknown to the agent", unknownTask: true, wantReason: "doesn't know the task"},
		{name: "card not found", status: http.StatusNotFound, wantReason: "doesn't know the task"},
		{name: "card unauthorized", status: http.StatusUnauthorized, wantReason: "refused access"},
		{name: "card forbidden", status: http.StatusForbidden, wantReason: "refused access"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newWatchesFixture(t)
			if !tc.unknownTask {
				broken := newStatusContact(t, tc.status)
				putContact(t, f.store, store.Contact{Name: "coder", URL: broken.URL})
			}
			w := f.addWatch(t, &a2a.Task{ID: "missing", ContextID: "remote"}, auth.OwnerID)

			f.ws.watch(w)

			got := f.nextTurn(t)
			if !strings.HasPrefix(got.Text, "[coder: stopped watching task missing: ") || !strings.Contains(got.Text, tc.wantReason) {
				t.Fatalf("injected text = %q, want a note that the watch stopped: %s", got.Text, tc.wantReason)
			}
			f.wantNoWatches(t)
			f.waitIdle(t)
		})
	}
}

func TestWatchesWatchStopsOnPermanentStatusOfTheTaskCall(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusNotFound, http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			f := newWatchesFixture(t)
			broken := newStatusContact(t, status)
			putContact(t, f.store, store.Contact{Name: "coder", URL: broken.URL})
			w := f.addWatch(t, &a2a.Task{ID: "t1", ContextID: "remote"}, auth.OwnerID)

			f.ws.watch(w)

			if got := f.nextTurn(t); !strings.HasPrefix(got.Text, "[coder: stopped watching task t1: ") {
				t.Fatalf("injected text = %q, want a note that the watch stopped", got.Text)
			}
			f.wantNoWatches(t)
		})
	}
}

func TestWatchesWatchAsksForNoHistory(t *testing.T) {
	t.Parallel()
	f := newWatchesFixture(t)
	task := f.remote.startTask(t, promptHold)
	f.ws.watch(f.addWatch(t, task, auth.OwnerID))

	f.waitPolls(t, 3)

	for _, got := range f.remote.getHistoryLengths() {
		if got != 0 {
			t.Fatalf("history length of a poll = %d, want 0", got)
		}
	}
}

func TestWatchesPollKeepsClientOnTransientError(t *testing.T) {
	t.Parallel()
	f := newWatchesFixture(t)
	down := newStatusContact(t, http.StatusServiceUnavailable)
	contact := store.Contact{Name: "coder", URL: down.URL, Via: ViaHTTPS}
	putContact(t, f.store, contact)
	built, err := f.clients.contactClient(context.Background(), "coder")
	if err != nil {
		t.Fatalf("ContactClients.contactClient() error = %v", err)
	}
	w := f.addWatch(t, &a2a.Task{ID: "t1", ContextID: "remote"}, auth.OwnerID)

	if _, err := f.ws.poll(context.Background(), w, auth.Owner()); err == nil {
		t.Fatalf("Watches.poll() of a failing contact error = nil, want an error")
	}

	if got := f.clients.cachedClient("coder", contact); got != built {
		t.Fatalf("Watches.poll() left the cached client %p, want the client %p kept", got, built)
	}
}

func TestWatchesResume(t *testing.T) {
	t.Parallel()
	f := newWatchesFixture(t)
	task := f.remote.startTask(t, promptDone)
	f.addWatch(t, task, auth.OwnerID)

	f.ws.resume(context.Background())

	got := f.nextTurn(t)
	if want := fmt.Sprintf("[coder finished task %s: completed]\n%s", task.ID, fakeContactResult); got.Text != want {
		t.Fatalf("injected text = %q, want %q", got.Text, want)
	}
	f.wantNoWatches(t)
}

func TestWatchesRelay(t *testing.T) {
	t.Parallel()
	task := &a2a.Task{ID: "t1", ContextID: "remote"}
	tests := []struct {
		name      string
		setup     func(t *testing.T, st *store.Store)
		ownerChat *approvals.Chat
		user      auth.Principal
		title     string
		wantAsked []askedQuestion
		wantUser  string
	}{
		{
			name: "conversation chat",
			setup: func(t *testing.T, st *store.Store) {
				if err := st.SetContext(context.Background(), "fake", "dm1", "origin"); err != nil {
					t.Fatalf("store.SetContext() error = %v", err)
				}
			},
			ownerChat: &approvals.Chat{Channel: "owner", Conv: "oc"},
			user:      anna,
			title:     "git push",
			wantAsked: []askedQuestion{{To: approvals.Chat{Channel: "fake", Conv: "dm1"}, Header: "coder wants to run git push"}},
			wantUser:  "pair:anna",
		},
		{
			name: "no title",
			setup: func(t *testing.T, st *store.Store) {
				if err := st.SetContext(context.Background(), "fake", "dm1", "origin"); err != nil {
					t.Fatalf("store.SetContext() error = %v", err)
				}
			},
			user:      anna,
			wantAsked: []askedQuestion{{To: approvals.Chat{Channel: "fake", Conv: "dm1"}, Header: "coder wants to"}},
			wantUser:  "pair:anna",
		},
		{
			name: "direct chat of the user",
			setup: func(t *testing.T, st *store.Store) {
				linkChat(t, st, "anna", "telegram", "telegram:2", "dm2")
			},
			ownerChat: &approvals.Chat{Channel: "owner", Conv: "oc"},
			user:      anna,
			title:     "git push",
			wantAsked: []askedQuestion{{To: approvals.Chat{Channel: "telegram", Conv: "dm2"}, Header: "coder wants to run git push"}},
			wantUser:  "pair:anna",
		},
		{
			name: "direct chat of the owner",
			setup: func(t *testing.T, st *store.Store) {
				linkChat(t, st, auth.LocalPairing, "telegram", "telegram:1", "dm1")
			},
			user:      auth.Owner(),
			title:     "git push",
			wantAsked: []askedQuestion{{To: approvals.Chat{Channel: "telegram", Conv: "dm1"}, Header: "coder wants to run git push"}},
			wantUser:  auth.OwnerID,
		},
		{
			name:      "owner chat when the user has none",
			ownerChat: &approvals.Chat{Channel: "owner", Conv: "oc"},
			user:      anna,
			title:     "git push",
			wantAsked: []askedQuestion{{To: approvals.Chat{Channel: "owner", Conv: "oc"}, Header: "coder wants to run git push for anna"}},
			wantUser:  "pair:anna",
		},
		{
			name:      "user without an account goes to the owner chat as the owner",
			ownerChat: &approvals.Chat{Channel: "owner", Conv: "oc"},
			user:      auth.Principal{},
			title:     "git push",
			wantAsked: []askedQuestion{{To: approvals.Chat{Channel: "owner", Conv: "oc"}, Header: "coder wants to run git push for owner"}},
			wantUser:  auth.OwnerID,
		},
		{
			name:     "no chat at all",
			user:     anna,
			title:    "git push",
			wantUser: "pair:anna",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newWatchesFixture(t)
			if tc.setup != nil {
				tc.setup(t, f.store)
			}
			if tc.ownerChat != nil {
				f.chat.owner, f.chat.hasOwn = *tc.ownerChat, true
			}
			ctx := context.Background()

			f.ws.relay(ctx, callerClaims{ContextID: "origin", User: tc.user}, "coder", task, permissionQuestion(t, tc.title))
			f.ws.relay(ctx, callerClaims{ContextID: "origin", User: tc.user}, "coder", task, permissionQuestion(t, tc.title))

			got := f.chat.questions()
			if diff := cmp.Diff(tc.wantAsked, got, cmpopts.IgnoreFields(askedQuestion{}, "Rec")); diff != "" {
				t.Fatalf("Watches.relay() questions asked wrong result (-want +got) diff = %s", diff)
			}
			for _, a := range got {
				want := store.Question{TaskID: "t1", ContextID: "remote", Contact: "coder", HitlID: "tool1", User: tc.wantUser}
				if diff := cmp.Diff(want, a.Rec, cmpopts.IgnoreFields(store.Question{}, "ID", "Token", "Data", "Created")); diff != "" {
					t.Fatalf("Watches.relay() question record wrong result (-want +got) diff = %s", diff)
				}
				stored, err := f.store.QuestionByID(ctx, a.Rec.ID)
				if err != nil {
					t.Fatalf("store.QuestionByID() error = %v", err)
				}
				if stored.Contact != "coder" || stored.TaskID != "t1" || stored.User != tc.wantUser {
					t.Fatalf("stored question = %+v, want the remote question of %s", stored, tc.wantUser)
				}
			}
		})
	}
}

func TestWatchesWatchRelaysPermissionQuestionOnce(t *testing.T) {
	t.Parallel()
	f := newWatchesFixture(t)
	if err := f.store.SetContext(context.Background(), "fake", "dm1", "origin"); err != nil {
		t.Fatalf("store.SetContext() error = %v", err)
	}
	task := f.remote.startTask(t, promptAsk)
	f.ws.watch(f.addWatch(t, task, auth.OwnerID))

	waitFor(t, "the question to be asked", func() bool { return len(f.chat.questions()) > 0 })
	f.waitPolls(t, 3)

	asked := f.chat.questions()
	if len(asked) != 1 {
		t.Fatalf("questions asked while the task waits = %d, want 1", len(asked))
	}
	if want := (approvals.Chat{Channel: "fake", Conv: channel.ConvKey("dm1")}); asked[0].To != want {
		t.Fatalf("question asked in %+v, want %+v", asked[0].To, want)
	}
	if n := len(f.turns.turns); n != 0 {
		t.Fatalf("turns injected while the task waits for an answer = %d, want 0", n)
	}
	got, err := f.store.Watches(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("store.Watches() while the question is open = %+v, %v, want the watch kept", got, err)
	}

	rec := asked[0].Rec
	q := approvals.FromRecord(rec)
	if err := f.clients.AnswerRemote(context.Background(), rec, q, q.Options[0], auth.Owner()); err != nil {
		t.Fatalf("ContactClients.AnswerRemote() error = %v", err)
	}
	turn := f.nextTurn(t)
	if want := fmt.Sprintf("[coder finished task %s: completed]\n%s", task.ID, fakeContactAnswer); turn.Text != want {
		t.Fatalf("injected text = %q, want %q", turn.Text, want)
	}
	f.wantNoWatches(t)
}

func linkChat(t *testing.T, st *store.Store, pairing, channelName, identity, conv string) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.EnsurePairing(ctx, store.Pairing{Name: pairing, Kind: pairapi.PairingPerson, Role: auth.RoleAuthenticated}); err != nil {
		t.Fatalf("store.EnsurePairing() error = %v", err)
	}
	if err := st.PutLink(ctx, store.Link{Channel: channelName, Identity: identity, Pairing: pairing, Conv: conv}, pairapi.PairingPerson); err != nil {
		t.Fatalf("store.PutLink() error = %v", err)
	}
}

func TestResultText(t *testing.T) {
	t.Parallel()
	status := func(text string) a2a.TaskStatus {
		return a2a.TaskStatus{Message: a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(text))}
	}
	tests := []struct {
		name string
		task *a2a.Task
		want string
	}{
		{name: "one artifact", task: &a2a.Task{Artifacts: []*a2a.Artifact{{Parts: a2a.ContentParts{a2a.NewTextPart("a"), a2a.NewTextPart("b")}}}}, want: "ab"},
		{name: "artifacts", task: &a2a.Task{Artifacts: []*a2a.Artifact{{Parts: a2a.ContentParts{a2a.NewTextPart("a")}}, {Parts: a2a.ContentParts{a2a.NewTextPart("b")}}}}, want: "a\n\nb"},
		{name: "artifact without text", task: &a2a.Task{Artifacts: []*a2a.Artifact{{}}, Status: status("fallback")}, want: "fallback"},
		{name: "artifacts win over the status", task: &a2a.Task{Artifacts: []*a2a.Artifact{{Parts: a2a.ContentParts{a2a.NewTextPart("a")}}}, Status: status("fallback")}, want: "a"},
		{name: "status message", task: &a2a.Task{Status: status("boom")}, want: "boom"},
		{name: "nothing", task: &a2a.Task{}, want: a2atext.Message(nil)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := resultText(tc.task); got != tc.want {
				t.Fatalf("resultText() = %q, want %q", got, tc.want)
			}
		})
	}
}
