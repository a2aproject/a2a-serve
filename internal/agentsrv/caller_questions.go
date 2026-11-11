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
	"slices"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aext/structuredinput"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"

	"github.com/a2aproject/a2a-serve/internal/a2aerr"
	"github.com/a2aproject/a2a-serve/internal/a2atext"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/agentproxy"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/approvals"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/chat"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// callerQuestionKey is the status message metadata which holds the record
// of another agent's question asked on a caller's task.
const callerQuestionKey = "a2a-serve/remote-question"

// errCallerGone is what a result is refused with when the task that waited
// for it stopped first.
var errCallerGone = errors.New("the task waiting for the result stopped")

// callerQuestionEvent asks q on task.
func callerQuestionEvent(task a2a.TaskInfoProvider, q *agentproxy.CallerQuestion) *a2a.TaskStatusUpdateEvent {
	parts := []*a2a.Part{a2a.NewTextPart(q.Header + "\n")}
	if q.Msg != nil {
		parts = append(parts, q.Msg.Parts...)
	}
	msg := a2a.NewMessage(a2a.MessageRoleAgent, parts...)
	info := task.TaskInfo()
	msg.TaskID, msg.ContextID = info.TaskID, info.ContextID
	msg.Metadata = map[string]any{callerQuestionKey: q.Rec.ID}
	return a2a.NewStatusUpdateEvent(task, a2a.TaskStateInputRequired, msg)
}

// questionTasks ask the questions of other agents, relayed with nobody to
// ask them, on tasks of their own in the conversations they come from. An
// answer on such a task goes through [observedExecutor.answerCallerQuestion].
type questionTasks struct {
	store *store.Store
	tasks *store.TaskStore
	bus   *chat.Bus
	ends  *taskEnds
}

var _ agentproxy.QuestionTasks = (*questionTasks)(nil)

// Ask implements [agentproxy.QuestionTasks]. The task belongs to whoever
// started the conversation.
func (qt *questionTasks) Ask(ctx context.Context, contextID string, q *agentproxy.CallerQuestion) error {
	ctx = auth.WithPrincipal(ctx, auth.Owner())
	starter, err := qt.tasks.ContextStarter(ctx, contextID)
	if err != nil {
		return err
	}
	task := &a2a.Task{ID: a2a.NewTaskID(), ContextID: contextID}
	task.Status = callerQuestionEvent(task, q).Status
	if _, err := qt.tasks.Create(auth.WithPrincipal(ctx, auth.Principal{ID: starter}), task); err != nil {
		return err
	}
	qt.bus.ExecutionEnded(contextID)
	return nil
}

// End implements [agentproxy.QuestionTasks]. A task that its caller
// answered meanwhile has moved on, and is left alone.
func (qt *questionTasks) End(ctx context.Context, contextID, agent, taskID string) {
	ctx = auth.WithPrincipal(ctx, auth.Owner())
	for _, q := range qt.waiting(ctx, contextID) {
		if q.rec.Contact == agent && q.rec.TaskID == taskID {
			qt.endWaiting(ctx, q)
		}
	}
}

// EndOrphaned implements [agentproxy.QuestionTasks].
func (qt *questionTasks) EndOrphaned(ctx context.Context, watched func(agent, taskID string) bool) {
	ctx = auth.WithPrincipal(ctx, auth.Owner())
	for _, q := range qt.waiting(ctx, "") {
		if !watched(q.rec.Contact, q.rec.TaskID) {
			qt.endWaiting(ctx, q)
		}
	}
}

// waitingQuestion is a task that waits for the answer to the question rec.
type waitingQuestion struct {
	task a2a.TaskID
	rec  store.Question
}

// waiting lists the question tasks of contextID, or of all conversations if
// it is empty, that wait for an answer.
func (qt *questionTasks) waiting(ctx context.Context, contextID string) []waitingQuestion {
	var out []waitingQuestion
	req := &a2a.ListTasksRequest{ContextID: contextID, Status: a2a.TaskStateInputRequired}
	for {
		res, err := qt.tasks.List(ctx, req)
		if err != nil {
			slog.Warn("listing the question tasks", "context", contextID, "error", err)
			return out
		}
		for _, t := range res.Tasks {
			recID, ok := callerQuestionOf(t)
			if !ok {
				continue
			}
			rec, err := qt.store.QuestionByID(ctx, recID)
			if err != nil {
				continue
			}
			out = append(out, waitingQuestion{task: t.ID, rec: rec})
		}
		if res.NextPageToken == "" {
			return out
		}
		req.PageToken = res.NextPageToken
	}
}

func (qt *questionTasks) endWaiting(ctx context.Context, q waitingQuestion) {
	ev, err := qt.end(ctx, q.task, q.rec)
	if err != nil {
		slog.Warn("ending a question task", "task", q.task, "error", err)
		return
	}
	if ev != nil {
		qt.ends.questionEnded(ev)
	}
}

// end completes the question task id, and returns its last event, or nil
// if the task moved on.
func (qt *questionTasks) end(ctx context.Context, id a2a.TaskID, rec store.Question) (*a2a.TaskStatusUpdateEvent, error) {
	stored, err := qt.tasks.Header(ctx, id)
	if err != nil {
		return nil, err
	}
	if cur, ok := callerQuestionOf(stored.Task); !ok || cur != rec.ID {
		return nil, nil
	}
	msg := a2a.NewMessageForTask(a2a.MessageRoleAgent, stored.Task, a2a.NewTextPart(fmt.Sprintf("%s no longer waits for this approval; its result comes into this conversation.", rec.Contact)))
	ev := a2a.NewStatusUpdateEvent(stored.Task, a2a.TaskStateCompleted, msg)
	_, err = qt.tasks.Update(ctx, &taskstore.UpdateRequest{Task: stored.Task, Event: ev, PrevVersion: stored.Version})
	if errors.Is(err, taskstore.ErrConcurrentModification) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return ev, nil
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
	if e.watches == nil || e.ends.unattended.Has(execCtx.ContextID) {
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
	return func(yield func(a2a.Event, error) bool) {
		rec, err := e.store.QuestionByID(ctx, recID)
		if err != nil {
			yield(nil, fmt.Errorf("loading the question of task %s: %w", execCtx.TaskID, err))
			return
		}
		q := approvals.FromRecord(rec)
		option, ok := replyOption(execCtx.Message, q)
		if !ok {
			yield(refusal(execCtx, q), nil)
			return
		}
		p, _ := auth.Caller(ctx)
		outcomes, stop := e.watches.Await(execCtx.ContextID, rec.Contact, rec.TaskID)
		defer stop()
		out, err := e.approvals.Answer(ctx, rec, p, option, auth.DisplayPrincipal(p.ID))
		switch {
		case errors.Is(err, approvals.ErrNotApprover):
			stop()
			yield(nil, fmt.Errorf("%w: only the person who started the task or an owner can answer this approval", a2a.ErrUnauthorized))
			return
		case err != nil:
			stop()
			yield(nil, fmt.Errorf("answering %s: %w", rec.Contact, err))
			return
		case out == approvals.InProgress:
			stop()
			yield(nil, a2aerr.InProgress(fmt.Sprintf("%s is busy with another request; send the answer again", rec.Contact)))
			return
		case out != approvals.Answered:
			// Someone answered it elsewhere, such as in the inspector, so its
			// result is posted into the conversation as a new turn.
			stop()
			msg := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(fmt.Sprintf("This approval for %s was already answered; its result comes into this conversation.", rec.Contact)))
			msg.TaskID, msg.ContextID = execCtx.TaskID, execCtx.ContextID
			yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCompleted, msg), nil)
			return
		}
		if !yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateWorking, nil), nil) {
			return
		}
		var o agentproxy.CallerOutcome
		select {
		case <-ctx.Done():
			return
		case <-e.stopped():
			failStopped(execCtx, true)(yield)
			return
		case o = <-outcomes:
		}
		stop()
		if o.Ended != "" {
			msg := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(o.Ended))
			msg.TaskID, msg.ContextID = execCtx.TaskID, execCtx.ContextID
			yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateFailed, msg), nil)
			return
		}
		if o.Question != nil {
			yield(callerQuestionEvent(execCtx, o.Question), nil)
			return
		}
		e.continueWithResult(ctx, execCtx, o.Result, yield)
	}
}

// stopped is closed when the server stops, and nil when the executor has no
// stop context.
func (e *observedExecutor) stopped() <-chan struct{} {
	if e.stopping == nil {
		return nil
	}
	return e.stopping.Done()
}

// continueWithResult runs the next turn of a task with the result of
// another agent it waited for.
func (e *observedExecutor) continueWithResult(ctx context.Context, execCtx *a2asrv.ExecutorContext, t *chat.PostedTurn, yield func(a2a.Event, error) bool) {
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
// question, a data part with an option, such as `a2a send --data-part`
// sends without the structured input tag, or typed words.
func replyOption(msg *a2a.Message, q *approvals.Question) (approvals.Option, bool) {
	if msg == nil {
		return approvals.Option{}, false
	}
	for _, p := range msg.Parts {
		meta, ok := structuredinput.GetPartMetadata(p)
		if ok && (meta.Type != structuredinput.PartTypeStructuredInput || meta.ID != q.ID) {
			continue
		}
		id, ok := dataOption(p)
		if !ok {
			continue
		}
		for _, o := range q.Options {
			if o.ID == id {
				return o, true
			}
		}
	}
	return q.Match(a2atext.Message(msg))
}

// dataOption returns the option of a data part.
func dataOption(p *a2a.Part) (string, bool) {
	data, _ := p.Data().(map[string]any)
	id, ok := data["option"].(string)
	return id, ok
}
