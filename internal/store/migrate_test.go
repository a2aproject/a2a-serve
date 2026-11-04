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
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
)

// prototypeSchema is the schema of databases created before user_version
// was kept, with the kv table that held handles and pending questions.
const prototypeSchema = `
CREATE TABLE kv (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE users (id TEXT PRIMARY KEY, roles TEXT NOT NULL, created INTEGER NOT NULL);
CREATE TABLE links (
	channel TEXT NOT NULL, identity TEXT NOT NULL, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	conv TEXT NOT NULL DEFAULT '', created INTEGER NOT NULL, PRIMARY KEY (channel, identity));
CREATE TABLE questions (
	id TEXT PRIMARY KEY, token TEXT NOT NULL UNIQUE, task_id TEXT NOT NULL, context_id TEXT NOT NULL,
	contact TEXT NOT NULL DEFAULT '', data TEXT NOT NULL, created INTEGER NOT NULL,
	answered_by TEXT NOT NULL DEFAULT '', answer TEXT NOT NULL DEFAULT '');
INSERT INTO kv VALUES ('handle:k3', '{"c":"ctx1","u":"user:owner","h":1}');
INSERT INTO kv VALUES ('handle-of:{"c":"ctx1","u":"user:owner","h":1}', 'k3');
INSERT INTO kv VALUES ('pending:telegram:42:7', 'q1');
INSERT INTO kv VALUES ('pending:email:a@b', '');
INSERT INTO kv VALUES ('email:lastuid', '17');
INSERT INTO questions (id, token, task_id, context_id, contact, data, created) VALUES
	('q1', 't1', 'task', 'ctx1', 'peer', '{"id":"tool1"}', 1),
	('q2', 't2', 'task', 'ctx1', 'peer', '{"id":"tool1"}', 2);
`

func TestMigrateFromPrototype(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "server.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	if _, err := db.Exec(prototypeSchema); err != nil {
		t.Fatalf("creating the prototype schema: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	ctx := context.Background()
	if _, err := s.HandleByName(ctx, "k3"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("HandleByName() of a handle with a user error = %v, want %v", err, ErrNotFound)
	}
	if id, err := s.PendingQuestion(ctx, "telegram", "42:7"); err != nil || id != "q1" {
		t.Fatalf("PendingQuestion() = %q, %v, want q1", id, err)
	}
	if _, err := s.PendingQuestion(ctx, "email", "a@b"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("PendingQuestion() of a cleared question error = %v, want %v", err, ErrNotFound)
	}
	q, err := s.QuestionByID(ctx, "q1")
	if err != nil || q.HitlID != "tool1" {
		t.Fatalf("QuestionByID() = %+v, %v, want hitl ID tool1", q, err)
	}
	if _, err := s.QuestionByID(ctx, "q2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("QuestionByID() of a duplicate remote question error = %v, want %v", err, ErrNotFound)
	}
	if v, ok, err := s.ChannelState().Get(ctx, "email:lastuid"); err != nil || !ok || v != "17" {
		t.Fatalf("ChannelState().Get() = %q, %v, %v, want 17", v, ok, err)
	}
	if _, ok, err := s.ChannelState().Get(ctx, "handle:k3"); err != nil || ok {
		t.Fatalf("ChannelState().Get() of a moved handle = %v, %v, want not set", ok, err)
	}
}

func TestOpenNewerDatabaseFails(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "server.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 99`); err != nil {
		t.Fatalf("setting user_version: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "newer than this a2a-serve supports") {
		t.Fatalf("Open() error = %v, want a newer version error naming a2a-serve", err)
	}
}

func TestReopenKeepsData(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "server.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	ctx := context.Background()
	if err := s.PutHandle(ctx, Handle{Handle: "ab", ContextID: "c"}); err != nil {
		t.Fatalf("PutHandle() error = %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatalf("Open() again error = %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	if _, err := s.HandleByName(ctx, "ab"); err != nil {
		t.Fatalf("HandleByName() after reopening error = %v", err)
	}
}

func TestMigrateTasksToEvents(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "server.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	ctx := context.Background()
	old := &Store{db: db, now: time.Now}
	for v := range 7 {
		if err := old.runMigration(ctx, v); err != nil {
			t.Fatalf("runMigration(%d) error = %v", v, err)
		}
	}
	task := newTask("t1", "c1")
	task.History = []*a2a.Message{a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi"))}
	task.Artifacts = []*a2a.Artifact{{ID: "a1", Parts: []*a2a.Part{a2a.NewTextPart("result")}}}
	data, err := json.Marshal(task)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if _, err := db.Exec(`INSERT INTO tasks (id, scope, context_id, owner, version, state, status_time, updated, data)
		VALUES ('t1', '', 'c1', 'alice', 3, 'TASK_STATE_WORKING', 0, 1, ?)`, string(data)); err != nil {
		t.Fatalf("inserting a task: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	got, err := s.Tasks("", testViewer).Get(asUser("alice"), "t1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	want := &taskstore.StoredTask{Task: task, Version: 3, User: "alice"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("Get() wrong result (-want +got) diff = %s", diff)
	}
	var header string
	if err := s.db.QueryRow(`SELECT data FROM tasks WHERE id = 't1'`).Scan(&header); err != nil {
		t.Fatalf("reading the task row: %v", err)
	}
	if strings.Contains(header, "history") || strings.Contains(header, "artifacts") {
		t.Fatalf("task row = %s, want no history and artifacts", header)
	}
}
