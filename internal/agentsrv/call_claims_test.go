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
	"iter"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/store"
)

var (
	anna = auth.Principal{ID: "pair:anna", Role: auth.RoleAuthenticated, Identity: auth.Identity{Pairing: "anna", Hops: 1}}
	bob  = auth.Principal{ID: "pair:bob", Role: auth.RoleAuthenticated}
)

func newTestCallClaims(t *testing.T, bridge harnessBridge) *CallClaims {
	t.Helper()
	return newCallClaims(CallClaimsDeps{Store: openTestStore(t), Bridge: bridge})
}

func inputRequiredTask(id a2a.TaskID, contextID string) *a2a.Task {
	return &a2a.Task{ID: id, ContextID: contextID, Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired}}
}

func statusEvents(info a2a.TaskInfoProvider, states ...a2a.TaskState) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		for _, s := range states {
			if !yield(a2a.NewStatusUpdateEvent(info, s, nil), nil) {
				return
			}
		}
	}
}

// probeClaims calls claimsOf with a short deadline, and reports whether it
// returned in time.
func probeClaims(cl *CallClaims, handle string) (callerClaims, error, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	c, err := cl.claimsOf(ctx, handle)
	return c, err, !errors.Is(err, context.DeadlineExceeded)
}

func TestCallClaimsHandle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cl := newTestCallClaims(t, harnessBridge{})

	h, err := cl.handle(ctx, "ctx1")
	if err != nil {
		t.Fatalf("CallClaims.handle() error = %v", err)
	}
	if len(h) != handleLen {
		t.Fatalf("CallClaims.handle() = %q, want %d characters", h, handleLen)
	}
	again, err := cl.handle(ctx, "ctx1")
	if err != nil || again != h {
		t.Fatalf("CallClaims.handle() again = %q, %v, want %q", again, err, h)
	}
	other, err := cl.handle(ctx, "ctx2")
	if err != nil || other == h {
		t.Fatalf("CallClaims.handle() of another conversation = %q, %v, want a handle different from %q", other, err, h)
	}
}

func TestCallClaimsHandleSurvivesRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTestStore(t)
	h, err := newCallClaims(CallClaimsDeps{Store: st}).handle(ctx, "ctx1")
	if err != nil {
		t.Fatalf("CallClaims.handle() error = %v", err)
	}
	got, err := newCallClaims(CallClaimsDeps{Store: st}).handle(ctx, "ctx1")
	if err != nil || got != h {
		t.Fatalf("CallClaims.handle() after a restart = %q, %v, want %q", got, err, h)
	}
}

func TestCallClaimsHandleConcurrent(t *testing.T) {
	t.Parallel()
	cl := newTestCallClaims(t, harnessBridge{})
	handles := make([]string, 8)
	var wg sync.WaitGroup
	for i := range handles {
		wg.Go(func() {
			h, err := cl.handle(context.Background(), "ctx1")
			if err != nil {
				t.Errorf("CallClaims.handle() error = %v", err)
			}
			handles[i] = h
		})
	}
	wg.Wait()
	for _, h := range handles {
		if h != handles[0] {
			t.Fatalf("CallClaims.handle() concurrently = %q, want %q for one conversation", h, handles[0])
		}
	}
}

func TestCallClaimsHandlesAreUnique(t *testing.T) {
	t.Parallel()
	cl := newTestCallClaims(t, harnessBridge{})
	seen := map[string]string{}
	for i := range 50 {
		contextID := fmt.Sprintf("ctx%d", i)
		h, err := cl.handle(context.Background(), contextID)
		if err != nil {
			t.Fatalf("CallClaims.handle() error = %v", err)
		}
		if prev, ok := seen[h]; ok {
			t.Fatalf("CallClaims.handle(%q) = %q, same as the handle of %q", contextID, h, prev)
		}
		seen[h] = contextID
	}
}

func TestCallClaimsClaimsOf(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cl := newTestCallClaims(t, harnessBridge{})
	h, err := cl.handle(ctx, "ctx1")
	if err != nil {
		t.Fatalf("CallClaims.handle() error = %v", err)
	}
	turn := callerClaims{ContextID: "ctx1", User: anna}

	if _, err := cl.claimsOf(ctx, "unknown"); !errors.Is(err, errUnknownHandle) {
		t.Fatalf("CallClaims.claimsOf(unknown) error = %v, want %v", err, errUnknownHandle)
	}
	if _, err := cl.claimsOf(ctx, h); !errors.Is(err, errNoTurn) {
		t.Fatalf("CallClaims.claimsOf() before a turn error = %v, want %v", err, errNoTurn)
	}
	end := cl.beginTurn(turn)
	got, err := cl.claimsOf(ctx, h)
	if err != nil {
		t.Fatalf("CallClaims.claimsOf() during a turn error = %v", err)
	}
	if diff := cmp.Diff(turn, got); diff != "" {
		t.Fatalf("CallClaims.claimsOf() wrong result (-want +got) diff = %s", diff)
	}
	end()
	if _, err := cl.claimsOf(ctx, h); !errors.Is(err, errNoTurn) {
		t.Fatalf("CallClaims.claimsOf() after the turn error = %v, want %v", err, errNoTurn)
	}
}

func TestCallClaimsClaimsOfIsPerConversation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cl := newTestCallClaims(t, harnessBridge{})
	h1, err := cl.handle(ctx, "ctx1")
	if err != nil {
		t.Fatalf("CallClaims.handle() error = %v", err)
	}
	h2, err := cl.handle(ctx, "ctx2")
	if err != nil {
		t.Fatalf("CallClaims.handle() error = %v", err)
	}
	t.Cleanup(cl.beginTurn(callerClaims{ContextID: "ctx1", User: anna}))

	if _, err := cl.claimsOf(ctx, h2); !errors.Is(err, errNoTurn) {
		t.Fatalf("CallClaims.claimsOf() of a conversation without a turn error = %v, want %v", err, errNoTurn)
	}
	t.Cleanup(cl.beginTurn(callerClaims{ContextID: "ctx2", User: bob}))
	for _, tc := range []struct {
		handle string
		want   callerClaims
	}{
		{handle: h1, want: callerClaims{ContextID: "ctx1", User: anna}},
		{handle: h2, want: callerClaims{ContextID: "ctx2", User: bob}},
	} {
		got, err := cl.claimsOf(ctx, tc.handle)
		if err != nil {
			t.Fatalf("CallClaims.claimsOf() error = %v", err)
		}
		if diff := cmp.Diff(tc.want, got); diff != "" {
			t.Fatalf("CallClaims.claimsOf() wrong result (-want +got) diff = %s", diff)
		}
	}
}

func TestCallClaimsClaimsOfWaitsForStartingTurn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cl := newTestCallClaims(t, harnessBridge{})
	h, err := cl.handle(ctx, "ctx1")
	if err != nil {
		t.Fatalf("CallClaims.handle() error = %v", err)
	}
	turn := callerClaims{ContextID: "ctx1", User: anna}
	done := cl.startingTurn("ctx1")
	defer done()

	if _, err, returned := probeClaims(cl, h); returned {
		t.Fatalf("CallClaims.claimsOf() while a turn starts returned %v, want it to wait", err)
	}
	type result struct {
		claims callerClaims
		err    error
	}
	res := make(chan result, 1)
	go func() {
		c, err := cl.claimsOf(ctx, h)
		res <- result{c, err}
	}()
	end := cl.beginTurn(turn)
	defer end()

	select {
	case got := <-res:
		if got.err != nil {
			t.Fatalf("CallClaims.claimsOf() error = %v", got.err)
		}
		if diff := cmp.Diff(turn, got.claims); diff != "" {
			t.Fatalf("CallClaims.claimsOf() wrong result (-want +got) diff = %s", diff)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("CallClaims.claimsOf() didn't return after the turn began")
	}
}

func TestCallClaimsClaimsOfEndsWhenStartingTurnDoesNot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cl := newTestCallClaims(t, harnessBridge{})
	h, err := cl.handle(ctx, "ctx1")
	if err != nil {
		t.Fatalf("CallClaims.handle() error = %v", err)
	}
	done := cl.startingTurn("ctx1")
	errs := make(chan error, 1)
	go func() {
		_, err := cl.claimsOf(ctx, h)
		errs <- err
	}()
	done()
	done()

	select {
	case err := <-errs:
		if !errors.Is(err, errNoTurn) {
			t.Fatalf("CallClaims.claimsOf() after the starting turn ended error = %v, want %v", err, errNoTurn)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("CallClaims.claimsOf() didn't return after the starting turn ended")
	}
}

func TestCallClaimsClaimsOfStopsWithContext(t *testing.T) {
	t.Parallel()
	cl := newTestCallClaims(t, harnessBridge{})
	h, err := cl.handle(context.Background(), "ctx1")
	if err != nil {
		t.Fatalf("CallClaims.handle() error = %v", err)
	}
	t.Cleanup(cl.startingTurn("ctx1"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := cl.claimsOf(ctx, h); !errors.Is(err, context.Canceled) {
		t.Fatalf("CallClaims.claimsOf() with a canceled context error = %v, want %v", err, context.Canceled)
	}
}

func TestCallClaimsEndOnlyClearsItsOwnTurn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cl := newTestCallClaims(t, harnessBridge{})
	h, err := cl.handle(ctx, "ctx1")
	if err != nil {
		t.Fatalf("CallClaims.handle() error = %v", err)
	}
	older := callerClaims{ContextID: "ctx1", User: anna}
	newer := callerClaims{ContextID: "ctx1", User: bob}
	endOlder := cl.beginTurn(older)
	endNewer := cl.beginTurn(newer)

	endOlder()
	got, err := cl.claimsOf(ctx, h)
	if err != nil {
		t.Fatalf("CallClaims.claimsOf() after the older turn ended error = %v", err)
	}
	if diff := cmp.Diff(newer, got); diff != "" {
		t.Fatalf("CallClaims.claimsOf() after the older turn ended wrong result (-want +got) diff = %s", diff)
	}
	endNewer()
	if _, err := cl.claimsOf(ctx, h); !errors.Is(err, errNoTurn) {
		t.Fatalf("CallClaims.claimsOf() after both turns ended error = %v, want %v", err, errNoTurn)
	}
}

func TestCallClaimsDuringTurnOpensURLFromWorkingUpdate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cl := newTestCallClaims(t, harnessBridge{})
	h, err := cl.handle(ctx, "ctx1")
	if err != nil {
		t.Fatalf("CallClaims.handle() error = %v", err)
	}
	execCtx := &a2asrv.ExecutorContext{ContextID: "ctx1", TaskID: "t1"}
	want := callerClaims{ContextID: "ctx1", User: auth.Owner()}
	var probes []string
	probe := func(when string) {
		c, err, returned := probeClaims(cl, h)
		switch {
		case !returned:
			probes = append(probes, when+": waits")
		case err != nil:
			probes = append(probes, when+": "+err.Error())
		case cmp.Diff(want, c) != "":
			probes = append(probes, when+": claims "+fmt.Sprint(c))
		default:
			probes = append(probes, when+": claims")
		}
	}

	for ev, err := range cl.duringTurn(auth.WithPrincipal(ctx, auth.Owner()), execCtx, statusEvents(execCtx, a2a.TaskStateSubmitted, a2a.TaskStateWorking, a2a.TaskStateCompleted)) {
		if err != nil {
			t.Fatalf("CallClaims.duringTurn() error = %v", err)
		}
		probe(string(ev.(*a2a.TaskStatusUpdateEvent).Status.State))
	}
	probe("after")

	wantProbes := []string{
		"TASK_STATE_SUBMITTED: waits",
		"TASK_STATE_WORKING: claims",
		"TASK_STATE_COMPLETED: claims",
		"after: " + errNoTurn.Error(),
	}
	if diff := cmp.Diff(wantProbes, probes); diff != "" {
		t.Fatalf("CallClaims.claimsOf() during the turn wrong result (-want +got) diff = %s", diff)
	}
}

func TestCallClaimsDuringTurnClosesWhenEventsEndWithoutWorking(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		events func(info a2a.TaskInfoProvider) iter.Seq2[a2a.Event, error]
		stop   bool
	}{
		{name: "rejected", events: func(info a2a.TaskInfoProvider) iter.Seq2[a2a.Event, error] {
			return statusEvents(info, a2a.TaskStateRejected)
		}},
		{name: "no events", events: func(a2a.TaskInfoProvider) iter.Seq2[a2a.Event, error] {
			return statusEvents(nil)
		}},
		{name: "stopped by the caller", stop: true, events: func(info a2a.TaskInfoProvider) iter.Seq2[a2a.Event, error] {
			return statusEvents(info, a2a.TaskStateSubmitted, a2a.TaskStateWorking)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cl := newTestCallClaims(t, harnessBridge{})
			h, err := cl.handle(context.Background(), "ctx1")
			if err != nil {
				t.Fatalf("CallClaims.handle() error = %v", err)
			}
			execCtx := &a2asrv.ExecutorContext{ContextID: "ctx1", TaskID: "t1"}
			for range cl.duringTurn(auth.WithPrincipal(context.Background(), auth.Owner()), execCtx, tc.events(execCtx)) {
				if tc.stop {
					break
				}
			}
			if _, err, returned := probeClaims(cl, h); !returned || !errors.Is(err, errNoTurn) {
				t.Fatalf("CallClaims.claimsOf() after the events ended error = %v (returned %v), want %v", err, returned, errNoTurn)
			}
		})
	}
}

func TestCallClaimsPausedTurnResumesForItsUser(t *testing.T) {
	t.Parallel()
	owner := auth.Owner()
	tests := []struct {
		name     string
		answer   *a2asrv.ExecutorContext
		answerBy auth.Principal
		wantUser auth.Principal
	}{
		{
			name:     "answer to the paused task",
			answer:   &a2asrv.ExecutorContext{ContextID: "ctx1", TaskID: "t1", StoredTask: inputRequiredTask("t1", "ctx1")},
			answerBy: owner,
			wantUser: anna,
		},
		{
			name:     "different task of the conversation",
			answer:   &a2asrv.ExecutorContext{ContextID: "ctx1", TaskID: "t2", StoredTask: inputRequiredTask("t2", "ctx1")},
			answerBy: owner,
			wantUser: owner,
		},
		{
			name:     "new task",
			answer:   &a2asrv.ExecutorContext{ContextID: "ctx1", TaskID: "t3"},
			answerBy: owner,
			wantUser: owner,
		},
		{
			name:     "paused task that no longer waits",
			answer:   &a2asrv.ExecutorContext{ContextID: "ctx1", TaskID: "t1", StoredTask: &a2a.Task{ID: "t1", ContextID: "ctx1", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}}},
			answerBy: owner,
			wantUser: owner,
		},
		{
			name:     "paused task without a stored task",
			answer:   &a2asrv.ExecutorContext{ContextID: "ctx1", TaskID: "t1"},
			answerBy: owner,
			wantUser: owner,
		},
		{
			name:     "same task id in another conversation",
			answer:   &a2asrv.ExecutorContext{ContextID: "ctx2", TaskID: "t1", StoredTask: inputRequiredTask("t1", "ctx2")},
			answerBy: owner,
			wantUser: owner,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			cl := newTestCallClaims(t, harnessBridge{})
			prompt := &a2asrv.ExecutorContext{ContextID: "ctx1", TaskID: "t1"}
			for range cl.duringTurn(auth.InProcess(ctx, anna), prompt, statusEvents(prompt, a2a.TaskStateWorking, a2a.TaskStateInputRequired)) {
			}

			got := cl.turnClaims(auth.InProcess(ctx, tc.answerBy), tc.answer)

			want := callerClaims{ContextID: tc.answer.ContextID, User: tc.wantUser}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Fatalf("CallClaims.turnClaims() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestCallClaimsPausedTurnIsResumedOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cl := newTestCallClaims(t, harnessBridge{})
	prompt := &a2asrv.ExecutorContext{ContextID: "ctx1", TaskID: "t1"}
	for range cl.duringTurn(auth.InProcess(ctx, anna), prompt, statusEvents(prompt, a2a.TaskStateWorking, a2a.TaskStateInputRequired)) {
	}
	answer := &a2asrv.ExecutorContext{ContextID: "ctx1", TaskID: "t1", StoredTask: inputRequiredTask("t1", "ctx1")}

	first := cl.turnClaims(auth.InProcess(ctx, auth.Owner()), answer)
	second := cl.turnClaims(auth.InProcess(ctx, auth.Owner()), answer)

	if first.User.ID != anna.ID || second.User.ID != auth.OwnerID {
		t.Fatalf("CallClaims.turnClaims() users = %q then %q, want %q then %q", first.User.ID, second.User.ID, anna.ID, auth.OwnerID)
	}
}

func TestCallClaimsAnsweredTurnActsForPausedUser(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cl := newTestCallClaims(t, harnessBridge{})
	h, err := cl.handle(ctx, "ctx1")
	if err != nil {
		t.Fatalf("CallClaims.handle() error = %v", err)
	}
	prompt := &a2asrv.ExecutorContext{ContextID: "ctx1", TaskID: "t1"}
	for range cl.duringTurn(auth.InProcess(ctx, anna), prompt, statusEvents(prompt, a2a.TaskStateWorking, a2a.TaskStateInputRequired)) {
	}
	if _, err := cl.claimsOf(ctx, h); !errors.Is(err, errNoTurn) {
		t.Fatalf("CallClaims.claimsOf() while paused error = %v, want %v", err, errNoTurn)
	}

	answer := &a2asrv.ExecutorContext{ContextID: "ctx1", TaskID: "t1", StoredTask: inputRequiredTask("t1", "ctx1")}
	var got callerClaims
	for range cl.duringTurn(auth.InProcess(ctx, auth.Owner()), answer, func(yield func(a2a.Event, error) bool) {
		if !yield(a2a.NewStatusUpdateEvent(answer, a2a.TaskStateWorking, nil), nil) {
			return
		}
		got, err = cl.claimsOf(ctx, h)
	}) {
	}
	if err != nil {
		t.Fatalf("CallClaims.claimsOf() during the answered turn error = %v", err)
	}
	if want := (callerClaims{ContextID: "ctx1", User: anna}); cmp.Diff(want, got) != "" {
		t.Fatalf("CallClaims.claimsOf() during the answered turn wrong result (-want +got) diff = %s", cmp.Diff(want, got))
	}
}

func TestCallClaimsTurnClaimsUser(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		ctx     context.Context
		execCtx *a2asrv.ExecutorContext
		want    auth.Principal
	}{
		{
			name:    "caller principal",
			ctx:     auth.WithPrincipal(context.Background(), anna),
			execCtx: &a2asrv.ExecutorContext{ContextID: "ctx1", TaskID: "t1", User: a2asrv.NewAuthenticatedUser("other", nil)},
			want:    anna,
		},
		{
			name:    "user of the executor context",
			ctx:     context.Background(),
			execCtx: &a2asrv.ExecutorContext{ContextID: "ctx1", TaskID: "t1", User: a2asrv.NewAuthenticatedUser("pair:carol", nil)},
			want:    auth.Principal{ID: "pair:carol"},
		},
		{
			name:    "in-process principal",
			ctx:     auth.InProcess(context.Background(), auth.Owner()),
			execCtx: &a2asrv.ExecutorContext{ContextID: "ctx1", TaskID: "t1"},
			want:    auth.Owner(),
		},
		{
			name:    "nobody",
			ctx:     context.Background(),
			execCtx: &a2asrv.ExecutorContext{ContextID: "ctx1", TaskID: "t1"},
			want:    auth.Principal{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cl := newTestCallClaims(t, harnessBridge{})

			got := cl.turnClaims(tc.ctx, tc.execCtx)

			if diff := cmp.Diff(callerClaims{ContextID: "ctx1", User: tc.want}, got); diff != "" {
				t.Fatalf("CallClaims.turnClaims() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestCallClaimsAgentsURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		bridge  harnessBridge
		wantURL func(handle string) string
		wantErr bool
	}{
		{name: "socket", bridge: harnessBridge{socket: "/tmp/h.sock"}, wantURL: func(h string) string { return "http://a2a/" + h }},
		{name: "socket wins over TCP", bridge: harnessBridge{socket: "/tmp/h.sock", url: "http://127.0.0.1:9"}, wantURL: func(h string) string { return "http://a2a/" + h }},
		{name: "TCP bridge", bridge: harnessBridge{url: "http://host.docker.internal:4000", key: "k"}, wantURL: func(h string) string { return "http://host.docker.internal:4000/" + h }},
		{name: "no listener", bridge: harnessBridge{}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			cl := newTestCallClaims(t, tc.bridge)

			got, err := cl.agentsURL(ctx, "ctx1")

			if (err != nil) != tc.wantErr {
				t.Fatalf("CallClaims.agentsURL() error = %v, want error %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			h, err := cl.handle(ctx, "ctx1")
			if err != nil {
				t.Fatalf("CallClaims.handle() error = %v", err)
			}
			if want := tc.wantURL(h); got != want {
				t.Fatalf("CallClaims.agentsURL() = %q, want %q", got, want)
			}
		})
	}
}

func TestCallClaimsWithAgentsLine(t *testing.T) {
	t.Parallel()
	prompt := func() *a2a.Message { return a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hello")) }
	tests := []struct {
		name     string
		ctx      context.Context
		bridge   harnessBridge
		execCtx  *a2asrv.ExecutorContext
		wantLine bool
	}{
		{
			name:     "owner",
			ctx:      auth.WithPrincipal(context.Background(), auth.Owner()),
			bridge:   harnessBridge{socket: "/tmp/h.sock"},
			execCtx:  &a2asrv.ExecutorContext{ContextID: "ctx1", TaskID: "t1", Message: prompt()},
			wantLine: true,
		},
		{
			name:     "authenticated",
			ctx:      auth.WithPrincipal(context.Background(), bob),
			bridge:   harnessBridge{socket: "/tmp/h.sock"},
			execCtx:  &a2asrv.ExecutorContext{ContextID: "ctx1", TaskID: "t1", Message: prompt()},
			wantLine: true,
		},
		{
			name:     "follow-up to a task that doesn't wait",
			ctx:      auth.WithPrincipal(context.Background(), auth.Owner()),
			bridge:   harnessBridge{socket: "/tmp/h.sock"},
			execCtx:  &a2asrv.ExecutorContext{ContextID: "ctx1", TaskID: "t1", Message: prompt(), StoredTask: &a2a.Task{Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}}},
			wantLine: true,
		},
		{
			name:    "answer to an input-required task",
			ctx:     auth.WithPrincipal(context.Background(), auth.Owner()),
			bridge:  harnessBridge{socket: "/tmp/h.sock"},
			execCtx: &a2asrv.ExecutorContext{ContextID: "ctx1", TaskID: "t1", Message: prompt(), StoredTask: inputRequiredTask("t1", "ctx1")},
		},
		{
			name:    "anonymous",
			ctx:     auth.WithPrincipal(context.Background(), auth.Principal{ID: "telegram:9", Role: auth.RoleAnonymous}),
			bridge:  harnessBridge{socket: "/tmp/h.sock"},
			execCtx: &a2asrv.ExecutorContext{ContextID: "ctx1", TaskID: "t1", Message: prompt()},
		},
		{
			name:    "caller without a role",
			ctx:     auth.WithPrincipal(context.Background(), auth.Principal{ID: "pair:gone"}),
			bridge:  harnessBridge{socket: "/tmp/h.sock"},
			execCtx: &a2asrv.ExecutorContext{ContextID: "ctx1", TaskID: "t1", Message: prompt()},
		},
		{
			name:    "no caller",
			ctx:     context.Background(),
			bridge:  harnessBridge{socket: "/tmp/h.sock"},
			execCtx: &a2asrv.ExecutorContext{ContextID: "ctx1", TaskID: "t1", Message: prompt()},
		},
		{
			name:    "no message",
			ctx:     auth.WithPrincipal(context.Background(), auth.Owner()),
			bridge:  harnessBridge{socket: "/tmp/h.sock"},
			execCtx: &a2asrv.ExecutorContext{ContextID: "ctx1", TaskID: "t1"},
		},
		{
			name:    "harness listener not running",
			ctx:     auth.WithPrincipal(context.Background(), auth.Owner()),
			execCtx: &a2asrv.ExecutorContext{ContextID: "ctx1", TaskID: "t1", Message: prompt()},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cl := newTestCallClaims(t, tc.bridge)
			if err := cl.store.PutContact(context.Background(), store.Contact{Name: "coder", URL: "http://coder.test", Via: ViaHTTPS}); err != nil {
				t.Fatalf("store.PutContact() error = %v", err)
			}
			var origParts a2a.ContentParts
			if tc.execCtx.Message != nil {
				origParts = slices.Clone(tc.execCtx.Message.Parts)
			}

			got := cl.withAgentsLine(tc.ctx, tc.execCtx)

			if !tc.wantLine {
				if got != tc.execCtx {
					t.Fatalf("CallClaims.withAgentsLine() = %+v, want the execution context unchanged", got)
				}
				return
			}
			u, err := cl.agentsURL(context.Background(), "ctx1")
			if err != nil {
				t.Fatalf("CallClaims.agentsURL() error = %v", err)
			}
			wantLine := fmt.Sprintf("%s Agents URL for this conversation: %s (agents: %s, coder; call one with a2a send -a %s/<name>)", agentsLinePrefix, u, schedulesAgent, u)
			if len(got.Message.Parts) != 2 || got.Message.Parts[0].Text() != wantLine || got.Message.Parts[1].Text() != "hello" {
				t.Fatalf("CallClaims.withAgentsLine() parts = %v, want the line %q before the message", got.Message.Parts, wantLine)
			}
			if diff := cmp.Diff(origParts, tc.execCtx.Message.Parts); diff != "" {
				t.Fatalf("CallClaims.withAgentsLine() changed the caller's message, wrong result (-want +got) diff = %s", diff)
			}
			if got.ContextID != "ctx1" || got.TaskID != "t1" {
				t.Fatalf("CallClaims.withAgentsLine() context = %q task = %q, want ctx1 and t1", got.ContextID, got.TaskID)
			}
		})
	}
}
