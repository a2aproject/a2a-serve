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
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/schedule"
	"github.com/a2aproject/a2a-serve/internal/store"
)

func TestScheduleRunIsShownInChat(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{channel: newFakeChannel("fake")})
	linkOwner(t, ts, "tg:1", "dm1")
	ctx := context.Background()
	ctl := controlapi.NewClient(ts.dir)

	info, err := ctl.AddSchedule(ctx, schedule.Request{Name: "brief", Cron: "0 7 * * 1-5", TZ: "Europe/Zurich", Prompt: "brief me", Deliver: "fake"})
	if err != nil {
		t.Fatalf("AddSchedule() error = %v", err)
	}
	if info.User != auth.OwnerID || info.Deliver != "fake:owner" || info.NextRun.IsZero() {
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
	if got := messageText(last.History[0]); !strings.Contains(got, "brief me") {
		t.Fatalf("the chat's conversation has %q, want the scheduled run, so that replying continues it", got)
	}
}

func TestAddScheduleErrors(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{channel: newFakeChannel("fake")})
	linkOwner(t, ts, "tg:1", "dm1")
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
		{name: "no chat on the channel", req: schedule.Request{Name: "x", Cron: "@daily", Prompt: "x", Deliver: "telegram"}},
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
	ts := startServer(t, serverOpts{channel: newFakeChannel("fake")})
	linkOwner(t, ts, "tg:1", "dm1")
	at := time.Now().Add(2 * time.Second).Format(time.RFC3339)

	ts.ch.write("tg:1", "dm1", fmt.Sprintf(`call schedules add soon --at %s --prompt "check the build"`, at))
	ts.ch.waitFor(t, "dm1", hasText("Scheduled soon"))
	ts.ch.write("tg:1", "dm1", "call schedules list")
	ts.ch.waitFor(t, "dm1", hasText("- soon (once"))
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

func TestHarnessSchedulesOfOthersAreHidden(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{channel: newFakeChannel("fake")})
	linkAs(t, ts, "anna", auth.RoleMember, "tg:2", "dm2")
	ctx := context.Background()
	if _, err := controlapi.NewClient(ts.dir).AddSchedule(ctx, schedule.Request{Name: "brief", Cron: "@daily", Prompt: "x"}); err != nil {
		t.Fatalf("AddSchedule() error = %v", err)
	}

	ts.ch.write("tg:2", "dm2", "call schedules list")
	ts.ch.waitFor(t, "dm2", hasText("No schedules."))
	ts.ch.write("tg:2", "dm2", "call schedules rm brief")
	ts.ch.waitFor(t, "dm2", hasText("no schedule brief"))
	ts.ch.write("tg:2", "dm2", `call schedules add mine --cron @daily --prompt "x"`)
	ts.ch.waitFor(t, "dm2", hasText("Scheduled mine"))

	mine, err := ts.srv.store.Schedule(ctx, "mine")
	if err != nil {
		t.Fatalf("Schedule(mine) error = %v", err)
	}
	if mine.UserID != "pair:anna" || mine.Conversation != schedule.ConversationSame || mine.ContextID == "" {
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
		Name: "hourly", Cron: "@hourly", Prompt: "x", UserID: auth.OwnerID, Conversation: schedule.ConversationNew,
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
