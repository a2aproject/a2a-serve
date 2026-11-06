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
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// startThroughProxy starts a task that runs prompt at coder in the turn of
// task taskID of conversation contextID, without waiting for it.
func (f *proxyFixture) startThroughProxy(t *testing.T, srvURL, contextID, taskID, prompt string) a2a.TaskID {
	t.Helper()
	end := f.claims.beginTurn(Claims{ContextID: contextID, TaskID: taskID, User: auth.Owner()})
	defer end()
	client := proxyClient(t, srvURL+"/"+f.handle(t, contextID)+"/coder")
	res, err := client.SendMessage(context.Background(), &a2a.SendMessageRequest{
		Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(prompt)),
		Config:  &a2a.SendMessageConfig{ReturnImmediately: true},
	})
	if err != nil {
		t.Fatalf("SendMessage(%q) through the agents URL error = %v", prompt, err)
	}
	task, ok := res.(*a2a.Task)
	if !ok {
		t.Fatalf("SendMessage(%q) = %T, want a task", prompt, res)
	}
	return task.ID
}

func TestWatchesCancelCallsCancelsTheTasksOfTheTurn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newProxyFixture(t)
	srv := f.serve(t, "")
	earlier := f.startThroughProxy(t, srv.URL, "c1", "turn1", promptHold)
	held := f.startThroughProxy(t, srv.URL, "c1", "turn2", promptHold)
	done := f.startThroughProxy(t, srv.URL, "c1", "turn2", promptDone)
	f.remote.waitState(t, done, a2a.TaskStateCompleted)
	other := f.startThroughProxy(t, srv.URL, "c2", "turn2", promptHold)

	if err := f.ws.CancelCalls(ctx, Claims{ContextID: "c1", TaskID: "turn2", User: auth.Owner()}); err != nil {
		t.Fatalf("Watches.CancelCalls() error = %v", err)
	}

	f.remote.waitState(t, held, a2a.TaskStateCanceled)
	got := map[a2a.TaskID]a2a.TaskState{}
	for _, id := range []a2a.TaskID{earlier, held, done, other} {
		task, err := f.remote.Handler.GetTask(ctx, &a2a.GetTaskRequest{ID: id})
		if err != nil {
			t.Fatalf("GetTask(%s) error = %v", id, err)
		}
		got[id] = task.Status.State
	}
	want := map[a2a.TaskID]a2a.TaskState{
		earlier: a2a.TaskStateWorking,
		held:    a2a.TaskStateCanceled,
		done:    a2a.TaskStateCompleted,
		other:   a2a.TaskStateWorking,
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("task states after Watches.CancelCalls() wrong result (-want +got) diff = %s", diff)
	}
	watches, err := f.store.Watches(ctx)
	if err != nil {
		t.Fatalf("store.Watches() error = %v", err)
	}
	var watched []string
	for _, w := range watches {
		watched = append(watched, w.TaskID)
	}
	if diff := cmp.Diff([]string{string(earlier), string(other)}, watched); diff != "" {
		t.Fatalf("watched tasks after Watches.CancelCalls() wrong result (-want +got) diff = %s", diff)
	}
}

func TestWatchesCancelCallsCancelsABlockingCall(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newProxyFixture(t)
	srv := f.serve(t, "")
	t.Cleanup(f.claims.beginTurn(Claims{ContextID: "c1", TaskID: "turn1", User: auth.Owner()}))
	client := proxyClient(t, srv.URL+"/"+f.handle(t, "c1")+"/coder")
	type result struct {
		res a2a.SendMessageResult
		err error
	}
	sent := make(chan result, 1)
	t.Cleanup(f.remote.releaseHeld)
	go func() {
		res, err := client.SendMessage(ctx, &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(promptHold))})
		sent <- result{res, err}
	}()
	var calls []store.CallTask
	waitFor(t, "the blocking call to be recorded", func() bool {
		var err error
		if calls, err = f.store.CallTasksOf(ctx, "c1", "turn1"); err != nil {
			t.Fatalf("store.CallTasksOf() error = %v", err)
		}
		return len(calls) == 1
	})

	if err := f.ws.CancelCalls(ctx, Claims{ContextID: "c1", TaskID: "turn1", User: auth.Owner()}); err != nil {
		t.Fatalf("Watches.CancelCalls() error = %v", err)
	}

	got := <-sent
	if got.err != nil {
		t.Fatalf("SendMessage() through the agents URL error = %v", got.err)
	}
	task, ok := got.res.(*a2a.Task)
	if !ok || string(task.ID) != calls[0].TaskID || task.Status.State != a2a.TaskStateCanceled {
		t.Fatalf("SendMessage() through the agents URL = %+v, want task %s canceled", got.res, calls[0].TaskID)
	}
}

func TestWatchesCancelCallsKeepsTheWatchOfATaskItCannotCancel(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newProxyFixture(t)
	srv := f.serve(t, "")
	held := f.startThroughProxy(t, srv.URL, "c1", "turn1", promptHold)
	putContact(t, f.store, store.Contact{Name: "coder", URL: "http://127.0.0.1:1"})

	if err := f.ws.CancelCalls(ctx, Claims{ContextID: "c1", TaskID: "turn1", User: auth.Owner()}); err == nil {
		t.Fatalf("Watches.CancelCalls() of an unreachable agent error = nil, want an error")
	}

	watches, err := f.store.Watches(ctx)
	if err != nil {
		t.Fatalf("store.Watches() error = %v", err)
	}
	if len(watches) != 1 || watches[0].TaskID != string(held) {
		t.Fatalf("store.Watches() = %+v, want the watch of task %s kept", watches, held)
	}
}
