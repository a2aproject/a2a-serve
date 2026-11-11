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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/a2aproject/a2a-serve/internal/a2atext"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/messenger"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// conversationsPrefix starts the IDs of the buttons of a list from
// /conversations.
const conversationsPrefix = "c-"

// maxPromptSnippet is how many characters of its first message a listed
// conversation shows.
const maxPromptSnippet = 30

// listedConversation is a conversation in a list from /conversations.
type listedConversation struct {
	contextID string
	current   bool
	// when is "Now" for the current conversation, and else how long ago it
	// was used.
	when    string
	summary string
	prompt  string
}

func (l listedConversation) label() string { return l.when + ": " + l.summary }

func (c *ChatTurns) newConversation(ctx context.Context, ch messenger.Messenger, in messenger.Inbound, p auth.Principal, _ string) {
	if err := c.store.NewConversation(ctx, in.Messenger, string(in.Conv), p.ID, a2a.NewContextID()); err != nil {
		slog.Error("starting a new context", "error", err)
		return
	}
	c.reply(ctx, ch, in, "Started a new conversation. /conversations lists the earlier ones.")
}

// conversationsCommand answers /conversations with p's recent conversations
// in the chat, as buttons that switch to them, or as a numbered list in a
// chat without buttons. "/conversations <number>" switches to one.
func (c *ChatTurns) conversationsCommand(ctx context.Context, ch messenger.Messenger, in messenger.Inbound, p auth.Principal, arg string) {
	list, err := c.conversationList(ctx, in, p)
	if err != nil {
		slog.Error("listing conversations", "user", p.ID, "error", err)
		c.reply(ctx, ch, in, "Couldn't list your conversations. Try again.")
		return
	}
	if arg != "" {
		n, err := strconv.Atoi(arg)
		if err != nil || n < 1 || n > len(list) {
			c.reply(ctx, ch, in, "Send /conversations to see your conversations, then /conversations <number> to switch to one.")
			return
		}
		c.switchConversation(ctx, ch, in, p, list[n-1])
		return
	}
	if len(list) == 0 {
		c.reply(ctx, ch, in, "You have no conversations here yet.")
		return
	}
	var (
		text strings.Builder
		q    *messenger.Question
	)
	text.WriteString("Your recent conversations here:")
	if ch.Caps().Buttons {
		q = &messenger.Question{ID: conversationsID(in, p, list)}
		for _, l := range list {
			q.Options = append(q.Options, l.label())
		}
	} else {
		for i, l := range list {
			fmt.Fprintf(&text, "\n%d. %s", i+1, l.label())
		}
		text.WriteString("\nTo switch, send: /conversations <number>")
	}
	if _, err := ch.Send(ctx, messenger.Outbound{Conv: in.Conv, Text: text.String(), Literal: true, Question: q, ReplyTo: messenger.Ref{Conv: in.Conv, ID: in.MessageID}}); err != nil {
		slog.Warn("sending the list of conversations", "messenger", ch.Name(), "error", err)
	}
}

// handleConversationButton switches to the conversation of a tapped button
// once the running turn of the chat ends. A list that changed since, or
// someone else's list, switches nothing.
func (c *ChatTurns) handleConversationButton(ctx context.Context, ch messenger.Messenger, in messenger.Inbound, p auth.Principal) {
	unlock := c.queue.lock(in.Messenger, in.Conv)
	defer unlock()
	list, err := c.conversationList(ctx, in, p)
	if err != nil {
		slog.Error("listing conversations", "user", p.ID, "error", err)
		c.reply(ctx, ch, in, "Couldn't switch. Try again.")
		return
	}
	if conversationsID(in, p, list) != in.Action.QuestionID || in.Action.Option >= len(list) {
		c.reply(ctx, ch, in, "This list changed or isn't yours. Send /conversations to see yours.")
		return
	}
	c.switchConversation(ctx, ch, in, p, list[in.Action.Option])
}

func (c *ChatTurns) switchConversation(ctx context.Context, ch messenger.Messenger, in messenger.Inbound, p auth.Principal, to listedConversation) {
	if to.current {
		c.reply(ctx, ch, in, "You're in this conversation already.")
		return
	}
	err := c.store.SwitchConversation(ctx, in.Messenger, string(in.Conv), p.ID, to.contextID)
	if errors.Is(err, store.ErrNotFound) {
		c.reply(ctx, ch, in, "That conversation is gone. Send /conversations to see yours.")
		return
	}
	if err != nil {
		slog.Error("switching conversations", "user", p.ID, "error", err)
		c.reply(ctx, ch, in, "Couldn't switch. Try again.")
		return
	}
	text := "Switched to the conversation from " + to.when
	if to.prompt != "" {
		text += ": " + to.prompt
	}
	c.replyLiteral(ctx, ch, in, text+".")
}

func (c *ChatTurns) replyLiteral(ctx context.Context, ch messenger.Messenger, in messenger.Inbound, text string) {
	if _, err := ch.Send(ctx, messenger.Outbound{Conv: in.Conv, Text: text, Literal: true, ReplyTo: messenger.Ref{Conv: in.Conv, ID: in.MessageID}}); err != nil {
		slog.Warn("replying in messenger", "messenger", ch.Name(), "error", err)
	}
}

// conversationList is p's history in the chat of in, most recently used
// first.
func (c *ChatTurns) conversationList(ctx context.Context, in messenger.Inbound, p auth.Principal) ([]listedConversation, error) {
	convs, err := c.store.Conversations(ctx, in.Messenger, string(in.Conv), p.ID)
	if err != nil {
		return nil, err
	}
	current, err := c.store.ContextOf(ctx, in.Messenger, string(in.Conv))
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	now := time.Now()
	out := make([]listedConversation, 0, len(convs))
	for _, conv := range convs {
		sum, err := c.tasks.ContextSummary(ctx, conv.ContextID)
		if err != nil {
			return nil, err
		}
		l := listedConversation{contextID: conv.ContextID, current: conv.ContextID == current, when: ago(now.Sub(conv.Used)), summary: turns(sum.Turns)}
		if l.current {
			l.when = "Now"
		}
		switch l.prompt = promptSnippet(sum.Prompt); {
		case l.prompt != "":
			l.summary += ", " + l.prompt
		case voiceNote(sum.Prompt):
			l.summary += ", voice note"
		}
		out = append(out, l)
	}
	return out, nil
}

// conversationsID identifies the list of p in the chat of in.
func conversationsID(in messenger.Inbound, p auth.Principal, list []listedConversation) string {
	parts := []string{p.ID, in.Messenger, string(in.Conv)}
	for _, l := range list {
		parts = append(parts, l.contextID)
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return conversationsPrefix + hex.EncodeToString(sum[:])[:16]
}

func turns(n int) string {
	switch {
	case n == 0:
		return "no messages yet"
	case n == 1:
		return "1 turn"
	case n > store.MaxCountedTurns:
		return fmt.Sprintf("%d+ turns", store.MaxCountedTurns)
	}
	return fmt.Sprintf("%d turns", n)
}

// promptSnippet is the start of the text of the first message of a
// conversation in quotes.
func promptSnippet(m *a2a.Message) string {
	if text := a2atext.Message(m); text != "" {
		return `"` + a2atext.Snippet(text, maxPromptSnippet) + `"`
	}
	return ""
}

// voiceNote reports whether m has a recording.
func voiceNote(m *a2a.Message) bool {
	if m == nil {
		return false
	}
	return slices.ContainsFunc(m.Parts, func(p *a2a.Part) bool { return strings.HasPrefix(p.MediaType, "audio/") })
}

// ago tells how long ago something happened, such as "3 days ago".
func ago(d time.Duration) string {
	const day = 24 * time.Hour
	unit := func(n int, name string) string {
		if n == 1 {
			return "1 " + name + " ago"
		}
		return fmt.Sprintf("%d %ss ago", n, name)
	}
	switch {
	case d < time.Minute:
		return "a moment ago"
	case d < time.Hour:
		return unit(int(d/time.Minute), "minute")
	case d < day:
		return unit(int(d/time.Hour), "hour")
	case d < 2*day:
		return "yesterday"
	case d < 14*day:
		return unit(int(d/day), "day")
	case d < 60*day:
		return unit(int(d/(7*day)), "week")
	}
	return unit(int(d/(30*day)), "month")
}
