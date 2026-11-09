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
	"fmt"
	"log/slog"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/integrations/executor/a2aacp"

	"github.com/a2aproject/a2a-serve/internal/agentsrv/approvals"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/store"
	"github.com/a2aproject/a2a-serve/internal/toolrules"
)

// ruleAnswers saves what an answer to a permission question with the
// server's options allows, once the answer's execution took the task and
// before the answer reaches the agent, so that an answer which loses a race
// saves nothing: "always" saves the grant of the conversation for the
// person whose task asked. A failed save refuses the answer, so the
// question keeps waiting. That person's own answers count toward the offer
// to allow the rule in all their conversations: "always" counts the
// conversation, and a rejection starts the count over.
type ruleAnswers struct {
	store *store.Store
	tasks *store.TaskStore
	// granted answers the approvals that wait and that a new grant allows,
	// besides the one of task except. Optional.
	granted func(ctx context.Context, except a2a.TaskID)
}

// record saves what opt, p's answer to q, the question of the task of
// execCtx, allows.
func (r *ruleAnswers) record(ctx context.Context, p auth.Principal, execCtx *a2asrv.ExecutorContext, q *approvals.Question, opt approvals.Option) error {
	if q.Rule == "" {
		return nil
	}
	stored, err := r.tasks.Header(auth.WithPrincipal(ctx, auth.Owner()), execCtx.TaskID)
	if err != nil {
		return fmt.Errorf("checking task %s: %w", execCtx.TaskID, err)
	}
	own := p.ID == stored.User
	if isRejection(opt.ID) && own {
		if err := r.store.ResetToolTaps(ctx, stored.User, q.Rule); err != nil {
			slog.Warn("resetting the taps on always allow", "task", execCtx.TaskID, "error", err)
		}
		return nil
	}
	if opt.ID != a2aacp.OptionAlways {
		return nil
	}
	if err := r.store.AddToolGrant(ctx, execCtx.ContextID, stored.User, q.Rule); err != nil {
		return fmt.Errorf("saving the grant of a conversation: %w", err)
	}
	slog.Info("tool calls allowed for the conversation", "task", execCtx.TaskID, "context", execCtx.ContextID, "rule", toolrules.Redact(q.Rule), "user", stored.User, "by", p.ID)
	if r.granted != nil {
		r.granted(ctx, execCtx.TaskID)
	}
	if !own {
		return nil
	}
	if _, err := r.store.CountToolTap(ctx, stored.User, q.Rule, execCtx.ContextID); err != nil {
		slog.Warn("counting a tap on always allow", "task", execCtx.TaskID, "error", err)
	}
	return nil
}
