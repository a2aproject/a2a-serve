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
	"log/slog"
	"sync"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/a2aproject/a2a-serve/internal/agentsrv/approvals"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// Principals that answer the approvals that waited when a rule,
// preference or grant that decides them was added.
var (
	rulesPrincipal   = auth.Principal{ID: "allow-tools", Role: auth.RoleOwner}
	allowedPrincipal = auth.Principal{ID: "always-allow", Role: auth.RoleOwner}
)

// answerWaiting answers the approvals that wait and that the rules, or the
// preferences and grants of the people who started their tasks, decide
// now, as if they had been asked now. It leaves the approvals of the tasks
// in except, which are being answered.
func (s *Server) answerWaiting(ctx context.Context, except map[a2a.TaskID]bool) error {
	waiting, err := s.waitingApprovals(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, t := range waiting {
		if except[t.ID] {
			continue
		}
		q := pendingApproval(t)
		user, err := s.approvals.TaskUser(ctx, t.ID)
		if err != nil {
			errs = append(errs, fmt.Errorf("looking up who started task %s: %w", t.ID, err))
			continue
		}
		dec, ok, err := s.decider.decide(ctx, t.ContextID, user, s.decider.call(q.Kind, q.ToolTitle, q.ToolInput))
		if err != nil {
			errs = append(errs, fmt.Errorf("deciding the approval of task %s: %w", t.ID, err))
			continue
		}
		if !ok {
			continue
		}
		ids := make([]string, len(q.Options))
		for i, o := range q.Options {
			ids[i] = o.ID
		}
		ans, ok := ruleAnswer(ids, dec.allow)
		if !ok {
			continue
		}
		opt, ok := q.Match(ans.Option)
		if !ok {
			continue
		}
		by, who := rulesPrincipal, rulesPrincipal.ID
		if dec.by != byRule {
			by, who = allowedPrincipal, approvals.AlwaysLabel(dec.entry)
		}
		if err := s.answerByRules(ctx, t, q, opt, by, who); err != nil {
			errs = append(errs, fmt.Errorf("answering the approval of task %s: %w", t.ID, err))
		}
	}
	return errors.Join(errs...)
}

// answerAllowed asks for the approvals that wait and that a new
// preference or grant decides to be answered in the background, leaving
// the approval of task except.
func (s *Server) answerAllowed(_ context.Context, except a2a.TaskID) {
	s.allowed.request(except)
}

// allowedAnswers runs answerWaiting for new preferences and grants in one
// loop of the server, so that runs don't overlap and stop with it. A
// request during a run starts one more run after it.
type allowedAnswers struct {
	wake   chan struct{}
	mu     sync.Mutex
	except map[a2a.TaskID]bool
}

func newAllowedAnswers() *allowedAnswers {
	return &allowedAnswers{wake: make(chan struct{}, 1), except: map[a2a.TaskID]bool{}}
}

// request asks for a run that leaves the approval of task except, if set.
func (a *allowedAnswers) request(except a2a.TaskID) {
	a.mu.Lock()
	if except != "" {
		a.except[except] = true
	}
	a.mu.Unlock()
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

// run answers the approvals for each request until ctx is done.
func (a *allowedAnswers) run(ctx context.Context, answer func(ctx context.Context, except map[a2a.TaskID]bool) error) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.wake:
		}
		a.mu.Lock()
		except := a.except
		a.except = map[a2a.TaskID]bool{}
		a.mu.Unlock()
		if err := answer(ctx, except); err != nil {
			slog.Warn("answering the approvals that a new preference or grant allows", "error", err)
		}
	}
}

// waitingApprovals lists the tasks that wait for an approval of a tool
// call of this agent. They are all listed before any is answered, since an
// answer changes the order of the pages.
func (s *Server) waitingApprovals(ctx context.Context) ([]*a2a.Task, error) {
	owner := auth.WithPrincipal(ctx, auth.Owner())
	history := 0
	req := &a2a.ListTasksRequest{Status: a2a.TaskStateInputRequired, PageSize: 100, HistoryLength: &history}
	var out []*a2a.Task
	for {
		page, err := s.tasks.List(owner, req)
		if err != nil {
			return nil, fmt.Errorf("listing the tasks that wait for an answer: %w", err)
		}
		for _, t := range page.Tasks {
			if q := pendingApproval(t); q != nil && q.Kind != "" && !asksCallerQuestion(t) {
				out = append(out, t)
			}
		}
		if page.NextPageToken == "" {
			return out, nil
		}
		req.PageToken = page.NextPageToken
	}
}

// answerByRules answers the approval q of task t with opt as by. A
// question shown in a chat or in the inspector is answered there, so that
// it shows who answered. The executor logs the answer.
func (s *Server) answerByRules(ctx context.Context, t *a2a.Task, q *approvals.Question, opt approvals.Option, by auth.Principal, who string) error {
	ids, err := s.store.UnansweredQuestionsOf(ctx, string(t.ID))
	if err != nil {
		return err
	}
	for _, id := range ids {
		rec, err := s.store.QuestionByID(ctx, id)
		if err != nil {
			return err
		}
		if rec.HitlID != q.ID {
			continue
		}
		err = s.approvals.Answer(ctx, rec, by, opt)
		if errors.Is(err, store.ErrAnswered) || errors.Is(err, approvals.ErrClosed) {
			return nil
		}
		if err != nil {
			return err
		}
		s.chatApprovals.MarkAnswered(ctx, rec.ID, opt.Label, who)
		return nil
	}
	msg, err := approvals.AnswerMessage(t.ID, t.ContextID, q.ID, opt.ID)
	if err != nil {
		return err
	}
	_, err = s.sender.SendMessage(auth.InProcess(ctx, by), &a2a.SendMessageRequest{
		Message: msg,
		Config:  &a2a.SendMessageConfig{ReturnImmediately: true},
	})
	return err
}
