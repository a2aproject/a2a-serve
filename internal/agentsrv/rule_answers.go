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

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/integrations/executor/a2aacp"

	"github.com/a2aproject/a2a-serve/internal/agentsrv/approvals"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// rulesPrincipal answers the approvals that waited when the allow and deny
// rules changed and that the new rules answer.
var rulesPrincipal = auth.Principal{ID: "allow-tools", Role: auth.RoleOwner}

// answerWaiting answers the approvals that wait and that the allow and
// deny rules of the current config answer, as if they had been asked now.
func (s *Server) answerWaiting(ctx context.Context) error {
	cfg := s.rt.Config()
	rules := a2aacp.ToolRules{Allow: cfg.Approvals.Allow, Deny: cfg.Approvals.Deny}
	if len(rules.Allow) == 0 && len(rules.Deny) == 0 {
		return nil
	}
	waiting, err := s.waitingApprovals(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, t := range waiting {
		q := pendingApproval(t)
		ids := make([]string, len(q.Options))
		for i, o := range q.Options {
			ids[i] = o.ID
		}
		ans, ok := rules.AutoAnswer(ctx, nil, a2aacp.Question{Options: ids, Tool: &a2aacp.ToolCall{Kind: q.Kind}})
		if !ok {
			continue
		}
		opt, ok := q.Match(ans.Option)
		if !ok {
			continue
		}
		if err := s.answerByRules(ctx, t, q, opt); err != nil {
			errs = append(errs, fmt.Errorf("answering the approval of task %s: %w", t.ID, err))
		}
	}
	return errors.Join(errs...)
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

// answerByRules answers the approval q of task t with opt. A question
// shown in a chat or in the inspector is answered there, so that it shows
// who answered. The executor logs the answer.
func (s *Server) answerByRules(ctx context.Context, t *a2a.Task, q *approvals.Question, opt approvals.Option) error {
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
		err = s.approvals.Answer(ctx, rec, rulesPrincipal, opt)
		if errors.Is(err, store.ErrAnswered) || errors.Is(err, approvals.ErrClosed) {
			return nil
		}
		if err != nil {
			return err
		}
		s.chatApprovals.MarkAnswered(ctx, rec.ID, opt.Label, rulesPrincipal.ID)
		return nil
	}
	msg, err := approvals.AnswerMessage(t.ID, t.ContextID, q.ID, opt.ID)
	if err != nil {
		return err
	}
	_, err = s.sender.SendMessage(auth.InProcess(ctx, rulesPrincipal), &a2a.SendMessageRequest{
		Message: msg,
		Config:  &a2a.SendMessageConfig{ReturnImmediately: true},
	})
	return err
}
