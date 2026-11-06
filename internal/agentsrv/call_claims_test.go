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
	"slices"
	"sync"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/integrations/executor/a2aacp"
	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentsrv/agentkey"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/agentproxy"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/contacts"
	"github.com/a2aproject/a2a-serve/internal/pairings"
	"github.com/a2aproject/a2a-serve/internal/store"
)

var (
	anna = auth.Principal{ID: "pair:anna", Role: auth.RoleAuthenticated, Identity: auth.Identity{Pairing: "anna", Hops: 1}}
	bob  = auth.Principal{ID: "pair:bob", Role: auth.RoleAuthenticated}
)

func newTestCallClaims(t *testing.T, bridge harnessBridge) *CallClaims {
	t.Helper()
	st := openTestStore(t)
	return newCallClaims(CallClaimsDeps{Store: st, Bridge: bridge, Sessions: st.Sessions()})
}

func inputRequiredTask(id a2a.TaskID, contextID string) *a2a.Task {
	return &a2a.Task{ID: id, ContextID: contextID, Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired}}
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
	turn := agentproxy.Claims{ContextID: "ctx1", User: anna}

	if _, err := cl.ClaimsOf(ctx, "unknown", ""); !errors.Is(err, agentproxy.ErrUnknownHandle) {
		t.Fatalf("CallClaims.ClaimsOf(unknown) error = %v, want %v", err, agentproxy.ErrUnknownHandle)
	}
	if _, err := cl.ClaimsOf(ctx, h, ""); !errors.Is(err, agentproxy.ErrNoTurn) {
		t.Fatalf("CallClaims.ClaimsOf() before a turn error = %v, want %v", err, agentproxy.ErrNoTurn)
	}
	end := cl.beginTurn(turn)
	got, err := cl.ClaimsOf(ctx, h, "")
	if err != nil {
		t.Fatalf("CallClaims.ClaimsOf() during a turn error = %v", err)
	}
	if diff := cmp.Diff(turn, got); diff != "" {
		t.Fatalf("CallClaims.ClaimsOf() wrong result (-want +got) diff = %s", diff)
	}
	end()
	if _, err := cl.ClaimsOf(ctx, h, ""); !errors.Is(err, agentproxy.ErrNoTurn) {
		t.Fatalf("CallClaims.ClaimsOf() after the turn error = %v, want %v", err, agentproxy.ErrNoTurn)
	}
}

func TestCallClaimsClaimsOfIsPerProcess(t *testing.T) {
	t.Parallel()
	annaKey := agentkey.User(anna.ID)
	keys := map[string]string{"owner": "", "anna": annaKey}
	tests := []struct {
		name      string
		contextID string
		process   string
		wantErr   error
	}{
		{name: "owner's conversation from the owner's process", contextID: "owner"},
		{name: "anna's conversation from anna's process", contextID: "anna", process: annaKey},
		{name: "owner's conversation from anna's process", contextID: "owner", process: annaKey, wantErr: agentproxy.ErrUnknownHandle},
		{name: "anna's conversation from the owner's process", contextID: "anna", wantErr: agentproxy.ErrUnknownHandle},
		{name: "anna's conversation from another user's process", contextID: "anna", process: agentkey.User(bob.ID), wantErr: agentproxy.ErrUnknownHandle},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			st := openTestStore(t)
			cl := newCallClaims(CallClaimsDeps{Store: st, Sessions: st.Sessions(), AgentKey: func(_ context.Context, contextID string) (string, error) {
				return keys[contextID], nil
			}})
			h, err := cl.handle(ctx, tc.contextID)
			if err != nil {
				t.Fatalf("CallClaims.handle() error = %v", err)
			}
			t.Cleanup(cl.beginTurn(agentproxy.Claims{ContextID: tc.contextID, User: anna}))

			_, err = cl.ClaimsOf(ctx, h, tc.process)

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("CallClaims.ClaimsOf() error = %v, want %v", err, tc.wantErr)
			}
		})
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
	t.Cleanup(cl.beginTurn(agentproxy.Claims{ContextID: "ctx1", User: anna}))

	if _, err := cl.ClaimsOf(ctx, h2, ""); !errors.Is(err, agentproxy.ErrNoTurn) {
		t.Fatalf("CallClaims.ClaimsOf() of a conversation without a turn error = %v, want %v", err, agentproxy.ErrNoTurn)
	}
	t.Cleanup(cl.beginTurn(agentproxy.Claims{ContextID: "ctx2", User: bob}))
	for _, tc := range []struct {
		handle string
		want   agentproxy.Claims
	}{
		{handle: h1, want: agentproxy.Claims{ContextID: "ctx1", User: anna}},
		{handle: h2, want: agentproxy.Claims{ContextID: "ctx2", User: bob}},
	} {
		got, err := cl.ClaimsOf(ctx, tc.handle, "")
		if err != nil {
			t.Fatalf("CallClaims.ClaimsOf() error = %v", err)
		}
		if diff := cmp.Diff(tc.want, got); diff != "" {
			t.Fatalf("CallClaims.ClaimsOf() wrong result (-want +got) diff = %s", diff)
		}
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
	older := agentproxy.Claims{ContextID: "ctx1", User: anna}
	newer := agentproxy.Claims{ContextID: "ctx1", User: bob}
	endOlder := cl.beginTurn(older)
	endNewer := cl.beginTurn(newer)

	endOlder()
	got, err := cl.ClaimsOf(ctx, h, "")
	if err != nil {
		t.Fatalf("CallClaims.ClaimsOf() after the older turn ended error = %v", err)
	}
	if diff := cmp.Diff(newer, got); diff != "" {
		t.Fatalf("CallClaims.ClaimsOf() after the older turn ended wrong result (-want +got) diff = %s", diff)
	}
	endNewer()
	if _, err := cl.ClaimsOf(ctx, h, ""); !errors.Is(err, agentproxy.ErrNoTurn) {
		t.Fatalf("CallClaims.ClaimsOf() after both turns ended error = %v, want %v", err, agentproxy.ErrNoTurn)
	}
}

func TestCallClaimsTurnOpensURLUntilEnd(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cl := newTestCallClaims(t, harnessBridge{})
	h, err := cl.handle(ctx, "ctx1")
	if err != nil {
		t.Fatalf("CallClaims.handle() error = %v", err)
	}
	execCtx := &a2asrv.ExecutorContext{ContextID: "ctx1", TaskID: "t1"}

	end := cl.turn(auth.InProcess(ctx, anna), execCtx)
	got, err := cl.ClaimsOf(ctx, h, "")
	end()

	if err != nil {
		t.Fatalf("CallClaims.ClaimsOf() during the turn error = %v", err)
	}
	if diff := cmp.Diff(agentproxy.Claims{ContextID: "ctx1", User: anna}, got); diff != "" {
		t.Fatalf("CallClaims.ClaimsOf() wrong result (-want +got) diff = %s", diff)
	}
	if _, err := cl.ClaimsOf(ctx, h, ""); !errors.Is(err, agentproxy.ErrNoTurn) {
		t.Fatalf("CallClaims.ClaimsOf() after the turn error = %v, want %v", err, agentproxy.ErrNoTurn)
	}
}

func TestCallClaimsTurnRecordsAgentsLineWhenItBegins(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cl := newTestCallClaims(t, harnessBridge{})
	if err := cl.sessions.Put(ctx, "ctx1", a2aacp.Session{ID: "s1"}); err != nil {
		t.Fatalf("SessionStore.Put() error = %v", err)
	}
	withLine := context.WithValue(auth.InProcess(ctx, anna), agentsLineKey{}, true)

	end := cl.turn(withLine, &a2asrv.ExecutorContext{ContextID: "ctx1", TaskID: "t1"})
	defer end()

	if cl.needsAgentsLine(ctx, "ctx1") {
		t.Fatalf("CallClaims.needsAgentsLine() during the turn = true, want false")
	}
}

func TestCallClaimsTurnCapsToProcessUser(t *testing.T) {
	t.Parallel()
	anonymous := auth.Principal{ID: auth.AnonymousID, Role: auth.RoleAnonymous, Identity: auth.Identity{Hops: anna.Hops}}
	tests := []struct {
		name     string
		agentKey func(context.Context, string) (string, error)
		process  string
		want     auth.Principal
		wantErr  bool
	}{
		{name: "no processes", want: anna},
		{name: "main process", agentKey: func(context.Context, string) (string, error) { return "", nil }, want: anna},
		{name: "anonymous process", agentKey: func(context.Context, string) (string, error) { return agentkey.Anonymous, nil }, process: agentkey.Anonymous, want: anonymous},
		{name: "unknown process", agentKey: func(context.Context, string) (string, error) { return "", errors.New("boom") }, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			st := openTestStore(t)
			cl := newCallClaims(CallClaimsDeps{Store: st, Sessions: st.Sessions(), AgentKey: tc.agentKey})
			h, err := cl.handle(ctx, "ctx1")
			if err != nil {
				t.Fatalf("CallClaims.handle() error = %v", err)
			}

			end := cl.turn(auth.InProcess(ctx, anna), &a2asrv.ExecutorContext{ContextID: "ctx1", TaskID: "t1"})
			got, err := cl.ClaimsOf(ctx, h, tc.process)
			end()

			if (err != nil) != tc.wantErr {
				t.Fatalf("CallClaims.ClaimsOf() error = %v, want error %v", err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if diff := cmp.Diff(agentproxy.Claims{ContextID: "ctx1", User: tc.want}, got); diff != "" {
				t.Fatalf("CallClaims.ClaimsOf() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestTurnClaims(t *testing.T) {
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

			got := turnClaims(tc.ctx, tc.execCtx)

			if diff := cmp.Diff(agentproxy.Claims{ContextID: "ctx1", User: tc.want}, got); diff != "" {
				t.Fatalf("turnClaims() wrong result (-want +got) diff = %s", diff)
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
		{name: "TCP bridge", bridge: harnessBridge{url: "http://host.docker.internal:4000"}, wantURL: func(h string) string { return "http://host.docker.internal:4000/" + h }},
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
			if err := cl.store.PutContact(context.Background(), store.Contact{Name: "coder", URL: "http://coder.test", Via: contacts.ViaHTTPS}); err != nil {
				t.Fatalf("store.PutContact() error = %v", err)
			}
			var origParts a2a.ContentParts
			if tc.execCtx.Message != nil {
				origParts = slices.Clone(tc.execCtx.Message.Parts)
			}

			ctx, got := cl.withAgentsLine(tc.ctx, tc.execCtx)

			if hadAgentsLine(ctx) != tc.wantLine {
				t.Fatalf("CallClaims.withAgentsLine() context marked %v, want %v", hadAgentsLine(ctx), tc.wantLine)
			}
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
			wantLine := fmt.Sprintf("%s Agents URL for this conversation: %s (agents: %s, coder; call one with a2a send -a %s/<name>)", agentsLinePrefix, u, pairings.SchedulesAgent, u)
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

func TestCallClaimsForgetPrunedDropsConversationsWithoutHandle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cl := newTestCallClaims(t, harnessBridge{})
	for _, id := range []string{"kept", "pruned"} {
		if err := cl.sessions.Put(ctx, id, a2aacp.Session{ID: "s-" + id}); err != nil {
			t.Fatalf("SessionStore.Put() error = %v", err)
		}
		cl.markLined(ctx, id)
	}
	if _, err := cl.handle(ctx, "kept"); err != nil {
		t.Fatalf("CallClaims.handle() error = %v", err)
	}

	cl.forgetPruned(ctx)

	if cl.needsAgentsLine(ctx, "kept") {
		t.Errorf("CallClaims.needsAgentsLine(kept) = true, want false")
	}
	if !cl.needsAgentsLine(ctx, "pruned") {
		t.Errorf("CallClaims.needsAgentsLine(pruned) = false, want true")
	}
}
