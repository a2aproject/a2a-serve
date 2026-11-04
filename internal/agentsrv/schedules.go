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
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/channel"
	"github.com/a2aproject/a2a-serve/internal/manage"
	"github.com/a2aproject/a2a-serve/internal/schedule"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// maxScheduleWait is the longest the scheduler sleeps between checks.
const maxScheduleWait = time.Minute

// schedules runs the agent's schedules, and adds and removes them for the
// owner and for the harness.
type schedules struct {
	// ctx ends when the server stops; runs happen in it.
	ctx    context.Context
	store  *store.Store
	router *Router
	// chain gives the user a schedule runs as its role at each run.
	chain *auth.Chain
	now   func() time.Time
	// minWait is the shortest wait between checks of the schedules.
	minWait time.Duration
	// wake makes the scheduler check the schedules again after a change.
	wake chan struct{}
}

func newSchedules(ctx context.Context, st *store.Store, router *Router, chain *auth.Chain, now func() time.Time, minWait time.Duration) *schedules {
	return &schedules{ctx: ctx, store: st, router: router, chain: chain, now: now, minWait: minWait, wake: make(chan struct{}, 1)}
}

// run runs due schedules until ctx ends. A run missed while the agent was
// stopped runs once when it starts.
func (sc *schedules) run(ctx context.Context) {
	for {
		wait := sc.runDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-sc.wake:
		case <-time.After(wait):
		}
	}
}

func (sc *schedules) changed() {
	select {
	case sc.wake <- struct{}{}:
	default:
	}
}

// runDue runs the schedules which are due and returns how long until the
// next one.
func (sc *schedules) runDue(ctx context.Context) time.Duration {
	wait := maxScheduleWait
	all, err := sc.store.Schedules(ctx)
	if err != nil {
		slog.Error("listing schedules", "error", err)
		return wait
	}
	now := sc.now()
	for _, s := range all {
		if s.NextRun.IsZero() {
			continue
		}
		if s.NextRun.After(now) {
			wait = min(wait, s.NextRun.Sub(now))
			continue
		}
		next, ok, nextErr := whenOf(s).Next(now)
		if nextErr != nil || !ok {
			next = time.Time{}
		}
		// A run whose next time isn't saved would repeat on every check.
		if err := sc.store.SetScheduleNextRun(ctx, s.Name, next); err != nil {
			slog.Error("saving the next run of a schedule", "schedule", s.Name, "error", err)
			continue
		}
		runErr := sc.start(ctx, s)
		if runErr == nil && nextErr == nil && !ok {
			if err := sc.store.DeleteSchedule(ctx, s.Name); err != nil && !errors.Is(err, store.ErrNotFound) {
				slog.Error("removing a finished schedule", "schedule", s.Name, "error", err)
			}
			continue
		}
		if err := sc.store.RecordScheduleResult(ctx, s.Name, now, errText(cmp.Or(runErr, nextErr))); err != nil {
			slog.Error("recording a schedule run", "schedule", s.Name, "error", err)
		}
		if !next.IsZero() {
			wait = min(wait, next.Sub(now))
		}
	}
	return max(wait, sc.minWait)
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func whenOf(s store.Schedule) schedule.When {
	return schedule.When{Cron: s.Cron, At: s.At, TZ: s.TZ}
}

// start posts a run of s into its conversation as a new turn. The turn runs
// in the background, as an ordinary task of the schedule's user, with the
// role the user has now.
func (sc *schedules) start(ctx context.Context, s store.Schedule) error {
	user, err := storedPrincipal(ctx, sc.chain, s.UserID, s.UserIdentity)
	if err == nil && user.Role == "" {
		err = fmt.Errorf("%s can no longer call this agent", auth.DisplayPrincipal(s.UserID))
	}
	if err != nil {
		slog.Warn("running a schedule", "schedule", s.Name, "error", err)
		return err
	}
	contextID, err := sc.contextFor(ctx, s)
	if err != nil {
		slog.Warn("running a schedule", "schedule", s.Name, "error", err)
		return err
	}
	text := fmt.Sprintf("Scheduled run of %q (no one is waiting at the keyboard; your answer is delivered to the user):\n%s", s.Name, s.Prompt)
	if err := sc.router.injectTurn(sc.ctx, contextID, user, text); err != nil {
		slog.Warn("running a schedule", "schedule", s.Name, "error", err)
		return err
	}
	return nil
}

// contextFor returns the context a run of s happens in. A run delivered to
// a chat becomes the chat's conversation, so replying there continues it.
func (sc *schedules) contextFor(ctx context.Context, s store.Schedule) (string, error) {
	if s.DeliverChannel == "" {
		if s.Conversation == schedule.ThreadSame && s.ContextID != "" {
			return s.ContextID, nil
		}
		return a2a.NewContextID(), nil
	}
	if sc.router.channel(s.DeliverChannel) == nil {
		return "", fmt.Errorf("channel %s is not running", s.DeliverChannel)
	}
	conv, err := manage.DirectConv(ctx, sc.store, s.DeliverChannel, s.DeliverUser)
	if err != nil {
		return "", err
	}
	if s.Conversation == schedule.ThreadSame {
		return sc.router.contextOf(ctx, s.DeliverChannel, channel.ConvKey(conv))
	}
	id := a2a.NewContextID()
	return id, sc.store.SetContext(ctx, s.DeliverChannel, conv, id)
}

// storedPrincipal is a principal stored to act later, such as the user a
// schedule runs as, with the role it has now. identity is its stored
// identity as JSON, or empty when the ID says it all. A principal the
// policy refuses now has no role.
func storedPrincipal(ctx context.Context, chain *auth.Chain, id, identity string) (auth.Principal, error) {
	var ident auth.Identity
	if identity != "" {
		if err := json.Unmarshal([]byte(identity), &ident); err != nil {
			return auth.Principal{}, fmt.Errorf("reading the identity of %s: %w", id, err)
		}
	}
	p, err := chain.Refresh(ctx, id, ident)
	var refused *auth.RefusedError
	if errors.As(err, &refused) || errors.Is(err, auth.ErrNoCredential) {
		return auth.Principal{ID: id, Identity: ident}, nil
	}
	return p, err
}

// add creates a schedule made by by, in the conversation contextID if set.
func (sc *schedules) add(ctx context.Context, req schedule.Request, by auth.Principal, contextID string) (store.Schedule, error) {
	return sc.manage().Add(ctx, req, by, contextID)
}

func (sc *schedules) manage() manage.Schedules {
	return manage.Schedules{Store: sc.store, Now: sc.now, Changed: sc.changed}
}

// runNow starts a run of a schedule at once, without changing when it runs
// next.
func (sc *schedules) runNow(ctx context.Context, name string) (store.Schedule, error) {
	s, err := sc.store.Schedule(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return store.Schedule{}, &agenterr.NotFoundError{Kind: "schedule", Name: name}
	}
	if err != nil {
		return store.Schedule{}, err
	}
	runErr := sc.start(ctx, s)
	s.LastRun, s.LastError = sc.now(), errText(runErr)
	if err := sc.store.RecordScheduleResult(ctx, s.Name, s.LastRun, s.LastError); err != nil {
		return store.Schedule{}, err
	}
	return s, runErr
}

// remove removes a schedule which by may manage.
func (sc *schedules) remove(ctx context.Context, name string, by auth.Principal) error {
	return sc.manage().Remove(ctx, name, by)
}

// visible lists the schedules by can see.
func (sc *schedules) visible(ctx context.Context, by auth.Principal) ([]store.Schedule, error) {
	return sc.manage().Visible(ctx, by)
}
