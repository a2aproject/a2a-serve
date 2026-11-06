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
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// slowCleanupExecutor completes every task, and holds its cleanup until
// release closes, which keeps the execution registered with the handler.
type slowCleanupExecutor struct {
	release chan struct{}
}

func (e *slowCleanupExecutor) Execute(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if !yield(a2a.NewSubmittedTask(execCtx, execCtx.Message), nil) {
			return
		}
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCompleted, nil), nil)
	}
}

func (e *slowCleanupExecutor) Cancel(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCanceled, nil), nil)
	}
}

func (e *slowCleanupExecutor) Cleanup(context.Context, *a2asrv.ExecutorContext, a2a.SendMessageResult, error) {
	<-e.release
}

func TestWithFinishedSubscribeRefusedDuringCleanup(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	exec := &slowCleanupExecutor{release: make(chan struct{})}
	defer close(exec.release)
	h := withFinishedSubscribeRefused(a2asrv.NewHandler(exec))
	res, err := h.SendMessage(ctx, &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi"))})
	if err != nil {
		t.Fatalf("SendMessage() error = %v", err)
	}
	task := res.(*a2a.Task)

	for ev, err := range h.SubscribeToTask(ctx, &a2a.SubscribeToTaskRequest{ID: task.ID}) {
		if !errors.Is(err, a2a.ErrUnsupportedOperation) {
			t.Fatalf("withFinishedSubscribeRefused().SubscribeToTask() = %v, %v, want error %v", ev, err, a2a.ErrUnsupportedOperation)
		}
		return
	}
	t.Fatalf("withFinishedSubscribeRefused().SubscribeToTask() ended without an error, want %v", a2a.ErrUnsupportedOperation)
}
