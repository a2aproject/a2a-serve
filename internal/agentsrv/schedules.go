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
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/channel"
	"github.com/a2aproject/a2a-serve/internal/schedule"
	schedulesvc "github.com/a2aproject/a2a-serve/internal/schedules"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// maxScheduleWait is the longest the scheduler sleeps between checks.
const maxScheduleWait = time.Minute

// schedules runs the agent's schedules, and adds and removes them for the
// owner and for the harness.
type schedules struct {
	// ctx ends when the server stops; runs happen in it.
	ctx        context.Context
	store      *store.Store
	turns      *ChatTurns
	supervisor *ChannelSupervisor
	// chain gives the user a schedule runs as its role at each run.
	chain *auth.Chain
	now   func() time.Time
	// minWait is the shortest wait between checks of the schedules.
	minWait time.Duration
	// wake makes the scheduler check the schedules again after a change.
	wake chan struct{}

	mu sync.Mutex
	// pending are the one-time schedules whose run waits to be accepted.
	// They stay due in the store, so that a restart runs them again.
	pending map[string]bool
}

// schedulesDeps are the dependencies of [schedules].
type schedulesDeps struct {
	Store      *store.Store
	Turns      *ChatTurns
	Supervisor *ChannelSupervisor
	Chain      *auth.Chain
	Now        func() time.Time
	MinWait    time.Duration
}

func newSchedules(ctx context.Context, deps schedulesDeps) *schedules {
	return &schedules{
		ctx:        ctx,
		store:      deps.Store,
		turns:      deps.Turns,
		supervisor: deps.Supervisor,
		chain:      deps.Chain,
		now:        deps.Now,
		minWait:    deps.MinWait,
		wake:       make(chan struct{}, 1),
		pending:    map[string]bool{},
	}
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
		if s.NextRun.IsZero() || sc.isPending(s.Name) {
			continue
		}
		if s.NextRun.After(now) {
			wait = min(wait, s.NextRun.Sub(now))
			continue
		}
		next, ok, nextErr := whenOf(s).Next(s.NextRun, now)
		if nextErr == nil && !ok {
			sc.runOnce(ctx, s, now)
			continue
		}
		// A run whose next time isn't saved would repeat on every check.
		if err := sc.store.SetScheduleNextRun(ctx, s.Name, next); err != nil {
			slog.Error("saving the next run of a schedule", "schedule", s.Name, "error", err)
			continue
		}
		runErr := sc.start(ctx, s, sc.recordFailure(s.Name))
		sc.record(ctx, s.Name, now, cmp.Or(runErr, nextErr))
		if !next.IsZero() {
			wait = min(wait, next.Sub(now))
		}
	}
	return max(wait, sc.minWait)
}

// runOnce starts the run of a one-time schedule, which stays due until the
// agent accepts its turn. A run that fails stays due, so that the next
// check retries it.
func (sc *schedules) runOnce(ctx context.Context, s store.Schedule, now time.Time) {
	sc.setPending(s.Name, true)
	err := sc.start(ctx, s, func(err error) {
		sc.setPending(s.Name, false)
		if err != nil {
			sc.record(context.WithoutCancel(sc.ctx), s.Name, sc.now(), err)
			return
		}
		sc.finish(context.WithoutCancel(sc.ctx), s.Name)
	})
	if err != nil {
		sc.setPending(s.Name, false)
		sc.record(ctx, s.Name, now, err)
	}
}

// recordFailure returns what records a run's turn that the agent refused.
func (sc *schedules) recordFailure(name string) func(error) {
	return func(err error) {
		if err != nil {
			sc.record(context.WithoutCancel(sc.ctx), name, sc.now(), err)
		}
	}
}

func (sc *schedules) record(ctx context.Context, name string, ran time.Time, runErr error) {
	if err := sc.store.RecordScheduleResult(ctx, name, ran, errText(runErr)); err != nil {
		slog.Error("recording a schedule run", "schedule", name, "error", err)
	}
}

func (sc *schedules) isPending(name string) bool {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.pending[name]
}

func (sc *schedules) setPending(name string, pending bool) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if pending {
		sc.pending[name] = true
		return
	}
	delete(sc.pending, name)
}

// finish removes the schedule of a one-time run that the agent accepted.
// If that fails, it clears the run so that it doesn't repeat.
func (sc *schedules) finish(ctx context.Context, name string) {
	err := sc.store.DeleteSchedule(ctx, name)
	if err == nil || errors.Is(err, store.ErrNotFound) {
		return
	}
	slog.Error("removing a finished schedule", "schedule", name, "error", err)
	if err := sc.store.SetScheduleNextRun(ctx, name, time.Time{}); err != nil {
		slog.Error("clearing the run of a finished schedule", "schedule", name, "error", err)
	}
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
// role the user has now. accepted is called once the agent accepted the
// turn or refused it, unless start fails.
func (sc *schedules) start(ctx context.Context, s store.Schedule, accepted func(error)) error {
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
	if err := sc.turns.injectTurn(sc.ctx, postedTurn{ContextID: contextID, User: user, Text: text, Key: scheduleTurnKey(s.Name), Accepted: accepted}); err != nil {
		slog.Warn("running a schedule", "schedule", s.Name, "error", err)
		return err
	}
	return nil
}

// scheduleTurnKey merges the runs of a schedule that wait in a context
// into one.
func scheduleTurnKey(name string) string { return "schedule:" + name }

// contextFor returns the context a run of s happens in. A run delivered to
// a chat becomes the chat's conversation, so replying there continues it.
func (sc *schedules) contextFor(ctx context.Context, s store.Schedule) (string, error) {
	if s.DeliverChannel == "" {
		if s.Conversation == schedule.ThreadSame && s.ContextID != "" {
			return s.ContextID, nil
		}
		return a2a.NewContextID(), nil
	}
	if sc.supervisor.channel(s.DeliverChannel) == nil {
		return "", fmt.Errorf("channel %s is not running", s.DeliverChannel)
	}
	conv, err := schedulesvc.DirectConv(ctx, sc.store, s.DeliverChannel, s.DeliverUser)
	if err != nil {
		return "", err
	}
	if s.Conversation == schedule.ThreadSame {
		return sc.turns.contextOf(ctx, s.DeliverChannel, channel.ConvKey(conv))
	}
	id := a2a.NewContextID()
	return id, sc.store.SetContext(ctx, s.DeliverChannel, conv, id)
}

// announce posts text into the chat conversation of a context, if it has
// one.
func (sc *schedules) announce(ctx context.Context, contextID, text string) {
	chName, conv, err := sc.store.ConvOf(ctx, contextID)
	if errors.Is(err, store.ErrNotFound) {
		return
	}
	if err != nil {
		slog.Warn("finding the conversation of a schedule", "context", contextID, "error", err)
		return
	}
	ch := sc.supervisor.channel(chName)
	if ch == nil {
		return
	}
	if _, err := ch.Send(ctx, channel.Outbound{Conv: channel.ConvKey(conv), Text: text}); err != nil {
		slog.Warn("announcing a schedule", "channel", chName, "error", err)
	}
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

// add creates a schedule the owner made.
func (sc *schedules) add(ctx context.Context, req schedule.Request, by auth.Principal) (store.Schedule, error) {
	return sc.manage(schedulesvc.Limits{}).Add(ctx, req, by, "")
}

// addByAgent creates a schedule the agent made for by in the conversation
// contextID, within [schedulesvc.AgentLimits].
func (sc *schedules) addByAgent(ctx context.Context, req schedule.Request, by auth.Principal, contextID string) (store.Schedule, error) {
	return sc.manage(schedulesvc.AgentLimits).Add(ctx, req, by, contextID)
}

func (sc *schedules) manage(limits schedulesvc.Limits) schedulesvc.Service {
	return schedulesvc.Service{Store: sc.store, Now: sc.now, Changed: sc.changed, Limits: limits}
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
	runErr := sc.start(ctx, s, sc.recordFailure(s.Name))
	s.LastRun, s.LastError = sc.now(), errText(runErr)
	if err := sc.store.RecordScheduleResult(ctx, s.Name, s.LastRun, s.LastError); err != nil {
		return store.Schedule{}, err
	}
	return s, runErr
}

// remove removes a schedule which by may manage, and its runs that wait
// to be posted.
func (sc *schedules) remove(ctx context.Context, name string, by auth.Principal) error {
	if err := sc.manage(schedulesvc.Limits{}).Remove(ctx, name, by); err != nil {
		return err
	}
	sc.turns.dropPosted(scheduleTurnKey(name))
	return nil
}

// visible lists the schedules by can see.
func (sc *schedules) visible(ctx context.Context, by auth.Principal) ([]store.Schedule, error) {
	return sc.manage(schedulesvc.Limits{}).Visible(ctx, by)
}
