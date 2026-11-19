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

package agentrun

import (
	"context"
	"errors"
	"iter"
	"log/slog"
	"maps"
	"slices"
	"sync"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// maxEndedTurns bounds the errors of AfterTurn that wait for the final
// event of their task, which a task that ends with no execution never
// takes.
const maxEndedTurns = 1024

// workspaceTurns runs the turn hooks of the workspaces of the running
// processes: BeforeTurn before the agent gets the prompt of a turn, and
// AfterTurn when the agent ends it. The turns of a process whose workspace
// has hooks run one at a time, from BeforeTurn to AfterTurn, so that a
// sync never sees another turn half done. An error of AfterTurn fails the
// task in its final event, which the agent sends after the end of the
// turn.
type workspaceTurns struct {
	keyOf   func(ctx context.Context, contextID string) (string, error)
	explain func(ctx context.Context, execCtx *a2asrv.ExecutorContext, err error) []*a2a.Part
	log     func() *slog.Logger

	mu    sync.Mutex
	hooks map[string]*turnHooks
	// held are the hooks whose lock a task took before its turn started.
	held map[a2a.TaskID]*turnHooks
	// ended are the errors of AfterTurn by task, in the order of order.
	ended map[a2a.TaskID]error
	order []a2a.TaskID
}

type turnHooks struct {
	before, after func(context.Context) error
	// lock is held from BeforeTurn to AfterTurn.
	lock chan struct{}
}

func newWorkspaceTurns(keyOf func(context.Context, string) (string, error), explain func(context.Context, *a2asrv.ExecutorContext, error) []*a2a.Part, log func() *slog.Logger) *workspaceTurns {
	return &workspaceTurns{keyOf: keyOf, explain: explain, log: log, hooks: map[string]*turnHooks{}, held: map[a2a.TaskID]*turnHooks{}, ended: map[a2a.TaskID]error{}}
}

// attach runs the turn hooks of att for the turns of key until detach.
func (w *workspaceTurns) attach(key string, att Attachment) (detach func()) {
	if att.BeforeTurn == nil && att.AfterTurn == nil {
		return func() {}
	}
	h := &turnHooks{before: att.BeforeTurn, after: att.AfterTurn, lock: make(chan struct{}, 1)}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.hooks[key] = h
	return func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		if w.hooks[key] == h {
			delete(w.hooks, key)
		}
	}
}

// hooksOf returns the hooks of the process of the conversation of execCtx.
func (w *workspaceTurns) hooksOf(ctx context.Context, execCtx *a2asrv.ExecutorContext) *turnHooks {
	key, err := w.keyOf(ctx, execCtx.ContextID)
	if err != nil {
		w.log().Warn("looking up the session of a conversation", "context", execCtx.ContextID, "error", err)
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.hooks[key]
}

// prompt wraps the Prompt hook of a2aacp, which runs once the turn before
// in the conversation ended, and whose context ends when the task is
// canceled. It waits for the turn of the workspace to end and runs
// BeforeTurn, whose error fails the task, then returns the parts of next,
// or of the message. The turn takes the lock over when it starts.
func (w *workspaceTurns) prompt(next func(context.Context, *a2asrv.ExecutorContext) ([]*a2a.Part, error)) func(context.Context, *a2asrv.ExecutorContext) ([]*a2a.Part, error) {
	return func(ctx context.Context, execCtx *a2asrv.ExecutorContext) ([]*a2a.Part, error) {
		if h := w.hooksOf(ctx, execCtx); h != nil {
			if err := w.begin(ctx, execCtx.TaskID, h); err != nil {
				return nil, err
			}
		}
		if next != nil {
			return next(ctx, execCtx)
		}
		if execCtx.Message == nil {
			return nil, errors.New("no message to send to the agent")
		}
		return execCtx.Message.Parts, nil
	}
}

// begin takes the lock of h for the task id and runs BeforeTurn.
func (w *workspaceTurns) begin(ctx context.Context, id a2a.TaskID, h *turnHooks) error {
	select {
	case h.lock <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	if h.before != nil {
		if err := h.before(ctx); err != nil {
			<-h.lock
			return err
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.held[id] = h
	return nil
}

// takeHeld returns and forgets the hooks whose lock the task id took.
func (w *workspaceTurns) takeHeld(id a2a.TaskID) *turnHooks {
	w.mu.Lock()
	defer w.mu.Unlock()
	h := w.held[id]
	delete(w.held, id)
	return h
}

// turn wraps the Turn hook of a2aacp, so that AfterTurn runs when the turn
// ends, before the end of next, and the lock is released after it. A turn
// whose process started during its prompt finds the hooks only now: it
// waits for the lock and runs BeforeTurn here, before the agent gets the
// prompt, and an error of BeforeTurn fails its final event.
func (w *workspaceTurns) turn(next func(context.Context, *a2asrv.ExecutorContext) func()) func(context.Context, *a2asrv.ExecutorContext) func() {
	return func(ctx context.Context, execCtx *a2asrv.ExecutorContext) func() {
		h := w.takeHeld(execCtx.TaskID)
		if h == nil {
			if h = w.hooksOf(ctx, execCtx); h != nil {
				h.lock <- struct{}{}
				if h.before != nil {
					if err := h.before(ctx); err != nil {
						w.end(execCtx.TaskID, err)
					}
				}
			}
		}
		end := next(ctx, execCtx)
		return func() {
			if h != nil {
				if h.after != nil {
					if err := h.after(ctx); err != nil {
						w.end(execCtx.TaskID, errors.Join(w.take(execCtx.TaskID), err))
					}
				}
				<-h.lock
			}
			if end != nil {
				end()
			}
		}
	}
}

// end keeps the error of AfterTurn for the final event of the task.
func (w *workspaceTurns) end(id a2a.TaskID, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.ended[id]; !ok {
		w.order = append(w.order, id)
	}
	w.ended[id] = err
	for len(w.order) > maxEndedTurns {
		delete(w.ended, w.order[0])
		w.order = w.order[1:]
	}
}

// take returns and forgets the error of AfterTurn of the task.
func (w *workspaceTurns) take(id a2a.TaskID) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	err, ok := w.ended[id]
	if !ok {
		return nil
	}
	delete(w.ended, id)
	w.order = slices.DeleteFunc(w.order, func(o a2a.TaskID) bool { return o == id })
	return err
}

// guard returns inner with the hooks of the workspaces run.
func (w *workspaceTurns) guard(inner a2asrv.AgentExecutor) a2asrv.AgentExecutor {
	return &workspaceGuardedExecutor{inner: inner, w: w}
}

type workspaceGuardedExecutor struct {
	inner a2asrv.AgentExecutor
	w     *workspaceTurns
}

var _ a2asrv.AgentExecutor = (*workspaceGuardedExecutor)(nil)

// Execute fails the final event of a turn whose AfterTurn failed. Once the
// execution returns, it releases the lock of a turn that didn't start, and
// drops the errors of AfterTurn that the final event didn't take, such as
// of a turn that the stop ended first.
func (e *workspaceGuardedExecutor) Execute(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		defer func() {
			if h := e.w.takeHeld(execCtx.TaskID); h != nil {
				<-h.lock
			}
			if err := e.w.take(execCtx.TaskID); err != nil {
				e.w.log().Warn("the workspace failed after a turn whose end was already sent", "task", execCtx.TaskID, "error", err)
			}
		}()
		for ev, err := range e.inner.Execute(ctx, execCtx) {
			if err == nil {
				ev = e.w.settle(ctx, execCtx, ev)
			}
			if !yield(ev, err) {
				return
			}
		}
	}
}

func (e *workspaceGuardedExecutor) Cancel(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return e.inner.Cancel(ctx, execCtx)
}

// settle fails a final event of a turn after which AfterTurn failed: a
// completed task fails, and a failed or canceled one keeps its state. The
// status message gets the metadata of a [*TurnError].
func (w *workspaceTurns) settle(ctx context.Context, execCtx *a2asrv.ExecutorContext, ev a2a.Event) a2a.Event {
	up, ok := ev.(*a2a.TaskStatusUpdateEvent)
	if !ok || !up.Status.State.Terminal() {
		return ev
	}
	err := w.take(execCtx.TaskID)
	if err == nil {
		return ev
	}
	if up.Status.State == a2a.TaskStateCompleted {
		return w.failure(ctx, execCtx, err)
	}
	out := *up
	if out.Status.Message == nil {
		out.Status.Message = a2a.NewMessageForTask(a2a.MessageRoleAgent, execCtx, w.parts(ctx, execCtx, err)...)
	} else {
		msg := *out.Status.Message
		msg.Metadata = maps.Clone(msg.Metadata)
		out.Status.Message = &msg
	}
	addTurnMetadata(out.Status.Message, err)
	return &out
}

// failure returns the event that fails the task with err.
func (w *workspaceTurns) failure(ctx context.Context, execCtx *a2asrv.ExecutorContext, err error) *a2a.TaskStatusUpdateEvent {
	msg := a2a.NewMessageForTask(a2a.MessageRoleAgent, execCtx, w.parts(ctx, execCtx, err)...)
	addTurnMetadata(msg, err)
	return a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateFailed, msg)
}

// parts words err with Hooks.Explain, or its text.
func (w *workspaceTurns) parts(ctx context.Context, execCtx *a2asrv.ExecutorContext, err error) []*a2a.Part {
	if w.explain != nil {
		if parts := w.explain(ctx, execCtx, err); len(parts) > 0 {
			return parts
		}
	}
	return []*a2a.Part{a2a.NewTextPart(err.Error())}
}

func addTurnMetadata(msg *a2a.Message, err error) {
	te, ok := errors.AsType[*TurnError](err)
	if !ok || len(te.Metadata) == 0 {
		return
	}
	if msg.Metadata == nil {
		msg.Metadata = map[string]any{}
	}
	maps.Copy(msg.Metadata, te.Metadata)
}
