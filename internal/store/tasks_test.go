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

package store

import (
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
)

func applyEvents(t *testing.T, ts *TaskStore, user string, task *a2a.Task, events ...a2a.Event) {
	t.Helper()
	ctx := asUser(user)
	if _, err := ts.Create(ctx, task); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	m := ts.UpdateMaterializer()
	for i, ev := range events {
		partial, err := m.ApplyUpdate(ctx, task, ev)
		if err != nil {
			t.Fatalf("ApplyUpdate(%T) error = %v", ev, err)
		}
		req := &taskstore.UpdateRequest{Task: partial, Event: ev, PrevVersion: taskstore.TaskVersion(i + 1)}
		if _, err := ts.Update(ctx, req); err != nil {
			t.Fatalf("Update(%T) error = %v", ev, err)
		}
	}
}

func TestTaskMaterializedFromEvents(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ts := s.Tasks("", testViewer)
	task := newTask("t1", "c1")
	question := a2a.NewMessageForTask(a2a.MessageRoleUser, task, a2a.NewTextPart("draw a cat"))
	task.History = []*a2a.Message{question}
	thinking := a2a.NewMessageForTask(a2a.MessageRoleAgent, task, a2a.NewTextPart("thinking"))
	followUp := a2a.NewMessageForTask(a2a.MessageRoleUser, task, a2a.NewTextPart("in red"), a2a.NewRawPart([]byte("palette")))
	chunk := &a2a.TaskArtifactUpdateEvent{TaskID: "t1", ContextID: "c1", Artifact: &a2a.Artifact{ID: "a1", Parts: []*a2a.Part{a2a.NewTextPart("cat ")}}}
	more := &a2a.TaskArtifactUpdateEvent{TaskID: "t1", ContextID: "c1", Append: true, Artifact: &a2a.Artifact{ID: "a1", Parts: []*a2a.Part{a2a.NewRawPart([]byte("png"))}}}
	done := a2a.NewStatusUpdateEvent(task, a2a.TaskStateCompleted, nil)
	applyEvents(t, ts, "alice", task, a2a.NewStatusUpdateEvent(task, a2a.TaskStateWorking, thinking), followUp, chunk, more, done)

	got, err := ts.Get(asUser("alice"), "t1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	want := &taskstore.StoredTask{
		Task: &a2a.Task{
			ID:        "t1",
			ContextID: "c1",
			Status:    done.Status,
			History:   []*a2a.Message{question, followUp, thinking},
			Artifacts: []*a2a.Artifact{{ID: "a1", Parts: []*a2a.Part{a2a.NewTextPart("cat "), a2a.NewRawPart([]byte("png"))}}},
		},
		Version: 6,
		User:    "alice",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("Get() wrong result (-want +got) diff = %s", diff)
	}
}

func TestTerminalTaskSnapshot(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ts := s.Tasks("", testViewer)
	owner := asUser("owner")
	task := newTask("t1", "c1")
	applyEvents(t, ts, "owner", task,
		&a2a.TaskArtifactUpdateEvent{TaskID: "t1", ContextID: "c1", Artifact: &a2a.Artifact{ID: "a1", Parts: []*a2a.Part{a2a.NewTextPart("result")}}},
		a2a.NewStatusUpdateEvent(task, a2a.TaskStateCompleted, nil))

	want, err := ts.Get(owner, "t1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if _, err := s.db.Exec(`DELETE FROM task_events`); err != nil {
		t.Fatalf("deleting events: %v", err)
	}
	got, err := ts.Get(owner, "t1")
	if err != nil {
		t.Fatalf("Get() from the snapshot error = %v", err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("Get() from the snapshot wrong result (-want +got) diff = %s", diff)
	}

	_, err = ts.Update(owner, &taskstore.UpdateRequest{Task: task, Event: a2a.NewStatusUpdateEvent(task, a2a.TaskStateFailed, nil), PrevVersion: 3})
	if !errors.Is(err, a2a.ErrUnsupportedOperation) {
		t.Fatalf("Update() of a terminal task error = %v, want %v", err, a2a.ErrUnsupportedOperation)
	}
}

func TestListMaterializesOnlyWhenNeeded(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ts := s.Tasks("", testViewer)
	task := newTask("t1", "c1")
	task.History = []*a2a.Message{a2a.NewMessageForTask(a2a.MessageRoleUser, task, a2a.NewTextPart("hi"))}
	artifact := &a2a.TaskArtifactUpdateEvent{TaskID: "t1", ContextID: "c1", Artifact: &a2a.Artifact{ID: "a1", Parts: []*a2a.Part{a2a.NewTextPart("result")}}}
	applyEvents(t, ts, "owner", task, artifact)
	noHistory := 0

	tests := []struct {
		name string
		req  *a2a.ListTasksRequest
		want *a2a.Task
	}{
		{
			name: "header",
			req:  &a2a.ListTasksRequest{HistoryLength: &noHistory},
			want: &a2a.Task{ID: "t1", ContextID: "c1", Status: task.Status, History: []*a2a.Message{}},
		},
		{
			name: "full",
			req:  &a2a.ListTasksRequest{IncludeArtifacts: true},
			want: &a2a.Task{ID: "t1", ContextID: "c1", Status: task.Status, History: task.History, Artifacts: []*a2a.Artifact{artifact.Artifact}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			resp, err := ts.List(asUser("owner"), tc.req)
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}
			if diff := cmp.Diff([]*a2a.Task{tc.want}, resp.Tasks); diff != "" {
				t.Fatalf("List() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}
