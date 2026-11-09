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
	"encoding/json"
	"fmt"
	"iter"
	"log/slog"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/integrations/executor/a2aacp"
)

// stallGuard fails a turn in which the agent sends nothing for timeout while
// none of its tool calls runs, or for toolTimeout while one runs. opencode
// retries a failed request to the model's provider before it reports the
// error over ACP, waiting as long as the provider's retry-after asks, so the
// task could stay working with no reason for hours.
type stallGuard struct {
	timeout     time.Duration
	toolTimeout time.Duration

	mu    sync.Mutex
	turns map[a2a.TaskID]*stallTurn
}

// stallTurn is the activity of a turn, guarded by stallGuard.mu.
type stallTurn struct {
	last time.Time
	// tools are the IDs of the tool calls that started and haven't ended.
	tools map[string]bool
}

func newStallGuard(timeout, toolTimeout time.Duration) *stallGuard {
	return &stallGuard{timeout: timeout, toolTimeout: toolTimeout, turns: map[a2a.TaskID]*stallTurn{}}
}

// turn wraps the Turn hook of a2aacp, so that the guard tracks a turn from
// its start to its end, across the waits for answers.
func (g *stallGuard) turn(next func(context.Context, *a2asrv.ExecutorContext) func()) func(context.Context, *a2asrv.ExecutorContext) func() {
	return func(ctx context.Context, execCtx *a2asrv.ExecutorContext) func() {
		st := &stallTurn{last: time.Now(), tools: map[string]bool{}}
		g.mu.Lock()
		g.turns[execCtx.TaskID] = st
		g.mu.Unlock()
		end := next(ctx, execCtx)
		return func() {
			g.mu.Lock()
			if g.turns[execCtx.TaskID] == st {
				delete(g.turns, execCtx.TaskID)
			}
			g.mu.Unlock()
			end()
		}
	}
}

// Convert implements [a2aacp.Converter]. It sees every update of the agent,
// also those the built-in conversion drops, such as thoughts.
func (g *stallGuard) Convert(ctx context.Context, execCtx *a2asrv.ExecutorContext, u a2aacp.Update) (*a2aacp.ConverterResult, error) {
	g.observe(execCtx.TaskID, u)
	return a2aacp.ConvertUpdate(ctx, execCtx, u)
}

func (g *stallGuard) observe(id a2a.TaskID, u a2aacp.Update) {
	var tool struct {
		ID     string `json:"toolCallId"`
		Status string `json:"status"`
	}
	if u.Type == "tool_call" || u.Type == "tool_call_update" {
		if err := json.Unmarshal(u.Raw, &tool); err != nil {
			slog.Debug("decoding a tool call update", "task", id, "error", err)
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	st := g.turns[id]
	if st == nil {
		return
	}
	st.last = time.Now()
	// The claude adapter never reports in_progress: its tool calls stay
	// pending until they end.
	switch {
	case tool.ID == "":
	case tool.Status == "completed" || tool.Status == "failed":
		delete(st.tools, tool.ID)
	case u.Type == "tool_call" || tool.Status != "":
		st.tools[tool.ID] = true
	}
}

// resume restarts the wait of a turn that continues after an answer.
func (g *stallGuard) resume(id a2a.TaskID) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if st := g.turns[id]; st != nil {
		st.last = time.Now()
	}
}

// remaining is how long the turn of a task may still stay silent, and
// whether one of its tool calls runs. A task whose turn hasn't started, such
// as one queued behind another task of its context, has the whole timeout.
func (g *stallGuard) remaining(id a2a.TaskID) (time.Duration, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	st := g.turns[id]
	if st == nil {
		return g.timeout, false
	}
	if len(st.tools) > 0 {
		return g.toolTimeout - time.Since(st.last), true
	}
	return g.timeout - time.Since(st.last), false
}

// guard returns inner, an executor whose a2aacp config uses the guard's
// Turn and Converter, with stalled turns failed.
func (g *stallGuard) guard(inner a2asrv.AgentExecutor) a2asrv.AgentExecutor {
	return &stallGuardedExecutor{inner: inner, g: g}
}

type stallGuardedExecutor struct {
	inner a2asrv.AgentExecutor
	g     *stallGuard
}

var _ a2asrv.AgentExecutor = (*stallGuardedExecutor)(nil)

type eventOrError struct {
	ev  a2a.Event
	err error
}

func (e *stallGuardedExecutor) Execute(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		e.g.resume(execCtx.TaskID)
		ctx, cancel := context.WithCancel(ctx)
		events := make(chan eventOrError)
		done := make(chan struct{})
		go func() {
			defer close(events)
			for ev, err := range e.inner.Execute(ctx, execCtx) {
				select {
				case events <- eventOrError{ev, err}:
				case <-done:
					return
				}
			}
		}()
		// Canceling the context stops the turn in a2aacp, if it still runs.
		defer func() {
			cancel()
			close(done)
			for range events {
			}
		}()

		timer := time.NewTimer(e.g.timeout)
		defer timer.Stop()
		for {
			select {
			case r, ok := <-events:
				if !ok || !yield(r.ev, r.err) {
					return
				}
			case <-timer.C:
				wait, tool := e.g.remaining(execCtx.TaskID)
				if wait > 0 {
					timer.Reset(wait)
					continue
				}
				timeout, text := e.g.timeout, fmt.Sprintf("the agent sent nothing for %s, so the server stopped the turn. Its model may refuse requests, for example a free model over its usage limit: set a model and its key with `a2a-serve config set --model <provider/model> --secret <KEY>`", e.g.timeout)
				if tool {
					timeout, text = e.g.toolTimeout, fmt.Sprintf("a tool call of the agent sent nothing for %s, so the server stopped the turn", e.g.toolTimeout)
				}
				slog.Warn("stopping a turn in which the agent sent nothing", "task", execCtx.TaskID, "tool", tool, "timeout", timeout)
				yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateFailed, a2a.NewMessageForTask(a2a.MessageRoleAgent, execCtx, a2a.NewTextPart(text))), nil)
				return
			}
		}
	}
}

func (e *stallGuardedExecutor) Cancel(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return e.inner.Cancel(ctx, execCtx)
}
