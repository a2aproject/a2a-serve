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

package a2aerr

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/go-cmp/cmp"
)

func TestSanitize(t *testing.T) {
	t.Parallel()
	errBusy := errors.New("every agent process is busy")
	tests := []struct {
		name       string
		err        error
		wantReason string
		wantMsg    string
	}{
		{
			name:       "terminal task under a2a-go's wrapping",
			err:        fmt.Errorf("executor setup failed: %w", fmt.Errorf("failed to load exec ctx: %w", fmt.Errorf("task in a terminal state %q: %w", "TASK_STATE_COMPLETED", a2a.ErrUnsupportedOperation))),
			wantReason: "UNSUPPORTED_OPERATION",
			wantMsg:    `task in a terminal state "TASK_STATE_COMPLETED": this operation is not supported`,
		},
		{
			name:       "unknown task",
			err:        fmt.Errorf("executor setup failed: %w", fmt.Errorf("failed to load exec ctx: %w", fmt.Errorf("task loading failed: %w", a2a.ErrTaskNotFound))),
			wantReason: "TASK_NOT_FOUND",
			wantMsg:    "task not found",
		},
		{
			name:       "detail after the A2A error",
			err:        fmt.Errorf("executor setup failed: %w", fmt.Errorf("%w: conversation c1 belongs to someone else", a2a.ErrUnauthorized)),
			wantReason: "UNAUTHORIZED",
			wantMsg:    "permission denied: conversation c1 belongs to someone else",
		},
		{
			name:       "server error with a reason",
			err:        fmt.Errorf("create execution failure: %w", fmt.Errorf("%w: %w", a2a.ErrServerError, errBusy)),
			wantReason: "SERVER_ERROR",
			wantMsg:    "server error: every agent process is busy",
		},
		{
			name:       "A2A error with its own message",
			err:        fmt.Errorf("cancelation failed: %w", a2a.NewError(a2a.ErrTaskNotCancelable, "the task already ended")),
			wantReason: "TASK_NOT_CANCELABLE",
			wantMsg:    "the task already ended",
		},
		{
			name:       "execution in progress",
			err:        errors.New("task execution is already in progress"),
			wantReason: "UNSUPPORTED_OPERATION",
			wantMsg:    "the task is already handling a request; try again when it is done",
		},
		{
			name:       "concurrency quota",
			err:        fmt.Errorf("create execution failure: %w", fmt.Errorf("concurrency quota exceeded: %w", errors.New(`max scope concurrency limit reached for "pair:mallory"`))),
			wantReason: "UNAUTHORIZED",
			wantMsg:    "too many tasks are running; try again when one finishes",
		},
		{
			name:       "internal failure",
			err:        fmt.Errorf("executor setup failed: %w", errors.New("open /home/me/.a2a/state/server.db: database is locked")),
			wantReason: "INTERNAL_ERROR",
			wantMsg:    "internal error",
		},
		{
			name:       "wrapped internal A2A error",
			err:        fmt.Errorf("saving: %w", fmt.Errorf("disk full: %w", a2a.ErrInternalError)),
			wantReason: "INTERNAL_ERROR",
			wantMsg:    "internal error",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := Sanitize(tc.err)

			type result struct{ Reason, Message string }
			want := result{Reason: tc.wantReason, Message: tc.wantMsg}
			if diff := cmp.Diff(want, result{Reason: a2a.ErrorReason(got), Message: got.Error()}); diff != "" {
				t.Fatalf("Sanitize(%v) wrong result (-want +got) diff = %s", tc.err, diff)
			}
		})
	}
}

func TestSanitizeKeepsContextErrors(t *testing.T) {
	t.Parallel()
	err := fmt.Errorf("executor setup failed: %w", context.Canceled)

	if got := Sanitize(err); got != context.Canceled {
		t.Fatalf("Sanitize(%v) = %v, want %v", err, got, context.Canceled)
	}
}
