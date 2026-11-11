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
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/a2aproject/a2a-serve/internal/agentsrv/agentproxy"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/store"
)

func TestEndingQuestionTaskKeepsContextUnattended(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	started := send(t, ts.client(ts.pair("me", auth.RoleOwner)), userText("hello", "", ""))
	ctx := context.Background()
	waitCtx, cancel := context.WithTimeout(ctx, waitTimeout)
	defer cancel()
	ts.srv.turns.wait(waitCtx)
	if waitCtx.Err() != nil {
		t.Fatalf("turns.wait() error = %v, want the turn of the first message ended", waitCtx.Err())
	}
	rec := store.Question{ID: "q1", Token: "tok1", TaskID: "remote-task", ContextID: "remote-context", Contact: "reviewer", HitlID: "h1", Data: map[string]any{"id": "h1", "type": "permission"}}
	if err := ts.srv.store.PutQuestion(ctx, rec); err != nil {
		t.Fatalf("PutQuestion() error = %v", err)
	}
	qt := &questionTasks{store: ts.srv.store, tasks: ts.srv.tasks, bus: ts.srv.bus, ends: ts.srv.ends}
	if err := qt.Ask(ctx, started.ContextID, &agentproxy.CallerQuestion{Rec: rec, Header: "reviewer asks"}); err != nil {
		t.Fatalf("questionTasks.Ask() error = %v", err)
	}
	ts.srv.unattended.Add(started.ContextID)

	qt.End(ctx, started.ContextID, "reviewer", "remote-task")

	_, state, err := ts.srv.tasks.LatestStateInContext(auth.WithPrincipal(ctx, auth.Owner()), started.ContextID)
	if err != nil || state != a2a.TaskStateCompleted {
		t.Fatalf("LatestStateInContext() = %s, %v, want the question task %s", state, err, a2a.TaskStateCompleted)
	}
	if !ts.srv.unattended.Has(started.ContextID) {
		t.Fatalf("unattended.Has() = false after a question task ended, want the context of the running turn kept")
	}
}

func TestQuestionTasksEndOrphaned(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		watched   bool
		wantState a2a.TaskState
	}{
		{name: "no watch", watched: false, wantState: a2a.TaskStateCompleted},
		{name: "watched", watched: true, wantState: a2a.TaskStateInputRequired},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ts := startServer(t, serverOpts{})
			started := send(t, ts.client(ts.pair("me", auth.RoleOwner)), userText("hello", "", ""))
			ctx := context.Background()
			rec := store.Question{ID: "q1", Token: "tok1", TaskID: "remote-task", ContextID: "remote-context", Contact: "reviewer", HitlID: "h1", Data: map[string]any{"id": "h1", "type": "permission"}}
			if err := ts.srv.store.PutQuestion(ctx, rec); err != nil {
				t.Fatalf("PutQuestion() error = %v", err)
			}
			qt := &questionTasks{store: ts.srv.store, tasks: ts.srv.tasks, bus: ts.srv.bus, ends: ts.srv.ends}
			if err := qt.Ask(ctx, started.ContextID, &agentproxy.CallerQuestion{Rec: rec, Header: "reviewer asks"}); err != nil {
				t.Fatalf("questionTasks.Ask() error = %v", err)
			}

			qt.EndOrphaned(ctx, func(agent, taskID string) bool {
				return tc.watched && agent == "reviewer" && taskID == "remote-task"
			})

			_, state, err := ts.srv.tasks.LatestStateInContext(auth.WithPrincipal(ctx, auth.Owner()), started.ContextID)
			if err != nil || state != tc.wantState {
				t.Fatalf("LatestStateInContext() = %s, %v, want the question task %s", state, err, tc.wantState)
			}
		})
	}
}
