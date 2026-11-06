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

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aevent"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/integrations/executor/a2aacp"

	"github.com/a2aproject/a2a-serve/internal/agentkey"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// Pools of agent processes besides the main one.
const (
	anonymousPool = "anonymous"
	usersPool     = "users"
)

func poolOf(key string) string {
	if key == agentkey.Anonymous {
		return anonymousPool
	}
	return usersPool
}

// agentKeys tells which agent process each conversation runs in, by its
// agent key.
type agentKeys struct {
	sessions a2aacp.SessionStore

	mu sync.Mutex
	// pending are the agent keys of conversations without a stored session
	// yet.
	pending map[string]string
}

func newAgentKeys(sessions a2aacp.SessionStore) *agentKeys {
	return &agentKeys{sessions: sessions, pending: map[string]string{}}
}

// of returns the agent key of a conversation: the one stored with its
// session, or, for a new conversation, the one of the caller who starts it.
func (k *agentKeys) of(ctx context.Context, contextID string) (string, error) {
	sess, ok, err := k.sessions.Get(ctx, contextID)
	if err != nil {
		return "", err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if ok {
		delete(k.pending, contextID)
		return sess.AgentKey, nil
	}
	if key, ok := k.pending[contextID]; ok {
		return key, nil
	}
	return agentkey.Of(auth.Caller(ctx)), nil
}

// remember keeps the agent key of a conversation until forget, in case its
// session isn't stored yet.
func (k *agentKeys) remember(contextID, key string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.pending[contextID] = key
}

// forget drops the agent key kept for a conversation once its turn ended:
// a conversation that has a session is looked up there, and one that has
// none after a whole turn starts over with its next caller.
func (k *agentKeys) forget(contextID string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.pending, contextID)
}

// anonymous reports whether a conversation runs in the agent process of
// anonymous callers.
func (k *agentKeys) anonymous(ctx context.Context, contextID string) bool {
	key, err := k.of(ctx, contextID)
	if err != nil {
		slog.Warn("looking up the session of a conversation", "context", contextID, "error", err)
	}
	return key == agentkey.Anonymous
}

// user reports whether a conversation runs in the agent process of an
// authenticated user.
func (k *agentKeys) user(ctx context.Context, contextID string) bool {
	key, err := k.of(ctx, contextID)
	if err != nil {
		slog.Warn("looking up the session of a conversation", "context", contextID, "error", err)
	}
	_, ok := agentkey.UserOf(key)
	return ok
}

// startPool starts the executor of a pool of agent processes.
type startPool func(ctx context.Context, pool string) (a2asrv.AgentExecutor, func(context.Context) error, error)

// splitExecutor runs each conversation in the agent process of the user
// who started it. A conversation stays with its process, so the owner's
// message to a user's conversation runs there too.
type splitExecutor struct {
	main   a2asrv.AgentExecutor
	keys   *agentKeys
	budget *anonymousBudget
	// start starts the executor of a pool. If nil, every conversation runs
	// in main.
	start startPool
	// ctx ends when the server stops; pools start in it.
	ctx context.Context

	startMu sync.Mutex
	mu      sync.Mutex
	pools   map[string]*startedPool
	// closed is set by close; a pool started after it would never stop.
	closed bool
	// closing is set once close starts, and running counts the turns in
	// progress, which close waits for: an agent process that a turn starts
	// after its pool closed would outlive the server.
	closing bool
	running turnCount
}

// splitDeps are the dependencies of [splitExecutor].
type splitDeps struct {
	Main   a2asrv.AgentExecutor
	Keys   *agentKeys
	Budget *anonymousBudget
	// Start starts the executor of a pool. If nil, every conversation runs
	// in Main.
	Start startPool
}

func newSplitExecutor(ctx context.Context, deps splitDeps) *splitExecutor {
	return &splitExecutor{main: deps.Main, keys: deps.Keys, budget: deps.Budget, start: deps.Start, ctx: ctx}
}

type startedPool struct {
	exec a2asrv.AgentExecutor
	stop func(context.Context) error
}

var _ a2asrv.AgentExecutor = (*splitExecutor)(nil)

// track registers a turn for close to wait for. The returned context ends
// when the server stops.
func (e *splitExecutor) track(ctx context.Context) (context.Context, func(), error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closing {
		return nil, nil, errServerStopping
	}
	e.running.add()
	ctx, cancel := context.WithCancel(ctx)
	stop := func() bool { return true }
	if e.ctx != nil {
		stop = context.AfterFunc(e.ctx, cancel)
	}
	return ctx, func() {
		stop()
		cancel()
		e.running.done()
	}, nil
}

func (e *splitExecutor) Execute(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		ctx, done, err := e.track(ctx)
		if err != nil {
			failTask(execCtx, err)(yield)
			return
		}
		defer done()
		exec, err := e.pick(ctx, execCtx.ContextID, true)
		defer e.keys.forget(execCtx.ContextID)
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
		sent, final := false, false
		for ev, err := range exec.Execute(ctx, execCtx) {
			if err != nil && e.stopped() {
				break
			}
			if err == nil {
				sent, final = true, a2aevent.IsFinal(ev)
			}
			if !yield(ev, err) {
				return
			}
		}
		if !final && e.stopped() {
			failStopped(execCtx, sent)(yield)
		}
	}
}

// stopped reports whether the server stopped, which ends the turns.
func (e *splitExecutor) stopped() bool { return e.ctx != nil && e.ctx.Err() != nil }

// stoppedMessage fails the turns that the server stopped.
const stoppedMessage = "The server stopped before this task finished. Send the message again."

// failStopped fails the task of execCtx, which the server stopped, so
// that its subscribers get a final event. sent is whether the task was
// already sent.
func failStopped(execCtx *a2asrv.ExecutorContext, sent bool) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if !sent && execCtx.StoredTask == nil && !yield(a2a.NewSubmittedTask(execCtx, execCtx.Message), nil) {
			return
		}
		msg := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(stoppedMessage))
		msg.Metadata = map[string]any{store.InterruptedKey: true}
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateFailed, msg), nil)
	}
}

func (e *splitExecutor) Cancel(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		ctx, done, err := e.track(ctx)
		if err != nil {
			yield(nil, err)
			return
		}
		defer done()
		exec, err := e.pick(ctx, execCtx.ContextID, false)
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

// pick returns the executor of a conversation, and starts its pool if
// needed. If remember is set, the conversation keeps its agent key until
// the keys forget it, in case its session isn't stored yet.
func (e *splitExecutor) pick(ctx context.Context, contextID string, remember bool) (a2asrv.AgentExecutor, error) {
	key, err := e.keys.of(ctx, contextID)
	if err != nil {
		return nil, err
	}
	if key == "" || e.start == nil {
		return e.main, nil
	}
	if remember {
		e.keys.remember(contextID, key)
	}
	return e.pool(poolOf(key))
}

// errServerStopping fails turns that start, or would start agent processes,
// while the server stops.
var errServerStopping = errors.New("the server is stopping")

func (e *splitExecutor) pool(name string) (a2asrv.AgentExecutor, error) {
	e.startMu.Lock()
	defer e.startMu.Unlock()
	e.mu.Lock()
	p, closed := e.pools[name], e.closed
	e.mu.Unlock()
	if p != nil {
		return p.exec, nil
	}
	if closed {
		return nil, errServerStopping
	}
	exec, stop, err := e.start(e.ctx, name)
	if err != nil {
		return nil, fmt.Errorf("starting the agent processes of %s callers: %w", name, err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.pools == nil {
		e.pools = map[string]*startedPool{}
	}
	e.pools[name] = &startedPool{exec: exec, stop: stop}
	return exec, nil
}

// close waits for the turns in progress, which end when the server stops,
// and then stops the agent processes of the pools that started.
func (e *splitExecutor) close(ctx context.Context) error {
	e.mu.Lock()
	e.closing = true
	e.mu.Unlock()
	e.running.wait(context.Background())
	e.startMu.Lock()
	defer e.startMu.Unlock()
	e.mu.Lock()
	pools := e.pools
	e.pools, e.closed = nil, true
	e.mu.Unlock()
	var (
		wg   sync.WaitGroup
		errs = make([]error, 0, len(pools))
		mu   sync.Mutex
	)
	for _, p := range pools {
		wg.Go(func() {
			err := p.stop(ctx)
			mu.Lock()
			defer mu.Unlock()
			errs = append(errs, err)
		})
	}
	wg.Wait()
	return errors.Join(errs...)
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

// roleRank orders roles by what they may do; no role ranks lowest.
func roleRank(role string) int {
	switch role {
	case auth.RoleOwner:
		return 3
	case auth.RoleAuthenticated:
		return 2
	case auth.RoleAnonymous:
		return 1
	}
	return 0
}

// capToProcessUser returns who a turn of user acts as through the agents
// URL of a conversation in the agent process of process. Other
// conversations in that process can read the URL, so a turn acts as the
// process's user unless its own user may do less.
func capToProcessUser(user, process auth.Principal) auth.Principal {
	if user.ID == process.ID || roleRank(user.Role) < roleRank(process.Role) {
		return user
	}
	process.Hops = max(process.Hops, user.Hops)
	return process
}
