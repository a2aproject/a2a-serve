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
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aext/structuredinput"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/limiter"
	"github.com/a2aproject/a2a-go/v2/integrations/executor/a2aacp"

	"github.com/a2aproject/a2a-serve/internal/a2aerr"
	"github.com/a2aproject/a2a-serve/internal/a2atext"
	"github.com/a2aproject/a2a-serve/internal/agentcfg"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agentkey"
	"github.com/a2aproject/a2a-serve/internal/agentrun"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/agentproxy"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/approvals"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/chat"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/question"
	"github.com/a2aproject/a2a-serve/internal/store"
	"github.com/a2aproject/a2a-serve/internal/toolrules"
	"github.com/a2aproject/a2a-serve/internal/turncount"
)

// failure returns the status message of a turn that fails with err, for
// callers. It drops the protocol details of the agent's error, and explains
// the failures that have a next step. It also covers the tasks whose agent
// stopped while they waited for an answer.
func (s *Server) failure(ctx context.Context, execCtx *a2asrv.ExecutorContext, err error) []*a2a.Part {
	reported := err.Error()
	cfg := s.rt.Config()
	granted := cfg.Secrets
	key := ""
	if k, err := s.engine.KeyOf(ctx, execCtx.ContextID); err == nil {
		key = k
		if sb, err := s.sandboxes.of(ctx, k); err == nil {
			granted = nil
			for _, g := range sb.Secrets {
				granted = append(granted, g.Name)
			}
		}
	}
	if missing, ok := errors.AsType[*agentcfg.MissingSecretError](err); ok && !missing.Main {
		slog.Warn("an agent process of a caller can't start: "+agentcfg.MissingSecretHint(missing.Name), "key", key)
	}
	text := explainFailure(cfg, granted, agentcfg.Explain(err).Error())
	if text == reported {
		return nil
	}
	slog.Info("the agent failed the turn", "task", execCtx.TaskID, "error", reported)
	return []*a2a.Part{a2a.NewTextPart(text)}
}

// agentExited ends the error of a turn whose agent process exited, such as
// "acp agent error: acp agent exited".
const agentExited = "acp agent exited"

// explainFailure returns the failure text of a turn for callers, in an
// agent container that gets the secrets granted. It tells which key is
// missing when the agent doesn't know the configured model: opencode lists
// no models of a provider without a key, so it reports only that the model
// isn't found.
func explainFailure(cfg *agentdir.Config, granted []string, reported string) string {
	if strings.HasSuffix(reported, agentExited) {
		return "the agent stopped unexpectedly; send the message again"
	}
	text := agentErrorText(reported)
	if !modelNotFound(text) && !authFailed(text) {
		return text
	}
	if provider, _ := agentcfg.ModelKeyWithheld(cfg, granted); provider != "" {
		return "The owner hasn't given this agent a model key. The agent reported: " + text
	}
	if !modelNotFound(text) {
		return text
	}
	if hint := agentcfg.MissingKey(cfg); hint != "" {
		return hint + "; the agent reported: " + text
	}
	// Other agents take their model in their command.
	if !slices.Contains(agentdir.Harnesses, cfg.Harness) {
		return text
	}
	model := "<model>"
	if cfg.Harness == "opencode" {
		model = "<provider/model>"
	}
	return text + "; choose another model with `a2a-serve config set --model " + model + " --restart`"
}

// authFailedText matches the failures of an agent or its provider that
// got no valid credentials, as in "Invalid API key", "Authentication
// required" or claude's "Please run /login".
var authFailedText = regexp.MustCompile(`(?i)\bapi[ _-]?key\b|\bauthenticat|\bunauthori[sz]ed\b|/login\b`)

func authFailed(text string) bool { return authFailedText.MatchString(text) }

// modelNotFoundText matches the failures of an agent or its provider that
// doesn't know the model, as in "model not found: google/x", "models/x is
// not found for API version v1beta", "The model `x` does not exist" or
// Anthropic's not_found_error for "model: x".
var modelNotFoundText = regexp.MustCompile("(?i)\\bmodels?[/: `][^\\n]*?\\b(?:not found|does not exist)|not_found_error[^\\n]*?\\bmodel\\b")

func modelNotFound(text string) bool { return modelNotFoundText.MatchString(text) }

// acpError matches a failure that a2aacp reports for an error response of
// the agent: its message, then its JSON-RPC code and data.
var acpError = regexp.MustCompile(`(?s)^(?:acp agent error: |configuring acp session: )?(.*?) \(code -?\d+\)(?:: .*)?$`)

// jsonrpcErrorNames start the messages of agents that report a standard
// JSON-RPC error with details.
var jsonrpcErrorNames = []string{"Parse error: ", "Invalid request: ", "Method not found: ", "Invalid params: ", "Internal error: "}

// agentErrorText returns the failure text of a turn without the protocol
// details of the agent's error.
func agentErrorText(text string) string {
	m := acpError.FindStringSubmatch(text)
	if m == nil {
		return text
	}
	msg := m[1]
	for _, name := range jsonrpcErrorNames {
		if i := strings.Index(msg, name); i >= 0 {
			msg = msg[:i] + msg[i+len(name):]
			break
		}
	}
	return "the agent failed: " + msg
}

// observedExecutor publishes every event of the agent to the server's event
// bus and relays questions which the task's caller can't answer.
type observedExecutor struct {
	inner     a2asrv.AgentExecutor
	bus       *chat.Bus
	claims    *CallClaims
	approvals *approvals.Service
	// ends does what follows the end of an execution.
	ends *taskEnds
	// watches ask the A2A caller of a turn the questions of the agents it
	// calls, on the caller's task.
	watches *agentproxy.Watches
	// store has the records of those questions.
	store *store.Store
	// notes are put in the results of tasks.
	notes *agentrun.Notes
	// moved continues conversations whose session is in another workspace.
	moved *movedConversations
	// call describes the tool call of a permission, for the rule that the
	// always option allows.
	call func(kind, title string, input any) toolrules.Call
	// turns counts the executions until a2a-go stored their last event and
	// called Cleanup.
	turns *turncount.Count
	// stopping ends when the server stops, so that a task waiting for
	// another agent doesn't hold the stop. Nil never ends.
	stopping context.Context
	// answers saves what answers to permission questions allow.
	answers *ruleAnswers
	// voice transcribes recordings and speaks replies; nil without a
	// speech service.
	voice *voiceTurns
	// queued fails the new tasks that wait behind a question in their
	// conversation; nil runs them as they come.
	queued *queuedTurns
}

var (
	_ a2asrv.AgentExecutor         = (*observedExecutor)(nil)
	_ a2asrv.AgentExecutionCleaner = (*observedExecutor)(nil)
)

// Cleanup ends the execution and relays the question the task stopped on.
// a2a-go calls it once the execution is done with its last event and
// stored it, so an answer to the question can start the next execution.
func (e *observedExecutor) Cleanup(ctx context.Context, execCtx *a2asrv.ExecutorContext, result a2a.SendMessageResult, err error) {
	if e.turns != nil {
		defer e.turns.Release(execCtx)
	}
	task, ok := result.(*a2a.Task)
	var state a2a.TaskState
	switch {
	case err != nil:
	case ok:
		state = task.Status.State
	default:
		state = a2a.TaskStateCompleted
	}
	e.ends.ended(ctx, execCtx.TaskID, execCtx.ContextID, state)
	if err != nil || !ok {
		return
	}
	if q := question.FromStatus(task.Status); q != nil && !askedBefore(execCtx.StoredTask, q) {
		e.approvals.OnQuestion(ctx, execCtx, q)
	}
	if state == a2a.TaskStateInputRequired {
		e.queued.refuseIn(execCtx.ContextID)
	}
}

func (e *observedExecutor) Execute(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	if note, ok := otherQuestionNote(execCtx); ok {
		return e.observe(ctx, execCtx, askAgain(reask(execCtx, note)))
	}
	if id, ok := callerQuestionOf(execCtx.StoredTask); ok {
		return e.observe(ctx, execCtx, e.answerCallerQuestion(ctx, execCtx, id))
	}
	// a2aacp takes a reply that isn't an answer as a new instruction, and
	// rejects the tool call without saying so.
	q := pendingApproval(execCtx.StoredTask)
	if q == nil {
		return e.observe(ctx, execCtx, e.withCallerQuestion(execCtx, e.queued.run(ctx, execCtx, e.turn)))
	}
	opt, chosen := replyOption(execCtx.Message, q)
	_, ownOptions := q.AgentOption(a2aacp.OptionOnce)
	if !answersQuestion(execCtx.Message, q) || (ownOptions && !chosen) {
		return e.observe(ctx, execCtx, refuseReply(execCtx, q))
	}
	answer := execCtx
	if ownOptions {
		p, _ := auth.Caller(ctx)
		if err := e.answers.record(ctx, p, execCtx, q, opt); err != nil {
			slog.Error("saving what an approval allows", "task", execCtx.TaskID, "error", err)
			e.approvals.Release(context.WithoutCancel(ctx), execCtx.TaskID, q.ID, p.ID)
			return e.observe(ctx, execCtx, askAgain(reask(execCtx, "I couldn't save the answer, so the question still waits for an answer. Try again.\n")))
		}
	}
	// The agent takes only structured input for the question, and the
	// reply may be words or an untagged data part.
	if chosen {
		var err error
		if answer, err = agentAnswer(execCtx, q, opt); err != nil {
			slog.Warn("translating an approval", "task", execCtx.TaskID, "error", err)
			p, _ := auth.Caller(ctx)
			e.approvals.Release(context.WithoutCancel(ctx), execCtx.TaskID, q.ID, p.ID)
			return e.observe(ctx, execCtx, askAgain(reask(execCtx, "I couldn't pass the answer on, so the question still waits for an answer. Try again.\n")))
		}
	}
	return e.observe(ctx, execCtx, e.withAnswer(ctx, execCtx, q, e.withCallerQuestion(answer, e.turn(ctx, answer))))
}

// agentAnswer returns execCtx with its reply to q as structured input with
// the agent's option. For a question with the server's options, "always"
// becomes the option that allows once: [ruleAnswers] saved the grant of
// the conversation, and the server enforces it.
func agentAnswer(execCtx *a2asrv.ExecutorContext, q *question.Question, opt question.Option) (*a2asrv.ExecutorContext, error) {
	agentOption, ok := q.AgentOption(opt.ID)
	if !ok {
		agentOption = opt.ID
	}
	part, err := structuredinput.NewResponsePart(q.ID, a2aacp.Answer{Option: agentOption})
	if err != nil {
		return nil, err
	}
	msg := *execCtx.Message
	msg.Parts = []*a2a.Part{part}
	out := *execCtx
	out.Message = &msg
	return &out, nil
}

// withServerOptions gives the permission questions of the agent the
// server's options, with the narrowest useful rule for the always option.
func (e *observedExecutor) withServerOptions(execCtx *a2asrv.ExecutorContext, events iter.Seq2[a2a.Event, error]) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		for ev, err := range events {
			if err == nil {
				ev = e.serverOptions(execCtx, ev)
			}
			if !yield(ev, err) {
				return
			}
		}
	}
}

func (e *observedExecutor) serverOptions(execCtx *a2asrv.ExecutorContext, ev a2a.Event) a2a.Event {
	var status a2a.TaskStatus
	switch v := ev.(type) {
	case *a2a.TaskStatusUpdateEvent:
		status = v.Status
	case *a2a.Task:
		status = v.Status
	default:
		return ev
	}
	q := question.FromStatus(status)
	if q == nil || q.Type != question.TypePermission {
		return ev
	}
	rule := ""
	if e.call != nil {
		rule = toolrules.Suggest(e.call(q.Kind, q.ToolTitle, q.ToolInput))
	}
	msg, err := question.WithServerOptions(status.Message, rule)
	if err != nil {
		slog.Warn("writing the options of an approval", "task", execCtx.TaskID, "error", err)
		return ev
	}
	switch v := ev.(type) {
	case *a2a.TaskStatusUpdateEvent:
		c := *v
		c.Status.Message = msg
		return &c
	case *a2a.Task:
		c := *v
		c.Status.Message = msg
		return &c
	}
	return ev
}

// turn runs a turn of the agent, with its recordings as text and its reply
// spoken when voice is set up.
func (e *observedExecutor) turn(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	if e.voice == nil {
		return e.agentTurn(ctx, execCtx)
	}
	return e.voice.turn(ctx, execCtx, e.agentTurn)
}

// agentTurn runs a turn of the agent, with the line of the agents it may
// call and the earlier messages of a moved conversation added to the prompt
// and kept out of the task store.
func (e *observedExecutor) agentTurn(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	ctx, withLine := e.claims.withAgentsLine(ctx, execCtx)
	if execCtx.Message != nil {
		e.claims.addTokens(execCtx.ContextID, estimateTokens(execCtx.Message.Parts))
	}
	prompted, moved := e.moved.withEarlier(ctx, withLine)
	events := agentrun.CoalesceArtifacts(e.claims.countTokens(execCtx.ContextID, e.withServerOptions(execCtx, e.inner.Execute(ctx, prompted))), agentrun.CoalesceWindow)
	if prompted == execCtx && !moved {
		return events
	}
	return func(yield func(a2a.Event, error) bool) {
		if moved {
			e.notes.Add(execCtx.TaskID, movedNote)
		}
		for ev, err := range events {
			if t, ok := ev.(*a2a.Task); ok {
				ev = withOriginalMessage(t, prompted.Message, execCtx.Message)
			}
			if !yield(ev, err) {
				return
			}
		}
	}
}

// withOriginalMessage replaces the message with the agents line in a task's
// history by the caller's message, so the line stays out of the task store.
func withOriginalMessage(t *a2a.Task, withLine, original *a2a.Message) *a2a.Task {
	if !slices.Contains(t.History, withLine) {
		return t
	}
	c := *t
	c.History = slices.Clone(t.History)
	for i, m := range c.History {
		if m == withLine {
			c.History[i] = original
		}
	}
	return &c
}

func (e *observedExecutor) Cancel(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	// a2asrv cancels ctx once it has the canceled event, which comes before
	// the inner Cancel runs, but the turn must still stop.
	return e.observe(ctx, execCtx, canceledFirst(execCtx, e.cancelingCalls(ctx, execCtx, e.inner.Cancel(context.WithoutCancel(ctx), execCtx))))
}

// cancelingCalls cancels the tasks that the task started at other agents,
// once the inner Cancel stopped its turn and with it new calls.
func (e *observedExecutor) cancelingCalls(ctx context.Context, execCtx *a2asrv.ExecutorContext, events iter.Seq2[a2a.Event, error]) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		defer e.cancelCalls(context.WithoutCancel(ctx), execCtx)
		for ev, err := range events {
			if !yield(ev, err) {
				return
			}
		}
	}
}

func (e *observedExecutor) cancelCalls(ctx context.Context, execCtx *a2asrv.ExecutorContext) {
	if e.watches == nil || e.claims == nil {
		return
	}
	if err := e.watches.CancelCalls(ctx, e.claims.claimsOf(ctx, execCtx)); err != nil {
		slog.Warn("canceling the calls of a canceled task", "task", execCtx.TaskID, "error", err)
	}
}

// canceledFirst yields the canceled event before the inner Cancel runs.
// a2aacp stops the turn before yielding it, which can end a running
// execution first, so that the cancel resolves to the state the execution
// ended in, such as input-required. The inner events still run for their
// side effects; its canceled event is dropped.
func canceledFirst(execCtx *a2asrv.ExecutorContext, events iter.Seq2[a2a.Event, error]) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		open := yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCanceled, nil), nil)
		for ev, err := range events {
			if !open {
				continue
			}
			if u, ok := ev.(*a2a.TaskStatusUpdateEvent); ok && err == nil && u.Status.State == a2a.TaskStateCanceled {
				continue
			}
			open = yield(ev, err)
		}
	}
}

func (e *observedExecutor) observe(ctx context.Context, execCtx *a2asrv.ExecutorContext, events iter.Seq2[a2a.Event, error]) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if e.turns != nil {
			e.turns.Hold(execCtx)
		}
		start := time.Now()
		from := ""
		if p, ok := auth.Caller(ctx); ok {
			from = p.ID
		}
		slog.Info("task received", "task", execCtx.TaskID, "context", execCtx.ContextID, "from", from)
		stopNotes := e.notes.Start(execCtx.TaskID)
		defer stopNotes()
		closed := false
		numbering := question.NewNumbering(execCtx.StoredTask)
		for ev, err := range events {
			if err != nil {
				slog.Info("task failed", "task", execCtx.TaskID, "duration", time.Since(start).Round(time.Millisecond), "error", err)
			}
			if err == nil {
				if numbered, nerr := numbering.Number(ev); nerr == nil {
					ev = numbered
				} else {
					slog.Warn("numbering a question", "task", execCtx.TaskID, "error", nerr)
				}
			}
			if state, ok := eventState(ev); ok && err == nil && state.Terminal() {
				ev = agentrun.WithNotes(execCtx, ev, e.notes.Take(execCtx.TaskID))
			}
			if err == nil {
				logTransition(execCtx.TaskID, ev, start)
				if state, ok := eventState(ev); ok && state != a2a.TaskStateInputRequired && !closed {
					closed = true
					e.approvals.CloseQuestions(ctx, execCtx.TaskID)
				}
				e.bus.Publish(execCtx.TaskID, ev)
			}
			if !yield(ev, err) {
				return
			}
		}
	}
}

// maxLoggedReason bounds the reason a failed task logs.
const maxLoggedReason = 300

// logTransition logs a task reaching a state where it stops: it finished or
// waits for input. A failed or rejected task logs the reason callers got.
func logTransition(id a2a.TaskID, ev a2a.Event, start time.Time) {
	state, ok := eventState(ev)
	if !ok {
		return
	}
	if !state.Terminal() && state != a2a.TaskStateInputRequired && state != a2a.TaskStateAuthRequired {
		return
	}
	attrs := []any{"task", id, "duration", time.Since(start).Round(time.Millisecond)}
	if msg := eventMessage(ev); msg != nil && (state == a2a.TaskStateFailed || state == a2a.TaskStateRejected) {
		attrs = append(attrs, "reason", a2atext.Snippet(a2atext.Message(msg), maxLoggedReason))
	}
	slog.Info("task "+a2atext.StateName(state), attrs...)
}

// eventMessage returns the status message an event reports, if any.
func eventMessage(ev a2a.Event) *a2a.Message {
	switch e := ev.(type) {
	case *a2a.Task:
		return e.Status.Message
	case *a2a.TaskStatusUpdateEvent:
		return e.Status.Message
	}
	return nil
}

// eventState returns the task state an event reports, if it reports one.
func eventState(ev a2a.Event) (a2a.TaskState, bool) {
	switch e := ev.(type) {
	case *a2a.Task:
		return e.Status.State, true
	case *a2a.TaskStatusUpdateEvent:
		return e.Status.State, true
	}
	return "", false
}

// callerInterceptor makes the authenticated principal the A2A user and
// enforces who may join a conversation, who may answer approvals, how deep
// agent chains go and the budget of guests.
type callerInterceptor struct {
	a2asrv.PassthroughCallInterceptor
	approvals *approvals.Service
	tasks     *store.TaskStore
	waiting   waitingTasks
	budget    *guestBudget
	// engine returns the engine of the agent.
	engine func() *agentrun.Runner
	// dir holds the agent homes of members.
	dir agentdir.Dir
	// removeHome forgets what the server keeps about an agent home that
	// goes.
	removeHome func(home string)
}

// newTaskKey marks the context of a request that starts a task.
type newTaskKey struct{}

// Before admits a request, and logs the refusal of a new task, so that the
// owner sees why callers' messages don't run.
func (i *callerInterceptor) Before(ctx context.Context, callCtx *a2asrv.CallContext, req *a2asrv.Request) (context.Context, any, error) {
	ctx, res, err := i.admit(ctx, callCtx, req)
	msg, ok := req.Payload.(*a2a.SendMessageRequest)
	if !ok || msg.Message == nil || msg.Message.TaskID != "" {
		return ctx, res, err
	}
	if err != nil {
		logRefused(ctx, err)
		return ctx, res, err
	}
	return context.WithValue(ctx, newTaskKey{}, true), res, nil
}

// logRefused logs that the server refused a new task of the caller with
// err.
func logRefused(ctx context.Context, err error) {
	from := ""
	if p, ok := auth.Caller(ctx); ok {
		from = p.ID
	}
	slog.Info("task refused", "from", from, "error", err)
}

func (i *callerInterceptor) admit(ctx context.Context, callCtx *a2asrv.CallContext, req *a2asrv.Request) (context.Context, any, error) {
	p, ok := auth.Caller(ctx)
	if !ok {
		return ctx, nil, a2a.ErrUnauthenticated
	}
	if p.Hops > auth.MaxHops {
		return ctx, nil, fmt.Errorf("%w: too many agents in this chain (%d)", a2a.ErrUnauthorized, p.Hops)
	}
	auth.SetCallUser(callCtx, p)
	if !p.IsOwner() {
		ctx = limiter.AttachScope(ctx, p.ID)
	}
	// a2a-go streams a running task to any subscriber, so subscribers get
	// the same scoping as GetTask.
	if sub, ok := req.Payload.(*a2a.SubscribeToTaskRequest); ok {
		_, err := i.tasks.Header(ctx, sub.ID)
		return ctx, nil, err
	}
	// Guests who proved nothing share one principal, so they
	// list only the conversation they name, whose ID proves it is theirs.
	if list, ok := req.Payload.(*a2a.ListTasksRequest); ok && p.ID == auth.GuestID && list.ContextID == "" {
		return ctx, &a2a.ListTasksResponse{Tasks: []*a2a.Task{}, PageSize: list.PageSize}, nil
	}
	msg, ok := req.Payload.(*a2a.SendMessageRequest)
	if !ok || msg.Message == nil {
		return ctx, nil, nil
	}
	if msg.Config != nil {
		ctx = withAcceptedModes(ctx, msg.Config.AcceptedOutputModes)
	}
	if err := i.checkConversation(ctx, p, msg.Message.ContextID); err != nil {
		return ctx, nil, err
	}
	if !p.IsOwner() && msg.Message.TaskID == "" {
		if err := i.checkUnfinished(ctx, p); err != nil {
			return ctx, nil, err
		}
	}
	if msg.Message.TaskID == "" {
		if err := i.checkWaiting(ctx, p, msg.Message.ContextID); err != nil {
			return ctx, nil, err
		}
		if err := i.checkUserAgents(ctx, msg.Message.ContextID); err != nil {
			return ctx, nil, err
		}
	}
	if p.Role == auth.RoleGuest && msg.Message.TaskID == "" {
		if err := i.budget.admit(budgetListener(p)); err != nil {
			return ctx, nil, fmt.Errorf("%w: %w", a2a.ErrUnauthorized, err)
		}
	}
	if msg.Message.TaskID != "" {
		// a2a-go reports a running execution before it loads the task in
		// the caller's scope, which tells another person's task from an
		// unknown one.
		header, headerErr := i.tasks.Header(ctx, msg.Message.TaskID)
		if errors.Is(headerErr, a2a.ErrTaskNotFound) {
			return ctx, nil, headerErr
		}
		if err := i.tasks.CheckNotFailed(ctx, msg.Message.TaskID); err != nil {
			return ctx, nil, err
		}
		if err := i.approvals.CheckAnswer(ctx, p, msg.Message.TaskID); err != nil {
			return ctx, nil, err
		}
		if headerErr == nil {
			ctx = i.withReply(ctx, msg.Message, header.Task)
		}
	}
	return ctx, nil, nil
}

// replyKey is the context key of the [reply] a request sends.
type replyKey struct{}

// reply is a message to an existing task, which the caller can see.
type reply struct {
	task      a2a.TaskID
	contextID string
	// approval is set when the reply answers the approval that the task
	// waits for, or that an answer just took.
	approval bool
}

// withReply returns ctx with msg, the reply to the task of header that it
// sends, for After. header is the task without its history, which is read
// only when the status holds no approval.
func (i *callerInterceptor) withReply(ctx context.Context, msg *a2a.Message, header *a2a.Task) context.Context {
	q := pendingApproval(header)
	if q == nil {
		stored, err := i.tasks.Get(ctx, msg.TaskID)
		if err != nil {
			return ctx
		}
		q = lastQuestion(stored.Task)
	}
	approval := q != nil && answersQuestion(msg, q)
	return context.WithValue(ctx, replyKey{}, reply{task: msg.TaskID, contextID: header.ContextID, approval: approval})
}

// lastQuestion returns the question of the last agent message in the
// history of task, which the messages after it answer.
func lastQuestion(task *a2a.Task) *question.Question {
	for _, m := range slices.Backward(task.History) {
		if m.Role == a2a.MessageRoleAgent {
			return question.FromMessage(m)
		}
	}
	return nil
}

// After names the task, and what to do next with the a2a CLI, in the error
// of a reply that the task refused since it already runs an execution.
// In-process senders, such as chats, render the outcome themselves.
// It also logs the refusal of a new task by the handler, such as at the
// limit of running tasks.
func (i *callerInterceptor) After(ctx context.Context, _ *a2asrv.CallContext, resp *a2asrv.Response) error {
	if newTask, _ := ctx.Value(newTaskKey{}).(bool); newTask && resp.Err != nil {
		logRefused(ctx, resp.Err)
	}
	r, ok := ctx.Value(replyKey{}).(reply)
	if !ok || auth.IsInProcess(ctx) || !a2aerr.ExecutionInProgress(resp.Err) {
		return nil
	}
	get := a2aCommand(ctx, "task get", string(r.task))
	if r.approval {
		resp.Err = a2aerr.InProgress(fmt.Sprintf("task %s already got an answer to this question; see where it stands with %s", r.task, get))
		return nil
	}
	resp.Err = a2aerr.InProgress(fmt.Sprintf("task %s is still working on an earlier message; check it with %s, and once it is done, continue with %s", r.task, get, a2aCommand(ctx, "send", "--context-id "+r.contextID)))
	return nil
}

// checkUnfinished refuses a new task of a caller who has
// maxCallerExecutions unfinished tasks. The execution limit counts only
// running tasks, and tasks waiting for an answer hold resources too.
func (i *callerInterceptor) checkUnfinished(ctx context.Context, p auth.Principal) error {
	ids, err := i.tasks.UnfinishedOf(ctx, p.ID)
	if err != nil {
		return fmt.Errorf("counting unfinished tasks: %w", err)
	}
	if len(ids) >= maxCallerExecutions {
		return fmt.Errorf("%w: %w", a2a.ErrUnauthorized, errCallerBusy)
	}
	return nil
}

// checkUserAgents refuses a new task in a conversation of a member while
// that member's agent process can't run, so that the caller gets
// an error to retry on and no failed task is stored.
func (i *callerInterceptor) checkUserAgents(ctx context.Context, contextID string) error {
	key, err := i.engine().KeyOf(ctx, contextID)
	if err != nil {
		return fmt.Errorf("looking up the session of conversation %s: %w", contextID, err)
	}
	if _, ok := agentkey.UserOf(key); ok {
		room, err := i.engine().HasRoom(ctx, key)
		if err != nil {
			return fmt.Errorf("looking up the agent process of conversation %s: %w", contextID, err)
		}
		if !room {
			return fmt.Errorf("%w: %w", a2a.ErrServerError, &agentrun.AgentsBusyError{Max: maxUserAgents})
		}
	}
	return i.checkCallerHomes(key)
}

// checkConversation refuses a message into a conversation that another
// user started, unless an owner sends it or the server picked the
// conversation, as for a group chat.
func (i *callerInterceptor) checkConversation(ctx context.Context, p auth.Principal, contextID string) error {
	if contextID == "" || p.IsOwner() || auth.IsInProcess(ctx) {
		return nil
	}
	starter, err := i.tasks.ContextStarter(ctx, contextID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("checking conversation %s: %w", contextID, err)
	}
	if starter != p.ID {
		return fmt.Errorf("%w: conversation %s belongs to someone else; leave out the context ID to start a new one", a2a.ErrUnauthorized, contextID)
	}
	return nil
}

// checkWaiting refuses a new task in a conversation with a task that waits
// for an answer to a question of the agent, since the new task would wait
// behind it without a word. Messages from chats and schedules still wait.
// [queuedTurns] fails the tasks that a question catches while they wait.
func (i *callerInterceptor) checkWaiting(ctx context.Context, p auth.Principal, contextID string) error {
	if contextID == "" || auth.IsInProcess(ctx) {
		return nil
	}
	refusal, err := i.waiting.refusal(ctx, p, contextID)
	if err != nil {
		return err
	}
	if refusal != "" {
		return fmt.Errorf("%w: %s", a2a.ErrInvalidRequest, refusal)
	}
	return nil
}
