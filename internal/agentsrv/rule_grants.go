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

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/integrations/executor/a2aacp"

	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// ruleAnswers saves what an answer to a permission question with the
// server's options allows, before the answer reaches the task: "always"
// saves the grant of the conversation for the person whose task asked. A
// failed save refuses the answer, so the question keeps waiting.
type ruleAnswers struct {
	store *store.Store
	tasks *store.TaskStore
}

// record saves what msg, p's reply to a stored task, allows.
func (r *ruleAnswers) record(ctx context.Context, p auth.Principal, msg *a2a.Message) error {
	stored, err := r.tasks.Get(auth.WithPrincipal(ctx, auth.Owner()), msg.TaskID)
	if errors.Is(err, a2a.ErrTaskNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("checking task %s: %w", msg.TaskID, err)
	}
	q := pendingApproval(stored.Task)
	if q == nil || q.Rule == "" {
		return nil
	}
	opt, ok := replyOption(msg, q)
	if !ok || opt.ID != a2aacp.OptionAlways {
		return nil
	}
	if err := r.store.AddToolGrant(ctx, stored.Task.ContextID, stored.User, q.Rule); err != nil {
		slog.Error("saving the grant of a conversation", "task", msg.TaskID, "error", err)
		return fmt.Errorf("%w: couldn't save the answer, so the question still waits for an answer; try again", a2a.ErrServerError)
	}
	slog.Info("tool calls allowed for the conversation", "task", msg.TaskID, "context", stored.Task.ContextID, "rule", q.Rule, "user", stored.User, "by", p.ID)
	return nil
}
