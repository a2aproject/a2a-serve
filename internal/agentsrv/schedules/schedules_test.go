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

package schedules

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/messenger"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/channel"
	"github.com/a2aproject/a2a-serve/internal/schedule"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// fakeTurns records the turns schedules post, which tests accept or refuse.
type fakeTurns struct {
	mu     sync.Mutex
	posted []messenger.PostedTurn
}

func (f *fakeTurns) InjectTurn(_ context.Context, t messenger.PostedTurn) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.posted = append(f.posted, t)
	return nil
}

func (f *fakeTurns) DropPosted(string) {}

func (f *fakeTurns) ContextOf(context.Context, string, channel.ConvKey) (string, error) {
	return "", errors.New("no chats")
}

// turns returns the turns posted so far.
func (f *fakeTurns) turns() []messenger.PostedTurn {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.posted)
}

// noChannels is a server without running channels.
type noChannels struct{}

func (noChannels) Channel(string) channel.Channel { return nil }

type schedulesFixture struct {
	ctx   context.Context
	store *store.Store
	turns *fakeTurns
	sc    *Runner
}

func newSchedulesFixture(t *testing.T) *schedulesFixture {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	chain := &auth.Chain{Store: st, Policy: func() auth.Policy { return auth.Policy{} }}
	turns := &fakeTurns{}
	sc := New(t.Context(), Deps{Store: st, Turns: turns, Supervisor: noChannels{}, Chain: chain, Now: time.Now})
	return &schedulesFixture{ctx: t.Context(), store: st, turns: turns, sc: sc}
}

func (fx *schedulesFixture) addSchedule(t *testing.T, s store.Schedule) {
	t.Helper()
	s.Prompt, s.UserID, s.CreatedBy = "x", auth.OwnerID, auth.OwnerID
	s.Conversation, s.ContextID = schedule.ThreadSame, "c-schedule"
	if err := fx.store.AddSchedule(fx.ctx, s); err != nil {
		t.Fatalf("AddSchedule() error = %v", err)
	}
}

func (fx *schedulesFixture) schedule(t *testing.T, name string) store.Schedule {
	t.Helper()
	s, err := fx.store.Schedule(fx.ctx, name)
	if err != nil {
		t.Fatalf("Schedule(%s) error = %v", name, err)
	}
	return s
}

func TestSchedulesOneTimeRunEndsWhenItsTurnIsAccepted(t *testing.T) {
	t.Parallel()
	fx := newSchedulesFixture(t)
	due := time.Now().Add(-time.Minute).Truncate(time.Second)
	fx.addSchedule(t, store.Schedule{Name: "once", At: due, NextRun: due})

	fx.sc.RunDue(fx.ctx)
	fx.sc.RunDue(fx.ctx)

	posted := fx.turns.turns()
	if len(posted) != 1 {
		t.Fatalf("turns posted while the first waits = %d, want 1", len(posted))
	}
	if got := fx.schedule(t, "once"); !got.NextRun.Equal(due) {
		t.Fatalf("Schedule() while its turn is held = %+v, want it still due at %v", got, due)
	}
	posted[0].Accept(nil)
	if _, err := fx.store.Schedule(fx.ctx, "once"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Schedule() after its turn was accepted error = %v, want %v", err, store.ErrNotFound)
	}
}

func TestSchedulesRecordErrorHints(t *testing.T) {
	t.Parallel()
	fx := newSchedulesFixture(t)
	due := time.Now().Add(-time.Minute).Truncate(time.Second)
	fx.addSchedule(t, store.Schedule{Name: "hourly", Cron: "@hourly", NextRun: due})

	fx.sc.RunDue(fx.ctx)
	fx.turns.turns()[0].Accept(agenterr.WithHint(agenterr.Invalidf("anna has no chat on telegram"), agenterr.HintLinkChat, ""))

	want := "anna has no chat on telegram: link it with a code from `a2a-serve pair invite`"
	if got := fx.schedule(t, "hourly").LastError; got != want {
		t.Fatalf("Schedule().LastError = %q, want %q", got, want)
	}
}

func TestSchedulesRecordFailedSends(t *testing.T) {
	t.Parallel()
	due := time.Now().Add(-time.Minute).Truncate(time.Second)
	tests := []struct {
		name      string
		sched     store.Schedule
		wantRetry bool
	}{
		{name: "one-time", sched: store.Schedule{Name: "once", At: due, NextRun: due}, wantRetry: true},
		{name: "recurring", sched: store.Schedule{Name: "hourly", Cron: "@hourly", NextRun: due}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fx := newSchedulesFixture(t)
			fx.addSchedule(t, tc.sched)

			fx.sc.RunDue(fx.ctx)
			fx.turns.turns()[0].Accept(errors.New("the agent is not running"))

			if got := fx.schedule(t, tc.sched.Name).LastError; !strings.Contains(got, "the agent is not running") {
				t.Fatalf("Schedule().LastError after a failed send = %q, want the failure", got)
			}
			if due := !fx.schedule(t, tc.sched.Name).NextRun.After(time.Now()); due != tc.wantRetry {
				t.Fatalf("Schedule() after a failed send is due = %v, want %v", due, tc.wantRetry)
			}
			fx.sc.RunDue(fx.ctx)
			want := 1
			if tc.wantRetry {
				want = 2
			}
			if got := len(fx.turns.turns()); got != want {
				t.Fatalf("turns posted after the next check = %d, want %d", got, want)
			}
		})
	}
}
