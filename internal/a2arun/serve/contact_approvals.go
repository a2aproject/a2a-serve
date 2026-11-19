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

package serve

import (
	"context"
	"fmt"
	"iter"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aext/structuredinput"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/a2aproject/a2a-serve/core/approval"
	"github.com/a2aproject/a2a-serve/core/contact"
	"github.com/a2aproject/a2a-serve/internal/agentrun"
	"github.com/a2aproject/a2a-serve/internal/question"
)

// ApprovalMetadataKey is the status message metadata of a task that asks
// the approval of a contact's task on its behalf: {"contact", "task"}.
const ApprovalMetadataKey = "a2a-run/contact-approval"

const (
	// maxPendingApprovals bounds the approvals of contacts that wait for
	// the host; the oldest is forgotten first.
	maxPendingApprovals = 1000
	// resultTimeout bounds the wait for a contact's task after the host
	// answered its approval.
	resultTimeout = time.Hour
	// resultPoll is how often a contact's task is read while its stream
	// isn't available.
	resultPoll = time.Second
	// stopCancelTimeout bounds canceling the tasks at contacts of a task
	// that the stop ends.
	stopCancelTimeout = 10 * time.Second
	// maxResultText bounds the text of a contact's result that goes to the
	// agent after an approval.
	maxResultText = 64 << 10
)

// pendingApproval is an approval that a contact's task waits for, which a
// turn of the agent started.
type pendingApproval struct {
	turn     contact.Turn
	contact  string
	task     *a2a.Task
	approval *approval.Request
	at       time.Time
}

// approvalQueue keeps the approvals that contacts ask in the turns of the
// agent, by the task of the turn, until the turn ends and its task asks
// them of the host. It is the [contact.Followup] of the proxy.
type approvalQueue struct {
	now func() time.Time

	mu sync.Mutex
	// waiting holds the approvals of each task, oldest first.
	waiting map[a2a.TaskID][]*pendingApproval
}

var _ contact.Followup = (*approvalQueue)(nil)

func newApprovalQueue(now func() time.Time) *approvalQueue {
	return &approvalQueue{now: now, waiting: map[a2a.TaskID][]*pendingApproval{}}
}

// Follow keeps an approval that a contact's task waits for. a2a-run has no
// way to bring other tasks the agent didn't wait for back into the
// conversation, so it leaves them.
func (q *approvalQueue) Follow(_ context.Context, t contact.Turn, o contact.Outstanding) {
	if o.Approval == nil || o.Task == nil {
		return
	}
	q.add(&pendingApproval{turn: t, contact: o.Contact, task: o.Task, approval: o.Approval})
}

func (q *approvalQueue) add(p *pendingApproval) {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := q.now()
	p.at = now
	q.prune(now)
	id := a2a.TaskID(p.turn.TaskID)
	list := q.waiting[id]
	if i := slices.IndexFunc(list, func(o *pendingApproval) bool { return o.contact == p.contact && o.task.ID == p.task.ID }); i >= 0 {
		list[i] = p
		return
	}
	if q.count() >= maxPendingApprovals {
		q.forgetOldest()
	}
	q.waiting[id] = append(q.waiting[id], p)
}

// prune forgets the approvals that waited longer than questionTimeout:
// the contact's task gave up on them too.
func (q *approvalQueue) prune(now time.Time) {
	for id, list := range q.waiting {
		list = slices.DeleteFunc(list, func(p *pendingApproval) bool { return now.Sub(p.at) > questionTimeout })
		if len(list) == 0 {
			delete(q.waiting, id)
			continue
		}
		q.waiting[id] = list
	}
}

func (q *approvalQueue) count() int {
	n := 0
	for _, list := range q.waiting {
		n += len(list)
	}
	return n
}

func (q *approvalQueue) forgetOldest() {
	var oldest *pendingApproval
	for _, list := range q.waiting {
		if len(list) > 0 && (oldest == nil || list[0].at.Before(oldest.at)) {
			oldest = list[0]
		}
	}
	if oldest != nil {
		q.remove(oldest)
	}
}

// next returns the oldest approval of the task id, or nil.
func (q *approvalQueue) next(id a2a.TaskID) *pendingApproval {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.prune(q.now())
	if list := q.waiting[id]; len(list) > 0 {
		return list[0]
	}
	return nil
}

// find returns the approval of the task id for the contact's task, or nil.
func (q *approvalQueue) find(id a2a.TaskID, name string, task a2a.TaskID) *pendingApproval {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.prune(q.now())
	for _, p := range q.waiting[id] {
		if p.contact == name && p.task.ID == task {
			return p
		}
	}
	return nil
}

// done forgets p once it is answered.
func (q *approvalQueue) done(p *pendingApproval) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.remove(p)
}

func (q *approvalQueue) remove(p *pendingApproval) {
	id := a2a.TaskID(p.turn.TaskID)
	list := slices.DeleteFunc(q.waiting[id], func(o *pendingApproval) bool { return o == p })
	if len(list) == 0 {
		delete(q.waiting, id)
		return
	}
	q.waiting[id] = list
}

// drop forgets the approvals of the task id, which ended.
func (q *approvalQueue) drop(id a2a.TaskID) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.waiting, id)
}

// middleware returns inner with the relay of the contacts around it, or
// inner alone without contacts.
func (c *contacts) middleware(t *turns, log *slog.Logger, inner func(a2asrv.AgentExecutor) a2asrv.AgentExecutor) func(a2asrv.AgentExecutor) a2asrv.AgentExecutor {
	if c == nil {
		return inner
	}
	return func(next a2asrv.AgentExecutor) a2asrv.AgentExecutor {
		return &relay{next: inner(next), contacts: c, turns: t, log: log}
	}
}

// relay is the executor of the agent that asks the host the approvals
// that contacts ask during a turn, once the turn ends, and sends the
// host's answers back to the contacts. It also cancels the calls of a
// canceled task.
type relay struct {
	next     a2asrv.AgentExecutor
	contacts *contacts
	turns    *turns
	log      *slog.Logger
}

var (
	_ a2asrv.AgentExecutor         = (*relay)(nil)
	_ a2asrv.AgentExecutionCleaner = (*relay)(nil)
)

func (r *relay) Execute(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	if name, task, ok := askedApproval(execCtx.StoredTask); ok {
		return r.answer(ctx, execCtx, name, task)
	}
	return r.watch(execCtx, r.next.Execute(ctx, execCtx))
}

// Cancel cancels the turn, then the tasks that the task started at
// contacts.
func (r *relay) Cancel(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		hops := 0
		if cur, ok := r.turns.current(execCtx.ContextID); ok && cur.task == string(execCtx.TaskID) {
			hops = cur.hops
		}
		for ev, err := range r.next.Cancel(ctx, execCtx) {
			if !yield(ev, err) {
				break
			}
		}
		r.contacts.relay.drop(execCtx.TaskID)
		t := contactTurn(execCtx.ContextID, string(execCtx.TaskID), hops)
		canceled, err := r.contacts.proxy.CancelCalls(context.WithoutCancel(ctx), t)
		if err != nil {
			r.log.Warn("canceling the tasks a canceled task started at contacts", "context", execCtx.ContextID, "task", execCtx.TaskID, "error", redactURLs(err.Error()))
		}
		for _, c := range canceled {
			r.log.Info("canceled a task at a contact", "context", execCtx.ContextID, "task", execCtx.TaskID, "contact", c.Contact, "contact_task", c.TaskID)
		}
	}
}

func (r *relay) Cleanup(ctx context.Context, execCtx *a2asrv.ExecutorContext, result a2a.SendMessageResult, err error) {
	if c, ok := r.next.(a2asrv.AgentExecutionCleaner); ok {
		c.Cleanup(ctx, execCtx, result, err)
	}
}

// watch passes on the events of a turn, and ends a completed turn on the
// first approval that a contact asked during it. A failed or canceled
// turn forgets them.
func (r *relay) watch(execCtx *a2asrv.ExecutorContext, events iter.Seq2[a2a.Event, error]) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		for ev, err := range events {
			if state, ok := stateOf(ev); ok && err == nil {
				switch state {
				case a2a.TaskStateCompleted:
					if p := r.contacts.relay.next(execCtx.TaskID); p != nil {
						ev = r.ask(execCtx, p, "")
					}
				case a2a.TaskStateFailed, a2a.TaskStateCanceled, a2a.TaskStateRejected:
					r.contacts.relay.drop(execCtx.TaskID)
				}
			}
			if !yield(ev, err) {
				return
			}
		}
	}
}

// ask returns the event that asks the host the approval p on the task of
// execCtx, after note, numbered as the task's next question.
func (r *relay) ask(execCtx *a2asrv.ExecutorContext, p *pendingApproval, note string) a2a.Event {
	head := fmt.Sprintf("%s, an agent that this task called, asks for an approval on its task %s:\n", p.contact, p.task.ID)
	parts := []*a2a.Part{a2a.NewTextPart(note + head)}
	if m := p.task.Status.Message; m != nil {
		parts = append(parts, m.Parts...)
	}
	msg := a2a.NewMessage(a2a.MessageRoleAgent, parts...)
	msg.TaskID, msg.ContextID = execCtx.TaskID, execCtx.ContextID
	msg.Metadata = map[string]any{ApprovalMetadataKey: map[string]any{"contact": p.contact, "task": string(p.task.ID)}}
	ev := a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateInputRequired, msg)
	numbered, err := question.NewNumbering(execCtx.StoredTask).Number(ev)
	if err != nil {
		r.log.Warn("numbering the approval of a contact", "task", execCtx.TaskID, "error", err)
		return ev
	}
	return numbered
}

// answer sends the host's reply to the approval of a contact's task that
// the task of execCtx asks, waits for the contact's task, and continues
// the turn with its result, or asks its next approval.
func (r *relay) answer(ctx context.Context, execCtx *a2asrv.ExecutorContext, name string, task a2a.TaskID) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		p := r.contacts.relay.find(execCtx.TaskID, name, task)
		if p == nil {
			yield(statusEvent(execCtx, a2a.TaskStateFailed, fmt.Sprintf("The approval of %s no longer waits for an answer here: its task ended, or a2a-run forgot it. Send your message again.", name)), nil)
			return
		}
		q := question.FromStatus(execCtx.StoredTask.Status)
		option, ok := replyOption(execCtx.Message, q)
		if !ok || q.Check(question.ReplyRef(execCtx.Message)) != question.Matches {
			ids := make([]string, len(q.Options))
			for i, o := range q.Options {
				ids[i] = o.ID
			}
			yield(r.ask(execCtx, p, fmt.Sprintf("I couldn't use the reply. This question still waits for an answer. Reply with one of: %s.\n", strings.Join(ids, ", "))), nil)
			return
		}
		outer := ctx
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		stopWatch := context.AfterFunc(r.contacts.stopped, cancel)
		defer stopWatch()
		target, err := r.contacts.dir.Target(ctx, p.turn, p.contact)
		if err != nil {
			r.contacts.relay.drop(execCtx.TaskID)
			yield(statusEvent(execCtx, a2a.TaskStateFailed, fmt.Sprintf("Sending the answer to %s failed: %v", p.contact, err)), nil)
			return
		}
		msg, err := approval.Answer(p.task, p.approval, option.ID)
		if err == nil {
			_, err = contact.Dial(ctx, p.turn, target, msg)
		}
		r.contacts.relay.done(p)
		if r.stopping(outer, execCtx, p, yield) {
			return
		}
		if err != nil {
			r.contacts.relay.drop(execCtx.TaskID)
			r.log.Warn("sending the host's answer to a contact", "contact", p.contact, "task", p.task.ID, "error", redactURLs(err.Error()))
			yield(statusEvent(execCtx, a2a.TaskStateFailed, fmt.Sprintf("Sending the answer to %s failed: %s", p.contact, redactURLs(err.Error()))), nil)
			return
		}
		if !yield(statusEvent(execCtx, a2a.TaskStateWorking, fmt.Sprintf("Sent the answer to %s; waiting for its task.", p.contact)), nil) {
			return
		}
		result, err := waitSettled(ctx, r.contacts.proxy.Guard(p.turn, p.contact, target), p.task.ID, p.task.Status)
		if r.stopping(outer, execCtx, p, yield) || outer.Err() != nil {
			return
		}
		if err != nil {
			r.contacts.relay.drop(execCtx.TaskID)
			yield(statusEvent(execCtx, a2a.TaskStateFailed, fmt.Sprintf("Waiting for %s after the answer failed: %s", p.contact, redactURLs(err.Error()))), nil)
			return
		}
		if a := approval.FromStatus(result.Status); a != nil && a.Type == approval.TypePermission && result.Status.State == a2a.TaskStateInputRequired {
			next := &pendingApproval{turn: p.turn, contact: p.contact, task: result, approval: a}
			r.contacts.relay.add(next)
			yield(r.ask(execCtx, next, ""), nil)
			return
		}
		r.continueTurn(ctx, execCtx, resultText(p.contact, option, result), yield)
	}
}

// stopping reports whether the server stops. Then it fails the task of
// execCtx as the runner fails the turns that the stop ends, and cancels
// the tasks it started at contacts, which nobody waits for anymore.
func (r *relay) stopping(ctx context.Context, execCtx *a2asrv.ExecutorContext, p *pendingApproval, yield func(a2a.Event, error) bool) bool {
	if r.contacts.stopped.Err() == nil {
		return false
	}
	r.contacts.relay.drop(execCtx.TaskID)
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopCancelTimeout)
	defer cancel()
	if _, err := r.contacts.proxy.CancelCalls(cctx, p.turn); err != nil {
		r.log.Warn("canceling the tasks at contacts of a task that the stop ended", "task", execCtx.TaskID, "error", redactURLs(err.Error()))
	}
	agentrun.FailStopped(execCtx, true)(yield)
	return true
}

// continueTurn runs the next turn of the task of execCtx with text.
func (r *relay) continueTurn(ctx context.Context, execCtx *a2asrv.ExecutorContext, text string, yield func(a2a.Event, error) bool) {
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))
	msg.TaskID, msg.ContextID = execCtx.TaskID, execCtx.ContextID
	next := *execCtx
	next.Message = msg
	// The agent fails a message to a task that waits for input it didn't
	// ask for; the task waits for nothing now.
	stored := *execCtx.StoredTask
	now := time.Now()
	stored.Status = a2a.TaskStatus{State: a2a.TaskStateWorking, Timestamp: &now}
	stored.History = slices.Clone(stored.History)
	next.StoredTask = &stored
	for ev, err := range r.watch(&next, r.next.Execute(ctx, &next)) {
		if !yield(ev, err) {
			return
		}
	}
}

// waitSettled waits until the contact's task id ends or waits for input
// again, and returns it. A status the same as answered, the one that the
// answer replied to, is still waiting for the answer: a server can return
// at once, before it processes the reply.
func waitSettled(ctx context.Context, h a2asrv.RequestHandler, id a2a.TaskID, answered a2a.TaskStatus) (*a2a.Task, error) {
	ctx, cancel := context.WithTimeout(ctx, resultTimeout)
	defer cancel()
	moved := func(st a2a.TaskStatus) bool { return settled(st.State) && !sameStatus(st, answered) }
	for {
		task, err := h.GetTask(ctx, &a2a.GetTaskRequest{ID: id})
		if err != nil {
			return nil, err
		}
		if moved(task.Status) {
			return task, nil
		}
		for ev, err := range h.SubscribeToTask(ctx, &a2a.SubscribeToTaskRequest{ID: id}) {
			if err != nil {
				break
			}
			if st, ok := statusOf(ev); ok && moved(st) {
				break
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(resultPoll):
		}
	}
}

// sameStatus reports whether a and b are one status: the same state,
// message and time.
func sameStatus(a, b a2a.TaskStatus) bool {
	if a.State != b.State || (a.Message == nil) != (b.Message == nil) || (a.Message != nil && a.Message.ID != b.Message.ID) {
		return false
	}
	if a.Timestamp == nil || b.Timestamp == nil {
		return a.Timestamp == b.Timestamp
	}
	return a.Timestamp.Equal(*b.Timestamp)
}

func settled(s a2a.TaskState) bool {
	return s.Terminal() || s == a2a.TaskStateInputRequired || s == a2a.TaskStateAuthRequired
}

// resultText tells the agent what became of the contact's task after the
// host answered its approval with option.
func resultText(name string, option question.Option, task *a2a.Task) string {
	var texts []string
	for _, art := range task.Artifacts {
		texts = append(texts, partsText(art.Parts))
	}
	if m := task.Status.Message; m != nil {
		texts = append(texts, partsText(m.Parts))
	}
	out := strings.TrimSpace(strings.Join(slices.DeleteFunc(texts, func(s string) bool { return s == "" }), "\n"))
	if len(out) > maxResultText {
		out = strings.ToValidUTF8(out[:maxResultText], "") + " (cut)"
	}
	s := fmt.Sprintf("The host answered %q to the approval that %s asked on its task %s. That task is now %s.", option.Label, name, task.ID, strings.TrimPrefix(strings.ToLower(string(task.Status.State)), "task_state_"))
	if task.Status.State == a2a.TaskStateInputRequired {
		s += fmt.Sprintf(" It asks for more input; answer with a2a send -a %s/%s --task-id %s.", agentsURL, name, task.ID)
	}
	if out != "" {
		s += " It says:\n" + out
	}
	return s
}

// askedApproval returns the contact and the task whose approval task asks,
// if it asks one.
func askedApproval(task *a2a.Task) (string, a2a.TaskID, bool) {
	if task == nil || task.Status.State != a2a.TaskStateInputRequired || task.Status.Message == nil {
		return "", "", false
	}
	ref, _ := task.Status.Message.Metadata[ApprovalMetadataKey].(map[string]any)
	name, _ := ref["contact"].(string)
	id, _ := ref["task"].(string)
	return name, a2a.TaskID(id), name != "" && id != ""
}

// replyOption returns the option a reply chooses: structured input for the
// question, a data part with an option, or typed words.
func replyOption(msg *a2a.Message, q *question.Question) (question.Option, bool) {
	if msg == nil || q == nil {
		return question.Option{}, false
	}
	for _, p := range msg.Parts {
		meta, ok := structuredinput.GetPartMetadata(p)
		if ok && (meta.Type != structuredinput.PartTypeStructuredInput || meta.ID != q.ID) {
			continue
		}
		data, _ := p.Data().(map[string]any)
		id, ok := data["option"].(string)
		if !ok {
			continue
		}
		for _, o := range q.Options {
			if o.ID == id {
				return o, true
			}
		}
	}
	return q.Match(partsText(msg.Parts))
}

func stateOf(ev a2a.Event) (a2a.TaskState, bool) {
	st, ok := statusOf(ev)
	return st.State, ok
}

func statusOf(ev a2a.Event) (a2a.TaskStatus, bool) {
	switch e := ev.(type) {
	case *a2a.TaskStatusUpdateEvent:
		return e.Status, true
	case *a2a.Task:
		return e.Status, true
	}
	return a2a.TaskStatus{}, false
}

func statusEvent(execCtx *a2asrv.ExecutorContext, state a2a.TaskState, text string) *a2a.TaskStatusUpdateEvent {
	msg := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(text))
	msg.TaskID, msg.ContextID = execCtx.TaskID, execCtx.ContextID
	return a2a.NewStatusUpdateEvent(execCtx, state, msg)
}

func partsText(parts []*a2a.Part) string {
	var out []string
	for _, p := range parts {
		if t := p.Text(); t != "" {
			out = append(out, t)
		}
	}
	return strings.Join(out, "")
}

var urlPattern = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://[^\s"']+`)

// redactURLs replaces the URLs in s, which may carry secrets.
func redactURLs(s string) string {
	return urlPattern.ReplaceAllString(s, "<url>")
}
