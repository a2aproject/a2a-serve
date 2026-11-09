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

// Package schedules runs the agent's schedules as turns in their
// conversations, and serves the built-in agent the harness manages them with.
package schedules

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/chat"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/messenger"
	"github.com/a2aproject/a2a-serve/internal/pairings"
	"github.com/a2aproject/a2a-serve/internal/schedule"
	"github.com/a2aproject/a2a-serve/internal/schedulesvc"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// Turns are the turns schedules post into conversations.
type Turns interface {
	// InjectTurn posts a turn into its context.
	InjectTurn(ctx context.Context, t chat.PostedTurn) error
	// DropPosted drops the posted turns with key that wait to be sent.
	DropPosted(key string)
	// ContextOf returns the context of a chat.
	ContextOf(ctx context.Context, ch string, conv messenger.ConvKey) (string, error)
}

// MessengerLookup finds a running messenger by name.
type MessengerLookup interface {
	// Messenger returns the running messenger name, or nil.
	Messenger(name string) messenger.Messenger
}

// maxScheduleWait is the longest the scheduler sleeps between checks.
const maxScheduleWait = time.Minute

// Runner runs the agent's schedules, and adds and removes them for the
// owner and for the harness.
type Runner struct {
	// ctx ends when the server stops; runs happen in it.
	ctx        context.Context
	store      *store.Store
	turns      Turns
	supervisor MessengerLookup
	// chain gives the user a schedule runs as its role at each run.
	chain *auth.Chain
	now   func() time.Time
	// minWait is the shortest wait between checks of the schedules.
	minWait time.Duration
	// wake makes the scheduler check the schedules again after a change.
	wake chan struct{}

	mu sync.Mutex
	// pending are the schedules whose run waits to be accepted: one-time
	// schedules, which stay due in the store so that a restart runs them
	// again, and the retries of refused recurring runs.
	pending map[string]bool
	// refused are the recurring schedules whose last run the agent refused.
	// Each check posts the run again until the agent accepts it, the user
	// loses access, or the next run is due.
	refused map[string]bool
}

// Deps are the dependencies of [Runner].
type Deps struct {
	Store      *store.Store
	Turns      Turns
	Supervisor MessengerLookup
	Chain      *auth.Chain
	Now        func() time.Time
	MinWait    time.Duration
}

// New returns the runner of deps, whose runs happen in ctx.
func New(ctx context.Context, deps Deps) *Runner {
	return &Runner{
		ctx:        ctx,
		store:      deps.Store,
		turns:      deps.Turns,
		supervisor: deps.Supervisor,
		chain:      deps.Chain,
		now:        deps.Now,
		minWait:    deps.MinWait,
		wake:       make(chan struct{}, 1),
		pending:    map[string]bool{},
		refused:    map[string]bool{},
	}
}

// Run runs due schedules until ctx ends. A run missed while the agent was
// stopped runs once when it starts.
func (sc *Runner) Run(ctx context.Context) {
	for {
		wait := sc.RunDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-sc.wake:
		case <-time.After(wait):
		}
	}
}

// Wake makes the runner check the schedules again, after a change.
func (sc *Runner) Wake() {
	select {
	case sc.wake <- struct{}{}:
	default:
	}
}

// RunDue runs the schedules which are due and returns how long until the
// next one.
func (sc *Runner) RunDue(ctx context.Context) time.Duration {
	wait := maxScheduleWait
	all, err := sc.store.Schedules(ctx)
	if err != nil {
		slog.Error("listing schedules", "error", err)
		return wait
	}
	now := sc.now()
	sc.forgetRefused(all)
	for _, s := range all {
		if s.NextRun.IsZero() || sc.isPending(s.Name) {
			continue
		}
		if s.NextRun.After(now) {
			wait = min(wait, s.NextRun.Sub(now))
			if sc.isRefused(s.Name) {
				sc.retry(ctx, s, now)
			}
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
		sc.setRefused(s.Name, false)
		runErr := sc.start(ctx, s, sc.retryRefused(s.Name))
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
func (sc *Runner) runOnce(ctx context.Context, s store.Schedule, now time.Time) {
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

// retry posts again the refused run of a recurring schedule.
func (sc *Runner) retry(ctx context.Context, s store.Schedule, now time.Time) {
	sc.setPending(s.Name, true)
	err := sc.start(ctx, s, func(err error) {
		sc.setPending(s.Name, false)
		if err != nil {
			sc.record(context.WithoutCancel(sc.ctx), s.Name, sc.now(), err)
			return
		}
		sc.setRefused(s.Name, false)
	})
	if err == nil {
		return
	}
	sc.setPending(s.Name, false)
	sc.record(ctx, s.Name, now, err)
	if errors.Is(err, errNoAccess) {
		sc.setRefused(s.Name, false)
	}
}

// retryRefused returns what records a recurring run's turn that the agent
// refused, and has the next checks post it again.
func (sc *Runner) retryRefused(name string) func(error) {
	return func(err error) {
		if err == nil {
			return
		}
		sc.record(context.WithoutCancel(sc.ctx), name, sc.now(), err)
		sc.setRefused(name, true)
	}
}

// recordFailure returns what records a run's turn that the agent refused.
func (sc *Runner) recordFailure(name string) func(error) {
	return func(err error) {
		if err != nil {
			sc.record(context.WithoutCancel(sc.ctx), name, sc.now(), err)
		}
	}
}

func (sc *Runner) record(ctx context.Context, name string, ran time.Time, runErr error) {
	if err := sc.store.RecordScheduleResult(ctx, name, ran, agenterr.Text(runErr)); err != nil {
		slog.Error("recording a schedule run", "schedule", name, "error", err)
	}
}

func (sc *Runner) isPending(name string) bool {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.pending[name]
}

func (sc *Runner) isRefused(name string) bool {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.refused[name]
}

func (sc *Runner) setRefused(name string, refused bool) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if refused {
		sc.refused[name] = true
		return
	}
	delete(sc.refused, name)
}

// forgetRefused forgets the refused runs of the schedules not in all,
// which were removed.
func (sc *Runner) forgetRefused(all []store.Schedule) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	for name := range sc.refused {
		if !slices.ContainsFunc(all, func(s store.Schedule) bool { return s.Name == name }) {
			delete(sc.refused, name)
		}
	}
}

func (sc *Runner) setPending(name string, pending bool) {
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
func (sc *Runner) finish(ctx context.Context, name string) {
	err := sc.store.DeleteSchedule(ctx, name)
	if err == nil || errors.Is(err, store.ErrNotFound) {
		return
	}
	slog.Error("removing a finished schedule", "schedule", name, "error", err)
	if err := sc.store.SetScheduleNextRun(ctx, name, time.Time{}); err != nil {
		slog.Error("clearing the run of a finished schedule", "schedule", name, "error", err)
	}
}

func whenOf(s store.Schedule) schedule.When {
	return schedule.When{Cron: s.Cron, At: s.At, TZ: s.TZ}
}

// start posts a run of s into its conversation as a new turn. The turn runs
// in the background, as an ordinary task of the schedule's user, with the
// role the user has now. accepted is called once the agent accepted the
// turn or refused it, unless start fails.
func (sc *Runner) start(ctx context.Context, s store.Schedule, accepted func(error)) error {
	user, err := sc.chain.Stored(ctx, s.UserID, s.UserIdentity)
	if err == nil && user.Role == "" {
		err = fmt.Errorf("%s %w", auth.DisplayPrincipal(s.UserID), errNoAccess)
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
	if err := sc.turns.InjectTurn(sc.ctx, chat.PostedTurn{ContextID: contextID, User: user, Text: text, Key: scheduleTurnKey(s.Name), Accepted: accepted}); err != nil {
		slog.Warn("running a schedule", "schedule", s.Name, "error", err)
		return err
	}
	return nil
}

// errNoAccess is what a run of a user who can no longer call this agent
// fails with.
var errNoAccess = errors.New("can no longer call this agent")

// scheduleTurnKey merges the runs of a schedule that wait in a context
// into one.
func scheduleTurnKey(name string) string { return "schedule:" + name }

// contextFor returns the context a run of s happens in. A run delivered to
// a chat becomes the chat's conversation, so replying there continues it.
func (sc *Runner) contextFor(ctx context.Context, s store.Schedule) (string, error) {
	if s.DeliverMessenger == "" {
		if s.Conversation == schedule.ThreadSame && s.ContextID != "" {
			return s.ContextID, nil
		}
		return a2a.NewContextID(), nil
	}
	if sc.supervisor.Messenger(s.DeliverMessenger) == nil {
		return "", fmt.Errorf("messenger %s is not running", s.DeliverMessenger)
	}
	conv, err := pairings.DirectConv(ctx, sc.store, s.DeliverMessenger, s.DeliverUser)
	if err != nil {
		return "", err
	}
	if s.Conversation == schedule.ThreadSame {
		return sc.turns.ContextOf(ctx, s.DeliverMessenger, messenger.ConvKey(conv))
	}
	id := a2a.NewContextID()
	return id, sc.store.SetContext(ctx, s.DeliverMessenger, conv, id)
}

// announce posts text into the chat conversation of a context, if it has
// one.
func (sc *Runner) announce(ctx context.Context, contextID, text string) {
	chName, conv, err := sc.store.ConvOf(ctx, contextID)
	if errors.Is(err, store.ErrNotFound) {
		return
	}
	if err != nil {
		slog.Warn("finding the conversation of a schedule", "context", contextID, "error", err)
		return
	}
	ch := sc.supervisor.Messenger(chName)
	if ch == nil {
		return
	}
	if _, err := ch.Send(ctx, messenger.Outbound{Conv: messenger.ConvKey(conv), Text: text}); err != nil {
		slog.Warn("announcing a schedule", "messenger", chName, "error", err)
	}
}

// addByAgent creates a schedule the agent made for by in the conversation
// contextID, within [schedulesvc.AgentLimits].
func (sc *Runner) addByAgent(ctx context.Context, req schedule.Request, by auth.Principal, contextID string) (store.Schedule, error) {
	return sc.manage(schedulesvc.AgentLimits).Add(ctx, req, by, contextID)
}

// Service adds, lists and removes the schedules the owner makes, which
// have no limits.
func (sc *Runner) Service() schedulesvc.Service { return sc.manage(schedulesvc.Limits{}) }

// manage adds, lists and removes the schedules this runner runs, within
// limits. Changes wake it, and a removal drops the schedule's runs that wait
// to be posted.
func (sc *Runner) manage(limits schedulesvc.Limits) schedulesvc.Service {
	return schedulesvc.Service{Store: sc.store, Now: sc.now, Changed: sc.Wake, Limits: limits, Removed: func(name string) {
		sc.turns.DropPosted(scheduleTurnKey(name))
	}}
}

// RunNow starts a run of a schedule at once, without changing when it runs
// next.
func (sc *Runner) RunNow(ctx context.Context, name string) (store.Schedule, error) {
	s, err := sc.store.Schedule(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return store.Schedule{}, &agenterr.NotFoundError{Kind: "schedule", Name: name}
	}
	if err != nil {
		return store.Schedule{}, err
	}
	runErr := sc.start(ctx, s, sc.recordFailure(s.Name))
	s.LastRun, s.LastError = sc.now(), agenterr.Text(runErr)
	if err := sc.store.RecordScheduleResult(ctx, s.Name, s.LastRun, s.LastError); err != nil {
		return store.Schedule{}, err
	}
	return s, runErr
}

// remove removes a schedule which by may manage, and its runs that wait
// to be posted.
func (sc *Runner) remove(ctx context.Context, name string, by auth.Principal) error {
	return sc.Service().Remove(ctx, name, by)
}

// visible lists the schedules by can see.
func (sc *Runner) visible(ctx context.Context, by auth.Principal) ([]store.Schedule, error) {
	return sc.Service().Visible(ctx, by)
}
