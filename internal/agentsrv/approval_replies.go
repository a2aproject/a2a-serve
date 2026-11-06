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
	"fmt"
	"iter"
	"slices"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aext/structuredinput"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/a2aproject/a2a-serve/internal/a2atext"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/approvals"
)

// refusedReplyKey is the part metadata which marks the note of a status
// message that refuses a reply to its question.
const refusedReplyKey = "a2a-serve/refused-reply"

// maxQuotedReply limits how much of a refused reply its note repeats.
const maxQuotedReply = 60

// pendingApproval returns the permission question a stored task waits on.
func pendingApproval(task *a2a.Task) *approvals.Question {
	if task == nil {
		return nil
	}
	q := approvals.FromStatus(task.Status)
	if q == nil || q.Type != approvals.TypePermission {
		return nil
	}
	return q
}

// answersQuestion reports whether a reply answers q: with structured input,
// which the agent checks, or with typed words for one of its options.
func answersQuestion(msg *a2a.Message, q *approvals.Question) bool {
	if _, ok := replyOption(msg, q); ok {
		return true
	}
	return msg != nil && slices.ContainsFunc(msg.Parts, func(p *a2a.Part) bool {
		meta, ok := structuredinput.GetPartMetadata(p)
		return ok && meta.Type == structuredinput.PartTypeStructuredInput
	})
}

// refuseReply asks the question of the stored task again, after a note that
// the reply chose none of its options. The question keeps waiting.
func refuseReply(execCtx *a2asrv.ExecutorContext, q *approvals.Question) iter.Seq2[a2a.Event, error] {
	return askAgain(execCtx, refusal(execCtx, q))
}

// askAgain answers a reply with ev, which asks the question of the stored
// task again.
func askAgain(execCtx *a2asrv.ExecutorContext, ev *a2a.TaskStatusUpdateEvent) iter.Seq2[a2a.Event, error] {
	return withAnswerOrder(execCtx, func(yield func(a2a.Event, error) bool) {
		yield(ev, nil)
	})
}

// refusal is the status update of refuseReply.
func refusal(execCtx *a2asrv.ExecutorContext, q *approvals.Question) *a2a.TaskStatusUpdateEvent {
	ids := make([]string, len(q.Options))
	for i, o := range q.Options {
		ids[i] = o.ID
	}
	reply := []rune(strings.TrimSpace(a2atext.Message(execCtx.Message)))
	if len(reply) > maxQuotedReply {
		reply = append(reply[:maxQuotedReply], '…')
	}
	return reask(execCtx, fmt.Sprintf("I couldn't use the reply %q: it isn't one of the options, so the question still waits for an answer. Reply with one of: %s.\n", string(reply), strings.Join(ids, ", ")))
}

// reask is the status update that asks the question of the stored task
// again, after a note.
func reask(execCtx *a2asrv.ExecutorContext, text string) *a2a.TaskStatusUpdateEvent {
	note := a2a.NewTextPart(text)
	note.Metadata = map[string]any{refusedReplyKey: true}
	asked := execCtx.StoredTask.Status.Message
	parts := []*a2a.Part{note}
	for _, p := range asked.Parts {
		if refused, _ := p.Metadata[refusedReplyKey].(bool); !refused {
			parts = append(parts, p)
		}
	}
	ask := a2a.NewMessage(a2a.MessageRoleAgent, parts...)
	ask.TaskID, ask.ContextID, ask.Metadata, ask.Extensions = execCtx.TaskID, execCtx.ContextID, asked.Metadata, asked.Extensions
	return a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateInputRequired, ask)
}

// askedBefore reports whether the stored task already asked q, as when a
// reply was refused and the question asked again.
func askedBefore(stored *a2a.Task, q *approvals.Question) bool {
	if stored == nil {
		return false
	}
	prev := approvals.FromStatus(stored.Status)
	return prev != nil && prev.ID == q.ID
}
