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
	"iter"
	"log/slog"
	"strings"
	"sync"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/approvals"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/channel"
	"github.com/a2aproject/a2a-serve/internal/pairings"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// ChatTurns turns channel messages into tasks of the agent, renders their
// events back into the conversation, answers questions from chats and posts
// turns into contexts.
type ChatTurns struct {
	store      *store.Store
	tasks      *store.TaskStore
	chain      *auth.Chain
	pairings   *pairings.Service
	approvals  *approvals.Service
	prefs      *approvals.Preferences
	chat       *ChatApprovals
	supervisor *ChannelSupervisor
	queue      *ConversationQueue
	sender     turnSender
	bus        *Bus
	// unattended are the contexts of turns posted with no conversation,
	// whose tasks nobody watches.
	unattended *ContextSet
	posted     *PostedTurns
	budget     Budget
	// downloads holds a place for each message being prepared.
	downloads chan struct{}
	config    func() *agentdir.Config
}

// ChatTurnsDeps are the dependencies of [ChatTurns].
type ChatTurnsDeps struct {
	Store     *store.Store
	Tasks     *store.TaskStore
	Chain     *auth.Chain
	Pairings  *pairings.Service
	Approvals *approvals.Service
	// Preferences are what people allowed in all their conversations,
	// which /allowed lists and offers add to.
	Preferences *approvals.Preferences
	Chat        *ChatApprovals
	Supervisor  *ChannelSupervisor
	Queue       *ConversationQueue
	Sender      turnSender
	Bus         *Bus
	Unattended  *ContextSet
	// Budget is the budget of anonymous callers, which is checked before
	// their attachments are downloaded.
	Budget Budget
	// Posted are the turns posted into contexts, waiting to be sent.
	Posted *PostedTurns
	// Config returns the current config of the agent.
	Config func() *agentdir.Config
}

// maxDownloads bounds the messages whose attachments are downloaded at once,
// across channels.
const maxDownloads = 4

// Budget limits the tasks of anonymous callers.
type Budget interface {
	// CheckRate refuses a message of the anonymous caller p when its
	// listener started all the tasks it may in an hour.
	CheckRate(p auth.Principal) error
}

// turnSender sends turns to the agent.
type turnSender interface {
	SendMessage(ctx context.Context, req *a2a.SendMessageRequest) (a2a.SendMessageResult, error)
	SendStreamingMessage(ctx context.Context, req *a2a.SendMessageRequest) iter.Seq2[a2a.Event, error]
	CancelTask(ctx context.Context, req *a2a.CancelTaskRequest) (*a2a.Task, error)
}

// NewChatTurns returns the chat turns of deps.
func NewChatTurns(deps ChatTurnsDeps) *ChatTurns {
	return &ChatTurns{
		store:      deps.Store,
		tasks:      deps.Tasks,
		chain:      deps.Chain,
		pairings:   deps.Pairings,
		approvals:  deps.Approvals,
		prefs:      deps.Preferences,
		chat:       deps.Chat,
		supervisor: deps.Supervisor,
		queue:      deps.Queue,
		sender:     deps.Sender,
		bus:        deps.Bus,
		unattended: deps.Unattended,
		budget:     deps.Budget,
		downloads:  make(chan struct{}, maxDownloads),
		posted:     deps.Posted,
		config:     deps.Config,
	}
}

// Start dispatches channel messages until ctx ends.
func (c *ChatTurns) Start(ctx context.Context) { go c.dispatch(ctx) }

// ContextSet is a set of context IDs, safe for concurrent use.
type ContextSet struct {
	mu  sync.Mutex
	ids map[string]bool
}

// Add adds id to the set.
func (s *ContextSet) Add(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ids == nil {
		s.ids = map[string]bool{}
	}
	s.ids[id] = true
}

// Remove removes id from the set.
func (s *ContextSet) Remove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.ids, id)
}

// Has reports whether id is in the set.
func (s *ContextSet) Has(id string) bool {
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
	ch := c.supervisor.Channel(in.Channel)
	if ch == nil {
		return
	}
	if in.Action != nil {
		if c.firstSeen(ctx, in) {
			c.handleAction(ctx, ch, in)
		}
		return
	}
	if code, ok := pairings.LinkCode(in.Text); ok {
		if !c.firstSeen(ctx, in) {
			return
		}
		p, err := c.pairings.Redeem(ctx, code, pairings.Via{Chat: &in})
		var limited *pairings.RateLimitError
		if errors.As(err, &limited) {
			c.reply(ctx, ch, in, fmt.Sprintf("Too many invalid codes. Try again in %d seconds.", limited.Seconds()))
			return
		}
		if errors.Is(err, pairings.ErrInvalidCode) || errors.Is(err, pairings.ErrUnverifiedSender) {
			c.reply(ctx, ch, in, "That code is invalid or has expired. Ask the owner for a new one.")
			return
		}
		if err != nil {
			slog.Error("linking a chat account", "channel", in.Channel, "error", err)
			c.reply(ctx, ch, in, "Couldn't link your account. Try again.")
			return
		}
		c.reply(ctx, ch, in, fmt.Sprintf("Linked as %s. Write to talk to %s.", p.Role, c.config().Name))
		return
	}
	p, err := c.chain.Authenticate(ctx, auth.Request{Channel: &in})
	if auth.Refused(err) {
		if in.Direct && in.Authentic {
			c.reply(ctx, ch, in, "I don't know you yet. Ask my owner for a code (`a2a-serve pair invite <you>`), then send: join <code>")
		}
		return
	}
	if err != nil {
		slog.Error("authenticating channel message", "error", err)
		return
	}
	// --public-allow-incoming anyone admits unlinked senders in direct chats only;
	// groups keep ignoring them.
	if p.Role == auth.RoleAnonymous && !in.Direct {
		return
	}
	if !c.firstSeen(ctx, in) {
		return
	}
	if in.Prepare != nil && !c.prepare(ctx, ch, &in, p) {
		return
	}
	// Stops and answers must not wait for the turn they stop or unblock,
	// which holds the conversation.
	if isCommand(in.Text, "stop") {
		c.queue.release(t)
		c.stop(ctx, ch, in, p)
		return
	}
	if first, _, _ := strings.Cut(strings.TrimSpace(in.Text), " "); isCommand(first, "allowed") && c.prefs != nil {
		c.queue.release(t)
		c.handleAllowed(ctx, ch, in, p)
		return
	}
	switch rec, q, opt, kind := c.typedAnswer(ctx, in, ch.Caps().Buttons); kind {
	case answer:
		c.queue.release(t)
		c.answerIn(ctx, ch, in, p, rec, q, opt)
		return
	case ambiguousAnswer:
		c.queue.release(t)
		c.reply(ctx, ch, in, "Your reply has more text than an answer, so I didn't take it as one. Reply with only one of: "+q.Words()+".")
		return
	case unaddressedAnswer:
		c.queue.release(t)
		c.reply(ctx, ch, in, "Several questions wait for an answer here. Reply to the message of the question you answer.")
		return
	}

	switch {
	case isCommand(in.Text, "new"):
		<-t.turn
		if err := c.store.SetContext(ctx, in.Channel, string(in.Conv), a2a.NewContextID()); err != nil {
			slog.Error("starting a new context", "error", err)
			return
		}
		c.reply(ctx, ch, in, "Started a new conversation.")
		return
	case isCommand(in.Text, "start"):
		<-t.turn
		c.reply(ctx, ch, in, fmt.Sprintf("Hi! I'm %s. Write what you need; /new starts a new conversation, /stop stops the current task, /allowed shows what runs without asking for you.", c.config().Name))
		return
	}

	batch := c.queue.waitTurn(t, &waitingMessage{in: in, p: p})
	if batch == nil {
		return
	}
	contextID, err := c.ContextOf(ctx, in.Channel, in.Conv)
	if err != nil {
		slog.Error("resolving conversation context", "error", err)
		return
	}
	last := batch[len(batch)-1].in
	msg := a2a.NewMessage(a2a.MessageRoleUser, messageParts(batch)...)
	msg.ID = in.Channel + ":" + in.MessageID
	msg.ContextID = contextID
	msg.Metadata = map[string]any{"channel": in.Channel, "identity": in.Identity, "sender": in.Name}
	if lastID, state, err := c.tasks.LatestStateInContext(ctx, contextID); err == nil && state == a2a.TaskStateInputRequired {
		approval, ok := c.canContinue(ctx, p, lastID)
		switch {
		case ok && approval != nil:
			c.reply(ctx, ch, last, "A tool call waits for your approval, so I didn't send this to the agent. Answer it with one of: "+approval.Words()+", or send /stop to stop the task.")
			return
		case ok:
			msg.TaskID = lastID
		default:
			c.reply(ctx, ch, last, "⏳ Another task in this conversation is waiting for an answer; yours starts after it.")
		}
	}
	c.stream(ctx, ch, in.Conv, channel.Ref{Conv: in.Conv, ID: last.MessageID}, p, &a2a.SendMessageRequest{Message: msg})
}

// waitingMessage is a chat message that waits for the turn of its
// conversation.
type waitingMessage struct {
	in channel.Inbound
	p  auth.Principal
}

// joins reports whether next may reach the agent in the same turn as m:
// the same person sent it to the same conversation, which a message that
// started a thread of its own is not.
func (m *waitingMessage) joins(next *waitingMessage) bool {
	return next.in.Conv == m.in.Conv && next.in.Identity == m.in.Identity && next.p.ID == m.p.ID && next.p.Role == m.p.Role
}

// prepare finishes an inbound message and tells its sender what couldn't be
// done. It reports whether the message is still to be handled. An
// anonymous sender's message is refused first if the budget of anonymous
// callers is used up, so that it costs no download.
func (c *ChatTurns) prepare(ctx context.Context, ch channel.Channel, in *channel.Inbound, p auth.Principal) bool {
	if p.Role == auth.RoleAnonymous {
		if err := c.budget.CheckRate(p); err != nil {
			c.reply(ctx, ch, *in, "⚠ "+err.Error())
			return false
		}
	}
	select {
	case c.downloads <- struct{}{}:
	case <-ctx.Done():
		return false
	}
	err := in.Prepare(ctx, in)
	<-c.downloads
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
// approval. approval is the permission question the task waits on, which
// only an answer continues.
func (c *ChatTurns) canContinue(ctx context.Context, p auth.Principal, taskID a2a.TaskID) (approval *approvals.Question, ok bool) {
	stored, err := c.tasks.Get(auth.WithPrincipal(ctx, auth.Owner()), taskID)
	if err != nil {
		slog.Warn("loading the pending task of a conversation", "task", taskID, "error", err)
		return nil, p.IsOwner()
	}
	if q := approvals.FromStatus(stored.Task.Status); q != nil && q.Type == approvals.TypePermission {
		approval = q
	}
	if !p.IsOwner() && stored.User != p.ID {
		return nil, false
	}
	return approval, approval == nil || approvals.MayAnswer(p, stored.User)
}

// isCommand reports whether text is the bot command /name, optionally
// addressed as /name@bot.
func isCommand(text, name string) bool {
	cmd := strings.ToLower(strings.TrimSpace(text))
	return cmd == "/"+name || strings.HasPrefix(cmd, "/"+name+"@")
}

func messageParts(batch []*waitingMessage) []*a2a.Part {
	var parts []*a2a.Part
	for _, m := range batch {
		if m.in.Text != "" {
			parts = append(parts, a2a.NewTextPart(m.in.Text))
		}
		for _, f := range m.in.Files {
			p := a2a.NewRawPart(f.Data)
			p.Filename, p.MediaType = f.Name, f.MediaType
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 {
		parts = append(parts, a2a.NewTextPart("(empty message)"))
	}
	return parts
}

// ContextOf returns the context of the conversation conv of the channel
// ch, which starts a new one if the conversation has none.
func (c *ChatTurns) ContextOf(ctx context.Context, ch string, conv channel.ConvKey) (string, error) {
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
