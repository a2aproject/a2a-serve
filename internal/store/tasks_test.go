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
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"

	"github.com/a2aproject/a2a-serve/internal/a2alimit"
	"github.com/a2aproject/a2a-serve/internal/a2atext"
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
	noHistory, history := 0, 10

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
			name: "default",
			req:  &a2a.ListTasksRequest{},
			want: &a2a.Task{ID: "t1", ContextID: "c1", Status: task.Status, History: []*a2a.Message{}},
		},
		{
			name: "artifacts without history by default",
			req:  &a2a.ListTasksRequest{IncludeArtifacts: true},
			want: &a2a.Task{ID: "t1", ContextID: "c1", Status: task.Status, History: []*a2a.Message{}, Artifacts: []*a2a.Artifact{artifact.Artifact}},
		},
		{
			name: "full",
			req:  &a2a.ListTasksRequest{HistoryLength: &history, IncludeArtifacts: true},
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

func TestListCutsLongParts(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ts := s.Tasks("", testViewer)
	limit := a2alimit.MaxListPart
	long := strings.Repeat("a", limit+10)
	blob := []byte(strings.Repeat("b", limit+1))
	task := newTask("t1", "c1")
	task.History = []*a2a.Message{a2a.NewMessageForTask(a2a.MessageRoleUser, task,
		a2a.NewTextPart(long), a2a.NewRawPart(blob), a2a.NewRawPart([]byte("small")), a2a.NewDataPart(map[string]any{"k": long}))}
	question := a2a.NewMessageForTask(a2a.MessageRoleAgent, task, a2a.NewTextPart(long))
	answer := a2a.NewMessageForTask(a2a.MessageRoleUser, task, a2a.NewTextPart("yes"))
	asked := a2a.NewStatusUpdateEvent(task, a2a.TaskStateInputRequired, question)
	applyEvents(t, ts, "owner", task, textChunk(task, long[:limit], false), textChunk(task, long[limit:], true), asked, answer)
	history := 10
	req := &a2a.ListTasksRequest{HistoryLength: &history, IncludeArtifacts: true}

	resp, err := ts.List(asUser("owner"), req)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}

	cutText := a2a.NewTextPart(long[:limit] + fmt.Sprintf("\n[cut: %d of %d bytes shown; get task t1 for the whole text]", limit, len(long)))
	cutQuestion := &a2a.Message{ID: question.ID, Role: a2a.MessageRoleAgent, TaskID: "t1", ContextID: "c1", Parts: a2a.ContentParts{cutText}}
	want := &a2a.Task{
		ID: "t1", ContextID: "c1",
		Status: a2a.TaskStatus{State: asked.Status.State, Timestamp: asked.Status.Timestamp, Message: cutQuestion},
		History: []*a2a.Message{
			{ID: task.History[0].ID, Role: a2a.MessageRoleUser, TaskID: "t1", ContextID: "c1", Parts: a2a.ContentParts{
				cutText,
				a2a.NewTextPart(fmt.Sprintf("[file of %d bytes left out of the list; get task t1 for it]", len(blob))),
				a2a.NewRawPart([]byte("small")),
				a2a.NewTextPart(fmt.Sprintf("[data of %d bytes left out of the list; get task t1 for it]", len(long)+8)),
			}},
			cutQuestion,
			answer,
		},
		Artifacts: []*a2a.Artifact{{ID: "story", Parts: a2a.ContentParts{cutText}}},
	}
	if diff := cmp.Diff([]*a2a.Task{want}, resp.Tasks); diff != "" {
		t.Fatalf("List() wrong result (-want +got) diff = %s", diff)
	}

	whole, _, err := ts.ListWithOwners(asUser("owner"), req)
	if err != nil {
		t.Fatalf("ListWithOwners() error = %v", err)
	}
	stored, err := ts.Get(asUser("owner"), "t1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if diff := cmp.Diff([]*a2a.Task{stored.Task}, whole.Tasks); diff != "" {
		t.Fatalf("ListWithOwners() wrong result (-want +got) diff = %s", diff)
	}
}

func TestListHistoryMatchesGet(t *testing.T) {
	t.Parallel()
	task := newTask("t1", "c1")
	task.History = []*a2a.Message{a2a.NewMessageForTask(a2a.MessageRoleUser, task, a2a.NewTextPart("write a story"))}
	thinking := a2a.NewMessageForTask(a2a.MessageRoleAgent, task, a2a.NewTextPart("thinking"))
	question := a2a.NewMessageForTask(a2a.MessageRoleAgent, task, a2a.NewTextPart("which ending?"))
	answer := a2a.NewMessageForTask(a2a.MessageRoleUser, task, a2a.NewTextPart("a happy one"))
	events := streamedEvents(task, snapshotEvery+10,
		a2a.NewStatusUpdateEvent(task, a2a.TaskStateInputRequired, question),
		answer,
		a2a.NewStatusUpdateEvent(task, a2a.TaskStateWorking, thinking),
		textChunk(task, "The end.", true))
	s := openStore(t)
	ts := s.Tasks("", testViewer)
	applyEvents(t, ts, "alice", task, events...)
	stored, err := ts.Get(asUser("alice"), "t1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}

	for _, n := range []int{1, 2, 3, 100} {
		t.Run(fmt.Sprintf("history %d", n), func(t *testing.T) {
			t.Parallel()
			want := *stored.Task
			want.History = want.History[max(len(want.History)-n, 0):]

			resp, err := ts.List(asUser("alice"), &a2a.ListTasksRequest{HistoryLength: &n, IncludeArtifacts: true})
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}

			if diff := cmp.Diff([]*a2a.Task{&want}, resp.Tasks); diff != "" {
				t.Fatalf("List() wrong result (-want +got) diff = %s", diff)
			}
		})
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

func TestUpdateWaitsForAnotherWriter(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "server.db")
	s := openStoreAt(t, path)
	ts := s.Tasks("", testViewer)
	owner := asUser("owner")
	task := newTask("t1", "c1")
	applyEvents(t, ts, "owner", task, streamedEvents(task, 1)...)
	other, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := other.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	ctx := context.Background()
	conn, err := other.Conn(ctx)
	if err != nil {
		t.Fatalf("Conn() error = %v", err)
	}
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatalf("BEGIN IMMEDIATE error = %v", err)
	}
	committed := make(chan error, 1)
	go func() {
		time.Sleep(200 * time.Millisecond)
		_, err := conn.ExecContext(ctx, `COMMIT`)
		committed <- errors.Join(err, conn.Close())
	}()

	_, err = ts.Update(owner, &taskstore.UpdateRequest{Task: task, Event: a2a.NewStatusUpdateEvent(task, a2a.TaskStateCompleted, nil)})

	if cerr := <-committed; cerr != nil {
		t.Fatalf("COMMIT error = %v", cerr)
	}
	if err != nil {
		t.Fatalf("Update() while another connection writes error = %v, want it to wait", err)
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

func TestHeader(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ts := s.Tasks("", testViewer)
	task := newTask("t1", "c1")
	task.History = []*a2a.Message{a2a.NewMessageForTask(a2a.MessageRoleUser, task, a2a.NewTextPart("draw a cat"), a2a.NewRawPart([]byte("sketch")))}
	chunk := &a2a.TaskArtifactUpdateEvent{TaskID: "t1", ContextID: "c1", Artifact: &a2a.Artifact{ID: "a1", Parts: []*a2a.Part{a2a.NewRawPart([]byte("png"))}}}
	asking := a2a.NewStatusUpdateEvent(task, a2a.TaskStateInputRequired,
		a2a.NewMessageForTask(a2a.MessageRoleAgent, task, a2a.NewTextPart("which color?"), a2a.NewRawPart([]byte("palette"))))
	applyEvents(t, ts, "alice", task, chunk, asking)
	want := &taskstore.StoredTask{
		Task:    &a2a.Task{ID: "t1", ContextID: "c1", Status: asking.Status},
		Version: 3,
		User:    "alice",
	}

	tests := []struct {
		name    string
		viewer  context.Context
		id      a2a.TaskID
		want    *taskstore.StoredTask
		wantErr error
	}{
		{name: "own task", viewer: asUser("alice"), id: "t1", want: want},
		{name: "owner sees every task", viewer: asUser("owner"), id: "t1", want: want},
		{name: "task of someone else", viewer: asUser("bob"), id: "t1", wantErr: a2a.ErrTaskNotFound},
		{name: "unknown task", viewer: asUser("alice"), id: "nope", wantErr: a2a.ErrTaskNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ts.Header(tc.viewer, tc.id)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("TaskStore.Header() error = %v, want %v", err, tc.wantErr)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("TaskStore.Header() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestListSummaries(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ts := s.Tasks("", testViewer)
	first := newTask("t1", "c1")
	first.History = []*a2a.Message{
		a2a.NewMessageForTask(a2a.MessageRoleAgent, first, a2a.NewTextPart("hello")),
		a2a.NewMessageForTask(a2a.MessageRoleUser, first, a2a.NewTextPart("draw a cat"), a2a.NewRawPart([]byte("sketch"))),
	}
	applyEvents(t, ts, "alice", first,
		a2a.NewMessageForTask(a2a.MessageRoleUser, first, a2a.NewTextPart("in red")),
		a2a.NewStatusUpdateEvent(first, a2a.TaskStateCompleted, nil))
	second := newTask("t2", "c1")
	second.History = []*a2a.Message{a2a.NewMessageForTask(a2a.MessageRoleAgent, second, a2a.NewTextPart("done"))}
	applyEvents(t, ts, "bob", second)
	later := newTask("t3", "c2")
	applyEvents(t, ts, "owner", later,
		a2a.NewStatusUpdateEvent(later, a2a.TaskStateInputRequired, a2a.NewMessageForTask(a2a.MessageRoleAgent, later, a2a.NewTextPart("what now?"))),
		a2a.NewMessageForTask(a2a.MessageRoleUser, later, a2a.NewTextPart("a dog")),
		a2a.NewMessageForTask(a2a.MessageRoleUser, later, a2a.NewTextPart("a bird")))
	snapshotted := newTask("t4", "c2")
	applyEvents(t, ts, "owner", snapshotted, streamedEvents(snapshotted, snapshotEvery+10,
		a2a.NewMessageForTask(a2a.MessageRoleUser, snapshotted, a2a.NewTextPart("a fish")))...)
	old := newTask("t5", "c3")
	old.History = []*a2a.Message{a2a.NewMessageForTask(a2a.MessageRoleUser, old, a2a.NewTextPart("an old one"))}
	writeOldLog(t, s, ts, "owner", old, a2a.NewStatusUpdateEvent(old, a2a.TaskStateCompleted, nil))

	type summary struct {
		ID     a2a.TaskID
		State  a2a.TaskState
		Owner  string
		Prompt string
		Bytes  int
	}
	tests := []struct {
		name   string
		viewer context.Context
		want   []summary
	}{
		{
			name:   "owner sees every task",
			viewer: asUser("owner"),
			want: []summary{
				{ID: "t1", State: a2a.TaskStateCompleted, Owner: "alice", Prompt: "draw a cat"},
				{ID: "t2", State: a2a.TaskStateWorking, Owner: "bob"},
				{ID: "t3", State: a2a.TaskStateInputRequired, Owner: "owner", Prompt: "a dog"},
				{ID: "t4", State: a2a.TaskStateWorking, Owner: "owner", Prompt: "a fish"},
				{ID: "t5", State: a2a.TaskStateCompleted, Owner: "owner", Prompt: "an old one"},
			},
		},
		{
			name:   "caller sees own tasks",
			viewer: asUser("alice"),
			want:   []summary{{ID: "t1", State: a2a.TaskStateCompleted, Owner: "alice", Prompt: "draw a cat"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sums, err := ts.ListSummaries(tc.viewer, &a2a.ListTasksRequest{})
			if err != nil {
				t.Fatalf("TaskStore.ListSummaries() error = %v", err)
			}
			var got []summary
			for _, sum := range sums {
				if sum.Task.History != nil || sum.Task.Artifacts != nil {
					t.Fatalf("TaskStore.ListSummaries() task %s has history or artifacts", sum.Task.ID)
				}
				sm := summary{ID: sum.Task.ID, State: sum.Task.Status.State, Owner: sum.Owner}
				if sum.Prompt != nil {
					sm.Prompt = a2atext.Message(sum.Prompt)
					for _, p := range sum.Prompt.Parts {
						sm.Bytes += len(p.Raw())
					}
				}
				got = append(got, sm)
			}
			slices.SortFunc(got, func(a, b summary) int { return strings.Compare(string(a.ID), string(b.ID)) })
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("TaskStore.ListSummaries() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}
