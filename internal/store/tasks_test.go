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
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
			History:   []*a2a.Message{question, thinking, followUp},
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

func TestLatestStateInContext(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ts := s.Tasks("", testViewer)
	first := newTask("t1", "c1")
	applyEvents(t, ts, "alice", first, a2a.NewStatusUpdateEvent(first, a2a.TaskStateCompleted, nil))
	second := newTask("t2", "c1")
	applyEvents(t, ts, "alice", second, a2a.NewStatusUpdateEvent(second, a2a.TaskStateInputRequired, nil))

	id, state, err := ts.LatestStateInContext(asUser("alice"), "c1")
	if err != nil {
		t.Fatalf("LatestStateInContext() error = %v", err)
	}
	if id != "t2" || state != a2a.TaskStateInputRequired {
		t.Fatalf("LatestStateInContext() = %q, %q, want %q, %q", id, state, "t2", a2a.TaskStateInputRequired)
	}
	if _, _, err := ts.LatestStateInContext(asUser("alice"), "c2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("LatestStateInContext() of an empty context error = %v, want %v", err, ErrNotFound)
	}
}

func textChunk(task *a2a.Task, text string, appendTo bool) *a2a.TaskArtifactUpdateEvent {
	return &a2a.TaskArtifactUpdateEvent{TaskID: task.ID, ContextID: task.ContextID, Append: appendTo,
		Artifact: &a2a.Artifact{ID: "story", Parts: []*a2a.Part{a2a.NewTextPart(text)}}}
}

func streamedEvents(task *a2a.Task, chunks int, then ...a2a.Event) []a2a.Event {
	var events []a2a.Event
	for i := range chunks {
		events = append(events, textChunk(task, fmt.Sprintf("token%d ", i), i > 0))
	}
	return append(events, then...)
}

// writeOldLog stores a task as a2a-serve did before rolling snapshots: a
// log of events, and no snapshot until a terminal task is read.
func writeOldLog(t *testing.T, s *Store, ts *TaskStore, user string, task *a2a.Task, events ...a2a.Event) {
	t.Helper()
	ctx := asUser(user)
	if _, err := ts.Create(ctx, task); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	h := header(task)
	for i, ev := range events {
		stored, err := encodeEvent(ev)
		if err != nil {
			t.Fatalf("encodeEvent(%T) error = %v", ev, err)
		}
		if h, err = applyToHeader(h, stored.event); err != nil {
			t.Fatalf("applyToHeader(%T) error = %v", ev, err)
		}
		data, err := json.Marshal(h)
		if err != nil {
			t.Fatalf("encoding the header: %v", err)
		}
		version := int64(i + 2)
		err = s.inTx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.Exec(`UPDATE tasks SET version = ?, state = ?, data = ? WHERE scope = ? AND id = ?`,
				version, string(h.Status.State), string(data), ts.scope, string(task.ID)); err != nil {
				return err
			}
			return ts.insertEvent(ctx, tx, task.ID, version, stored)
		})
		if err != nil {
			t.Fatalf("appending an old event: %v", err)
		}
	}
}

// storedLog returns the version of the snapshot of a task, 0 without one,
// and the number of its events.
func storedLog(t *testing.T, s *Store, id string) (int64, int) {
	t.Helper()
	var (
		version int64
		events  int
	)
	if err := s.db.QueryRow(`SELECT coalesce((SELECT version FROM task_snapshots WHERE task_id = ?), 0), (SELECT COUNT(*) FROM task_events WHERE task_id = ?)`,
		id, id).Scan(&version, &events); err != nil {
		t.Fatalf("reading the log of task %s: %v", id, err)
	}
	return version, events
}

func TestOldEventLogsMaterializeIdentically(t *testing.T) {
	t.Parallel()
	task := newTask("t1", "c1")
	task.History = []*a2a.Message{a2a.NewMessageForTask(a2a.MessageRoleUser, task, a2a.NewTextPart("write a story"))}
	thinking := a2a.NewMessageForTask(a2a.MessageRoleAgent, task, a2a.NewTextPart("thinking"))
	question := a2a.NewMessageForTask(a2a.MessageRoleAgent, task, a2a.NewTextPart("which ending?"))
	answer := a2a.NewMessageForTask(a2a.MessageRoleUser, task, a2a.NewTextPart("a happy one"))

	tests := []struct {
		name   string
		events []a2a.Event
	}{
		{
			name:   "working past the snapshot interval",
			events: streamedEvents(task, 2*snapshotEvery+50, a2a.NewStatusUpdateEvent(task, a2a.TaskStateWorking, thinking)),
		},
		{
			name: "answered question",
			events: streamedEvents(task, 10,
				a2a.NewStatusUpdateEvent(task, a2a.TaskStateInputRequired, question),
				answer,
				a2a.NewStatusUpdateEvent(task, a2a.TaskStateWorking, thinking),
				textChunk(task, "The end.", true)),
		},
		{
			name:   "completed",
			events: streamedEvents(task, snapshotEvery+50, a2a.NewStatusUpdateEvent(task, a2a.TaskStateCompleted, nil)),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			current, old := openStore(t), openStore(t)
			applyEvents(t, current.Tasks("", testViewer), "alice", task, tc.events...)
			writeOldLog(t, old, old.Tasks("", testViewer), "alice", task, tc.events...)

			want, err := current.Tasks("", testViewer).Get(asUser("alice"), "t1")
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			for _, read := range []string{"first", "second"} {
				got, err := old.Tasks("", testViewer).Get(asUser("alice"), "t1")
				if err != nil {
					t.Fatalf("Get() of the old log, %s read, error = %v", read, err)
				}
				if diff := cmp.Diff(want, got); diff != "" {
					t.Fatalf("Get() of the old log, %s read, wrong result (-want +got) diff = %s", read, diff)
				}
			}
		})
	}
}

func TestTaskSnapshots(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ts := s.Tasks("", testViewer)
	task := newTask("t1", "c1")
	applyEvents(t, ts, "owner", task, streamedEvents(task, snapshotEvery+50)...)
	version := int64(snapshotEvery + 51)

	steps := []struct {
		name         string
		event        a2a.Event
		wantSnapshot int64
		wantEvents   int
	}{
		{name: "chunk", event: textChunk(task, "more", true), wantSnapshot: snapshotEvery, wantEvents: snapshotEvery + 52},
		{name: "input required", event: a2a.NewStatusUpdateEvent(task, a2a.TaskStateInputRequired, nil), wantSnapshot: snapshotEvery + 53, wantEvents: snapshotEvery + 53},
		{name: "completed", event: a2a.NewStatusUpdateEvent(task, a2a.TaskStateCompleted, nil), wantSnapshot: snapshotEvery + 54, wantEvents: 0},
	}
	for _, step := range steps {
		if _, err := ts.Update(asUser("owner"), &taskstore.UpdateRequest{Task: task, Event: step.event, PrevVersion: taskstore.TaskVersion(version)}); err != nil {
			t.Fatalf("Update() with %s error = %v", step.name, err)
		}
		version++
		gotSnapshot, gotEvents := storedLog(t, s, "t1")
		if gotSnapshot != step.wantSnapshot || gotEvents != step.wantEvents {
			t.Fatalf("after %s: snapshot version, events = %d, %d, want %d, %d", step.name, gotSnapshot, gotEvents, step.wantSnapshot, step.wantEvents)
		}
	}

	got, err := ts.Get(asUser("owner"), "t1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	var text strings.Builder
	for i := range snapshotEvery + 50 {
		fmt.Fprintf(&text, "token%d ", i)
	}
	want := a2a.ContentParts{a2a.NewTextPart(text.String() + "more")}
	if diff := cmp.Diff(want, got.Task.Artifacts[0].Parts); diff != "" {
		t.Fatalf("Get() artifact parts wrong result (-want +got) diff = %s", diff)
	}
}

func TestOldTerminalTaskCompactedOnRead(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ts := s.Tasks("", testViewer)
	task := newTask("t1", "c1")
	writeOldLog(t, s, ts, "owner", task, streamedEvents(task, 3, a2a.NewStatusUpdateEvent(task, a2a.TaskStateCompleted, nil))...)

	if _, err := ts.Get(asUser("owner"), "t1"); err != nil {
		t.Fatalf("Get() error = %v", err)
	}

	if gotSnapshot, gotEvents := storedLog(t, s, "t1"); gotSnapshot != 5 || gotEvents != 0 {
		t.Fatalf("after Get(): snapshot version, events = %d, %d, want 5, 0", gotSnapshot, gotEvents)
	}
}

func TestGetWhileTasksComplete(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ts := s.Tasks("", testViewer)
	owner := asUser("owner")
	const tasks = 50
	for i := range tasks {
		task := newTask(fmt.Sprintf("t%d", i), "c1")
		applyEvents(t, ts, "owner", task, streamedEvents(task, 5)...)
	}

	done := make(chan struct{})
	errs := make(chan error, 4)
	for range 4 {
		go func() {
			for {
				select {
				case <-done:
					errs <- nil
					return
				default:
				}
				for i := range tasks {
					if _, err := ts.Get(owner, a2a.TaskID(fmt.Sprintf("t%d", i))); err != nil {
						errs <- err
						return
					}
				}
			}
		}()
	}
	for i := range tasks {
		task := newTask(fmt.Sprintf("t%d", i), "c1")
		if _, err := ts.Update(owner, &taskstore.UpdateRequest{Task: task, Event: a2a.NewStatusUpdateEvent(task, a2a.TaskStateCompleted, nil)}); err != nil {
			t.Fatalf("Update() error = %v", err)
		}
	}
	close(done)
	for range 4 {
		if err := <-errs; err != nil {
			t.Fatalf("Get() while tasks complete error = %v", err)
		}
	}
}
