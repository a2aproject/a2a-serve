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

package approvals

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"maps"
	"net/http"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/a2aproject/a2a-serve/internal/a2aerr"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/cryptoutil"
	"github.com/a2aproject/a2a-serve/internal/messenger"
	"github.com/a2aproject/a2a-serve/internal/store"
	"github.com/a2aproject/a2a-serve/internal/webpage"
)

// Chat is a conversation on a messenger.
type Chat struct {
	Messenger string
	Conv      messenger.ConvKey
}

// Notifier shows questions to people and updates them once answered.
type Notifier interface {
	// Ask sends a question to a chat.
	Ask(ctx context.Context, to Chat, header string, rec store.Question)
	// OwnerChat returns the chat where an owner answers the questions of
	// tasks nobody watches, or false if there is none.
	OwnerChat(ctx context.Context) (Chat, bool)
	// MarkAnswered shows who answered a question where it was asked.
	MarkAnswered(ctx context.Context, questionID, label, who string)
	// MarkClosed shows where a question was asked that it can no longer be
	// answered.
	MarkClosed(ctx context.Context, questionID string)
	// Follow calls deliver to send p's answer to the task of rec, and shows
	// the rest of the task in the conversation of its context, if it has one.
	Follow(ctx context.Context, rec store.Question, p auth.Principal, deliver func() error) error
}

// RemoteAnswerer forwards answers to the questions of other agents' tasks.
type RemoteAnswerer interface {
	// AnswerRemote sends by's answer to the task of the agent rec.Contact.
	AnswerRemote(ctx context.Context, rec store.Question, q *Question, opt Option, by auth.Principal) error
}

// Sender sends answers to the agent's own questions to its A2A handler.
type Sender interface {
	// SendMessage sends a message to the agent.
	SendMessage(ctx context.Context, req *a2a.SendMessageRequest) (a2a.SendMessageResult, error)
}

// RecordsDeps are the dependencies of [Records].
type RecordsDeps struct {
	Store *store.Store
	// Tasks is the agent's task store.
	Tasks *store.TaskStore
	// Chain gives the user of a question the role they have now.
	Chain *auth.Chain
	// BaseURL returns the URL people reach the agent at.
	BaseURL func() string
}

// Records stores questions and looks up who may answer them, without
// showing or delivering them.
type Records struct {
	store   *store.Store
	tasks   *store.TaskStore
	chain   *auth.Chain
	baseURL func() string
}

// NewRecords returns the question records of deps.
func NewRecords(deps RecordsDeps) *Records {
	return &Records{store: deps.Store, tasks: deps.Tasks, chain: deps.Chain, baseURL: deps.BaseURL}
}

// Deps are the dependencies of [Service].
type Deps struct {
	Records *Records
	// Sender is where answers to the agent's own questions are sent.
	Sender   Sender
	Notifier Notifier
	Remote   RemoteAnswerer
	// Unattended reports whether nobody watches the tasks of a context,
	// such as the runs of a schedule without a chat. Their questions go to
	// the owner chat.
	Unattended func(contextID string) bool
	// Anonymous reports whether a conversation is anonymous. Its starter
	// can't answer its questions, so they go to the owner chat, and are
	// rejected after AnonymousTimeout.
	Anonymous        func(ctx context.Context, contextID string) bool
	AnonymousTimeout time.Duration
	// User reports whether a conversation runs in the agent process of an
	// authenticated user. An open question keeps that process busy, so its
	// questions are rejected after UserTimeout.
	User        func(ctx context.Context, contextID string) bool
	UserTimeout time.Duration
	// Config returns the current config of the agent.
	Config func() *agentdir.Config
}

// Service handles the questions of the agent and of agents it calls.
type Service struct {
	*Records
	sender           Sender
	notifier         Notifier
	remote           RemoteAnswerer
	unattended       func(contextID string) bool
	anonymous        func(ctx context.Context, contextID string) bool
	anonymousTimeout time.Duration
	user             func(ctx context.Context, contextID string) bool
	userTimeout      time.Duration
	config           func() *agentdir.Config
}

// New returns the approvals service of deps.
func New(deps Deps) *Service {
	return &Service{
		Records:          deps.Records,
		sender:           deps.Sender,
		notifier:         deps.Notifier,
		remote:           deps.Remote,
		unattended:       deps.Unattended,
		anonymous:        deps.Anonymous,
		anonymousTimeout: deps.AnonymousTimeout,
		user:             deps.User,
		userTimeout:      deps.UserTimeout,
		config:           deps.Config,
	}
}

// ErrNotApprover is returned when someone who may not approve answers an approval.
var ErrNotApprover = errors.New("only the person who started the task or an owner can answer this")

// NewRecord stores a question so that buttons and pages can answer it.
// contact is the agent whose task asked, or empty for this agent; user is
// who may answer it besides owners. It returns [store.ErrExists] for a
// remote question stored before.
func (r *Records) NewRecord(ctx context.Context, taskID a2a.TaskID, contextID, contact, user string, q *Question) (store.Question, error) {
	rec := store.Question{
		ID:        cryptoutil.RandomString(8, cryptoutil.Alphanumeric),
		Token:     cryptoutil.RandomString(32, cryptoutil.Alphanumeric),
		TaskID:    string(taskID),
		ContextID: contextID,
		Contact:   contact,
		HitlID:    q.ID,
		User:      user,
		Data:      q.Data,
	}
	if q.Text != "" {
		rec.Data = maps.Clone(q.Data)
		rec.Data["text"] = q.Text
	}
	return rec, r.store.PutQuestion(ctx, rec)
}

// MayAnswer reports whether p may answer a permission question of a task
// user started: user themselves, or an owner. Anonymous callers never do.
func MayAnswer(p auth.Principal, user string) bool {
	switch p.Role {
	case auth.RoleOwner:
		return true
	case auth.RoleAuthenticated:
		return user != "" && p.ID == user
	}
	return false
}

// PageURL is the page where the user of a question answers it, or empty
// when they may not answer it.
func (r *Records) PageURL(ctx context.Context, rec store.Question) (string, error) {
	p, err := r.pageUser(ctx, rec)
	if err != nil || !MayAnswer(p, rec.User) {
		return "", err
	}
	return r.baseURL() + "/hitl/" + rec.Token, nil
}

// pageUser is who answers a question on its page: its user, with the role
// they have now. A user the policy refuses now has no role.
func (r *Records) pageUser(ctx context.Context, rec store.Question) (auth.Principal, error) {
	p, err := r.chain.Refresh(ctx, rec.User, auth.Identity{})
	var refused *auth.RefusedError
	if errors.As(err, &refused) || errors.Is(err, auth.ErrNoCredential) {
		return auth.Principal{ID: rec.User}, nil
	}
	return p, err
}

// OnQuestion sends a permission question of a task nobody watches, or of
// an anonymous conversation, to the owner chat. Whoever started any other
// task answers in the place it came from: inline in an A2A task, or in its
// conversation. Questions of authenticated users are rejected after
// UserTimeout.
func (s *Service) OnQuestion(ctx context.Context, execCtx *a2asrv.ExecutorContext, q *Question) {
	if q.Type != TypePermission {
		return
	}
	anonymous := s.anonymous != nil && s.anonymous(ctx, execCtx.ContextID)
	if !anonymous && s.user != nil && s.user(ctx, execCtx.ContextID) {
		taskID, contextID := execCtx.TaskID, execCtx.ContextID
		time.AfterFunc(s.userTimeout, func() { s.expireTask(context.WithoutCancel(ctx), taskID, contextID, q) })
	}
	if !anonymous && (s.unattended == nil || !s.unattended(execCtx.ContextID)) {
		return
	}
	ctx = context.WithoutCancel(ctx)
	starter := s.Starter(ctx, execCtx.TaskID)
	go func() {
		rec, err := s.NewRecord(ctx, execCtx.TaskID, execCtx.ContextID, "", starter, q)
		if err != nil {
			slog.Error("storing question", "error", err)
			return
		}
		header := fmt.Sprintf("%s needs your approval", auth.DisplayPrincipal(starter))
		if anonymous {
			time.AfterFunc(s.anonymousTimeout, func() { s.expire(ctx, rec.ID) })
			header = fmt.Sprintf("%s needs your approval, or it is rejected in %s", anonymousName(starter), ApprovalTimeout(s.anonymousTimeout))
		}
		to, ok := s.notifier.OwnerChat(ctx)
		if !ok {
			slog.Info("approval waits in the inspector: no owner chat", "task", execCtx.TaskID, "question", rec.ID)
			return
		}
		slog.Info("approval sent to the owner chat", "task", execCtx.TaskID, "from", starter, "question", rec.ID, "messenger", to.Messenger)
		s.notifier.Ask(ctx, to, header, rec)
	}()
}

// anonymousName names an anonymous caller in the owner chat.
func anonymousName(id string) string {
	if id == auth.AnonymousID || id == "" {
		return "An anonymous caller"
	}
	return id + ", an anonymous caller,"
}

// ApprovalTimeout describes how long an approval waits, such as "10 min".
func ApprovalTimeout(d time.Duration) string {
	if d < time.Minute {
		return d.String()
	}
	return fmt.Sprintf("%d min", int(d.Minutes()))
}

// timeoutPrincipal answers the questions nobody answered in time.
var timeoutPrincipal = auth.Principal{ID: "timeout", Role: auth.RoleOwner}

// Retries of a timeout that found its task busy, such as with a reply that
// it refused.
const (
	expireRetry    = time.Second
	expireAttempts = 60
)

// expire rejects a question if nobody answered it yet.
func (s *Service) expire(ctx context.Context, id string) {
	s.expireWith(func() (Outcome, error) {
		rec, err := s.store.QuestionByID(ctx, id)
		if err != nil {
			return 0, err
		}
		opt, ok := FromRecord(rec).Match("reject")
		if !ok {
			return 0, errors.New("the question has no option to reject it")
		}
		return s.Answer(ctx, rec, timeoutPrincipal, opt, timeoutPrincipal.ID)
	}, "question", id)
}

// expireTask rejects the question of a task if the task still waits for
// its answer. Unlike expire, it needs no stored record.
func (s *Service) expireTask(ctx context.Context, taskID a2a.TaskID, contextID string, q *Question) {
	s.expireWith(func() (Outcome, error) {
		opt, ok := q.Match("reject")
		if !ok {
			return 0, errors.New("the question has no option to reject it")
		}
		return s.AnswerTask(ctx, taskID, contextID, q, timeoutPrincipal, opt, timeoutPrincipal.ID)
	}, "task", taskID)
}

// expireWith runs reject, the answer of a timeout, again while the task is
// busy with another request, or another answer holds the question: that
// answer is cleared again if the task doesn't take it.
func (s *Service) expireWith(reject func() (Outcome, error), logArgs ...any) {
	for attempt := 1; ; attempt++ {
		out, err := reject()
		switch {
		case err != nil:
			slog.Warn("rejecting an expired question", append(logArgs, "error", err)...)
			return
		case out == Answered:
			slog.Info("rejected an unanswered approval", logArgs...)
			return
		case out == Closed:
			return
		case attempt == expireAttempts:
			slog.Warn("rejecting an expired question: the task stayed busy", logArgs...)
			return
		}
		time.Sleep(expireRetry)
	}
}

// TaskUser returns the user who started a stored task.
func (r *Records) TaskUser(ctx context.Context, taskID a2a.TaskID) (string, error) {
	stored, err := r.tasks.Get(auth.WithPrincipal(ctx, auth.Owner()), taskID)
	if err != nil {
		return "", err
	}
	return stored.User, nil
}

// Starter returns the user who started the task, or the caller when the
// task isn't stored yet.
func (r *Records) Starter(ctx context.Context, taskID a2a.TaskID) string {
	if user, err := r.TaskUser(ctx, taskID); err == nil {
		return user
	}
	if p, ok := auth.Caller(ctx); ok {
		return p.ID
	}
	return ""
}

// Waiting reports whether the task of a question still waits for its
// answer. Questions of remote agents wait until they are answered, and
// closed questions never wait again.
func (r *Records) Waiting(ctx context.Context, rec store.Question) (bool, error) {
	if rec.Closed {
		return false, nil
	}
	if rec.Contact != "" {
		return true, nil
	}
	stored, err := r.tasks.Get(auth.WithPrincipal(ctx, auth.Owner()), a2a.TaskID(rec.TaskID))
	if errors.Is(err, a2a.ErrTaskNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking task %s: %w", rec.TaskID, err)
	}
	q := FromStatus(stored.Task.Status)
	return q != nil && q.ID == rec.HitlID, nil
}

// CloseQuestions closes the unanswered questions of a task which no longer
// waits for input. The questions are listed before it returns, so that a
// question the task asks next stays open, and closed in the background.
func (s *Service) CloseQuestions(ctx context.Context, taskID a2a.TaskID) {
	ids, err := s.store.UnansweredQuestionsOf(ctx, string(taskID))
	if err != nil {
		slog.Warn("listing the questions of a task", "task", taskID, "error", err)
		return
	}
	if len(ids) == 0 {
		return
	}
	go s.Close(context.WithoutCancel(ctx), ids...)
}

// CloseStale closes the questions of tasks which stopped waiting for input
// while the server didn't run.
func (s *Service) CloseStale(ctx context.Context) {
	ids, err := s.store.StaleQuestions(ctx)
	if err != nil {
		slog.Warn("listing stale questions", "error", err)
		return
	}
	s.Close(ctx, ids...)
}

// Close marks questions closed, stops them from taking typed replies, and
// shows where they were asked that they can no longer be answered.
func (s *Service) Close(ctx context.Context, ids ...string) {
	for _, id := range ids {
		if err := s.store.CloseQuestion(ctx, id); err != nil {
			slog.Warn("marking a question closed", "question", id, "error", err)
		}
		if err := s.store.ForgetPendingQuestion(ctx, id); err != nil {
			slog.Warn("forgetting a closed question", "question", id, "error", err)
		}
		s.notifier.MarkClosed(ctx, id)
	}
}

// CheckAnswer rejects a reply to the agent's pending permission question
// from a caller other than the task's starter or an owner. A caller who
// can't see the task gets [a2a.ErrTaskNotFound], as for an unknown task, so
// that callers can't tell which IDs wait for approval.
func (r *Records) CheckAnswer(ctx context.Context, p auth.Principal, taskID a2a.TaskID) error {
	stored, err := r.tasks.Get(auth.WithPrincipal(ctx, auth.Owner()), taskID)
	if errors.Is(err, a2a.ErrTaskNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("checking task %s: %w", taskID, err)
	}
	task := stored.Task
	if task.Status.State != a2a.TaskStateInputRequired {
		return nil
	}
	q := FromStatus(task.Status)
	if q == nil || q.Type != TypePermission {
		return nil
	}
	if MayAnswer(p, stored.User) {
		return nil
	}
	if stored.User != p.ID {
		return a2a.ErrTaskNotFound
	}
	return fmt.Errorf("%w: only the person who started the task or an owner can answer this approval", a2a.ErrUnauthorized)
}

// Outcome is what became of an answer to a question.
type Outcome int

const (
	// Answered means that the answer reached the task.
	Answered Outcome = iota + 1
	// Closed means that the task no longer waits for an answer to the
	// question.
	Closed
	// AlreadyAnswered means that another answer to the question came first.
	AlreadyAnswered
	// InProgress means that the task was busy with another request, such as
	// another answer. The question may still wait.
	InProgress
)

// Send sends msg, an answer to the agent's own task, and calls accepted
// once with the error of the send, as soon as the task took it or refused
// it, before it returns. The question stays claimed until accepted runs.
type Send func(ctx context.Context, msg *a2a.Message, accepted func(error))

// Answer answers a stored question with option on behalf of p, whom who
// names where the question was asked. It claims the question, so that only
// the first answer is delivered, and forwards the answers to the questions
// of remote agents to them. An answer that isn't delivered isn't kept, so
// that the question can be answered again. Errors are failures; the
// Outcome tells the rest.
func (s *Service) Answer(ctx context.Context, rec store.Question, p auth.Principal, option Option, who string) (Outcome, error) {
	return s.AnswerWith(ctx, rec, p, option, who, nil)
}

// AnswerWith answers like Answer. It sends the answer to a question of the
// agent's own task with send, such as one that shows the rest of the task
// where it was answered; a nil send uses the notifier.
func (s *Service) AnswerWith(ctx context.Context, rec store.Question, p auth.Principal, option Option, who string, send Send) (Outcome, error) {
	q := FromRecord(rec)
	if q.Type == TypePermission && !MayAnswer(p, rec.User) {
		return 0, ErrNotApprover
	}
	waiting, err := s.Waiting(ctx, rec)
	if err != nil {
		return 0, err
	}
	if !waiting {
		s.Close(ctx, rec.ID)
		return Closed, nil
	}
	err = s.store.AnswerQuestion(ctx, rec.ID, p.ID, option.ID)
	// The answer that holds the question shows itself once the task takes
	// it, and is cleared if the task doesn't.
	if errors.Is(err, store.ErrAnswered) {
		return AlreadyAnswered, nil
	}
	if err != nil {
		return 0, err
	}
	var (
		out    Outcome
		outErr error
	)
	accepted := func(err error) { out, outErr = s.settle(ctx, rec, p, option, who, err) }
	if send == nil || rec.Contact != "" {
		accepted(s.notifier.Follow(ctx, rec, p, func() error { return s.deliver(ctx, rec, q, p, option) }))
		return out, outErr
	}
	msg, err := AnswerMessage(a2a.TaskID(rec.TaskID), rec.ContextID, q.ID, option.ID)
	if err != nil {
		accepted(err)
		return out, outErr
	}
	send(ctx, msg, accepted)
	return out, outErr
}

// settle keeps the answer of p to rec if its delivery succeeded, and clears
// it if it failed with err.
func (s *Service) settle(ctx context.Context, rec store.Question, p auth.Principal, option Option, who string, err error) (Outcome, error) {
	if err == nil {
		s.notifier.MarkAnswered(ctx, rec.ID, option.Label, who)
		return Answered, nil
	}
	ctx = context.WithoutCancel(ctx)
	if uerr := s.store.UnanswerQuestion(ctx, rec.ID, p.ID); uerr != nil {
		return 0, errors.Join(err, fmt.Errorf("clearing the undelivered answer: %w", uerr))
	}
	if a2aerr.ExecutionInProgress(err) {
		return InProgress, nil
	}
	if waiting, werr := s.Waiting(ctx, rec); werr == nil && !waiting {
		s.Close(ctx, rec.ID)
		return Closed, nil
	}
	return 0, err
}

// Release clears by's answer to the question hitlID of the agent's own
// task taskID, which the task took but couldn't act on, so that the
// question can be answered again.
func (s *Service) Release(ctx context.Context, taskID a2a.TaskID, hitlID, by string) {
	rec, err := s.store.OpenQuestionOf(ctx, string(taskID), hitlID)
	if errors.Is(err, store.ErrNotFound) {
		return
	}
	if err != nil {
		slog.Warn("loading the record of an answered question", "task", taskID, "error", err)
		return
	}
	if err := s.store.UnanswerQuestion(ctx, rec.ID, by); err != nil {
		slog.Warn("clearing an answer the task couldn't act on", "question", rec.ID, "error", err)
	}
}

// AnswerTask answers q, a question of the agent's own task taskID, with
// option on behalf of p, like Answer. It claims the record of the question
// if there is one; without one, the task takes one answer at a time, and
// refuses the rest.
func (s *Service) AnswerTask(ctx context.Context, taskID a2a.TaskID, contextID string, q *Question, p auth.Principal, option Option, who string) (Outcome, error) {
	rec, err := s.store.OpenQuestionOf(ctx, string(taskID), q.ID)
	if err == nil {
		return s.Answer(ctx, rec, p, option, who)
	}
	if !errors.Is(err, store.ErrNotFound) {
		return 0, err
	}
	waits := func() (bool, error) {
		stored, err := s.tasks.Get(auth.WithPrincipal(ctx, auth.Owner()), taskID)
		if errors.Is(err, a2a.ErrTaskNotFound) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("checking task %s: %w", taskID, err)
		}
		if q.Type == TypePermission && !MayAnswer(p, stored.User) {
			return false, ErrNotApprover
		}
		open := FromStatus(stored.Task.Status)
		return open != nil && open.ID == q.ID, nil
	}
	waiting, err := waits()
	if err != nil {
		return 0, err
	}
	if !waiting {
		return Closed, nil
	}
	err = s.send(ctx, taskID, contextID, q, p, option)
	if err == nil {
		return Answered, nil
	}
	if a2aerr.ExecutionInProgress(err) {
		return InProgress, nil
	}
	if waiting, werr := waits(); werr == nil && !waiting {
		return Closed, nil
	}
	return 0, err
}

func (s *Service) deliver(ctx context.Context, rec store.Question, q *Question, p auth.Principal, option Option) error {
	if rec.Contact != "" {
		return s.remote.AnswerRemote(ctx, rec, q, option, p)
	}
	return s.send(ctx, a2a.TaskID(rec.TaskID), rec.ContextID, q, p, option)
}

// send sends p's answer option to q, a question of the agent's own task.
func (s *Service) send(ctx context.Context, taskID a2a.TaskID, contextID string, q *Question, p auth.Principal, option Option) error {
	msg, err := AnswerMessage(taskID, contextID, q.ID, option.ID)
	if err != nil {
		return err
	}
	_, err = s.sender.SendMessage(auth.InProcess(ctx, p), &a2a.SendMessageRequest{
		Message: msg,
		Config:  &a2a.SendMessageConfig{ReturnImmediately: true},
	})
	return err
}

var hitlPage = template.Must(template.New("hitl").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Agent}}: approval</title>
<style>body{font-family:system-ui,sans-serif;max-width:40rem;margin:2rem auto;padding:0 1rem}
pre{white-space:pre-wrap;background:#f4f4f4;padding:1rem;border-radius:.5rem}
button{font-size:1rem;margin:.25rem;padding:.5rem 1rem}</style></head>
<body><h2>{{.Agent}}</h2>
{{if .Answered}}<p>Answered: <b>{{.Answered}}</b> by {{.By}}.</p>{{else}}
<pre>{{.Text}}</pre>
<form method="post">{{range .Options}}<button name="option" value="{{.ID}}">{{.Label}}</button>{{end}}</form>{{end}}
</body></html>`))

// HandlePage serves the page of a question at /hitl/{token}. Its token is the credential
// of the question's user: it is sent only where they may answer, and the answer is sent
// as them.
func (s *Service) HandlePage(w http.ResponseWriter, r *http.Request) {
	webpage.SetSecurityHeaders(w)
	rec, err := s.store.QuestionByToken(r.Context(), r.PathValue("token"))
	if err != nil {
		http.Error(w, "This question does not exist.", http.StatusNotFound)
		return
	}
	p, err := s.pageUser(r.Context(), rec)
	if err != nil {
		slog.Error("resolving the user of a question", "question", rec.ID, "error", err)
		http.Error(w, "Couldn't load the question. Try again.", http.StatusInternalServerError)
		return
	}
	if !MayAnswer(p, rec.User) {
		http.Error(w, "Only the owner can answer this question.", http.StatusForbidden)
		return
	}
	q := FromRecord(rec)
	if r.Method == http.MethodPost && rec.AnsweredBy == "" {
		opt, ok := q.Match(r.FormValue("option"))
		if !ok {
			http.Error(w, "Unknown option.", http.StatusBadRequest)
			return
		}
		out, err := s.Answer(r.Context(), rec, p, opt, "web")
		switch {
		case err != nil:
			slog.Error("answering question from the web", "question", rec.ID, "error", err)
			http.Error(w, "Couldn't send the answer. Try again.", http.StatusInternalServerError)
		case out == Closed:
			http.Error(w, "This question is no longer open.", http.StatusConflict)
		case out == InProgress:
			http.Error(w, "The task is already handling another answer. Reload the page to see where it stands.", http.StatusConflict)
		default:
			http.Redirect(w, r, r.URL.Path, http.StatusSeeOther)
		}
		return
	}
	if rec, err = s.store.QuestionByToken(r.Context(), r.PathValue("token")); err != nil {
		slog.Error("reloading question", "error", err)
		http.Error(w, "Couldn't load the question. Try again.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := hitlPage.Execute(w, map[string]any{
		"Agent": s.config().Name, "Text": q.Display(), "Options": q.Options,
		"Answered": rec.Answer, "By": auth.DisplayPrincipal(rec.AnsweredBy),
	}); err != nil {
		slog.Warn("rendering hitl page", "error", err)
	}
}
