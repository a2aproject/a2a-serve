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
	"bytes"
	"context"
	"errors"
	"iter"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/integrations/executor/a2aacp"
	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/approvals"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/store"
)

func TestEventBusKeepsEventsOfSlowFollower(t *testing.T) {
	t.Parallel()
	bus := newEventBus()
	sub, unsubscribe := bus.subscribe("t1")
	defer unsubscribe()
	task := &a2a.Task{ID: "t1"}
	const n = 200
	for range n - 1 {
		bus.publish("t1", a2a.NewStatusUpdateEvent(task, a2a.TaskStateWorking, nil))
	}
	bus.publish("t1", a2a.NewStatusUpdateEvent(task, a2a.TaskStateCompleted, nil))

	var last a2a.Event
	for i := range n {
		ev, ok := sub.next(context.Background(), time.After(time.Second))
		if !ok {
			t.Fatalf("subscription.next() after %d events ok = false, want %d events", i, n)
		}
		last = ev
	}
	if got := last.(*a2a.TaskStatusUpdateEvent).Status.State; got != a2a.TaskStateCompleted {
		t.Fatalf("last event state = %v, want %v", got, a2a.TaskStateCompleted)
	}
}

// turnExecutor cancels like a2aacp: it stops the turn, which ends the
// running Execute, and only then yields the canceled event.
type turnExecutor struct {
	mu       sync.Mutex
	canceled bool
}

func (e *turnExecutor) Execute(context.Context, *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(func(a2a.Event, error) bool) {}
}

func (e *turnExecutor) Cancel(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if err := ctx.Err(); err != nil {
			yield(nil, err)
			return
		}
		e.mu.Lock()
		e.canceled = true
		e.mu.Unlock()
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCanceled, nil), nil)
	}
}

func (e *turnExecutor) turnCanceled() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.canceled
}

func openTestStore(t *testing.T) *store.Store {
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
	return st
}

func TestCancelYieldsCanceledBeforeTurnEnds(t *testing.T) {
	t.Parallel()
	inner := &turnExecutor{}
	exec := &observedExecutor{inner: inner, bus: newEventBus(), approvals: approvals.New(approvals.Deps{Records: approvals.NewRecords(approvals.RecordsDeps{Store: openTestStore(t)})})}
	execCtx := &a2asrv.ExecutorContext{TaskID: "t1", ContextID: "c1"}

	var states []a2a.TaskState
	for ev, err := range exec.Cancel(context.Background(), execCtx) {
		if err != nil {
			t.Fatalf("Cancel() error = %v", err)
		}
		if len(states) == 0 && inner.turnCanceled() {
			t.Fatalf("Cancel() first event after the turn was canceled, want it before, while the execution's queue is open")
		}
		states = append(states, ev.(*a2a.TaskStatusUpdateEvent).Status.State)
	}
	if diff := cmp.Diff([]a2a.TaskState{a2a.TaskStateCanceled}, states); diff != "" {
		t.Fatalf("Cancel() wrong states (-want +got) diff = %s", diff)
	}
	if !inner.turnCanceled() {
		t.Fatalf("Cancel() didn't cancel the turn")
	}
}

func TestCancelStopsTurnAfterConsumerStops(t *testing.T) {
	t.Parallel()
	inner := &turnExecutor{}
	exec := &observedExecutor{inner: inner, bus: newEventBus(), approvals: approvals.New(approvals.Deps{Records: approvals.NewRecords(approvals.RecordsDeps{Store: openTestStore(t)})})}
	execCtx := &a2asrv.ExecutorContext{TaskID: "t1", ContextID: "c1"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// a2asrv stops consuming at the terminal event and cancels the context.
	var states []a2a.TaskState
	for ev, err := range exec.Cancel(ctx, execCtx) {
		if err != nil {
			t.Fatalf("Cancel() error = %v", err)
		}
		states = append(states, ev.(*a2a.TaskStatusUpdateEvent).Status.State)
		cancel()
		break
	}

	if diff := cmp.Diff([]a2a.TaskState{a2a.TaskStateCanceled}, states); diff != "" {
		t.Fatalf("Cancel() wrong states (-want +got) diff = %s", diff)
	}
	if !inner.turnCanceled() {
		t.Fatalf("Cancel() didn't cancel the turn")
	}
}

// statusExecutor yields one status update.
type statusExecutor struct {
	state a2a.TaskState
	msg   *a2a.Message
}

func (e statusExecutor) Execute(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		yield(a2a.NewStatusUpdateEvent(execCtx, e.state, e.msg), nil)
	}
}

func (statusExecutor) Cancel(context.Context, *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(func(a2a.Event, error) bool) {}
}

func TestUnattendedQuestionIsAskedAfterExecutionEnds(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{channel: newFakeChannel("telegram")})
	linkOwner(t, ts, "telegram:1", "dm1")
	ctx := auth.WithPrincipal(context.Background(), auth.Owner())
	asked := send(t, ts.client(ts.pair("me", auth.RoleOwner)), userText("ask", "", ""))
	contextID := a2a.NewContextID()
	ts.srv.chatTurns.unattended.add(contextID)
	exec := &observedExecutor{
		inner:     statusExecutor{state: a2a.TaskStateInputRequired, msg: asked.Status.Message},
		bus:       newEventBus(),
		claims:    ts.srv.claims,
		approvals: ts.srv.approvals,
	}
	execCtx := &a2asrv.ExecutorContext{TaskID: a2a.NewTaskID(), ContextID: contextID}

	// a2a-go processes an event after the executor yields it. An answer sent
	// before the execution is done with the event fails as concurrent.
	var result *a2a.Task
	for ev, err := range exec.Execute(ctx, execCtx) {
		if err != nil {
			t.Fatalf("Execute() error = %v", err)
		}
		time.Sleep(200 * time.Millisecond)
		if got := ts.ch.questions(); got != 0 {
			t.Fatalf("questions sent while the execution handles the event = %d, want 0; messages:\n%s", got, ts.ch.dump())
		}
		u := ev.(*a2a.TaskStatusUpdateEvent)
		result = &a2a.Task{ID: u.TaskID, ContextID: u.ContextID, Status: u.Status}
	}
	exec.Cleanup(ctx, execCtx, result, nil)
	ts.ch.waitFor(t, "dm1", hasQuestion)
}

func TestExecutionEndWakesFollowersOfItsContext(t *testing.T) {
	t.Parallel()
	bus := newEventBus()
	exec := &observedExecutor{bus: bus, unattended: &contextSet{}}
	changed, unsubscribe := bus.subscribeContext("c1")
	defer unsubscribe()
	other, unsubscribeOther := bus.subscribeContext("c2")
	defer unsubscribeOther()

	exec.Cleanup(context.Background(), &a2asrv.ExecutorContext{TaskID: "t1", ContextID: "c1"}, nil, errors.New("the agent crashed"))

	select {
	case <-changed:
	default:
		t.Fatal("followers of the context were not woken when its execution ended")
	}
	select {
	case <-other:
		t.Fatal("followers of another context were woken")
	default:
	}
}

func TestLiveToolRulesReadCurrentConfig(t *testing.T) {
	t.Parallel()
	cfg := &agentdir.Config{}
	rules := liveToolRules(func() *agentdir.Config { return cfg })
	q := a2aacp.Question{Options: []string{a2aacp.OptionOnce, a2aacp.OptionReject}, Tool: &a2aacp.ToolCall{Kind: "read"}}

	if _, ok := rules.AutoAnswer(context.Background(), nil, q); ok {
		t.Fatalf("AutoAnswer() without rules ok = true, want false")
	}
	cfg = &agentdir.Config{Approvals: agentdir.ApprovalsConfig{Allow: []string{"read"}}}
	got, ok := rules.AutoAnswer(context.Background(), nil, q)
	if !ok || got.Option != a2aacp.OptionOnce {
		t.Fatalf("AutoAnswer() after allowing read = %+v, %v, want %q", got, ok, a2aacp.OptionOnce)
	}
}

// lockedBuffer is a log destination safe for concurrent writes.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Not parallel: it replaces the default logger, which the server logs to.
func TestTaskLifecycleIsLogged(t *testing.T) {
	var logs lockedBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	ts := startServer(t, serverOpts{})
	c := ts.client(ts.pair("me", auth.RoleOwner))

	task := send(t, c, userText("ask", "", ""))
	send(t, c, userText("approve", string(task.ID), task.ContextID))

	var got []string
	for line := range strings.Lines(logs.String()) {
		if strings.Contains(line, "task="+string(task.ID)) && strings.Contains(line, `msg="task `) {
			msg := line[strings.Index(line, `msg="`)+5:]
			got = append(got, msg[:strings.Index(msg, `"`)])
		}
	}
	want := []string{"task received", "task input-required", "task received", "task completed"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("task log wrong result (-want +got) diff = %s", diff)
	}
}

// Not parallel: it changes PATH.
func TestStartReportsOutdatedA2AOnce(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\necho '{\"version\":\"v0.1.0\"}'\n"
	if err := os.WriteFile(filepath.Join(bin, "a2a"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	var out lockedBuffer
	startServer(t, serverOpts{out: &out, setupProblems: func(context.Context) []string {
		return []string{"a2a v0.1.0 at " + filepath.Join(bin, "a2a") + " is too old"}
	}})

	got := 0
	for line := range strings.Lines(out.String()) {
		if strings.Contains(line, "a2a v0.1.0") {
			got++
		}
	}
	if got != 1 {
		t.Fatalf("lines about the outdated a2a at start = %d, want 1; output:\n%s", got, out.String())
	}
}

func TestMessageJoinsOnlyItsSendersConversation(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	anna := ts.client(ts.pair("anna", auth.RoleAuthenticated))
	ben := ts.client(ts.pair("ben", auth.RoleAuthenticated))
	owner := ts.client(ts.pair("me", auth.RoleOwner))
	contextID := send(t, anna, userText("hello", "", "")).ContextID
	tests := []struct {
		name    string
		client  *a2aclient.Client
		wantErr bool
	}{
		{name: "the conversation's user", client: anna},
		{name: "another user", client: ben, wantErr: true},
		{name: "an owner", client: owner},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.client.SendMessage(context.Background(), &a2a.SendMessageRequest{Message: userText("hello", "", contextID)})
			if (err != nil) != tc.wantErr {
				t.Fatalf("SendMessage() into anna's conversation error = %v, want error %v", err, tc.wantErr)
			}
		})
	}
}

func TestObservedExecutorCleanupForgetsUnattendedContextOfEndedTask(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		result  a2a.SendMessageResult
		err     error
		wantHas bool
	}{
		{name: "completed task", result: &a2a.Task{Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}}},
		{name: "failed task", result: &a2a.Task{Status: a2a.TaskStatus{State: a2a.TaskStateFailed}}},
		{name: "execution error", err: context.Canceled},
		{name: "task waiting for input", result: &a2a.Task{Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired}}, wantHas: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			unattended := &contextSet{}
			unattended.add("c1")
			unattended.add("c2")
			e := &observedExecutor{bus: newEventBus(), unattended: unattended}

			e.Cleanup(context.Background(), &a2asrv.ExecutorContext{ContextID: "c1", TaskID: "t1"}, tc.result, tc.err)

			if got := unattended.has("c1"); got != tc.wantHas {
				t.Fatalf("unattended.has(c1) = %v, want %v", got, tc.wantHas)
			}
			if !unattended.has("c2") {
				t.Fatalf("unattended.has(c2) = false, want true for another context")
			}
		})
	}
}
