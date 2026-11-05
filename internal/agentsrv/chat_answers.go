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
	"log/slog"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/a2aproject/a2a-serve/internal/agentsrv/approvals"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/channel"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// handleAction answers a question from a button click.
func (c *ChatTurns) handleAction(ctx context.Context, ch channel.Channel, in channel.Inbound) {
	rec, err := c.store.QuestionByID(ctx, in.Action.QuestionID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			slog.Error("loading question", "question", in.Action.QuestionID, "error", err)
		}
		return
	}
	if rec.AnsweredBy != "" {
		c.reply(ctx, ch, in, "Already answered.")
		return
	}
	p, err := c.chain.Authenticate(ctx, auth.Request{Channel: &in})
	if refused(err) {
		return
	}
	if err != nil {
		slog.Error("authenticating channel action", "error", err)
		return
	}
	q := approvals.FromRecord(rec)
	if in.Action.Option >= len(q.Options) {
		return
	}
	c.answerIn(ctx, ch, in, p, rec, q, q.Options[in.Action.Option])
}

// typedAnswer returns the answer a message types to the question pending
// in its conversation, for channels without buttons and for people who
// type instead of clicking. A question whose task no longer waits for it
// is closed, and the message goes to the agent.
func (c *ChatTurns) typedAnswer(ctx context.Context, in channel.Inbound) (store.Question, *approvals.Question, approvals.Option, bool) {
	id, err := c.store.PendingQuestion(ctx, in.Channel, string(in.Conv))
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			slog.Warn("loading the pending question", "error", err)
		}
		return store.Question{}, nil, approvals.Option{}, false
	}
	rec, err := c.store.QuestionByID(ctx, id)
	if err != nil || rec.AnsweredBy != "" {
		return store.Question{}, nil, approvals.Option{}, false
	}
	q := approvals.FromRecord(rec)
	opt, ok := q.Match(in.Text)
	if !ok {
		return store.Question{}, nil, approvals.Option{}, false
	}
	waiting, err := c.approvals.Waiting(ctx, rec)
	if err != nil {
		slog.Warn("checking whether the pending question is still open", "question", rec.ID, "error", err)
		return store.Question{}, nil, approvals.Option{}, false
	}
	if !waiting {
		c.approvals.Close(ctx, rec.ID)
		return store.Question{}, nil, approvals.Option{}, false
	}
	return rec, q, opt, true
}

// answerIn answers a question from a conversation. If the question belongs
// to the task of this conversation, the rest of the task is rendered here.
func (c *ChatTurns) answerIn(ctx context.Context, ch channel.Channel, in channel.Inbound, p auth.Principal, rec store.Question, q *approvals.Question, opt approvals.Option) {
	if q.Type == approvals.TypePermission && !approvals.MayAnswer(p, rec.User) {
		c.reply(ctx, ch, in, "Only the person who started the task or an owner can answer this.")
		return
	}
	if err := c.store.ClearPendingQuestion(ctx, in.Channel, string(in.Conv)); err != nil {
		slog.Warn("clearing pending question", "error", err)
	}
	who := in.Name
	if who == "" {
		who = auth.DisplayPrincipal(p.ID)
	}
	own, err := c.store.ContextOf(ctx, in.Channel, string(in.Conv))
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		slog.Warn("resolving conversation context", "error", err)
	}
	// A turn queued behind the task holds a busy conversation, so the rest
	// of the task is rendered without it, as for answers given elsewhere.
	if rec.Contact == "" && rec.ContextID == own && !c.queue.busy(in.Channel, in.Conv) {
		waiting, err := c.approvals.Waiting(ctx, rec)
		if err != nil {
			slog.Error("checking whether a question is still open", "question", rec.ID, "error", err)
			c.reply(ctx, ch, in, "Couldn't send the answer. Try again.")
			c.restorePending(ctx, in, rec)
			return
		}
		if !waiting {
			c.approvals.Close(ctx, rec.ID)
			c.reply(ctx, ch, in, "This question is no longer open.")
			return
		}
		msg, err := approvals.AnswerMessage(a2a.TaskID(rec.TaskID), rec.ContextID, q.ID, opt.ID)
		if err != nil {
			slog.Error("answering question", "question", rec.ID, "error", err)
			c.reply(ctx, ch, in, "Couldn't send the answer. Try again.")
			c.restorePending(ctx, in, rec)
			return
		}
		if err := c.store.AnswerQuestion(ctx, rec.ID, p.ID, opt.ID); err != nil {
			if !errors.Is(err, store.ErrAnswered) {
				slog.Error("answering question", "question", rec.ID, "error", err)
				c.reply(ctx, ch, in, "Couldn't record the answer. Try again.")
				c.restorePending(ctx, in, rec)
				return
			}
			c.reply(ctx, ch, in, "Already answered.")
			return
		}
		unlock := c.queue.lock(in.Channel, in.Conv)
		defer unlock()
		answered := false
		c.streamThen(ctx, ch, in.Conv, channel.Ref{}, p, &a2a.SendMessageRequest{Message: msg}, func(err error) {
			if err == nil {
				answered = true
				c.chat.MarkAnswered(ctx, rec.ID, opt.Label, who)
				return
			}
			if err := c.store.UnanswerQuestion(context.WithoutCancel(ctx), rec.ID, p.ID); err != nil {
				slog.Error("clearing an undelivered answer", "question", rec.ID, "error", err)
			}
		})
		if !answered {
			c.restorePending(ctx, in, rec)
		}
		return
	}
	c.answered(ctx, ch, in, rec, opt, who, c.approvals.Answer(ctx, rec, p, opt))
}

// answered reports the result of answering a question in a conversation.
func (c *ChatTurns) answered(ctx context.Context, ch channel.Channel, in channel.Inbound, rec store.Question, opt approvals.Option, who string, err error) {
	switch {
	case errors.Is(err, store.ErrAnswered):
		c.reply(ctx, ch, in, "Already answered.")
	case errors.Is(err, approvals.ErrClosed):
		c.approvals.Close(ctx, rec.ID)
		c.reply(ctx, ch, in, "This question is no longer open.")
	case err != nil:
		c.restorePending(ctx, in, rec)
		c.reply(ctx, ch, in, "Couldn't send the answer: "+err.Error())
	default:
		c.chat.MarkAnswered(ctx, rec.ID, opt.Label, who)
	}
}

// restorePending makes a question whose answer wasn't delivered the one a
// typed reply in the conversation answers again, while its task waits for
// the answer.
func (c *ChatTurns) restorePending(ctx context.Context, in channel.Inbound, rec store.Question) {
	ctx = context.WithoutCancel(ctx)
	waiting, err := c.approvals.Waiting(ctx, rec)
	if err != nil {
		slog.Warn("checking whether a question is still open", "question", rec.ID, "error", err)
		return
	}
	if !waiting {
		c.approvals.Close(ctx, rec.ID)
		return
	}
	if err := c.store.SetPendingQuestion(ctx, in.Channel, string(in.Conv), rec.ID); err != nil {
		slog.Warn("restoring the pending question", "error", err)
	}
}
