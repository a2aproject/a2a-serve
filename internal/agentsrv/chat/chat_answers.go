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

package chat

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/a2aproject/a2a-serve/internal/agentsrv/approvals"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/messenger"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// handleAction answers a question from a button click.
func (c *ChatTurns) handleAction(ctx context.Context, ch messenger.Messenger, in messenger.Inbound) {
	id := in.Action.QuestionID
	if strings.HasPrefix(id, conversationsPrefix) || (c.prefs != nil && (strings.HasPrefix(id, suggestionPrefix) || strings.HasPrefix(id, allowedPrefix))) {
		p, err := c.chain.Authenticate(ctx, auth.Request{Messenger: &in})
		if auth.Refused(err) {
			return
		}
		if err != nil {
			slog.Error("authenticating messenger action", "error", err)
			return
		}
		switch {
		case strings.HasPrefix(id, conversationsPrefix):
			c.handleConversationButton(ctx, ch, in, p)
		case strings.HasPrefix(id, suggestionPrefix):
			c.handleSuggestion(ctx, ch, in, p)
		default:
			c.handleRemove(ctx, ch, in, p)
		}
		return
	}
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
	p, err := c.chain.Authenticate(ctx, auth.Request{Messenger: &in})
	if auth.Refused(err) {
		return
	}
	if err != nil {
		slog.Error("authenticating messenger action", "error", err)
		return
	}
	q := approvals.FromRecord(rec)
	if in.Action.Option >= len(q.Options) {
		return
	}
	c.answerIn(ctx, ch, in, p, rec, q, q.Options[in.Action.Option])
}

// typedReply is what a typed message is to the questions pending in its
// conversation.
type typedReply int

const (
	// notAnAnswer goes to the agent.
	notAnAnswer typedReply = iota
	answer
	// ambiguousAnswer starts with an answer on its own line, but goes on.
	// Messengers without buttons ask for a plain answer instead of guessing.
	ambiguousAnswer
	// unaddressedAnswer is an answer that replies to no question while
	// several are open in the conversation. Its sender is asked to reply
	// to the question they answer.
	unaddressedAnswer
)

// typedAnswer returns the answer a message types to a question, for
// messengers without buttons and for people who type instead of clicking.
// The question is the one whose message it replies to, or else the only
// one open in its conversation. Questions whose tasks no longer wait for
// them are closed, and the message goes to the agent.
func (c *ChatTurns) typedAnswer(ctx context.Context, in messenger.Inbound, buttons bool) (store.Question, *approvals.Question, approvals.Option, typedReply) {
	recs := c.unanswered(ctx, c.addressedQuestions(ctx, in))
	if !slices.ContainsFunc(recs, func(rec store.Question) bool {
		_, kind := matchTyped(approvals.FromRecord(rec), in.Text, buttons)
		return kind != notAnAnswer
	}) {
		return store.Question{}, nil, approvals.Option{}, notAnAnswer
	}
	recs = c.waiting(ctx, recs)
	switch len(recs) {
	case 0:
		return store.Question{}, nil, approvals.Option{}, notAnAnswer
	case 1:
	default:
		return store.Question{}, nil, approvals.Option{}, unaddressedAnswer
	}
	q := approvals.FromRecord(recs[0])
	opt, kind := matchTyped(q, in.Text, buttons)
	return recs[0], q, opt, kind
}

// matchTyped returns the option a typed message selects, and whether it is
// an answer.
func matchTyped(q *approvals.Question, text string, buttons bool) (approvals.Option, typedReply) {
	if opt, ok := q.Match(text); ok {
		return opt, answer
	}
	first, rest, _ := strings.Cut(strings.TrimSpace(text), "\n")
	if _, ok := q.Match(first); !ok || buttons || strings.TrimSpace(rest) == "" {
		return approvals.Option{}, notAnAnswer
	}
	return approvals.Option{}, ambiguousAnswer
}

// addressedQuestions returns the IDs of the questions a message may answer:
// the one shown by the message it replies to, or else those pending in its
// conversation.
func (c *ChatTurns) addressedQuestions(ctx context.Context, in messenger.Inbound) []string {
	if in.ReplyTo.ID != "" {
		id, err := c.store.QuestionOfMessage(ctx, in.Messenger, string(in.Conv), in.ReplyTo.ID)
		if err == nil {
			return []string{id}
		}
		if !errors.Is(err, store.ErrNotFound) {
			slog.Warn("loading the question of a replied-to message", "error", err)
			return nil
		}
	}
	ids, err := c.store.PendingQuestions(ctx, in.Messenger, string(in.Conv))
	if err != nil {
		slog.Warn("loading the pending questions", "error", err)
	}
	return ids
}

// unanswered loads the questions of ids that nobody answered yet.
func (c *ChatTurns) unanswered(ctx context.Context, ids []string) []store.Question {
	var out []store.Question
	for _, id := range ids {
		rec, err := c.store.QuestionByID(ctx, id)
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				slog.Warn("loading a pending question", "question", id, "error", err)
			}
			continue
		}
		if rec.AnsweredBy == "" {
			out = append(out, rec)
		}
	}
	return out
}

// waiting returns the questions whose tasks still wait for them, and
// closes the others. A question that can't be checked counts as waiting,
// so that it can't make another one look like the only one open.
func (c *ChatTurns) waiting(ctx context.Context, recs []store.Question) []store.Question {
	var out []store.Question
	for _, rec := range recs {
		waiting, err := c.approvals.Waiting(ctx, rec)
		if err != nil {
			slog.Warn("checking whether a pending question is still open", "question", rec.ID, "error", err)
			out = append(out, rec)
			continue
		}
		if !waiting {
			c.approvals.Close(ctx, rec.ID)
			continue
		}
		out = append(out, rec)
	}
	return out
}

// answerIn answers a question from a conversation. If the question belongs
// to the task of this conversation, the rest of the task is rendered here.
func (c *ChatTurns) answerIn(ctx context.Context, ch messenger.Messenger, in messenger.Inbound, p auth.Principal, rec store.Question, q *approvals.Question, opt approvals.Option) {
	if q.Type == approvals.TypePermission && !approvals.MayAnswer(p, rec.User) {
		c.reply(ctx, ch, in, "Only the person who started the task or an owner can answer this.")
		return
	}
	if err := c.store.ForgetPendingQuestion(ctx, rec.ID); err != nil {
		slog.Warn("clearing pending question", "error", err)
	}
	who := in.Name
	if who == "" {
		who = auth.DisplayPrincipal(p.ID)
	}
	own, err := c.store.ContextOf(ctx, in.Messenger, string(in.Conv))
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		slog.Warn("resolving conversation context", "error", err)
	}
	// A turn queued behind the task holds a busy conversation, so the rest
	// of the task is rendered without it, as for answers given elsewhere.
	if rec.Contact == "" && rec.ContextID == own && !c.queue.busy(in.Messenger, in.Conv) {
		unlock := c.queue.lock(in.Messenger, in.Conv)
		defer unlock()
		sent := false
		send := func(ctx context.Context, msg *a2a.Message, accepted func(error)) {
			sent = true
			c.streamThen(ctx, ch, in.Conv, messenger.Ref{}, p, &a2a.SendMessageRequest{Message: msg}, false, accepted)
		}
		out, err := c.approvals.AnswerWith(ctx, rec, p, opt, who, send)
		if !sent {
			c.answered(ctx, ch, in, rec, out, err)
			return
		}
		// The stream shows why an answer that was sent didn't take.
		if err != nil || out != approvals.Answered {
			c.restorePending(ctx, in, rec)
			return
		}
		c.offerPreference(ctx, ch, in, p, rec, q, opt)
		return
	}
	out, err := c.approvals.Answer(ctx, rec, p, opt, who)
	c.answered(ctx, ch, in, rec, out, err)
	if err == nil && out == approvals.Answered {
		c.offerPreference(ctx, ch, in, p, rec, q, opt)
	}
}

// answered reports the outcome of answering a question in a conversation,
// or err.
func (c *ChatTurns) answered(ctx context.Context, ch messenger.Messenger, in messenger.Inbound, rec store.Question, out approvals.Outcome, err error) {
	switch {
	case err != nil:
		c.restorePending(ctx, in, rec)
		c.reply(ctx, ch, in, "Couldn't send the answer: "+err.Error())
	case out == approvals.AlreadyAnswered:
		c.reply(ctx, ch, in, "Already answered.")
	case out == approvals.Closed:
		c.reply(ctx, ch, in, "This question is no longer open.")
	case out == approvals.InProgress:
		c.restorePending(ctx, in, rec)
		c.reply(ctx, ch, in, "The task is already handling another answer. Try again if the question still waits.")
	}
}

// restorePending makes a question whose answer wasn't delivered the one a
// typed reply in the conversation answers again, while its task waits for
// the answer.
func (c *ChatTurns) restorePending(ctx context.Context, in messenger.Inbound, rec store.Question) {
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
	if err := c.store.AddPendingQuestion(ctx, in.Messenger, string(in.Conv), rec.ID); err != nil {
		slog.Warn("restoring the pending question", "error", err)
	}
}
