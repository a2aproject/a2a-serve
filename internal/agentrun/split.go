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
	"fmt"
	"iter"
	"log/slog"
	"slices"
	"sync"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aevent"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/a2aproject/a2a-serve/internal/turncount"
)

// InterruptedKey is the status message metadata that marks a task which a
// stop or a restart ended.
const InterruptedKey = "a2a-serve/interrupted"

// startPoolFunc starts the executor of a pool of agent processes.
type startPoolFunc func(ctx context.Context, pool string) (a2asrv.AgentExecutor, func(context.Context) error, error)

// splitExecutor runs each conversation in the agent process of its key. A
// conversation stays with its process.
type splitExecutor struct {
	r    *Runner
	main a2asrv.AgentExecutor
	// start starts the executor of a pool. If nil, every conversation runs
	// in main.
	start startPoolFunc
	// ctx ends when the runner stops; pools start in it.
	ctx context.Context

	startMu sync.Mutex
	mu      sync.Mutex
	pools   map[string]*startedPool
	// closed is set by close; a pool started after it would never stop.
	closed bool
	// closing is set once close starts, and running counts the turns in
	// progress, which close waits for: an agent process that a turn starts
	// after its pool closed would outlive the runner.
	closing bool
	running turncount.Count
	// turns cancels the context of each turn in progress, in the order the
	// turns started, and canceled is set once the runner stopped them.
	turns    []*turnCancel
	canceled bool
}

type turnCancel struct{ cancel context.CancelFunc }

func newSplitExecutor(ctx context.Context, r *Runner, main a2asrv.AgentExecutor, start startPoolFunc) *splitExecutor {
	e := &splitExecutor{r: r, main: main, start: start, ctx: ctx}
	if ctx != nil {
		context.AfterFunc(ctx, e.stopTurns)
	}
	return e
}

// stopTurns ends the turns in progress, the newest first. A turn that waits
// for the turn before it in its conversation is then stopped before that
// turn can end and let it start. Callbacks of one context.AfterFunc per
// turn run in goroutines of their own, in any order.
func (e *splitExecutor) stopTurns() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.canceled = true
	for _, t := range slices.Backward(e.turns) {
		t.cancel()
	}
}

type startedPool struct {
	exec a2asrv.AgentExecutor
	stop func(context.Context) error
}

var _ a2asrv.AgentExecutor = (*splitExecutor)(nil)

// track registers a turn for close to wait for. The returned context ends
// when the runner stops.
func (e *splitExecutor) track(ctx context.Context) (context.Context, func(), error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closing {
		return nil, nil, ErrStopping
	}
	e.running.Add()
	ctx, cancel := context.WithCancel(ctx)
	if e.canceled {
		cancel()
	}
	t := &turnCancel{cancel: cancel}
	e.turns = append(e.turns, t)
	return ctx, func() {
		cancel()
		e.mu.Lock()
		e.turns = slices.DeleteFunc(e.turns, func(o *turnCancel) bool { return o == t })
		e.mu.Unlock()
		e.running.Done()
	}, nil
}

func (e *splitExecutor) Execute(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		ctx, done, err := e.track(ctx)
		if err != nil {
			failTask(execCtx, e.r.refusal(err))(yield)
			return
		}
		defer done()
		exec, key, err := e.pick(ctx, execCtx.ContextID, true)
		defer e.r.keys.forget(execCtx.ContextID)
		if err == nil {
			var admitted func()
			if admitted, err = e.admit(ctx, key, execCtx); err == nil {
				defer admitted()
			} else if AnswersWaiting(execCtx) {
				cancelRefused(ctx, exec, execCtx)
			}
		}
		if err != nil {
			failTask(execCtx, e.r.refusal(err))(yield)
			return
		}
		if hold := e.r.cfg.Hooks.Hold; hold != nil {
			release, err := hold(ctx, key, execCtx)
			if err != nil {
				failTask(execCtx, e.r.refusal(err))(yield)
				return
			}
			defer release()
		}
		sent, final := false, false
		for ev, err := range exec.Execute(ctx, execCtx) {
			if err != nil && e.stopped() {
				break
			}
			// A turn that the stop ended, or that waited for one, fails
			// with the error of its canceled context.
			if failed(ev) && e.stopped() {
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
			FailStopped(execCtx, sent)(yield)
		}
	}
}

// admit lets the turn of execCtx run in the process of key: Callers.Admit
// first, then the runner counts the turn in the process, unless
// Callers.Admit counted it with [Runner.AdmitTurn].
func (e *splitExecutor) admit(ctx context.Context, key string, execCtx *a2asrv.ExecutorContext) (func(), error) {
	release := func() {}
	a := &admission{}
	if callers := e.r.cfg.Callers; callers != nil {
		var err error
		if release, err = callers.Admit(context.WithValue(ctx, admissionKey{}, a), key, execCtx); err != nil {
			return nil, err
		}
	}
	if a.counted {
		return release, nil
	}
	counted, err := e.r.admitTurn(ctx, key, nil, nil, AnswersWaiting(execCtx))
	if err != nil {
		release()
		return nil, err
	}
	return func() {
		counted()
		release()
	}, nil
}

// AnswersWaiting reports whether execCtx answers a task that waits for
// input, which continues in the process that asked.
func AnswersWaiting(execCtx *a2asrv.ExecutorContext) bool {
	return execCtx != nil && execCtx.StoredTask != nil && execCtx.StoredTask.Status.State == a2a.TaskStateInputRequired
}

// cancelRefused ends the turn of a waiting task in the agent once an answer
// to it is refused, which fails the task; the agent would otherwise keep
// its session waiting for the answer.
func cancelRefused(ctx context.Context, exec a2asrv.AgentExecutor, execCtx *a2asrv.ExecutorContext) {
	for _, err := range exec.Cancel(ctx, execCtx) {
		if err != nil {
			slog.Warn("canceling a waiting task whose answer was refused", "task", execCtx.TaskID, "error", err)
		}
	}
}

// failed reports whether ev fails its task.
func failed(ev a2a.Event) bool {
	up, ok := ev.(*a2a.TaskStatusUpdateEvent)
	return ok && up.Status.State == a2a.TaskStateFailed
}

// stopped reports whether the runner stopped, which ends the turns: its
// context ended, or Close stopped the turns.
func (e *splitExecutor) stopped() bool {
	if e.ctx != nil && e.ctx.Err() != nil {
		return true
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.canceled
}

// StoppedMessage fails the turns that the stop ended.
const StoppedMessage = "The server stopped before this task finished. Send the message again."

// FailStopped fails the task of execCtx, which the stop ended, so that its
// subscribers get a final event. sent is whether the task was already sent.
func FailStopped(execCtx *a2asrv.ExecutorContext, sent bool) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if !sent && execCtx.StoredTask == nil && !yield(a2a.NewSubmittedTask(execCtx, execCtx.Message), nil) {
			return
		}
		msg := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(StoppedMessage))
		msg.Metadata = map[string]any{InterruptedKey: true}
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
		exec, _, err := e.pick(ctx, execCtx.ContextID, false)
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

// pick returns the executor of a conversation and its key, and starts its
// pool if needed. If remember is set, the conversation keeps its key until
// the keys forget it, in case its session isn't stored yet.
func (e *splitExecutor) pick(ctx context.Context, contextID string, remember bool) (a2asrv.AgentExecutor, string, error) {
	key, err := e.r.keys.of(ctx, contextID)
	if err != nil {
		return nil, "", err
	}
	if key == "" || e.start == nil {
		return e.main, key, nil
	}
	env, err := e.r.environment(ctx, key)
	if err != nil {
		return nil, "", err
	}
	if remember {
		e.r.keys.remember(contextID, key)
	}
	exec, err := e.pool(env.Pool)
	return exec, key, err
}

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
		return nil, ErrStopping
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

// close waits until wait ends for the turns in progress, which end when
// the runner stops, and then stops the agent processes of the pools that
// started.
func (e *splitExecutor) close(ctx, wait context.Context) error {
	e.mu.Lock()
	e.closing = true
	e.mu.Unlock()
	e.running.Wait(wait)
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

// refusal returns err as Hooks.Refused words it.
func (r *Runner) refusal(err error) error {
	if r.cfg.Hooks.Refused == nil {
		return err
	}
	if text := r.cfg.Hooks.Refused(err); text != "" && text != err.Error() {
		return &wordedError{text: text, err: err}
	}
	return err
}

// wordedError is an error with the text the caller gave it.
type wordedError struct {
	text string
	err  error
}

func (e *wordedError) Error() string { return e.text }

func (e *wordedError) Unwrap() error { return e.err }
