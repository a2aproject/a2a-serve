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
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/google/go-cmp/cmp"
)

type cancelResult struct {
	task *a2a.Task
	err  error
}

type scriptedCanceler struct {
	a2asrv.RequestHandler
	results []cancelResult
	calls   int
}

func (s *scriptedCanceler) CancelTask(context.Context, *a2a.CancelTaskRequest) (*a2a.Task, error) {
	r := s.results[min(s.calls, len(s.results)-1)]
	s.calls++
	return r.task, r.err
}

func TestWithRetryingCancel(t *testing.T) {
	t.Parallel()
	canceled := &a2a.Task{ID: "t1", Status: a2a.TaskStatus{State: a2a.TaskStateCanceled}}
	notCancelable := cancelResult{err: a2a.ErrTaskNotCancelable}
	other := errors.New("boom")
	tests := []struct {
		name      string
		results   []cancelResult
		wantTask  *a2a.Task
		wantErr   error
		wantCalls int
	}{
		{name: "canceled", results: []cancelResult{{task: canceled}}, wantTask: canceled, wantCalls: 1},
		{name: "execution ended before the cancel", results: []cancelResult{notCancelable, {task: canceled}}, wantTask: canceled, wantCalls: 2},
		{name: "never cancelable", results: []cancelResult{notCancelable}, wantErr: a2a.ErrTaskNotCancelable, wantCalls: cancelAttempts},
		{name: "other error", results: []cancelResult{{err: other}}, wantErr: other, wantCalls: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			inner := &scriptedCanceler{results: tt.results}
			h := withRetryingCancel(inner)

			got, err := h.CancelTask(t.Context(), &a2a.CancelTaskRequest{ID: "t1"})

			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("withRetryingCancel().CancelTask() error = %v, want %v", err, tt.wantErr)
			}
			if diff := cmp.Diff(tt.wantTask, got); diff != "" {
				t.Fatalf("withRetryingCancel().CancelTask() wrong result (-want +got) diff = %s", diff)
			}
			if inner.calls != tt.wantCalls {
				t.Fatalf("withRetryingCancel().CancelTask() calls = %d, want %d", inner.calls, tt.wantCalls)
			}
		})
	}
}

type inputRequiredExecutor struct {
	once    sync.Once
	release chan struct{}
}

func (e *inputRequiredExecutor) Execute(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if execCtx.StoredTask == nil && !yield(a2a.NewSubmittedTask(execCtx, execCtx.Message), nil) {
			return
		}
		if !yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateInputRequired, nil), nil) {
			return
		}
		<-e.release
	}
}

func (e *inputRequiredExecutor) Cancel(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		e.once.Do(func() { close(e.release) })
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCanceled, nil), nil)
	}
}

func TestWithRetryingCancelOfExecutionEndingInInputRequired(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	h := withRetryingCancel(a2asrv.NewHandler(&inputRequiredExecutor{release: make(chan struct{})}))
	req := &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi"))}
	var taskID a2a.TaskID
	for ev, err := range h.SendStreamingMessage(ctx, req) {
		if err != nil {
			t.Fatalf("SendStreamingMessage() error = %v", err)
		}
		taskID = ev.TaskInfo().TaskID
		if u, ok := ev.(*a2a.TaskStatusUpdateEvent); ok && u.Status.State == a2a.TaskStateInputRequired {
			break
		}
	}

	got, err := h.CancelTask(ctx, &a2a.CancelTaskRequest{ID: taskID})

	if err != nil {
		t.Fatalf("withRetryingCancel().CancelTask() error = %v", err)
	}
	if got.Status.State != a2a.TaskStateCanceled {
		t.Fatalf("withRetryingCancel().CancelTask() state = %v, want %v", got.Status.State, a2a.TaskStateCanceled)
	}
}
