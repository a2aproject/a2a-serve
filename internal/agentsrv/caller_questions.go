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
	"slices"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aext/structuredinput"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/a2aproject/a2a-serve/internal/a2atext"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/agentproxy"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/approvals"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/messenger"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// callerQuestionKey is the status message metadata which holds the record
// of another agent's question asked on a caller's task.
const callerQuestionKey = "a2a-serve/remote-question"

// errCallerGone is what a result is refused with when the task that waited
// for it stopped first.
var errCallerGone = errors.New("the task waiting for the result stopped")

// callerQuestionEvent asks q on the task of execCtx.
func callerQuestionEvent(execCtx *a2asrv.ExecutorContext, q *agentproxy.CallerQuestion) *a2a.TaskStatusUpdateEvent {
	parts := []*a2a.Part{a2a.NewTextPart(q.Header + "\n")}
	if q.Msg != nil {
		parts = append(parts, q.Msg.Parts...)
	}
	msg := a2a.NewMessage(a2a.MessageRoleAgent, parts...)
	msg.TaskID, msg.ContextID = execCtx.TaskID, execCtx.ContextID
	msg.Metadata = map[string]any{callerQuestionKey: q.Rec.ID}
	return a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateInputRequired, msg)
}

// callerQuestionOf returns the record ID of another agent's question which
// a stored task asks, if it asks one.
func callerQuestionOf(task *a2a.Task) (string, bool) {
	if task == nil || task.Status.State != a2a.TaskStateInputRequired || task.Status.Message == nil {
		return "", false
	}
	id, ok := task.Status.Message.Metadata[callerQuestionKey].(string)
	return id, ok && id != ""
}

// asksCallerQuestion reports whether a task asks another agent's question,
// which waits at that agent and survives a restart of this server.
func asksCallerQuestion(task *a2a.Task) bool {
	_, ok := callerQuestionOf(task)
	return ok
}

// withCallerQuestion ends a completed turn on the question of another
// agent, relayed during the turn, which the turn's A2A caller answers.
func (e *observedExecutor) withCallerQuestion(execCtx *a2asrv.ExecutorContext, events iter.Seq2[a2a.Event, error]) iter.Seq2[a2a.Event, error] {
	if e.watches == nil || e.unattended.Has(execCtx.ContextID) {
		return events
	}
	end := e.watches.Attend(execCtx.ContextID)
	return func(yield func(a2a.Event, error) bool) {
		defer end(false)
		for ev, err := range events {
			if state, ok := eventState(ev); ok && err == nil && state == a2a.TaskStateCompleted {
				if q := end(true); q != nil {
					ev = callerQuestionEvent(execCtx, q)
				}
			}
			if !yield(ev, err) {
				return
			}
		}
	}
}

// answerCallerQuestion forwards the caller's answer to another agent's
// question asked on its task, waits for that agent, and continues the turn
// with its result, or asks its next question.
func (e *observedExecutor) answerCallerQuestion(ctx context.Context, execCtx *a2asrv.ExecutorContext, recID string) iter.Seq2[a2a.Event, error] {
	return withAnswerOrder(execCtx, func(yield func(a2a.Event, error) bool) {
		rec, err := e.store.QuestionByID(ctx, recID)
		if err != nil {
			yield(nil, fmt.Errorf("loading the question of task %s: %w", execCtx.TaskID, err))
			return
		}
		q := approvals.FromRecord(rec)
		option, ok := replyOption(execCtx.Message, q)
		if !ok {
			asked := execCtx.StoredTask.Status.Message
			ask := a2a.NewMessage(a2a.MessageRoleAgent, asked.Parts...)
			ask.TaskID, ask.ContextID, ask.Metadata = execCtx.TaskID, execCtx.ContextID, asked.Metadata
			yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateInputRequired, ask), nil)
			return
		}
		p, _ := auth.Caller(ctx)
		outcomes, stop := e.watches.Await(execCtx.ContextID)
		defer stop()
		err = e.approvals.Answer(ctx, rec, p, option)
		switch {
		case errors.Is(err, approvals.ErrNotApprover):
			stop()
			yield(nil, fmt.Errorf("%w: only the person who started the task or an owner can answer this approval", a2a.ErrUnauthorized))
			return
		case errors.Is(err, store.ErrAnswered) || errors.Is(err, approvals.ErrClosed):
			// Someone answered it elsewhere, such as in the inspector, so its
			// result is posted into the conversation as a new turn.
			stop()
			msg := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(fmt.Sprintf("This approval for %s was already answered; its result comes into this conversation.", rec.Contact)))
			msg.TaskID, msg.ContextID = execCtx.TaskID, execCtx.ContextID
			yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCompleted, msg), nil)
			return
		case err != nil:
			stop()
			yield(nil, fmt.Errorf("answering %s: %w", rec.Contact, err))
			return
		}
		if !yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateWorking, nil), nil) {
			return
		}
		var o agentproxy.CallerOutcome
		select {
		case <-ctx.Done():
			return
		case o = <-outcomes:
		}
		stop()
		if o.Question != nil {
			yield(callerQuestionEvent(execCtx, o.Question), nil)
			return
		}
		e.continueWithResult(ctx, execCtx, o.Result, yield)
	})
}

// continueWithResult runs the next turn of a task with the result of
// another agent it waited for.
func (e *observedExecutor) continueWithResult(ctx context.Context, execCtx *a2asrv.ExecutorContext, t *messenger.PostedTurn, yield func(a2a.Event, error) bool) {
	msg := a2a.NewMessage(a2a.MessageRoleUser, t.Parts()...)
	msg.TaskID, msg.ContextID = execCtx.TaskID, execCtx.ContextID
	shown := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(t.Text))
	shown.TaskID, shown.ContextID = execCtx.TaskID, execCtx.ContextID
	if !yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateWorking, shown), nil) {
		t.Accept(errCallerGone)
		return
	}
	t.Accept(nil)
	next := *execCtx
	next.Message = msg
	// a2aacp fails a message to a task that waits for input it didn't ask
	// for; the task waits for nothing now.
	stored := *execCtx.StoredTask
	now := time.Now()
	stored.Status = a2a.TaskStatus{State: a2a.TaskStateWorking, Timestamp: &now}
	stored.History = slices.Clone(stored.History)
	next.StoredTask = &stored
	for ev, err := range e.withCallerQuestion(&next, e.turn(ctx, &next)) {
		if !yield(ev, err) {
			return
		}
	}
}

// replyOption returns the option a reply chooses: structured input for the
// question, or typed words.
func replyOption(msg *a2a.Message, q *approvals.Question) (approvals.Option, bool) {
	if msg == nil {
		return approvals.Option{}, false
	}
	for _, p := range msg.Parts {
		meta, ok := structuredinput.GetPartMetadata(p)
		if !ok || meta.Type != structuredinput.PartTypeStructuredInput || meta.ID != q.ID {
			continue
		}
		data, _ := p.Data().(map[string]any)
		id, _ := data["option"].(string)
		for _, o := range q.Options {
			if o.ID == id {
				return o, true
			}
		}
	}
	return q.Match(a2atext.Message(msg))
}
