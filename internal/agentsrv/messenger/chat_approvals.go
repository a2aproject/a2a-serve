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

package messenger

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/approvals"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/channel"
	schedulesvc "github.com/a2aproject/a2a-serve/internal/schedules"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// ChatApprovals shows questions in chats, renders the tasks of questions
// answered elsewhere in their conversation, and finds the owner chat.
type ChatApprovals struct {
	store      *store.Store
	tasks      *store.TaskStore
	records    *approvals.Records
	supervisor *ChannelSupervisor
	bus        *Bus
	rt         *agentdir.Runtime
	anonymous  func(ctx context.Context, contextID string) bool
	timings    Timings
	// ctx ends when the server stops; tasks followed after an answer
	// render in it.
	ctx context.Context
}

// ChatApprovalsDeps are the dependencies of [ChatApprovals].
type ChatApprovalsDeps struct {
	Store      *store.Store
	Tasks      *store.TaskStore
	Records    *approvals.Records
	Supervisor *ChannelSupervisor
	Bus        *Bus
	// Runtime has the config of the agent.
	Runtime *agentdir.Runtime
	// Anonymous reports whether a conversation is anonymous, so that an
	// owner answers its questions in the owner chat.
	Anonymous func(ctx context.Context, contextID string) bool
	Timings   Timings
}

var _ approvals.Notifier = (*ChatApprovals)(nil)

// NewChatApprovals returns the chat approvals of deps. Tasks followed
// after an answer render until ctx ends.
func NewChatApprovals(ctx context.Context, deps ChatApprovalsDeps) *ChatApprovals {
	return &ChatApprovals{
		store:      deps.Store,
		tasks:      deps.Tasks,
		records:    deps.Records,
		supervisor: deps.Supervisor,
		bus:        deps.Bus,
		rt:         deps.Runtime,
		anonymous:  deps.Anonymous,
		timings:    deps.Timings,
		ctx:        ctx,
	}
}

// followOwnerAnswer renders the rest of a task in its conversation after
// an owner answered its question in the owner chat, or the question was
// rejected after the timeout.
func (a *ChatApprovals) followOwnerAnswer(ctx context.Context, ch channel.Channel, conv channel.ConvKey, p auth.Principal, taskID a2a.TaskID) {
	events, unsubscribe := a.bus.Subscribe(taskID)
	go func() {
		defer unsubscribe()
		for {
			rd := newRenderer(a, ch, conv, channel.Ref{}, p)
			if !renderUntilStop(ctx, rd, events, a.timings.AnonymousQuestionTimeout+time.Minute) {
				return
			}
			if _, waits := rd.waitingForOwner(); !waits {
				return
			}
		}
	}()
}

// renderUntilStop renders events until the task stops, and reports false
// if it didn't within timeout.
func renderUntilStop(ctx context.Context, rd *renderer, events *Subscription, timeout time.Duration) bool {
	t := time.NewTimer(timeout)
	defer t.Stop()
	for {
		ev, ok := events.Next(ctx, t.C)
		if !ok {
			return false
		}
		if rd.onEvent(ctx, ev) {
			return true
		}
	}
}

// Follow delivers an answer to a question of the agent's task and renders
// the rest of the task in the conversation of its context. Anonymous
// conversations follow owner answers on their own.
func (a *ChatApprovals) Follow(ctx context.Context, rec store.Question, p auth.Principal, deliver func() error) error {
	if rec.Contact != "" || a.anonymous(ctx, rec.ContextID) {
		return deliver()
	}
	chName, conv, err := a.store.ConvOf(ctx, rec.ContextID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		slog.Warn("resolving the conversation of a question", "question", rec.ID, "error", err)
	}
	ch := a.supervisor.Channel(chName)
	if ch == nil {
		return deliver()
	}
	events, unsubscribe := a.bus.Subscribe(a2a.TaskID(rec.TaskID))
	if err := deliver(); err != nil {
		unsubscribe()
		return err
	}
	go a.render(a.ctx, ch, channel.ConvKey(conv), p, events, unsubscribe)
	return nil
}

// render renders the events of a task in a conversation until the task
// stops, and then unsubscribes.
func (a *ChatApprovals) render(ctx context.Context, ch channel.Channel, conv channel.ConvKey, p auth.Principal, events *Subscription, unsubscribe func()) {
	defer unsubscribe()
	rd := newRenderer(a, ch, conv, channel.Ref{}, p)
	timeout := time.NewTimer(25 * time.Hour)
	defer timeout.Stop()
	for {
		ev, ok := events.Next(ctx, timeout.C)
		if !ok || rd.onEvent(ctx, ev) {
			return
		}
	}
}

// MarkAnswered stops a question from taking typed replies, and replaces
// its buttons with who answered it.
func (a *ChatApprovals) MarkAnswered(ctx context.Context, questionID, label, who string) {
	if err := a.store.ForgetPendingQuestion(ctx, questionID); err != nil {
		slog.Warn("forgetting an answered question", "question", questionID, "error", err)
	}
	a.endQuestion(ctx, questionID, fmt.Sprintf("✓ %s by %s", label, who))
}

// MarkClosed replaces the buttons of a question whose task no longer waits
// for it with a note.
func (a *ChatApprovals) MarkClosed(ctx context.Context, questionID string) {
	a.endQuestion(ctx, questionID, "✗ The task no longer waits for an answer.")
}

// endQuestion replaces the buttons of the messages which show a question
// with a note.
func (a *ChatApprovals) endQuestion(ctx context.Context, questionID, note string) {
	msgs, err := a.store.TakeQuestionMessages(ctx, questionID)
	if err != nil {
		slog.Warn("loading the messages of a question", "question", questionID, "error", err)
		return
	}
	for _, m := range msgs {
		ch := a.supervisor.Channel(m.Channel)
		if ch == nil || !ch.Caps().Edit {
			continue
		}
		ref := channel.Ref{Conv: channel.ConvKey(m.Conv), ID: m.MessageID}
		if err := ch.Edit(ctx, ref, channel.Outbound{Conv: ref.Conv, Text: m.Text + "\n\n" + note, Literal: true}); err != nil {
			slog.Debug("updating a question message", "error", err)
		}
	}
}

// Ask sends a question to a chat.
func (a *ChatApprovals) Ask(ctx context.Context, to approvals.Chat, header string, rec store.Question) {
	ch := a.supervisor.Channel(to.Channel)
	if ch == nil || to.Conv == "" {
		slog.Warn("sending question: no such chat", "channel", to.Channel, "question", rec.ID)
		return
	}
	q := approvals.FromRecord(rec)
	text := header + ":\n" + q.Display()
	out := channel.Outbound{Conv: to.Conv, Text: text, Subject: header}
	if ch.Caps().Buttons {
		out.Question = &channel.Question{ID: rec.ID, Options: q.Labels()}
	} else {
		out.Text += a.answerHint(ctx, rec, q)
	}
	a.sendQuestion(ctx, ch, out, rec.ID, text, newRetrier(a.timings.SendRetry))
}

// answerHint tells how to answer a question in a chat without buttons: by
// typing an option, or on its page when the task's user may answer there.
func (a *ChatApprovals) answerHint(ctx context.Context, rec store.Question, q *approvals.Question) string {
	hint := "\n\nReply " + q.Words()
	link, err := a.records.PageURL(ctx, rec)
	switch {
	case err != nil:
		slog.Warn("checking who may answer a question on its page", "question", rec.ID, "error", err)
	case link != "":
		hint += ", or open " + link
	}
	return hint
}

// sendQuestion sends a question, retrying it so that an approval isn't lost
// to a rate limit or a brief outage, and records it. It is pending in the
// chat before it is sent, so that a reply typed right after it arrives
// answers it. The text is shown literally: the owner has to see all of what
// they approve.
func (a *ChatApprovals) sendQuestion(ctx context.Context, ch channel.Channel, out channel.Outbound, questionID, text string, r *retrier) {
	if err := a.store.AddPendingQuestion(ctx, ch.Name(), string(out.Conv), questionID); err != nil {
		slog.Warn("recording pending question", "error", err)
	}
	out.Literal = true
	var ref channel.Ref
	err := r.do(ctx, retryAll, func() error {
		var err error
		ref, err = ch.Send(ctx, out)
		return err
	})
	if err != nil {
		slog.Warn("sending question", "channel", ch.Name(), "error", err)
		if err := a.store.ForgetPendingQuestion(ctx, questionID); err != nil {
			slog.Warn("clearing pending question", "error", err)
		}
		return
	}
	if err := a.store.AddQuestionMessage(ctx, store.QuestionMessage{QuestionID: questionID, Channel: ch.Name(), Conv: string(ref.Conv), MessageID: ref.ID, Text: text}); err != nil {
		slog.Warn("recording a question message", "error", err)
	}
}

// ownerChatOrder is the order in which, without --owner-chat, the
// messengers are tried for the owner chat.
var ownerChatOrder = []string{agentdir.ChannelTelegram, agentdir.ChannelDiscord, agentdir.ChannelEmail}

// OwnerChat returns the chat of --owner-chat, or else the direct chat of an
// owner on the first running messenger, in ownerChatOrder.
func (a *ChatApprovals) OwnerChat(ctx context.Context) (approvals.Chat, bool) {
	if v := a.rt.Config().Approvals.OwnerChat; v != "" {
		chName, who, _ := strings.Cut(v, ":")
		c, err := a.directChat(ctx, chName, who)
		if err != nil {
			slog.Warn("the owner chat is unavailable", "owner-chat", v, "error", err)
			return approvals.Chat{}, false
		}
		return c, true
	}
	for _, chName := range ownerChatOrder {
		if c, err := a.directChat(ctx, chName, ""); err == nil {
			return c, true
		}
	}
	return approvals.Chat{}, false
}

// directChat returns the direct chat on a running channel of an owner: the
// owner pairing who, which is yours for the local pairing; or, if who is
// empty or "owner", yours or else any owner pairing's. Only owners can
// answer the questions sent there.
func (a *ChatApprovals) directChat(ctx context.Context, chName, who string) (approvals.Chat, error) {
	if a.supervisor.Channel(chName) == nil {
		return approvals.Chat{}, fmt.Errorf("channel %s is not running", chName)
	}
	anyOwner := who == "" || who == auth.OwnerName || who == auth.OwnerID
	if anyOwner {
		conv, err := schedulesvc.DirectConv(ctx, a.store, chName, auth.OwnerID)
		if err == nil {
			return approvals.Chat{Channel: chName, Conv: channel.ConvKey(conv)}, nil
		}
		c, ok, ownerErr := a.ownerPairingChat(ctx, chName)
		if ownerErr != nil || ok {
			return c, ownerErr
		}
		return approvals.Chat{}, err
	}
	links, err := a.store.LinksOf(ctx, who)
	if err != nil {
		return approvals.Chat{}, err
	}
	if len(links) > 0 {
		owner, err := a.isOwnerPairing(ctx, who)
		if err != nil {
			return approvals.Chat{}, err
		}
		if !owner {
			return approvals.Chat{}, fmt.Errorf("%s is not an owner, so can't answer in the owner chat; make them one with `a2a-serve pair set %s --owner`", who, who)
		}
	}
	for _, l := range links {
		if l.Channel == chName && l.Conv != "" {
			return approvals.Chat{Channel: chName, Conv: channel.ConvKey(l.Conv)}, nil
		}
	}
	return approvals.Chat{}, fmt.Errorf("%s has no chat on %s: link it with a code from `a2a-serve pair invite %s`", who, chName, who)
}

// ownerPairingChat returns the direct chat on a channel of the first owner
// pairing by name which has one.
func (a *ChatApprovals) ownerPairingChat(ctx context.Context, chName string) (approvals.Chat, bool, error) {
	links, err := a.store.Links(ctx)
	if err != nil {
		return approvals.Chat{}, false, err
	}
	for _, name := range slices.Sorted(maps.Keys(links)) {
		for _, l := range links[name] {
			if l.Channel != chName || l.Conv == "" {
				continue
			}
			owner, err := a.isOwnerPairing(ctx, name)
			if err != nil {
				return approvals.Chat{}, false, err
			}
			if owner {
				return approvals.Chat{Channel: chName, Conv: channel.ConvKey(l.Conv)}, true, nil
			}
		}
	}
	return approvals.Chat{}, false, nil
}

// isOwnerPairing reports whether a pairing is an owner: you, or an owner
// pairing.
func (a *ChatApprovals) isOwnerPairing(ctx context.Context, name string) (bool, error) {
	if name == auth.LocalPairing {
		return true, nil
	}
	p, err := a.store.Pairing(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return err == nil && p.Role == auth.RoleOwner, err
}
