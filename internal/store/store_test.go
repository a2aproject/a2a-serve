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
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/a2aproject/a2a-go/v2/executor/a2aacp"
)

func openStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	return s
}

type userKey struct{}

func asUser(user string) context.Context {
	return context.WithValue(context.Background(), userKey{}, user)
}

func testViewer(ctx context.Context) (string, bool, error) {
	u, _ := ctx.Value(userKey{}).(string)
	return u, u == "owner", nil
}

func newTask(id, contextID string) *a2a.Task {
	return &a2a.Task{ID: a2a.TaskID(id), ContextID: contextID, Status: a2a.TaskStatus{State: a2a.TaskStateWorking}}
}

func TestTaskStoreVisibility(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ts := s.Tasks("", testViewer)
	alice, bob, owner := asUser("alice"), asUser("bob"), asUser("owner")

	if _, err := ts.Create(alice, newTask("t1", "c1")); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if _, err := ts.Create(alice, newTask("t1", "c1")); !errors.Is(err, taskstore.ErrTaskAlreadyExists) {
		t.Fatalf("Create() error = %v, want %v", err, taskstore.ErrTaskAlreadyExists)
	}
	if _, err := ts.Get(bob, "t1"); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Fatalf("Get() as other user error = %v, want %v", err, a2a.ErrTaskNotFound)
	}
	got, err := ts.Get(owner, "t1")
	if err != nil {
		t.Fatalf("Get() as owner error = %v", err)
	}
	if got.User != "alice" || got.Version != 1 {
		t.Fatalf("Get() = {User: %q, Version: %d}, want {alice, 1}", got.User, got.Version)
	}

	updated := newTask("t1", "c1")
	updated.Status.State = a2a.TaskStateCompleted
	v, err := ts.Update(owner, &taskstore.UpdateRequest{Task: updated, PrevVersion: 1})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if v != 2 {
		t.Fatalf("Update() = %d, want 2", v)
	}
	if _, err := ts.Update(alice, &taskstore.UpdateRequest{Task: updated, PrevVersion: 1}); !errors.Is(err, taskstore.ErrConcurrentModification) {
		t.Fatalf("Update() stale error = %v, want %v", err, taskstore.ErrConcurrentModification)
	}
	got, err = ts.Get(alice, "t1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.User != "alice" || got.Task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("Get() = {User: %q, State: %v}, want {alice, completed}", got.User, got.Task.Status.State)
	}

	if _, err := s.Tasks("n/grok", testViewer).Get(owner, "t1"); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Fatalf("Get() in other scope error = %v, want %v", err, a2a.ErrTaskNotFound)
	}
}

func TestTaskStoreList(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ts := s.Tasks("", testViewer)
	for _, id := range []string{"a", "b", "c"} {
		if _, err := ts.Create(asUser("alice"), newTask(id, "c1")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ts.Create(asUser("bob"), newTask("d", "c2")); err != nil {
		t.Fatal(err)
	}

	var ids []string
	req := &a2a.ListTasksRequest{PageSize: 2}
	for {
		resp, err := ts.List(asUser("owner"), req)
		if err != nil {
			t.Fatalf("List() error = %v", err)
		}
		if resp.TotalSize != 4 {
			t.Fatalf("List() TotalSize = %d, want 4", resp.TotalSize)
		}
		for _, task := range resp.Tasks {
			ids = append(ids, string(task.ID))
		}
		if resp.NextPageToken == "" {
			break
		}
		req.PageToken = resp.NextPageToken
	}
	if diff := cmp.Diff([]string{"d", "c", "b", "a"}, ids); diff != "" {
		t.Fatalf("List() wrong result (-want +got) diff = %s", diff)
	}

	resp, err := ts.List(asUser("bob"), &a2a.ListTasksRequest{})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(resp.Tasks) != 1 || resp.Tasks[0].ID != "d" {
		t.Fatalf("List() as bob = %v, want [d]", resp.Tasks)
	}
}

func TestFailUnfinished(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ts := s.Tasks("", testViewer)
	owner := asUser("owner")
	done := newTask("done", "c")
	done.Status.State = a2a.TaskStateCompleted
	for _, task := range []*a2a.Task{newTask("running", "c"), done} {
		if _, err := ts.Create(owner, task); err != nil {
			t.Fatal(err)
		}
	}
	n, err := ts.FailUnfinished(context.Background(), "restarted")
	if err != nil {
		t.Fatalf("FailUnfinished() error = %v", err)
	}
	if n != 1 {
		t.Fatalf("FailUnfinished() = %d, want 1", n)
	}
	got, err := ts.Get(owner, "running")
	if err != nil {
		t.Fatal(err)
	}
	if got.Task.Status.State != a2a.TaskStateFailed {
		t.Fatalf("Get() state = %v, want %v", got.Task.Status.State, a2a.TaskStateFailed)
	}
}

func TestCodes(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	now := time.Now()
	s.now = func() time.Time { return now }

	if err := s.PutCode(ctx, "K7Q29XF4", Code{Purpose: "pair", Name: "grok", Role: "member", Expires: now.Add(time.Minute)}); err != nil {
		t.Fatalf("PutCode() error = %v", err)
	}
	if _, err := s.TakeCode(ctx, "K7Q29XF4", "node"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("TakeCode() wrong purpose error = %v, want %v", err, ErrNotFound)
	}
	got, err := s.TakeCode(ctx, "K7Q29XF4", "pair")
	if err != nil {
		t.Fatalf("TakeCode() error = %v", err)
	}
	if got.Name != "grok" || got.Role != "member" {
		t.Fatalf("TakeCode() = %+v, want grok/member", got)
	}
	if _, err := s.TakeCode(ctx, "K7Q29XF4", "pair"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("TakeCode() second use error = %v, want %v", err, ErrNotFound)
	}

	if err := s.PutCode(ctx, "OLD", Code{Purpose: "pair", Expires: now.Add(-time.Second)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TakeCode(ctx, "OLD", "pair"); !errors.Is(err, ErrCodeExpired) {
		t.Fatalf("TakeCode() expired error = %v, want %v", err, ErrCodeExpired)
	}

	if err := s.PutCode(ctx, "NODE", Code{Purpose: "node", Name: "grok", Expires: now.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	got, err = s.TakeCode(ctx, "NODE", "")
	if err != nil {
		t.Fatalf("TakeCode() any purpose error = %v", err)
	}
	if diff := cmp.Diff(Code{Purpose: "node", Name: "grok", Expires: got.Expires}, got); diff != "" {
		t.Fatalf("TakeCode() any purpose wrong result (-want +got) diff = %s", diff)
	}
}

func TestTokens(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	if _, err := s.EnsureUser(ctx, "pair:grok", []string{"member"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutLink(ctx, Link{Channel: "telegram", Identity: "tg:1", UserID: "pair:grok"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutToken(ctx, "a2p_secret", Token{Name: "grok", Principal: "pair:grok", Roles: []string{"member"}, URL: "http://x"}); err != nil {
		t.Fatalf("PutToken() error = %v", err)
	}
	got, err := s.LookupToken(ctx, "a2p_secret")
	if err != nil {
		t.Fatalf("LookupToken() error = %v", err)
	}
	if got.Principal != "pair:grok" || got.Name != "grok" {
		t.Fatalf("LookupToken() = %+v, want pair:grok", got)
	}
	n, err := s.RevokeTokens(ctx, "grok", "pair:grok")
	if err != nil {
		t.Fatalf("RevokeTokens() error = %v", err)
	}
	if n != 2 {
		t.Fatalf("RevokeTokens() = %d, want 2", n)
	}
	if _, err := s.LookupToken(ctx, "a2p_secret"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("LookupToken() after revoke error = %v, want %v", err, ErrNotFound)
	}
	if _, err := s.LinkOf(ctx, "telegram", "tg:1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("LinkOf() after revoke error = %v, want %v", err, ErrNotFound)
	}
}

func TestFirstSeen(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	for i, want := range []bool{true, false} {
		got, err := s.FirstSeen(ctx, "tg:1:2")
		if err != nil {
			t.Fatalf("FirstSeen() error = %v", err)
		}
		if got != want {
			t.Fatalf("FirstSeen() call %d = %v, want %v", i, got, want)
		}
	}
}

func TestQuestionAnsweredOnce(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	q := Question{ID: "abcd1234", Token: "tok", TaskID: "t", ContextID: "c", Data: map[string]any{"id": "x"}}
	if err := s.PutQuestion(ctx, q); err != nil {
		t.Fatalf("PutQuestion() error = %v", err)
	}
	if err := s.AnswerQuestion(ctx, q.ID, "user:owner", "once"); err != nil {
		t.Fatalf("AnswerQuestion() error = %v", err)
	}
	if err := s.AnswerQuestion(ctx, q.ID, "user:owner", "reject"); !errors.Is(err, ErrAnswered) {
		t.Fatalf("AnswerQuestion() second error = %v, want %v", err, ErrAnswered)
	}
	got, err := s.QuestionByToken(ctx, "tok")
	if err != nil {
		t.Fatalf("QuestionByToken() error = %v", err)
	}
	if got.Answer != "once" || got.Data["id"] != "x" {
		t.Fatalf("QuestionByToken() = %+v, want answer once", got)
	}
	if err := s.AnswerQuestion(ctx, "missing1", "user:owner", "once"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("AnswerQuestion() unknown question error = %v, want %v", err, ErrNotFound)
	}
}

func TestOpenPathWithURLCharacters(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "a?b#c%d", "server.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
}

func TestSessions(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	ss := s.Sessions()
	if _, ok, err := ss.Get(ctx, "c"); err != nil || ok {
		t.Fatalf("Get() = %v, %v, want false, nil", ok, err)
	}
	if err := ss.Put(ctx, "c", a2aacp.Session{ID: "s1"}); err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	want := a2aacp.Session{ID: "s2", AgentKey: "k"}
	if err := ss.Put(ctx, "c", want); err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	got, ok, err := ss.Get(ctx, "c")
	if err != nil || !ok {
		t.Fatalf("Get() = %v, %v, want true, nil", ok, err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("Get() wrong result (-want +got) diff = %s", diff)
	}
}

func TestRemoteQuestionStoredOnce(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	tests := []struct {
		name    string
		q       Question
		wantErr error
	}{
		{name: "remote", q: Question{ID: "q1", Token: "t1", TaskID: "task", Contact: "peer", HitlID: "tool1"}},
		{name: "same remote question", q: Question{ID: "q2", Token: "t2", TaskID: "task", Contact: "peer", HitlID: "tool1"}, wantErr: ErrExists},
		{name: "next remote question", q: Question{ID: "q3", Token: "t3", TaskID: "task", Contact: "peer", HitlID: "tool2"}},
		{name: "local", q: Question{ID: "q4", Token: "t4", TaskID: "task", HitlID: "tool1"}},
		{name: "same local question", q: Question{ID: "q5", Token: "t5", TaskID: "task", HitlID: "tool1"}},
	}
	for _, tc := range tests {
		if err := s.PutQuestion(ctx, tc.q); !errors.Is(err, tc.wantErr) {
			t.Fatalf("PutQuestion(%s) error = %v, want %v", tc.name, err, tc.wantErr)
		}
	}
}

func TestQuestionMessages(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	if err := s.PutQuestion(ctx, Question{ID: "q1", Token: "t1", TaskID: "task"}); err != nil {
		t.Fatalf("PutQuestion() error = %v", err)
	}
	want := []QuestionMessage{
		{QuestionID: "q1", Channel: "telegram", Conv: "1", MessageID: "10", Text: "approve?"},
		{QuestionID: "q1", Channel: "discord", Conv: "2", MessageID: "20", Text: "approve?"},
	}
	for _, m := range want {
		if err := s.AddQuestionMessage(ctx, m); err != nil {
			t.Fatalf("AddQuestionMessage() error = %v", err)
		}
	}
	got, err := s.TakeQuestionMessages(ctx, "q1")
	if err != nil {
		t.Fatalf("TakeQuestionMessages() error = %v", err)
	}
	if diff := cmp.Diff(want, got, cmpopts.SortSlices(func(a, b QuestionMessage) bool { return a.Channel < b.Channel })); diff != "" {
		t.Fatalf("TakeQuestionMessages() wrong result (-want +got) diff = %s", diff)
	}
	if got, err := s.TakeQuestionMessages(ctx, "q1"); err != nil || len(got) != 0 {
		t.Fatalf("TakeQuestionMessages() again = %v, %v, want none", got, err)
	}
}

func TestPendingQuestion(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	if _, err := s.PendingQuestion(ctx, "telegram", "1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("PendingQuestion() error = %v, want %v", err, ErrNotFound)
	}
	for _, id := range []string{"q1", "q2"} {
		if err := s.SetPendingQuestion(ctx, "telegram", "1", id); err != nil {
			t.Fatalf("SetPendingQuestion() error = %v", err)
		}
	}
	if id, err := s.PendingQuestion(ctx, "telegram", "1"); err != nil || id != "q2" {
		t.Fatalf("PendingQuestion() = %q, %v, want q2", id, err)
	}
	if err := s.ClearPendingQuestion(ctx, "telegram", "1"); err != nil {
		t.Fatalf("ClearPendingQuestion() error = %v", err)
	}
	if _, err := s.PendingQuestion(ctx, "telegram", "1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("PendingQuestion() after clearing error = %v, want %v", err, ErrNotFound)
	}
}

func TestHandles(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	h := Handle{Handle: "k3", ContextID: "c1", OnBehalfOf: "user:owner", Hops: 1}
	if err := s.PutHandle(ctx, h); err != nil {
		t.Fatalf("PutHandle() error = %v", err)
	}
	for name, dup := range map[string]Handle{
		"taken handle": {Handle: "k3", ContextID: "c2"},
		"same claims":  {Handle: "zz", ContextID: "c1", OnBehalfOf: "user:owner", Hops: 1},
	} {
		if err := s.PutHandle(ctx, dup); !errors.Is(err, ErrExists) {
			t.Fatalf("PutHandle(%s) error = %v, want %v", name, err, ErrExists)
		}
	}
	got, err := s.HandleFor(ctx, Handle{ContextID: "c1", OnBehalfOf: "user:owner", Hops: 1})
	if err != nil {
		t.Fatalf("HandleFor() error = %v", err)
	}
	if diff := cmp.Diff(h, got); diff != "" {
		t.Fatalf("HandleFor() wrong result (-want +got) diff = %s", diff)
	}
	if _, err := s.HandleFor(ctx, Handle{ContextID: "c1"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("HandleFor() of other claims error = %v, want %v", err, ErrNotFound)
	}
}

func TestTaskAttachmentsStoredOnce(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ts := s.Tasks("", testViewer)
	owner := asUser("owner")
	photo := bytes.Repeat([]byte{0xff, 0xd8, 0x01}, 4096)
	attachment := a2a.NewRawPart(photo)
	attachment.Filename, attachment.MediaType = "photo.jpg", "image/jpeg"
	task := newTask("t1", "c1")
	task.History = []*a2a.Message{a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("what is this?"), attachment)}
	if _, err := ts.Create(owner, task); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	for v := 1; v < 4; v++ {
		task.Artifacts = append(task.Artifacts, &a2a.Artifact{ID: a2a.NewArtifactID(), Parts: []*a2a.Part{a2a.NewTextPart("chunk")}})
		if _, err := ts.Update(owner, &taskstore.UpdateRequest{Task: task, PrevVersion: taskstore.TaskVersion(v)}); err != nil {
			t.Fatalf("Update() error = %v", err)
		}
	}

	var rowSize, blobs int
	if err := s.db.QueryRow(`SELECT length(data) FROM tasks WHERE id = 't1'`).Scan(&rowSize); err != nil {
		t.Fatalf("reading the task row: %v", err)
	}
	if rowSize >= len(photo) {
		t.Fatalf("task row size = %d, want less than the attachment (%d)", rowSize, len(photo))
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM blobs`).Scan(&blobs); err != nil {
		t.Fatalf("counting blobs: %v", err)
	}
	if blobs != 1 {
		t.Fatalf("blobs = %d, want 1", blobs)
	}

	got, err := ts.Get(owner, "t1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if diff := cmp.Diff(task, got.Task); diff != "" {
		t.Fatalf("Get() wrong result (-want +got) diff = %s", diff)
	}
	latest, err := ts.LatestInContext(context.Background(), "c1")
	if err != nil {
		t.Fatalf("LatestInContext() error = %v", err)
	}
	if diff := cmp.Diff(task, latest); diff != "" {
		t.Fatalf("LatestInContext() wrong result (-want +got) diff = %s", diff)
	}
	list, err := ts.List(owner, &a2a.ListTasksRequest{IncludeArtifacts: true})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if diff := cmp.Diff([]*a2a.Task{task}, list.Tasks); diff != "" {
		t.Fatalf("List() wrong result (-want +got) diff = %s", diff)
	}
}

func TestUpdateKeepsCallerTask(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ts := s.Tasks("", testViewer)
	owner := asUser("owner")
	task := newTask("t1", "c1")
	task.Status.Message = a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewRawPart([]byte("file")))
	if _, err := ts.Create(owner, task); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	want := a2a.NewRawPart([]byte("file"))
	if diff := cmp.Diff(want, task.Status.Message.Parts[0]); diff != "" {
		t.Fatalf("Create() changed the task (-want +got) diff = %s", diff)
	}
}

func TestPrune(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	start := time.Now()
	now := start
	s.now = func() time.Time { return now }
	ts := s.Tasks("", testViewer)
	owner := asUser("owner")

	finished := newTask("old", "c-old")
	finished.Status.State = a2a.TaskStateCompleted
	finished.History = []*a2a.Message{a2a.NewMessage(a2a.MessageRoleUser, a2a.NewRawPart([]byte("photo")))}
	for _, task := range []*a2a.Task{finished, newTask("running", "c-running")} {
		if _, err := ts.Create(owner, task); err != nil {
			t.Fatalf("Create() error = %v", err)
		}
	}
	for _, q := range []Question{
		{ID: "q-old", Token: "t-old", TaskID: "old", ContextID: "c-old"},
		{ID: "q-running", Token: "t-running", TaskID: "running", ContextID: "c-running"},
	} {
		if err := s.PutQuestion(ctx, q); err != nil {
			t.Fatalf("PutQuestion() error = %v", err)
		}
	}
	if err := s.SetPendingQuestion(ctx, "telegram", "1", "q-old"); err != nil {
		t.Fatalf("SetPendingQuestion() error = %v", err)
	}
	for _, h := range []Handle{{Handle: "ab", ContextID: "c-old"}, {Handle: "cd", ContextID: "c-running"}} {
		if err := s.PutHandle(ctx, h); err != nil {
			t.Fatalf("PutHandle() error = %v", err)
		}
	}
	if err := s.PutCode(ctx, "EXPIRED", Code{Purpose: "pair", Expires: start.Add(time.Minute)}); err != nil {
		t.Fatalf("PutCode() error = %v", err)
	}

	now = start.Add(taskRetention + time.Hour)
	if err := s.PutCode(ctx, "FRESH", Code{Purpose: "pair", Expires: now.Add(time.Minute)}); err != nil {
		t.Fatalf("PutCode() error = %v", err)
	}
	if err := s.Prune(ctx); err != nil {
		t.Fatalf("Prune() error = %v", err)
	}

	if _, err := ts.Get(owner, "old"); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Fatalf("Get() of a pruned task error = %v, want %v", err, a2a.ErrTaskNotFound)
	}
	if _, err := ts.Get(owner, "running"); err != nil {
		t.Fatalf("Get() of an unfinished task error = %v", err)
	}
	if _, err := s.QuestionByID(ctx, "q-old"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("QuestionByID() of a pruned task error = %v, want %v", err, ErrNotFound)
	}
	if _, err := s.QuestionByID(ctx, "q-running"); err != nil {
		t.Fatalf("QuestionByID() of an unfinished task error = %v", err)
	}
	if _, err := s.PendingQuestion(ctx, "telegram", "1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("PendingQuestion() of a pruned question error = %v, want %v", err, ErrNotFound)
	}
	if _, err := s.HandleByName(ctx, "ab"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("HandleByName() of a pruned context error = %v, want %v", err, ErrNotFound)
	}
	if _, err := s.HandleByName(ctx, "cd"); err != nil {
		t.Fatalf("HandleByName() of a live context error = %v", err)
	}
	if _, err := s.TakeCode(ctx, "EXPIRED", "pair"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("TakeCode() of an expired code error = %v, want %v", err, ErrNotFound)
	}
	if _, err := s.TakeCode(ctx, "FRESH", "pair"); err != nil {
		t.Fatalf("TakeCode() of a fresh code error = %v", err)
	}
	var blobs int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM blobs`).Scan(&blobs); err != nil {
		t.Fatalf("counting blobs: %v", err)
	}
	if blobs != 0 {
		t.Fatalf("blobs after pruning = %d, want 0", blobs)
	}
}

func TestReplaceCode(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	expires := time.Now().Add(time.Minute)
	owner := Code{Purpose: "pair", Name: "owner", Role: "owner", Expires: expires}
	if err := s.PutCode(ctx, "OTHER", Code{Purpose: "pair", Name: "grok", Role: "member", Expires: expires}); err != nil {
		t.Fatalf("PutCode() error = %v", err)
	}
	for _, code := range []string{"FIRST", "SECOND"} {
		if err := s.ReplaceCode(ctx, code, owner); err != nil {
			t.Fatalf("ReplaceCode(%s) error = %v", code, err)
		}
	}
	tests := []struct {
		code    string
		wantErr error
	}{
		{code: "FIRST", wantErr: ErrNotFound},
		{code: "SECOND"},
		{code: "OTHER"},
	}
	for _, tc := range tests {
		if _, err := s.TakeCode(ctx, tc.code, "pair"); !errors.Is(err, tc.wantErr) {
			t.Fatalf("TakeCode(%s) error = %v, want %v", tc.code, err, tc.wantErr)
		}
	}
}
