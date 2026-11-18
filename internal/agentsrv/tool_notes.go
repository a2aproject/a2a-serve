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
	"iter"
	"log/slog"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/a2aproject/a2a-serve/core/runner"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/question"
)

// withAnswer logs the answer that a reply gives to the approval q of the
// task, and notes it if it rejects the tool call, before events run.
func (e *observedExecutor) withAnswer(ctx context.Context, execCtx *a2asrv.ExecutorContext, q *question.Question, events iter.Seq2[a2a.Event, error]) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		rejected := ""
		if opt, ok := replyOption(execCtx.Message, q); ok {
			p, _ := auth.Caller(ctx)
			slog.Info("approval answered", "task", execCtx.TaskID, "by", p.ID, "kind", q.Kind, "option", opt.ID)
			switch {
			case p.ID == denyPrincipal.ID:
				e.notes.Add(execCtx.TaskID, "Tool call denied by --deny-tools: "+q.Title+".")
			case runner.IsRejection(opt.ID):
				e.notes.Add(execCtx.TaskID, "Tool call rejected: "+q.Title+".")
				rejected = q.ToolCallID
			}
		}
		for ev, err := range events {
			if rejected != "" && err == nil {
				ev = runner.RejectedCall(ev, rejected)
			}
			if !yield(ev, err) {
				return
			}
		}
	}
}
