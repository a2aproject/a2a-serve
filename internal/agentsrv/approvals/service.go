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

	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/channel"
	"github.com/a2aproject/a2a-serve/internal/cryptoutil"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// Chat is a conversation on a channel.
type Chat struct {
	Channel string
	Conv    channel.ConvKey
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

// Service handles the questions of the agent and of agents it calls. Its
// fields are set once, before the server serves.
type Service struct {
	Store *store.Store
	// Tasks is the agent's task store.
	Tasks *store.TaskStore
	// Handler is the agent's A2A handler, where answers to its own
	// questions are sent.
	Handler  a2asrv.RequestHandler
	Notifier Notifier
	Remote   RemoteAnswerer
	// Chain gives the user of a question the role they have now.
	Chain *auth.Chain
	// Unattended reports whether nobody watches the tasks of a context,
	// such as the runs of a schedule without a chat. Their questions go to
	// the owner chat.
	Unattended func(contextID string) bool
	// Anonymous reports whether a conversation is anonymous. Its starter
	// can't answer its questions, so they go to the owner chat, and are
	// rejected after AnonymousTimeout.
	Anonymous        func(ctx context.Context, contextID string) bool
	AnonymousTimeout time.Duration
	// BaseURL returns the URL people reach the agent at.
	BaseURL func() string
	// AgentName returns the name of the agent.
	AgentName func() string
}

// ErrNotApprover is returned when someone who may not approve answers an approval.
var ErrNotApprover = errors.New("only the person who started the task or an owner can answer this")

// ErrClosed is returned when the task of a question no longer waits for its answer.
var ErrClosed = errors.New("the task no longer waits for an answer to this question")

// NewRecord stores a question so that buttons and pages can answer it.
// contact is the agent whose task asked, or empty for this agent; user is
// who may answer it besides owners. It returns [store.ErrExists] for a
// remote question stored before.
func (s *Service) NewRecord(ctx context.Context, taskID a2a.TaskID, contextID, contact, user string, q *Question) (store.Question, error) {
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
	return rec, s.Store.PutQuestion(ctx, rec)
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
func (s *Service) PageURL(ctx context.Context, rec store.Question) (string, error) {
	p, err := s.pageUser(ctx, rec)
	if err != nil || !MayAnswer(p, rec.User) {
		return "", err
	}
	return s.BaseURL() + "/hitl/" + rec.Token, nil
}

// pageUser is who answers a question on its page: its user, with the role
// they have now. A user the policy refuses now has no role.
func (s *Service) pageUser(ctx context.Context, rec store.Question) (auth.Principal, error) {
	p, err := s.Chain.Refresh(ctx, rec.User, auth.Identity{})
	var refused *auth.RefusedError
	if errors.As(err, &refused) || errors.Is(err, auth.ErrNoCredential) {
		return auth.Principal{ID: rec.User}, nil
	}
	return p, err
}

// OnQuestion sends a permission question of a task nobody watches, or of
// an anonymous conversation, to the owner chat. Whoever started any other
// task answers in the place it came from: inline in an A2A task, or in its
// conversation.
func (s *Service) OnQuestion(ctx context.Context, execCtx *a2asrv.ExecutorContext, q *Question) {
	if q.Type != TypePermission {
		return
	}
	anonymous := s.Anonymous != nil && s.Anonymous(ctx, execCtx.ContextID)
	if !anonymous && (s.Unattended == nil || !s.Unattended(execCtx.ContextID)) {
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
			time.AfterFunc(s.AnonymousTimeout, func() { s.expire(ctx, rec.ID) })
			header = fmt.Sprintf("%s needs your approval, or it is rejected in %s", anonymousName(starter), ApprovalTimeout(s.AnonymousTimeout))
		}
		to, ok := s.Notifier.OwnerChat(ctx)
		if !ok {
			slog.Info("approval waits in the inspector: no owner chat", "task", execCtx.TaskID, "question", rec.ID)
			return
		}
		slog.Info("approval sent to the owner chat", "task", execCtx.TaskID, "from", starter, "question", rec.ID, "channel", to.Channel)
		s.Notifier.Ask(ctx, to, header, rec)
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

// expire rejects a question if nobody answered it yet.
func (s *Service) expire(ctx context.Context, id string) {
	rec, err := s.Store.QuestionByID(ctx, id)
	if err != nil {
		slog.Warn("loading an expired question", "question", id, "error", err)
		return
	}
	if rec.AnsweredBy != "" {
		return
	}
	q := FromRecord(rec)
	opt, ok := q.Match("reject")
	if !ok {
		slog.Warn("an expired question has no option to reject it", "question", id)
		return
	}
	err = s.Answer(ctx, rec, timeoutPrincipal, opt)
	switch {
	case errors.Is(err, store.ErrAnswered), errors.Is(err, ErrClosed):
		return
	case err != nil:
		slog.Warn("rejecting an expired question", "question", id, "error", err)
		return
	}
	slog.Info("rejected an unanswered approval", "task", rec.TaskID, "question", id)
	s.Notifier.MarkAnswered(ctx, rec.ID, opt.Label, "timeout")
}

// Starter returns the user who started the task.
func (s *Service) Starter(ctx context.Context, taskID a2a.TaskID) string {
	stored, err := s.Tasks.Get(auth.WithPrincipal(ctx, auth.Owner()), taskID)
	if err == nil {
		return stored.User
	}
	if p, ok := auth.Caller(ctx); ok {
		return p.ID
	}
	return ""
}

// Waiting reports whether the task of a question still waits for its
// answer. Questions of remote agents wait until they are answered.
func (s *Service) Waiting(ctx context.Context, rec store.Question) (bool, error) {
	if rec.Contact != "" {
		return true, nil
	}
	stored, err := s.Tasks.Get(auth.WithPrincipal(ctx, auth.Owner()), a2a.TaskID(rec.TaskID))
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
	ids, err := s.Store.UnansweredQuestionsOf(ctx, string(taskID))
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
	ids, err := s.Store.StaleQuestions(ctx)
	if err != nil {
		slog.Warn("listing stale questions", "error", err)
		return
	}
	s.Close(ctx, ids...)
}

// Close stops questions from taking typed replies, and shows where they
// were asked that they can no longer be answered.
func (s *Service) Close(ctx context.Context, ids ...string) {
	for _, id := range ids {
		if err := s.Store.ForgetPendingQuestion(ctx, id); err != nil {
			slog.Warn("forgetting a closed question", "question", id, "error", err)
		}
		s.Notifier.MarkClosed(ctx, id)
	}
}

// CheckAnswer rejects a reply to the agent's pending permission question
// from a caller other than the task's starter or an owner.
func (s *Service) CheckAnswer(ctx context.Context, p auth.Principal, taskID a2a.TaskID) error {
	stored, err := s.Tasks.Get(auth.WithPrincipal(ctx, auth.Owner()), taskID)
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
	return fmt.Errorf("%w: only the person who started the task or an owner can answer this approval", a2a.ErrUnauthorized)
}

// Answer answers a stored question with option on behalf of p. Questions of
// remote agents are forwarded to them. An answer which can't be delivered
// isn't kept, so that the question can be answered again. It returns
// [ErrClosed] if the task no longer waits for the answer.
func (s *Service) Answer(ctx context.Context, rec store.Question, p auth.Principal, option Option) error {
	q := FromRecord(rec)
	if q.Type == TypePermission && !MayAnswer(p, rec.User) {
		return ErrNotApprover
	}
	waiting, err := s.Waiting(ctx, rec)
	if err != nil {
		return err
	}
	if !waiting {
		return ErrClosed
	}
	if err := s.Store.AnswerQuestion(ctx, rec.ID, p.ID, option.ID); err != nil {
		return err
	}
	err = s.Notifier.Follow(ctx, rec, p, func() error { return s.deliver(ctx, rec, q, p, option) })
	if err == nil {
		return nil
	}
	if uerr := s.Store.UnanswerQuestion(context.WithoutCancel(ctx), rec.ID, p.ID); uerr != nil {
		return errors.Join(err, fmt.Errorf("clearing the undelivered answer: %w", uerr))
	}
	return err
}

func (s *Service) deliver(ctx context.Context, rec store.Question, q *Question, p auth.Principal, option Option) error {
	if rec.Contact != "" {
		return s.Remote.AnswerRemote(ctx, rec, q, option, p)
	}
	msg, err := AnswerMessage(a2a.TaskID(rec.TaskID), rec.ContextID, q.ID, option.ID)
	if err != nil {
		return err
	}
	_, err = s.Handler.SendMessage(auth.InProcess(ctx, p), &a2a.SendMessageRequest{
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
	rec, err := s.Store.QuestionByToken(r.Context(), r.PathValue("token"))
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
		err := s.Answer(r.Context(), rec, p, opt)
		if errors.Is(err, ErrClosed) {
			http.Error(w, "This question is no longer open.", http.StatusConflict)
			return
		}
		if err != nil && !errors.Is(err, store.ErrAnswered) {
			slog.Error("answering question from the web", "question", rec.ID, "error", err)
			http.Error(w, "Couldn't send the answer. Try again.", http.StatusInternalServerError)
			return
		}
		s.Notifier.MarkAnswered(r.Context(), rec.ID, opt.Label, "web")
		http.Redirect(w, r, r.URL.Path, http.StatusSeeOther)
		return
	}
	if rec, err = s.Store.QuestionByToken(r.Context(), r.PathValue("token")); err != nil {
		slog.Error("reloading question", "error", err)
		http.Error(w, "Couldn't load the question. Try again.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := hitlPage.Execute(w, map[string]any{
		"Agent": s.AgentName(), "Text": q.Display(), "Options": q.Options,
		"Answered": rec.Answer, "By": auth.DisplayPrincipal(rec.AnsweredBy),
	}); err != nil {
		slog.Warn("rendering hitl page", "error", err)
	}
}
