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
	"iter"
	"log/slog"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/executor/a2aacp"

	"github.com/a2aproject/a2a-serve/internal/auth"
)

// The budget of anonymous callers on each listener, and how long their
// approvals wait for an owner.
var (
	anonymousTasksPerHour    = 20
	anonymousConcurrentTasks = 2
	anonymousQuestionTimeout = 10 * time.Minute
)

// anonymousAgentKey is the agent key a2aacp stores with the sessions of
// anonymous conversations, which run in the separate agent process.
const anonymousAgentKey = "anonymous"

// splitExecutor runs anonymous conversations in a separate agent process.
// A conversation stays with the process that started it, so an owner's
// message to an anonymous conversation runs there too.
type splitExecutor struct {
	main     a2asrv.AgentExecutor
	sessions a2aacp.SessionStore
	budget   *anonymousBudget
	// start starts the executor of anonymous conversations. If nil, they
	// run in main.
	start func(ctx context.Context) (a2asrv.AgentExecutor, func(context.Context) error, error)
	// ctx ends when the server stops; the anonymous executor starts in it.
	ctx context.Context

	startMu sync.Mutex
	mu      sync.Mutex
	anon    a2asrv.AgentExecutor
	stop    func(context.Context) error
	// pending are the anonymous conversations without a stored session yet.
	pending map[string]bool
}

var _ a2asrv.AgentExecutor = (*splitExecutor)(nil)

func (e *splitExecutor) Execute(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		exec, err := e.pick(ctx, execCtx.ContextID)
		if err != nil {
			failTask(execCtx, err)(yield)
			return
		}
		if p, ok := auth.Caller(ctx); ok && p.Role == auth.RoleAnonymous {
			release, err := e.budget.run(budgetListener(p))
			if err != nil {
				failTask(execCtx, err)(yield)
				return
			}
			defer release()
		}
		for ev, err := range exec.Execute(ctx, execCtx) {
			if !yield(ev, err) {
				return
			}
		}
	}
}

func (e *splitExecutor) Cancel(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		exec, err := e.pick(ctx, execCtx.ContextID)
		if err != nil {
			yield(nil, err)
			return
		}
		for ev, err := range exec.Cancel(ctx, execCtx) {
			if !yield(ev, err) {
				return
			}
		}
	}
}

// pick returns the executor of a conversation, and starts the one of
// anonymous conversations if needed.
func (e *splitExecutor) pick(ctx context.Context, contextID string) (a2asrv.AgentExecutor, error) {
	anon, err := e.anonymous(ctx, contextID)
	if err != nil {
		return nil, err
	}
	if !anon || e.start == nil {
		return e.main, nil
	}
	e.mu.Lock()
	if e.pending == nil {
		e.pending = map[string]bool{}
	}
	e.pending[contextID] = true
	e.mu.Unlock()
	return e.anonymousExecutor()
}

// Anonymous reports whether a conversation runs in the agent process of
// anonymous callers.
func (e *splitExecutor) Anonymous(ctx context.Context, contextID string) bool {
	anon, err := e.anonymous(ctx, contextID)
	if err != nil {
		slog.Warn("looking up the session of a conversation", "context", contextID, "error", err)
	}
	return anon
}

// anonymous reports whether a conversation is anonymous: its session is in
// the process of anonymous callers, or, for a new conversation, an
// anonymous caller starts it.
func (e *splitExecutor) anonymous(ctx context.Context, contextID string) (bool, error) {
	sess, ok, err := e.sessions.Get(ctx, contextID)
	if err != nil {
		return false, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if ok {
		delete(e.pending, contextID)
		return sess.AgentKey == anonymousAgentKey, nil
	}
	if e.pending[contextID] {
		return true, nil
	}
	p, ok := auth.Caller(ctx)
	return ok && p.Role == auth.RoleAnonymous, nil
}

func (e *splitExecutor) anonymousExecutor() (a2asrv.AgentExecutor, error) {
	e.startMu.Lock()
	defer e.startMu.Unlock()
	e.mu.Lock()
	anon := e.anon
	e.mu.Unlock()
	if anon != nil {
		return anon, nil
	}
	anon, stop, err := e.start(e.ctx)
	if err != nil {
		return nil, fmt.Errorf("starting the agent of anonymous callers: %w", err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.anon, e.stop = anon, stop
	return anon, nil
}

// close stops the agent process of anonymous callers, if it started.
func (e *splitExecutor) close(ctx context.Context) error {
	e.startMu.Lock()
	defer e.startMu.Unlock()
	e.mu.Lock()
	stop := e.stop
	e.anon, e.stop = nil, nil
	e.mu.Unlock()
	if stop == nil {
		return nil
	}
	return stop(ctx)
}

// failTask fails the task of execCtx with err.
func failTask(execCtx *a2asrv.ExecutorContext, err error) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if execCtx.StoredTask == nil && !yield(a2a.NewSubmittedTask(execCtx, execCtx.Message), nil) {
			return
		}
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateFailed, a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(err.Error()))), nil)
	}
}

var (
	errAnonymousBusy = errors.New("this agent is busy with other anonymous callers; try again in a few minutes")
	errAnonymousRate = errors.New("this agent took all the anonymous tasks it takes in an hour; try again later")
)

// anonymousBudget limits the new and the running tasks of anonymous
// callers on each listener.
type anonymousBudget struct {
	perHour    int
	concurrent int
	now        func() time.Time

	mu        sync.Mutex
	listeners map[string]*listenerBudget
}

type listenerBudget struct {
	// starts are the times of the tasks started in the last hour.
	starts  []time.Time
	running int
}

func newAnonymousBudget() *anonymousBudget {
	return &anonymousBudget{perHour: anonymousTasksPerHour, concurrent: anonymousConcurrentTasks, now: time.Now, listeners: map[string]*listenerBudget{}}
}

// budgetListener is where an anonymous caller arrived: a listener, or a
// messenger.
func budgetListener(p auth.Principal) string {
	if p.Network == auth.NetworkChat {
		return auth.NetworkChat + ":" + p.Provider
	}
	return p.Network
}

// admit counts a new task on a listener, or refuses it when the listener
// used up its budget.
func (b *anonymousBudget) admit(listener string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	l := b.listener(listener)
	switch {
	case l.running >= b.concurrent:
		return errAnonymousBusy
	case len(l.starts) >= b.perHour:
		return errAnonymousRate
	}
	l.starts = append(l.starts, b.now())
	return nil
}

// run counts a running task on a listener until release is called, or
// refuses it when the listener runs as many as it may.
func (b *anonymousBudget) run(listener string) (release func(), err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	l := b.listener(listener)
	if l.running >= b.concurrent {
		return nil, errAnonymousBusy
	}
	l.running++
	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			l.running--
		})
	}, nil
}

// listener returns the budget of a listener, without the starts older than
// an hour. b.mu must be held.
func (b *anonymousBudget) listener(name string) *listenerBudget {
	l := b.listeners[name]
	if l == nil {
		l = &listenerBudget{}
		b.listeners[name] = l
	}
	cutoff := b.now().Add(-time.Hour)
	i := 0
	for i < len(l.starts) && !l.starts[i].After(cutoff) {
		i++
	}
	l.starts = l.starts[i:]
	return l
}
