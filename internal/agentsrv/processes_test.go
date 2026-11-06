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
	"iter"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/harness"
)

func TestAuthenticatedUsersRunInTheirOwnProcesses(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	owner := ts.client(ts.pair("me", auth.RoleOwner))
	anna := ts.client(ts.pair("anna", auth.RoleAuthenticated))
	ben := ts.client(ts.pair("ben", auth.RoleAuthenticated))

	annaTask := send(t, anna, userText("pid", "", ""))
	ownerPID, annaPID, benPID := resultText(send(t, owner, userText("pid", "", ""))), resultText(annaTask), resultText(send(t, ben, userText("pid", "", "")))
	if ownerPID == annaPID || ownerPID == benPID || annaPID == benPID {
		t.Fatalf("owner, anna and ben ran in %q, %q and %q, want separate processes", ownerPID, annaPID, benPID)
	}
	if got := resultText(send(t, anna, userText("pid", "", ""))); got != annaPID {
		t.Fatalf("anna's second conversation ran in %q, want %q", got, annaPID)
	}
	if got := resultText(send(t, owner, userText("pid", "", annaTask.ContextID))); got != annaPID {
		t.Fatalf("owner message in anna's conversation ran in %q, want %q", got, annaPID)
	}
}

func TestOwnerTurnInUserProcessActsAsTheUser(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	owner := ts.client(ts.pair("me", auth.RoleOwner))
	anna := ts.client(ts.pair("anna", auth.RoleAuthenticated))
	annaTask := send(t, anna, userText("hello", "", ""))

	task := send(t, owner, userText(`call schedules add mine --cron @daily --prompt "x"`, "", annaTask.ContextID))
	wantResult(t, task, a2a.TaskStateCompleted, "Scheduled mine")
	got, err := ts.srv.store.Schedule(context.Background(), "mine")
	if err != nil {
		t.Fatalf("Schedule(mine) error = %v", err)
	}
	if got.UserID != "pair:anna" {
		t.Fatalf("Schedule(mine) user = %q, want %q", got.UserID, "pair:anna")
	}
}

func TestCapToProcessUser(t *testing.T) {
	t.Parallel()
	owner := auth.Owner()
	anna := auth.Principal{ID: "pair:anna", Role: auth.RoleAuthenticated}
	ben := auth.Principal{ID: "pair:ben", Role: auth.RoleAuthenticated}
	anon := auth.Principal{ID: auth.AnonymousID, Role: auth.RoleAnonymous}
	tests := []struct {
		name    string
		turn    auth.Principal
		process auth.Principal
		want    auth.Principal
	}{
		{name: "same user", turn: anna, process: anna, want: anna},
		{name: "owner in a user's process", turn: owner, process: anna, want: anna},
		{name: "another user in a user's process", turn: ben, process: anna, want: anna},
		{name: "anonymous in a user's process", turn: anon, process: anna, want: anon},
		{name: "owner in the anonymous process", turn: owner, process: anon, want: anon},
		{name: "user in a process without a role", turn: anna, process: auth.Principal{ID: "pair:gone"}, want: auth.Principal{ID: "pair:gone"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := capToProcessUser(tc.turn, tc.process); got != tc.want {
				t.Fatalf("capToProcessUser() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestAgentsLineOncePerSession(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	owner := ts.client(ts.pair("me", auth.RoleOwner))

	first := send(t, owner, userText("line", "", ""))
	wantResult(t, first, a2a.TaskStateCompleted, "with agents line")
	wantResult(t, send(t, owner, userText("line", "", first.ContextID)), a2a.TaskStateCompleted, "without agents line")
	wantResult(t, send(t, owner, userText("call schedules list", "", first.ContextID)), a2a.TaskStateCompleted, "No schedules.")
}

func TestAgentsLineAgainAfterManyTokens(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	ts.srv.claims.linedMu.Lock()
	ts.srv.claims.lineEvery = 10
	ts.srv.claims.linedMu.Unlock()
	owner := ts.client(ts.pair("me", auth.RoleOwner))

	first := send(t, owner, userText("line", "", ""))
	wantResult(t, first, a2a.TaskStateCompleted, "with agents line")
	wantResult(t, send(t, owner, userText("line "+strings.Repeat("x", 40), "", first.ContextID)), a2a.TaskStateCompleted, "without agents line")
	wantResult(t, send(t, owner, userText("line", "", first.ContextID)), a2a.TaskStateCompleted, "with agents line")
	wantResult(t, send(t, owner, userText("line", "", first.ContextID)), a2a.TaskStateCompleted, "without agents line")
}

func TestLocalSandboxWarning(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		sandbox  string
		access   controlapi.Access
		wantWarn bool
	}{
		{name: "docker with others", sandbox: harness.SandboxDocker, access: controlapi.Access{Pairings: 1}},
		{name: "local for you only", sandbox: harness.SandboxLocal, access: controlapi.Access{Pairings: 1, OwnerPairings: 1}},
		{name: "local with a paired user", sandbox: harness.SandboxLocal, access: controlapi.Access{Pairings: 2, OwnerPairings: 1}, wantWarn: true},
		{name: "local open to others", sandbox: harness.SandboxLocal, access: controlapi.Access{Others: []string{"anyone on the public URL or in a direct chat"}}, wantWarn: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := localSandboxWarning(tc.sandbox, &tc.access); (got != "") != tc.wantWarn {
				t.Fatalf("localSandboxWarning() = %q, want a warning %v", got, tc.wantWarn)
			}
		})
	}
}

func TestSplitExecutorClosePoolsInParallel(t *testing.T) {
	t.Parallel()
	var running sync.WaitGroup
	running.Add(2)
	stop := func(context.Context) error {
		running.Done()
		done := make(chan struct{})
		go func() { running.Wait(); close(done) }()
		select {
		case <-done:
			return nil
		case <-time.After(5 * time.Second):
			return errors.New("the other pool did not stop at the same time")
		}
	}
	e := &splitExecutor{pools: map[string]*startedPool{
		anonymousPool: {stop: stop},
		usersPool:     {stop: stop},
	}}

	if err := e.close(context.Background()); err != nil {
		t.Fatalf("splitExecutor.close() error = %v", err)
	}
}

// emptyExecutor ends every turn without events, as a turn that fails before
// the agent stores a session.
type emptyExecutor struct{ a2asrv.AgentExecutor }

func (emptyExecutor) Execute(context.Context, *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(func(a2a.Event, error) bool) {}
}

func TestSplitExecutorForgetsPendingKeyAfterTurnWithoutSession(t *testing.T) {
	t.Parallel()
	e := &splitExecutor{
		main:     emptyExecutor{},
		sessions: openTestStore(t).Sessions(),
		budget:   newAnonymousBudget(),
		start: func(context.Context, string) (a2asrv.AgentExecutor, func(context.Context) error, error) {
			return emptyExecutor{}, func(context.Context) error { return nil }, nil
		},
	}
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{ID: "pair:anna", Role: auth.RoleAuthenticated})

	for range e.Execute(ctx, &a2asrv.ExecutorContext{ContextID: "c1", TaskID: "t1"}) {
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if got := len(e.pending); got != 0 {
		t.Fatalf("splitExecutor.pending has %d entries after the turn, want 0", got)
	}
}

func TestSplitExecutorStartsNoPoolAfterClose(t *testing.T) {
	t.Parallel()
	started := false
	e := &splitExecutor{
		main:     emptyExecutor{},
		sessions: openTestStore(t).Sessions(),
		budget:   newAnonymousBudget(),
		start: func(context.Context, string) (a2asrv.AgentExecutor, func(context.Context) error, error) {
			started = true
			return emptyExecutor{}, func(context.Context) error { return nil }, nil
		},
	}
	if err := e.close(context.Background()); err != nil {
		t.Fatalf("splitExecutor.close() error = %v", err)
	}

	_, err := e.pool(usersPool)

	if err == nil {
		t.Fatalf("splitExecutor.pool() after close error = nil, want error")
	}
	if started {
		t.Fatalf("splitExecutor.pool() after close started the pool, want it not started")
	}
}
