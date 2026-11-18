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
	"github.com/a2aproject/a2a-go/v2/a2aevent"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/a2aproject/a2a-serve/core/question"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/approvals"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// waitingTasks finds the tasks that hold a conversation while they wait for
// an answer to a question of the agent.
type waitingTasks struct {
	tasks   *store.TaskStore
	records *approvals.Records
}

// refusal returns why a new task of p in the conversation of contextID would
// wait behind a task that waits for an answer, and how to answer it, or ""
// when no such task holds the conversation. Questions of other agents don't
// hold the conversation.
func (w waitingTasks) refusal(ctx context.Context, p auth.Principal, contextID string) (string, error) {
	history := 0
	waiting, err := w.tasks.List(auth.WithPrincipal(ctx, auth.Owner()), &a2a.ListTasksRequest{
		ContextID: contextID, Status: a2a.TaskStateInputRequired, HistoryLength: &history,
	})
	if err != nil {
		return "", fmt.Errorf("checking conversation %s: %w", contextID, err)
	}
	for _, t := range waiting.Tasks {
		q := question.FromStatus(t.Status)
		if q == nil || asksCallerQuestion(t) {
			continue
		}
		err := w.records.CheckAnswer(ctx, p, t.ID)
		if errors.Is(err, a2a.ErrTaskNotFound) || errors.Is(err, a2a.ErrUnauthorized) {
			return fmt.Sprintf("conversation %s waits for the agent's owner to answer %q; send the message again after they answer, or leave out the context ID to start a new conversation", contextID, q.Title), nil
		}
		if err != nil {
			return "", err
		}
		ids := make([]string, len(q.Options))
		for i, o := range q.Options {
			ids[i] = o.ID
		}
		answer := "with " + a2aCommand(ctx, "send", "--task-id "+string(t.ID)+" <answer>")
		if len(ids) > 0 {
			answer = fmt.Sprintf("with one of %s, as in %s", strings.Join(ids, ", "), a2aCommand(ctx, "send", "--task-id "+string(t.ID)+" "+ids[0]))
		}
		return fmt.Sprintf("task %s in conversation %s waits for an answer to %q; answer it first %s, or leave out the context ID to start a new conversation", t.ID, contextID, q.Title, answer), nil
	}
	return "", nil
}

// queuedTurns are the new tasks of A2A callers whose turn hasn't started,
// usually since a turn ahead of them holds their conversation. When a task
// there stops for a question, they fail with the refusal that a new task
// gets, since they would wait without a word until someone answers.
type queuedTurns struct {
	waiting waitingTasks

	mu    sync.Mutex
	turns map[a2a.TaskID]*queuedTurn
}

type queuedTurn struct {
	// ctx has the caller and the URL that the refusal names.
	ctx       context.Context
	caller    auth.Principal
	contextID string
	// stop ends the turn while it waits.
	stop context.CancelCauseFunc
	// refused is why the task failed, guarded by queuedTurns.mu.
	refused string
}

func newQueuedTurns(waiting waitingTasks) *queuedTurns {
	return &queuedTurns{waiting: waiting, turns: map[a2a.TaskID]*queuedTurn{}}
}

// run runs the new task of execCtx with run, and fails it if a task ahead
// of it waits for an answer before its turn starts. Chats and schedules
// queue their messages themselves.
func (q *queuedTurns) run(ctx context.Context, execCtx *a2asrv.ExecutorContext, run func(context.Context, *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error]) iter.Seq2[a2a.Event, error] {
	caller, ok := auth.Caller(ctx)
	if q == nil || !ok || execCtx.StoredTask != nil || auth.IsInProcess(ctx) {
		return run(ctx, execCtx)
	}
	return func(yield func(a2a.Event, error) bool) {
		turnCtx, stop := context.WithCancelCause(ctx)
		defer stop(nil)
		t := &queuedTurn{ctx: context.WithoutCancel(ctx), caller: caller, contextID: execCtx.ContextID, stop: stop}
		q.add(execCtx.TaskID, t)
		defer q.remove(execCtx.TaskID, t)
		// A question asked after the caller's check passed, and before the
		// task was added here, found nothing to refuse.
		q.refuse(execCtx.TaskID)
		sent, final := false, false
		for ev, err := range run(turnCtx, execCtx) {
			if refused := q.refusedText(t); refused != "" && err == nil && failed(ev) {
				ev = refusedEvent(execCtx, refused)
			}
			if err == nil {
				sent, final = true, a2aevent.IsFinal(ev)
			}
			if !yield(ev, err) {
				return
			}
		}
		// A turn may end at the refusal without a word, as while a
		// recording is transcribed; a canceled task ends with the cancel.
		if refused := q.refusedText(t); refused != "" && !final && ctx.Err() == nil {
			if !sent && !yield(a2a.NewSubmittedTask(execCtx, execCtx.Message), nil) {
				return
			}
			yield(refusedEvent(execCtx, refused), nil)
		}
	}
}

func refusedEvent(execCtx *a2asrv.ExecutorContext, text string) *a2a.TaskStatusUpdateEvent {
	return a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateFailed, a2a.NewMessageForTask(a2a.MessageRoleAgent, execCtx, a2a.NewTextPart(text)))
}

func (q *queuedTurns) add(id a2a.TaskID, t *queuedTurn) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.turns[id] = t
}

func (q *queuedTurns) remove(id a2a.TaskID, t *queuedTurn) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.turns[id] == t {
		delete(q.turns, id)
	}
}

func (q *queuedTurns) refusedText(t *queuedTurn) string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return t.refused
}

// refuseIn fails the queued tasks in the conversation of contextID, if a
// task there waits for an answer. Call it once that task is stored.
func (q *queuedTurns) refuseIn(contextID string) {
	if q == nil {
		return
	}
	q.mu.Lock()
	var queued []a2a.TaskID
	for id, t := range q.turns {
		if t.contextID == contextID && t.refused == "" {
			queued = append(queued, id)
		}
	}
	q.mu.Unlock()
	for _, id := range queued {
		q.refuse(id)
	}
}

// refuse fails the queued task of id if a task ahead of it waits for an
// answer. It marks the task refused before it stops the wait, so that the
// turn can't start once it is refused.
func (q *queuedTurns) refuse(id a2a.TaskID) {
	q.mu.Lock()
	t := q.turns[id]
	q.mu.Unlock()
	if t == nil {
		return
	}
	text, err := q.waiting.refusal(t.ctx, t.caller, t.contextID)
	if err != nil {
		slog.Warn("checking the conversation of a queued task", "task", id, "error", err)
		return
	}
	if text == "" {
		return
	}
	q.mu.Lock()
	if q.turns[id] != t || t.refused != "" {
		q.mu.Unlock()
		return
	}
	t.refused = text
	q.mu.Unlock()
	slog.Info("refusing a queued task: a task ahead of it waits for an answer", "task", id, "context", t.contextID)
	t.stop(errors.New(text))
}

// prompt is the [a2aacp.Config.Prompt] of the agent's executors. The
// executor calls it once the turn holds its conversation, so a queued task
// starts here unless it was refused.
func (q *queuedTurns) prompt(_ context.Context, execCtx *a2asrv.ExecutorContext) ([]*a2a.Part, error) {
	if err := q.start(execCtx.TaskID); err != nil {
		return nil, err
	}
	if execCtx.Message == nil {
		return nil, errors.New("no message to send to the agent")
	}
	return execCtx.Message.Parts, nil
}

// start takes the task of id out of the queue as its turn starts, or fails
// with its refusal.
func (q *queuedTurns) start(id a2a.TaskID) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	t := q.turns[id]
	if t == nil {
		return nil
	}
	if t.refused != "" {
		return errors.New(t.refused)
	}
	delete(q.turns, id)
	return nil
}

// failed reports whether ev fails its task.
func failed(ev a2a.Event) bool {
	up, ok := ev.(*a2a.TaskStatusUpdateEvent)
	return ok && up.Status.State == a2a.TaskStateFailed
}
