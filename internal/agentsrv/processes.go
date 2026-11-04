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
	"strings"
	"sync"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/integrations/executor/a2aacp"

	"github.com/a2aproject/a2a-serve/internal/auth"
)

// Agent keys, which a2aacp stores with the session of each conversation:
// the owner's conversations run in the main agent process, which has no
// key, all anonymous conversations in one process, and the conversations of
// each authenticated user in a process of their own. Processes don't share
// a home, so a conversation can't read what another user's left there.
const (
	anonymousAgentKey  = "anonymous"
	userAgentKeyPrefix = "user:"
)

// Pools of agent processes besides the main one.
const (
	anonymousPool = "anonymous"
	usersPool     = "users"
)

// agentKeyOf returns the agent key of a new conversation that p starts.
// Calls without a principal come from the server itself.
func agentKeyOf(p auth.Principal, ok bool) string {
	if !ok {
		return ""
	}
	switch p.Role {
	case auth.RoleAnonymous:
		return anonymousAgentKey
	case auth.RoleAuthenticated:
		return userAgentKeyPrefix + p.ID
	}
	return ""
}

// userOfKey returns the principal ID of a user's agent key.
func userOfKey(key string) (string, bool) {
	return strings.CutPrefix(key, userAgentKeyPrefix)
}

func poolOf(key string) string {
	if key == anonymousAgentKey {
		return anonymousPool
	}
	return usersPool
}

// splitExecutor runs each conversation in the agent process of the user
// who started it. A conversation stays with its process, so the owner's
// message to a user's conversation runs there too.
type splitExecutor struct {
	main     a2asrv.AgentExecutor
	sessions a2aacp.SessionStore
	budget   *anonymousBudget
	// start starts the executor of a pool. If nil, every conversation runs
	// in main.
	start func(ctx context.Context, pool string) (a2asrv.AgentExecutor, func(context.Context) error, error)
	// ctx ends when the server stops; pools start in it.
	ctx context.Context

	startMu sync.Mutex
	mu      sync.Mutex
	pools   map[string]*startedPool
	// pending are the agent keys of conversations without a stored session
	// yet.
	pending map[string]string
}

type startedPool struct {
	exec a2asrv.AgentExecutor
	stop func(context.Context) error
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

// pick returns the executor of a conversation, and starts its pool if
// needed.
func (e *splitExecutor) pick(ctx context.Context, contextID string) (a2asrv.AgentExecutor, error) {
	key, err := e.agentKey(ctx, contextID)
	if err != nil {
		return nil, err
	}
	if key == "" || e.start == nil {
		return e.main, nil
	}
	e.mu.Lock()
	if e.pending == nil {
		e.pending = map[string]string{}
	}
	e.pending[contextID] = key
	e.mu.Unlock()
	return e.pool(poolOf(key))
}

// agentKey returns the agent key of a conversation: the one stored with its
// session, or, for a new conversation, the one of the caller who starts it.
func (e *splitExecutor) agentKey(ctx context.Context, contextID string) (string, error) {
	sess, ok, err := e.sessions.Get(ctx, contextID)
	if err != nil {
		return "", err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if ok {
		delete(e.pending, contextID)
		return sess.AgentKey, nil
	}
	if key, ok := e.pending[contextID]; ok {
		return key, nil
	}
	return agentKeyOf(auth.Caller(ctx)), nil
}

// Anonymous reports whether a conversation runs in the agent process of
// anonymous callers.
func (e *splitExecutor) Anonymous(ctx context.Context, contextID string) bool {
	key, err := e.agentKey(ctx, contextID)
	if err != nil {
		slog.Warn("looking up the session of a conversation", "context", contextID, "error", err)
	}
	return key == anonymousAgentKey
}

func (e *splitExecutor) pool(name string) (a2asrv.AgentExecutor, error) {
	e.startMu.Lock()
	defer e.startMu.Unlock()
	e.mu.Lock()
	p := e.pools[name]
	e.mu.Unlock()
	if p != nil {
		return p.exec, nil
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

// close stops the agent processes of the pools that started.
func (e *splitExecutor) close(ctx context.Context) error {
	e.startMu.Lock()
	defer e.startMu.Unlock()
	e.mu.Lock()
	pools := e.pools
	e.pools = nil
	e.mu.Unlock()
	var errs []error
	for _, p := range pools {
		errs = append(errs, p.stop(ctx))
	}
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
