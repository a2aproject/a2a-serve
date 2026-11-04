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
	"iter"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/executor/a2aacp"
	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/auth"
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

func (e *turnExecutor) Cancel(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
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

func TestCancelYieldsCanceledBeforeTurnEnds(t *testing.T) {
	t.Parallel()
	inner := &turnExecutor{}
	exec := &observedExecutor{inner: inner, bus: newEventBus()}
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

func TestMessageJoinsOnlyItsSendersConversation(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	anna := ts.client(ts.pair("anna", auth.RoleMember))
	ben := ts.client(ts.pair("ben", auth.RoleMember))
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
