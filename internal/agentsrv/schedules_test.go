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
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-serve/internal/a2atext"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/schedule"
	"github.com/a2aproject/a2a-serve/internal/store"
)

func TestScheduleRunIsShownInChat(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{channel: newFakeChannel("fake")})
	linkOwner(t, ts, "telegram:1", "dm1")
	ctx := context.Background()
	ctl := controlapi.NewClient(ts.dir)

	info, err := ctl.AddSchedule(ctx, schedule.Request{Name: "brief", Cron: "0 7 * * 1-5", TZ: "Europe/Zurich", Prompt: "brief me", Chat: "fake"})
	if err != nil {
		t.Fatalf("AddSchedule() error = %v", err)
	}
	if info.User != auth.OwnerID || info.Chat != "fake:owner" || info.NextRun.IsZero() {
		t.Fatalf("AddSchedule() = %+v, want a schedule of the owner shown on fake", info)
	}
	if _, err := ctl.RunSchedule(ctx, "brief"); err != nil {
		t.Fatalf("RunSchedule() error = %v", err)
	}
	ts.ch.waitFor(t, "dm1", hasText(`ran Scheduled run of "brief"`))

	contextID, err := ts.srv.store.ContextOf(ctx, "fake", "dm1")
	if err != nil {
		t.Fatalf("ContextOf() error = %v", err)
	}
	last, err := ts.srv.tasks.LatestInContext(ctx, contextID)
	if err != nil {
		t.Fatalf("LatestInContext() error = %v", err)
	}
	if got := a2atext.Message(last.History[0]); !strings.Contains(got, "brief me") {
		t.Fatalf("the chat's conversation has %q, want the scheduled run, so that replying continues it", got)
	}
}

func TestScheduleApprovalGoesToItsChat(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{channel: newFakeChannel("fake")})
	linkOwner(t, ts, "telegram:1", "dm1")
	linkAs(t, ts, "anna", auth.RoleAuthenticated, "telegram:2", "dm2")
	ctx := context.Background()
	ctl := controlapi.NewClient(ts.dir)
	if _, err := ctl.AddSchedule(ctx, schedule.Request{Name: "push", Cron: "@daily", Prompt: "ask", As: "anna", Chat: "fake:anna"}); err != nil {
		t.Fatalf("AddSchedule() error = %v", err)
	}

	if _, err := ctl.RunSchedule(ctx, "push"); err != nil {
		t.Fatalf("RunSchedule() error = %v", err)
	}

	ref, q := ts.ch.waitFor(t, "dm2", hasQuestion)
	if got := ts.ch.questions(); got != 1 {
		t.Fatalf("questions sent = %d, want only the one in anna's chat: she is the schedule's user", got)
	}
	ts.ch.click("telegram:2", "dm2", ref, q.Question, 0)
	ts.ch.waitFor(t, "dm2", hasText("Pushed (allow)."))
}

func TestScheduleRunsWaitingForAnApprovalAreMerged(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{channel: newFakeChannel("fake")})
	linkOwner(t, ts, "telegram:1", "dm1")
	ctx := context.Background()
	ctl := controlapi.NewClient(ts.dir)
	if _, err := ctl.AddSchedule(ctx, schedule.Request{Name: "brief", Cron: "@daily", Prompt: "brief me", Chat: "fake", Thread: schedule.ThreadSame}); err != nil {
		t.Fatalf("AddSchedule() error = %v", err)
	}
	ts.ch.write("telegram:1", "dm1", "ask")
	ref, q := ts.ch.waitFor(t, "dm1", hasQuestion)

	for range 3 {
		if _, err := ctl.RunSchedule(ctx, "brief"); err != nil {
			t.Fatalf("RunSchedule() error = %v", err)
		}
	}
	ts.ch.click("telegram:1", "dm1", ref, q.Question, 0)

	ts.ch.waitFor(t, "dm1", hasText("Pushed (allow)."))
	ts.ch.waitFor(t, "dm1", hasText("Runs skipped while this one waited for an answer in this conversation: 2."))
	time.Sleep(200 * time.Millisecond)
	runs := 0
	for _, m := range ts.ch.transcript() {
		if strings.Contains(m.Text, `ran Scheduled run of "brief"`) {
			runs++
		}
	}
	if got := runs; got != 1 {
		t.Fatalf("scheduled runs shown = %d, want 1; messages:\n%s", got, ts.ch.dump())
	}
}

func TestAddScheduleErrors(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{channel: newFakeChannel("fake")})
	linkOwner(t, ts, "telegram:1", "dm1")
	ctx := context.Background()
	ctl := controlapi.NewClient(ts.dir)
	if _, err := ctl.AddSchedule(ctx, schedule.Request{Name: "taken", Cron: "@daily", Prompt: "x"}); err != nil {
		t.Fatalf("AddSchedule() error = %v", err)
	}
	tests := []struct {
		name string
		req  schedule.Request
	}{
		{name: "existing name", req: schedule.Request{Name: "taken", Cron: "@daily", Prompt: "x"}},
		{name: "time passed", req: schedule.Request{Name: "late", At: "2020-01-01 10:00", Prompt: "x"}},
		{name: "unknown user", req: schedule.Request{Name: "x", Cron: "@daily", Prompt: "x", As: "nobody"}},
		{name: "no chat on the channel", req: schedule.Request{Name: "x", Cron: "@daily", Prompt: "x", Chat: "telegram"}},
		{name: "invalid cron", req: schedule.Request{Name: "x", Cron: "every day", Prompt: "x"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ctl.AddSchedule(ctx, tc.req); err == nil {
				t.Fatalf("AddSchedule(%+v) error = nil, want error", tc.req)
			}
		})
	}
}

func TestHarnessSchedulesInConversation(t *testing.T) {
	t.Parallel()
	var skew atomic.Int64
	now := func() time.Time { return time.Now().Add(time.Duration(skew.Load())) }
	ts := startServer(t, serverOpts{channel: newFakeChannel("fake"), now: now})
	linkOwner(t, ts, "telegram:1", "dm1")
	at := time.Now().Add(time.Hour).Format(time.RFC3339Nano)

	ts.ch.write("telegram:1", "dm1", fmt.Sprintf(`call schedules add soon --at %s --prompt "check the build"`, at))
	ts.ch.waitFor(t, "dm1", hasText("Scheduled soon"))
	ts.ch.write("telegram:1", "dm1", "call schedules list")
	ts.ch.waitFor(t, "dm1", hasText("- soon (once"))
	skew.Store(int64(time.Hour))
	ts.srv.schedules.changed()
	ts.ch.waitFor(t, "dm1", hasText(`ran Scheduled run of "soon"`))

	deadline := time.Now().Add(10 * time.Second)
	for {
		_, err := ts.srv.store.Schedule(context.Background(), "soon")
		if errors.Is(err, store.ErrNotFound) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Schedule(soon) after its run error = %v, want %v", err, store.ErrNotFound)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestHarnessScheduleIsAnnouncedInChat(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{channel: newFakeChannel("fake")})
	linkOwner(t, ts, "telegram:1", "dm1")

	ts.ch.write("telegram:1", "dm1", `call schedules add triage --cron "@every 2h" --prompt "triage new issues"`)

	ts.ch.waitFor(t, "dm1", hasText(`The agent added the schedule triage (@every 2h): "triage new issues". The next run is`))
}

func TestHarnessSchedulesOfOthersAreHidden(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{channel: newFakeChannel("fake")})
	linkAs(t, ts, "anna", auth.RoleAuthenticated, "telegram:2", "dm2")
	ctx := context.Background()
	if _, err := controlapi.NewClient(ts.dir).AddSchedule(ctx, schedule.Request{Name: "brief", Cron: "@daily", Prompt: "x"}); err != nil {
		t.Fatalf("AddSchedule() error = %v", err)
	}

	ts.ch.write("telegram:2", "dm2", "call schedules list")
	ts.ch.waitFor(t, "dm2", hasText("No schedules."))
	ts.ch.write("telegram:2", "dm2", "call schedules rm brief")
	ts.ch.waitFor(t, "dm2", hasText("no schedule brief"))
	ts.ch.write("telegram:2", "dm2", `call schedules add mine --cron @daily --prompt "x"`)
	ts.ch.waitFor(t, "dm2", hasText("Scheduled mine"))

	mine, err := ts.srv.store.Schedule(ctx, "mine")
	if err != nil {
		t.Fatalf("Schedule(mine) error = %v", err)
	}
	if mine.UserID != "pair:anna" || mine.Conversation != schedule.ThreadSame || mine.ContextID == "" {
		t.Fatalf("Schedule(mine) = %+v, want it to run as anna in her conversation", mine)
	}
	if _, err := ts.srv.store.Schedule(ctx, "brief"); err != nil {
		t.Fatalf("Schedule(brief) error = %v, want the owner's schedule kept", err)
	}
}

func TestMissedScheduleRunsOnStart(t *testing.T) {
	t.Parallel()
	d, err := os.MkdirTemp("", "a2a")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(d); err != nil {
			t.Errorf("removing %s: %v", d, err)
		}
	})
	dir, err := agentdir.Open(d)
	if err != nil {
		t.Fatal(err)
	}
	if err := dir.Init(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(dir.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	missed := time.Now().Add(-time.Hour)
	if err := st.AddSchedule(context.Background(), store.Schedule{
		Name: "hourly", Cron: "@hourly", Prompt: "x", UserID: auth.OwnerID, Conversation: schedule.ThreadNew,
		CreatedBy: auth.OwnerID, NextRun: missed,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	ts := startServer(t, serverOpts{dir: d})
	deadline := time.Now().Add(10 * time.Second)
	for {
		sc, err := ts.srv.store.Schedule(context.Background(), "hourly")
		if err != nil {
			t.Fatalf("Schedule() error = %v", err)
		}
		if !sc.LastRun.IsZero() {
			if !sc.NextRun.After(time.Now()) {
				t.Fatalf("Schedule() after the missed run = %+v, want the next run in the future", sc)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Schedule() = %+v, want the missed run to have run", sc)
		}
		time.Sleep(20 * time.Millisecond)
	}
	for {
		tasks, err := ts.srv.tasks.TaskCounts(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(tasks) > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("TaskCounts() = %v, want the run's task", tasks)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestFailedOneTimeScheduleIsKept(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	ctx := context.Background()
	due := time.Now().Add(-time.Minute)
	if err := ts.srv.store.AddSchedule(ctx, store.Schedule{
		Name: "once", At: due, Prompt: "x", UserID: auth.OwnerID, Conversation: schedule.ThreadNew,
		DeliverChannel: "gone", DeliverUser: auth.OwnerID, CreatedBy: auth.OwnerID, NextRun: due,
	}); err != nil {
		t.Fatalf("AddSchedule() error = %v", err)
	}

	ts.srv.schedules.runDue(ctx)

	got, err := ts.srv.store.Schedule(ctx, "once")
	if err != nil {
		t.Fatalf("Schedule() after a failed run error = %v, want the schedule kept", err)
	}
	if !strings.Contains(got.LastError, "channel gone is not running") || !got.NextRun.IsZero() || got.LastRun.IsZero() {
		t.Fatalf("Schedule() = %+v, want the error recorded and no next run", got)
	}
}

func TestScheduleDoesNotRunWhenItsNextRunIsNotSaved(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	ctx := context.Background()
	// An immediate transaction waits for the server's writers; a deferred
	// one fails with SQLITE_BUSY when one commits before it writes.
	db, err := sql.Open("sqlite", "file:"+ts.dir.DBPath()+"?_pragma=busy_timeout(5000)&_txlock=immediate")
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx() error = %v", err)
	}
	if _, err := tx.Exec(`CREATE TRIGGER no_updates BEFORE UPDATE ON schedules BEGIN SELECT RAISE(ABORT, 'read only'); END`); err != nil {
		t.Fatalf("creating the trigger: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}

	due := time.Now().Add(-time.Hour)
	if err := ts.srv.store.AddSchedule(ctx, store.Schedule{
		Name: "hourly", Cron: "@hourly", Prompt: "x", UserID: auth.OwnerID, Conversation: schedule.ThreadNew,
		CreatedBy: auth.OwnerID, NextRun: due,
	}); err != nil {
		t.Fatalf("AddSchedule() error = %v", err)
	}

	ts.srv.schedules.runDue(ctx)

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		counts, err := ts.srv.tasks.TaskCounts(ctx)
		if err != nil {
			t.Fatalf("TaskCounts() error = %v", err)
		}
		if len(counts) > 0 {
			t.Fatalf("TaskCounts() = %v, want no run when the next run can't be saved", counts)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
