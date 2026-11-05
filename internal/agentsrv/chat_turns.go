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
	"iter"
	"log/slog"
	"strings"
	"sync"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/a2aproject/a2a-serve/internal/agentsrv/approvals"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/channel"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// ChatTurns turns channel messages into tasks of the agent, renders their
// events back into the conversation, answers questions from chats and posts
// turns into contexts.
type ChatTurns struct {
	store      *store.Store
	tasks      *store.TaskStore
	chain      *auth.Chain
	approvals  *approvals.Service
	chat       *ChatApprovals
	supervisor *ChannelSupervisor
	queue      *ConversationQueue
	sender     turnSender
	bus        *eventBus
	// unattended are the contexts of turns posted with no conversation,
	// whose tasks nobody watches.
	unattended *contextSet
	posted     postedTurns
	agentName  func() string
}

// ChatTurnsDeps are the dependencies of [ChatTurns].
type ChatTurnsDeps struct {
	Store      *store.Store
	Tasks      *store.TaskStore
	Chain      *auth.Chain
	Approvals  *approvals.Service
	Chat       *ChatApprovals
	Supervisor *ChannelSupervisor
	Queue      *ConversationQueue
	Sender     turnSender
	Bus        *eventBus
	Unattended *contextSet
	AgentName  func() string
}

// turnSender sends turns to the agent.
type turnSender interface {
	SendMessage(ctx context.Context, req *a2a.SendMessageRequest) (a2a.SendMessageResult, error)
	SendStreamingMessage(ctx context.Context, req *a2a.SendMessageRequest) iter.Seq2[a2a.Event, error]
	CancelTask(ctx context.Context, req *a2a.CancelTaskRequest) (*a2a.Task, error)
}

func newChatTurns(deps ChatTurnsDeps) *ChatTurns {
	return &ChatTurns{
		store:      deps.Store,
		tasks:      deps.Tasks,
		chain:      deps.Chain,
		approvals:  deps.Approvals,
		chat:       deps.Chat,
		supervisor: deps.Supervisor,
		queue:      deps.Queue,
		sender:     deps.Sender,
		bus:        deps.Bus,
		unattended: deps.Unattended,
		agentName:  deps.AgentName,
	}
}

// start dispatches channel messages until ctx ends.
func (c *ChatTurns) start(ctx context.Context) { go c.dispatch(ctx) }

// contextSet is a set of context IDs, safe for concurrent use.
type contextSet struct {
	mu  sync.Mutex
	ids map[string]bool
}

func (s *contextSet) add(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ids == nil {
		s.ids = map[string]bool{}
	}
	s.ids[id] = true
}

func (s *contextSet) remove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.ids, id)
}

func (s *contextSet) has(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ids[id]
}

func (c *ChatTurns) dispatch(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case in := <-c.supervisor.inbound():
			// Messages take their place in line on arrival, so that a slow
			// Prepare doesn't let a later message overtake them.
			var t *convTicket
			if in.Action == nil {
				t = c.queue.enqueue(in.Channel, in.Conv)
			}
			go c.handle(ctx, in, t)
		}
	}
}

func (c *ChatTurns) reply(ctx context.Context, ch channel.Channel, in channel.Inbound, text string) {
	if _, err := ch.Send(ctx, channel.Outbound{Conv: in.Conv, Text: text, ReplyTo: channel.Ref{Conv: in.Conv, ID: in.MessageID}}); err != nil {
		slog.Warn("replying in channel", "channel", ch.Name(), "error", err)
	}
}

// firstSeen reports whether the message is new, so that redelivered ones are
// dropped. It runs after the checks that refuse a sender, so that strangers
// don't make the server write.
func (c *ChatTurns) firstSeen(ctx context.Context, in channel.Inbound) bool {
	first, err := c.store.FirstSeen(ctx, in.Channel+"|"+in.MessageID)
	if err != nil {
		slog.Error("dedup", "error", err)
		return false
	}
	return first
}

func (c *ChatTurns) handle(ctx context.Context, in channel.Inbound, t *convTicket) {
	if t != nil {
		defer c.queue.release(t)
	}
	ch := c.supervisor.channel(in.Channel)
	if ch == nil {
		return
	}
	if in.Action != nil {
		if c.firstSeen(ctx, in) {
			c.handleAction(ctx, ch, in)
		}
		return
	}
	if code, ok := auth.LinkCode(in.Text); ok {
		if !c.firstSeen(ctx, in) {
			return
		}
		p, err := auth.Link(ctx, c.store, &in, code)
		if errors.Is(err, auth.ErrInvalidCode) || errors.Is(err, auth.ErrUnverifiedSender) {
			c.reply(ctx, ch, in, "That code is invalid or has expired. Ask the owner for a new one.")
			return
		}
		if err != nil {
			slog.Error("linking a chat account", "channel", in.Channel, "error", err)
			c.reply(ctx, ch, in, "Couldn't link your account. Try again.")
			return
		}
		c.reply(ctx, ch, in, fmt.Sprintf("Linked as %s. Write to talk to %s.", p.Role, c.agentName()))
		return
	}
	p, err := c.chain.Authenticate(ctx, auth.Request{Channel: &in})
	if refused(err) {
		if in.Direct && in.Authentic {
			c.reply(ctx, ch, in, "I don't know you yet. Ask my owner for a code (`a2a-serve pair invite <you>`), then send: join <code>")
		}
		return
	}
	if err != nil {
		slog.Error("authenticating channel message", "error", err)
		return
	}
	// --public-access anyone admits unlinked senders in direct chats only;
	// groups keep ignoring them.
	if p.Role == auth.RoleAnonymous && !in.Direct {
		return
	}
	if !c.firstSeen(ctx, in) {
		return
	}
	if in.Prepare != nil && !c.prepare(ctx, ch, &in) {
		return
	}
	// Stops and answers must not wait for the turn they stop or unblock,
	// which holds the conversation.
	if isCommand(in.Text, "stop") {
		c.queue.release(t)
		c.stop(ctx, ch, in, p)
		return
	}
	if rec, q, opt, ok := c.typedAnswer(ctx, in); ok {
		c.queue.release(t)
		c.answerIn(ctx, ch, in, p, rec, q, opt)
		return
	}

	<-t.turn
	switch {
	case isCommand(in.Text, "new"):
		if err := c.store.SetContext(ctx, in.Channel, string(in.Conv), a2a.NewContextID()); err != nil {
			slog.Error("starting a new context", "error", err)
			return
		}
		c.reply(ctx, ch, in, "Started a new conversation.")
		return
	case isCommand(in.Text, "start"):
		c.reply(ctx, ch, in, fmt.Sprintf("Hi! I'm %s. Write what you need; /new starts a new conversation, /stop stops the current task.", c.agentName()))
		return
	}

	contextID, err := c.contextOf(ctx, in.Channel, in.Conv)
	if err != nil {
		slog.Error("resolving conversation context", "error", err)
		return
	}
	msg := a2a.NewMessage(a2a.MessageRoleUser, messageParts(in)...)
	msg.ID = in.Channel + ":" + in.MessageID
	msg.ContextID = contextID
	msg.Metadata = map[string]any{"channel": in.Channel, "identity": in.Identity, "sender": in.Name}
	if lastID, state, err := c.tasks.LatestStateInContext(ctx, contextID); err == nil && state == a2a.TaskStateInputRequired {
		if c.canContinue(ctx, p, lastID) {
			msg.TaskID = lastID
		} else {
			c.reply(ctx, ch, in, "⏳ Another task in this conversation is waiting for an answer; yours starts after it.")
		}
	}
	c.stream(ctx, ch, in.Conv, channel.Ref{Conv: in.Conv, ID: in.MessageID}, p, &a2a.SendMessageRequest{Message: msg})
}

// prepare finishes an inbound message and tells its sender what couldn't be
// done. It reports whether the message is still to be handled.
func (c *ChatTurns) prepare(ctx context.Context, ch channel.Channel, in *channel.Inbound) bool {
	err := in.Prepare(ctx, in)
	if err == nil {
		return true
	}
	slog.Warn("preparing channel message", "channel", in.Channel, "error", err)
	var dropped *channel.DroppedError
	if errors.As(err, &dropped) {
		c.reply(ctx, ch, *in, dropped.Reason)
		return false
	}
	failed := channel.FailedAttachments(err)
	if len(failed) == 0 {
		return true
	}
	names := make([]string, len(failed))
	for i, f := range failed {
		names[i] = f.Name
	}
	notice := "⚠ I couldn't download " + strings.Join(names, ", ") + ", so the agent doesn't see it."
	if len(names) > 1 {
		notice = strings.TrimSuffix(notice, "it.") + "them."
	}
	c.reply(ctx, ch, *in, notice)
	return in.Text != "" || len(in.Files) > 0
}

// canContinue reports whether p's message continues the task: p started it
// or is an owner, and may answer its question. Others start a task of their
// own, so that a group member's message doesn't land in another member's
// pending task, and an anonymous caller's message doesn't answer an
// approval.
func (c *ChatTurns) canContinue(ctx context.Context, p auth.Principal, taskID a2a.TaskID) bool {
	if p.IsOwner() {
		return true
	}
	stored, err := c.tasks.Get(auth.WithPrincipal(ctx, auth.Owner()), taskID)
	if err != nil {
		slog.Warn("loading the pending task of a conversation", "task", taskID, "error", err)
		return false
	}
	if stored.User != p.ID {
		return false
	}
	q := approvals.FromStatus(stored.Task.Status)
	return q == nil || q.Type != approvals.TypePermission || approvals.MayAnswer(p, stored.User)
}

// refused reports whether err refuses a channel sender: one the policy
// doesn't allow, or one whose message proves nothing.
func refused(err error) bool {
	var re *auth.RefusedError
	return errors.As(err, &re) || errors.Is(err, auth.ErrNoCredential)
}

// isCommand reports whether text is the bot command /name, optionally
// addressed as /name@bot.
func isCommand(text, name string) bool {
	cmd := strings.ToLower(strings.TrimSpace(text))
	return cmd == "/"+name || strings.HasPrefix(cmd, "/"+name+"@")
}

func messageParts(in channel.Inbound) []*a2a.Part {
	var parts []*a2a.Part
	if in.Text != "" {
		parts = append(parts, a2a.NewTextPart(in.Text))
	}
	for _, f := range in.Files {
		p := a2a.NewRawPart(f.Data)
		p.Filename, p.MediaType = f.Name, f.MediaType
		parts = append(parts, p)
	}
	if len(parts) == 0 {
		parts = append(parts, a2a.NewTextPart("(empty message)"))
	}
	return parts
}

func (c *ChatTurns) contextOf(ctx context.Context, ch string, conv channel.ConvKey) (string, error) {
	id, err := c.store.ContextOf(ctx, ch, string(conv))
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	id = a2a.NewContextID()
	return id, c.store.SetContext(ctx, ch, string(conv), id)
}

func (c *ChatTurns) stop(ctx context.Context, ch channel.Channel, in channel.Inbound, p auth.Principal) {
	contextID, err := c.store.ContextOf(ctx, in.Channel, string(in.Conv))
	if err != nil {
		c.reply(ctx, ch, in, "Nothing is running.")
		return
	}
	lastID, state, err := c.tasks.LatestStateInContext(ctx, contextID)
	if err != nil || state.Terminal() {
		c.reply(ctx, ch, in, "Nothing is running.")
		return
	}
	if _, err := c.sender.CancelTask(auth.InProcess(ctx, p), &a2a.CancelTaskRequest{ID: lastID}); err != nil {
		c.reply(ctx, ch, in, "Couldn't stop the task: "+err.Error())
		return
	}
	c.reply(ctx, ch, in, "Stopped.")
}

// stream sends a message to the agent and renders the task in the conversation.
func (c *ChatTurns) stream(ctx context.Context, ch channel.Channel, conv channel.ConvKey, replyTo channel.Ref, p auth.Principal, req *a2a.SendMessageRequest) {
	c.streamThen(ctx, ch, conv, replyTo, p, req, func(error) {})
}

// streamThen is stream which calls accepted once the agent accepted the
// message, with its first event, or with the error it was refused with.
func (c *ChatTurns) streamThen(ctx context.Context, ch channel.Channel, conv channel.ConvKey, replyTo channel.Ref, p auth.Principal, req *a2a.SendMessageRequest, accepted func(error)) {
	rd := newRenderer(c.chat, ch, conv, replyTo, p)
	rd.onWaitForOwner = func(taskID a2a.TaskID) { c.chat.followOwnerAnswer(ctx, ch, conv, p, taskID) }
	first := true
	for ev, err := range c.sender.SendStreamingMessage(auth.InProcess(ctx, p), req) {
		if first {
			first = false
			accepted(err)
		}
		if err != nil {
			rd.fail(ctx, err)
			return
		}
		if rd.onEvent(ctx, ev) {
			return
		}
	}
	if first {
		accepted(errors.New("the agent ended the stream without a reply"))
	}
	rd.finish(ctx)
}
