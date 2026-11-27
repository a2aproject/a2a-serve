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
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agentkey"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/store"
)

func TestRestartPrunesMemberHomesWithoutTasks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts := serverOpts{
		configure: func(c *agentdir.Config) {
			c.Policy.AllowEmails = []string{"anna@example.com"}
			c.TaskTTL = "1h"
		},
	}
	ts := startServer(t, opts)
	const email = "anna@example.com"
	const token = "a2p_google"
	id := auth.EmailPrefix + email
	if err := ts.srv.store.PutToken(ctx, token, store.Token{Email: email, URL: ts.localURL()}, ""); err != nil {
		t.Fatalf("PutToken() error = %v", err)
	}
	c := ts.client(token)
	through := func(caller string) *a2a.Task {
		t.Helper()
		callerCtx := a2aclient.AttachServiceParams(ctx, a2aclient.ServiceParams{
			auth.HeaderHops: {"1"}, auth.HeaderCaller: {caller},
		})
		res, err := c.SendMessage(callerCtx, &a2a.SendMessageRequest{Message: userText("hello", "", "")})
		if err != nil {
			t.Fatalf("SendMessage() for %q error = %v", caller, err)
		}
		task, ok := res.(*a2a.Task)
		if !ok || task.Status.State != a2a.TaskStateCompleted {
			t.Fatalf("SendMessage() for %q = %v, want a completed task", caller, res)
		}
		return task
	}
	callerHome := func(caller string) string {
		return ts.dir.CallerHomeDir(id, agentkey.CallerOf(agentkey.Caller(id, caller)))
	}

	old := send(t, c, userText("hello", "", ""))
	oldCaller := through("old-person")
	paired := ts.client(ts.pair("paired", auth.RoleMember))
	pairedTask := send(t, paired, userText("hello", "", ""))
	recent := through("recent-person")
	if err := ts.srv.store.PutNode(ctx, store.Node{ID: "webhook", Name: "Webhook"}); err != nil {
		t.Fatalf("PutNode() error = %v", err)
	}

	expired := []string{
		ts.dir.UserHomeDir(id),
		callerHome("old-person"),
	}
	orphan := ts.dir.UserHomeDir(auth.EmailPrefix + "orphan@example.com")
	webhook := ts.dir.UserHomeDir(auth.NodePrincipal("webhook"))
	for _, home := range []string{orphan, webhook} {
		if err := os.MkdirAll(home, 0o700); err != nil {
			t.Fatalf("MkdirAll(%q) error = %v", home, err)
		}
	}
	expired = append(expired, orphan)
	kept := []string{
		callerHome("recent-person"),
		ts.dir.UserHomeDir(auth.PairPrincipal("paired")),
		webhook,
	}
	for _, home := range append(expired, kept...) {
		if info, err := os.Stat(home); err != nil || !info.IsDir() {
			t.Fatalf("Stat(%q) before restart = %v, %v, want an agent home", home, info, err)
		}
		if err := os.WriteFile(filepath.Join(home, "saved"), []byte("fixture"), 0o600); err != nil {
			t.Fatalf("WriteFile() in %q error = %v", home, err)
		}
	}

	opts.dir = ts.dir.Root
	ts.stop()
	ageHomeTasks(t, ts.dir, old.ID, oldCaller.ID, pairedTask.ID)
	ts = startServer(t, opts)
	c = ts.client(token)
	waitFor(t, "startup task pruning", func() bool {
		_, err := c.GetTask(ctx, &a2a.GetTaskRequest{ID: old.ID})
		return errors.Is(err, a2a.ErrTaskNotFound)
	})
	waitFor(t, "member home pruning", func() bool {
		for _, home := range expired {
			if _, err := os.Stat(home); !errors.Is(err, os.ErrNotExist) {
				return false
			}
		}
		return true
	})
	waitFor(t, "retired homes to be removed", func() bool {
		_, err := os.Stat(filepath.Join(ts.dir.StateDir(), "pruned-homes"))
		return errors.Is(err, os.ErrNotExist)
	})
	if _, err := c.GetTask(ctx, &a2a.GetTaskRequest{ID: oldCaller.ID}); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Fatalf("GetTask() for expired caller error = %v, want %v", err, a2a.ErrTaskNotFound)
	}
	for _, home := range kept {
		if contents, err := os.ReadFile(filepath.Join(home, "saved")); err != nil || string(contents) != "fixture" {
			t.Fatalf("ReadFile() in %q = %q, %v, want fixture", home, contents, err)
		}
	}
	if _, err := c.GetTask(ctx, &a2a.GetTaskRequest{ID: recent.ID}); err != nil {
		t.Fatalf("GetTask() for recent caller error = %v", err)
	}
	restarted := send(t, c, userText("hello", "", ""))
	wantResult(t, restarted, a2a.TaskStateCompleted, "Hello from fakeacp.")
	if _, err := os.Stat(filepath.Join(expired[0], "saved")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stat() of the old home marker error = %v, want %v", err, os.ErrNotExist)
	}

	key := agentkey.User(id)
	ageHomeTasks(t, ts.dir, restarted.ID)
	if err := ts.srv.store.Prune(ctx); err != nil {
		t.Fatalf("Prune() error = %v", err)
	}
	if err := ts.srv.pruneMemberHomes(ctx); err != nil {
		t.Fatalf("pruneMemberHomes() with a running process error = %v", err)
	}
	if _, err := os.Stat(expired[0]); err != nil {
		t.Fatalf("Stat() of the running member home error = %v", err)
	}
	waitFor(t, "the member process to become idle and stop", func() bool {
		ok, err := ts.srv.engine.StopIdle(key)
		if err != nil {
			t.Fatalf("StopIdle() error = %v", err)
		}
		return ok
	})
	if err := ts.srv.pruneMemberHomes(ctx); err != nil {
		t.Fatalf("pruneMemberHomes() after the process stopped error = %v", err)
	}
	if _, err := os.Stat(expired[0]); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stat() of the stopped member home error = %v, want %v", err, os.ErrNotExist)
	}
}

func ageHomeTasks(t *testing.T, dir agentdir.Dir, ids ...a2a.TaskID) {
	t.Helper()
	db, err := sql.Open("sqlite", dir.DBPath()+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	}()
	for _, id := range ids {
		if _, err := db.ExecContext(t.Context(), "UPDATE tasks SET updated = ? WHERE id = ?", time.Now().Add(-2*time.Hour).UnixNano(), id); err != nil {
			t.Fatalf("aging completed fixture task %v: %v", id, err)
		}
	}
}
