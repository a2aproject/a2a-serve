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
	"strings"

	"github.com/a2aproject/a2a-go/v2/integrations/executor/a2aacp"

	"github.com/a2aproject/a2a-serve/internal/agentsrv/approvals"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/cryptoutil"
	"github.com/a2aproject/a2a-serve/internal/messenger"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// Prefixes of the IDs of buttons that answer no question of the agent: an
// offer to allow a rule in all conversations, and a list of what someone
// allowed. Question IDs are alphanumeric, so they never start with these.
const (
	suggestionPrefix = "s-"
	allowedPrefix    = "a-"
)

// Options of an offer.
const (
	suggestionAllow = iota
	suggestionNotNow
)

// allowedHint tells how to see and remove preferences.
const allowedHint = "/allowed shows what you allowed, and removes it."

// offerPreference offers p to allow the rule of q in all their
// conversations, after p allowed it always on a question of their own
// task, in SuggestAfter conversations in a row. Only chats with buttons get
// the offer, and only its buttons answer it.
func (c *ChatTurns) offerPreference(ctx context.Context, ch messenger.Messenger, in messenger.Inbound, p auth.Principal, rec store.Question, q *approvals.Question, opt approvals.Option) {
	if c.prefs == nil || opt.ID != a2aacp.OptionAlways || q.Rule == "" || rec.Contact != "" || rec.User != p.ID || !ch.Caps().Buttons {
		return
	}
	ctx = context.WithoutCancel(ctx)
	due, err := c.prefs.Due(ctx, p.ID, q.Rule)
	if err != nil {
		slog.Warn("checking whether to offer a preference", "user", p.ID, "error", err)
		return
	}
	if !due {
		return
	}
	sg := store.ToolSuggestion{ID: suggestionPrefix + cryptoutil.RandomString(8, cryptoutil.Alphanumeric), User: p.ID, Rule: q.Rule, ContextID: rec.ContextID}
	if err := c.store.PutToolSuggestion(ctx, sg); err != nil {
		slog.Warn("storing an offer of a preference", "user", p.ID, "error", err)
		return
	}
	text := fmt.Sprintf("You allowed %s in %d conversations in a row.\nAllow it in all your conversations?", approvals.RuleText(q.Rule), approvals.SuggestAfter)
	out := messenger.Outbound{Conv: in.Conv, Text: text, Literal: true, Question: &messenger.Question{ID: sg.ID, Options: []string{"Allow for me", "Not now"}}}
	if _, err := ch.Send(ctx, out); err != nil {
		slog.Warn("sending an offer of a preference", "messenger", ch.Name(), "error", err)
	}
}

// handleSuggestion answers an offer to allow a rule in all conversations.
// Only the person it was made for can answer it.
func (c *ChatTurns) handleSuggestion(ctx context.Context, ch messenger.Messenger, in messenger.Inbound, p auth.Principal) {
	sg, err := c.store.TakeToolSuggestion(ctx, in.Action.QuestionID, p.ID)
	if errors.Is(err, store.ErrNotFound) {
		user, err := c.store.ToolSuggestionUser(ctx, in.Action.QuestionID)
		switch {
		case err == nil && user != p.ID:
			c.reply(ctx, ch, in, "Only "+auth.DisplayPrincipal(user)+" can answer this.")
		case errors.Is(err, store.ErrNotFound):
			c.reply(ctx, ch, in, "Already answered.")
		default:
			slog.Warn("loading an offer of a preference", "error", err)
		}
		return
	}
	if err != nil {
		slog.Error("answering an offer of a preference", "error", err)
		c.reply(ctx, ch, in, "Couldn't save the answer. Try again.")
		return
	}
	rule := approvals.RuleText(sg.Rule)
	if in.Action.Option != suggestionAllow {
		c.prefs.Decline(ctx, p.ID, sg.Rule)
		c.edit(ctx, ch, in, fmt.Sprintf("You allowed %s in %d conversations in a row.\n\nNot now. I ask again after you allow it in %d more.", rule, approvals.SuggestAfter, approvals.SuggestAfter))
		return
	}
	err = c.prefs.Allow(ctx, p.ID, sg.Rule)
	if err != nil {
		if perr := c.store.PutToolSuggestion(context.WithoutCancel(ctx), sg); perr != nil {
			slog.Warn("restoring an offer of a preference", "user", p.ID, "error", perr)
		}
	}
	if errors.Is(err, store.ErrPreferenceLimit) {
		c.reply(ctx, ch, in, fmt.Sprintf("You allowed %d calls in all your conversations already. %s", store.MaxToolPreferences, allowedHint))
		return
	}
	if err != nil {
		slog.Error("saving a preference", "user", p.ID, "error", err)
		c.reply(ctx, ch, in, "Couldn't save the answer. Try again.")
		return
	}
	c.edit(ctx, ch, in, fmt.Sprintf("✓ %s runs without asking in your conversations.\n%s", rule, allowedHint))
}

// handleAllowed answers /allowed: the list of p's preferences, with a
// button that removes each, or with how to remove one in a chat without
// buttons. "/allowed remove <rule>" removes one.
func (c *ChatTurns) handleAllowed(ctx context.Context, ch messenger.Messenger, in messenger.Inbound, p auth.Principal) {
	_, arg, _ := strings.Cut(strings.TrimSpace(in.Text), " ")
	if rule, ok := strings.CutPrefix(strings.TrimSpace(arg), "remove "); ok {
		c.removeTyped(ctx, ch, in, p, strings.TrimSpace(rule))
		return
	}
	text, q, err := c.allowedList(ctx, p, ch.Caps().Buttons)
	if err != nil {
		slog.Error("listing preferences", "user", p.ID, "error", err)
		c.reply(ctx, ch, in, "Couldn't list what you allowed. Try again.")
		return
	}
	if _, err := ch.Send(ctx, messenger.Outbound{Conv: in.Conv, Text: text, Literal: true, Question: q, ReplyTo: messenger.Ref{Conv: in.Conv, ID: in.MessageID}}); err != nil {
		slog.Warn("sending the list of preferences", "messenger", ch.Name(), "error", err)
	}
}

// allowedList is the message that lists p's preferences, with its buttons.
// The buttons share an ID made of p and the list, so that a tap on a list
// that changed since, or on someone else's list, removes nothing.
func (c *ChatTurns) allowedList(ctx context.Context, p auth.Principal, buttons bool) (string, *messenger.Question, error) {
	rules, err := c.prefs.List(ctx, p.ID)
	if err != nil {
		return "", nil, err
	}
	if len(rules) == 0 {
		return fmt.Sprintf("Nothing runs without asking in your conversations yet. When you tap \"Always allow\" for the same call in %d conversations in a row, I offer to remember it.", approvals.SuggestAfter), nil, nil
	}
	shown := 0
	if buttons {
		shown = min(len(rules), maxAllowedButtons)
	}
	var b strings.Builder
	b.WriteString("These run without asking in your conversations:")
	for _, r := range rules[shown:] {
		fmt.Fprintf(&b, "\n- %s", approvals.RuleText(r))
	}
	if shown < len(rules) {
		b.WriteString("\nTo remove one, send: /allowed remove <one of them>")
	}
	if shown == 0 {
		return b.String(), nil, nil
	}
	q := &messenger.Question{ID: allowedID(p.ID, rules)}
	for _, r := range rules[:shown] {
		q.Options = append(q.Options, "Remove "+approvals.RuleText(r))
	}
	return b.String(), q, nil
}

// maxAllowedButtons is how many preferences a list from /allowed shows as
// buttons, within Discord's limit of 25 buttons on a message.
const maxAllowedButtons = 25

// allowedID identifies the list of rules of user.
func allowedID(user string, rules []string) string {
	sum := sha256.Sum256([]byte(user + "\x00" + strings.Join(rules, "\x00")))
	return allowedPrefix + hex.EncodeToString(sum[:])[:16]
}

// handleRemove removes a preference by its button in a list from
// /allowed, and shows the list as it is now.
func (c *ChatTurns) handleRemove(ctx context.Context, ch messenger.Messenger, in messenger.Inbound, p auth.Principal) {
	rules, err := c.prefs.List(ctx, p.ID)
	if err != nil {
		slog.Error("listing preferences", "user", p.ID, "error", err)
		return
	}
	if allowedID(p.ID, rules) != in.Action.QuestionID || in.Action.Option >= min(len(rules), maxAllowedButtons) {
		c.reply(ctx, ch, in, "This list changed or isn't yours. Send /allowed to see yours.")
		return
	}
	if err := c.prefs.Remove(ctx, p.ID, rules[in.Action.Option]); err != nil && !errors.Is(err, store.ErrNotFound) {
		slog.Error("removing a preference", "user", p.ID, "error", err)
		c.reply(ctx, ch, in, "Couldn't remove it. Try again.")
		return
	}
	text, q, err := c.allowedList(ctx, p, true)
	if err != nil {
		slog.Error("listing preferences", "user", p.ID, "error", err)
		return
	}
	text = fmt.Sprintf("✓ Removed %s.\n\n%s", approvals.RuleText(rules[in.Action.Option]), text)
	if ch.Caps().Edit && in.ReplyTo.ID != "" {
		if err := ch.Edit(ctx, in.ReplyTo, messenger.Outbound{Conv: in.Conv, Text: text, Literal: true, Question: q}); err == nil {
			return
		}
	}
	if _, err := ch.Send(ctx, messenger.Outbound{Conv: in.Conv, Text: text, Literal: true, Question: q}); err != nil {
		slog.Warn("sending the list of preferences", "messenger", ch.Name(), "error", err)
	}
}

// removeTyped removes the preference that "/allowed remove <rule>" names,
// as listed or with its tool: or command: prefix.
func (c *ChatTurns) removeTyped(ctx context.Context, ch messenger.Messenger, in messenger.Inbound, p auth.Principal, name string) {
	rules, err := c.prefs.List(ctx, p.ID)
	if err != nil {
		slog.Error("listing preferences", "user", p.ID, "error", err)
		c.reply(ctx, ch, in, "Couldn't remove it. Try again.")
		return
	}
	for _, r := range rules {
		if r != name && approvals.RuleText(r) != name {
			continue
		}
		if err := c.prefs.Remove(ctx, p.ID, r); err != nil && !errors.Is(err, store.ErrNotFound) {
			slog.Error("removing a preference", "user", p.ID, "error", err)
			c.reply(ctx, ch, in, "Couldn't remove it. Try again.")
			return
		}
		c.reply(ctx, ch, in, fmt.Sprintf("✓ Removed %s. It asks again in your conversations.", approvals.RuleText(r)))
		return
	}
	c.reply(ctx, ch, in, fmt.Sprintf("You didn't allow %q. Send /allowed to see what you allowed.", name))
}

// edit replaces the message of a tapped button with text, or replies with
// it in a chat that can't edit messages.
func (c *ChatTurns) edit(ctx context.Context, ch messenger.Messenger, in messenger.Inbound, text string) {
	if ch.Caps().Edit && in.ReplyTo.ID != "" {
		if err := ch.Edit(ctx, in.ReplyTo, messenger.Outbound{Conv: in.Conv, Text: text, Literal: true}); err == nil {
			return
		}
	}
	c.reply(ctx, ch, in, text)
}
