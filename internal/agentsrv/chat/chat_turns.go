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
	"fmt"
	"iter"
	"log/slog"
	"strings"
	"sync"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/approvals"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/messenger"
	"github.com/a2aproject/a2a-serve/internal/pairings"
	"github.com/a2aproject/a2a-serve/internal/speech"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// ChatTurns turns messenger messages into tasks of the agent, renders their
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
	supervisor *MessengerSupervisor
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
	// running counts the dispatcher and the handlers it started.
	running sync.WaitGroup
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
	Supervisor  *MessengerSupervisor
	Queue       *ConversationQueue
	Sender      turnSender
	Bus         *Bus
	Unattended  *ContextSet
	// Budget is the budget of guests, which is checked before
	// their attachments are downloaded.
	Budget Budget
	// Posted are the turns posted into contexts, waiting to be sent.
	Posted *PostedTurns
	// Config returns the current config of the agent.
	Config func() *agentdir.Config
}

// maxDownloads bounds the messages whose attachments are downloaded at once,
// across messengers.
const maxDownloads = 4

// Budget limits the tasks of guests.
type Budget interface {
	// CheckRate refuses a message of the guest p when its
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

// Start dispatches messenger messages until ctx ends.
func (c *ChatTurns) Start(ctx context.Context) {
	c.running.Go(func() { c.dispatch(ctx) })
}

// Wait returns once the ctx of [ChatTurns.Start] ended and the messages it
// dispatched are handled, so that the store can close, or when ctx ends.
func (c *ChatTurns) Wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		c.running.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

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
				t = c.queue.enqueue(in.Messenger, in.Conv)
			}
			c.running.Go(func() { c.handle(ctx, in, t) })
		}
	}
}

func (c *ChatTurns) reply(ctx context.Context, ch messenger.Messenger, in messenger.Inbound, text string) {
	if _, err := ch.Send(ctx, messenger.Outbound{Conv: in.Conv, Text: text, ReplyTo: messenger.Ref{Conv: in.Conv, ID: in.MessageID}}); err != nil {
		slog.Warn("replying in messenger", "messenger", ch.Name(), "error", err)
	}
}

// firstSeen reports whether the message is new, so that redelivered ones are
// dropped. It runs after the checks that refuse a sender, so that strangers
// don't make the server write.
func (c *ChatTurns) firstSeen(ctx context.Context, in messenger.Inbound) bool {
	first, err := c.store.FirstSeen(ctx, in.Messenger+"|"+in.MessageID)
	if err != nil {
		slog.Error("dedup", "error", err)
		return false
	}
	return first
}

func (c *ChatTurns) handle(ctx context.Context, in messenger.Inbound, t *convTicket) {
	if t != nil {
		defer c.queue.release(t)
	}
	ch := c.supervisor.Messenger(in.Messenger)
	if ch == nil {
		return
	}
	if !in.Direct {
		ctx = auth.InGroupChat(ctx)
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
			slog.Error("linking a chat account", "messenger", in.Messenger, "error", err)
			c.reply(ctx, ch, in, "Couldn't link your account. Try again.")
			return
		}
		if l, ok := ch.(messenger.Linker); ok {
			if err := l.Linked(ctx, in); err != nil {
				slog.Warn("recording a linked chat", "messenger", in.Messenger, "error", err)
			}
		}
		c.reply(ctx, ch, in, fmt.Sprintf("Linked as %s. Write to talk to %s.", p.Role, c.config().Name))
		return
	}
	p, err := c.chain.Authenticate(ctx, auth.Request{Messenger: &in})
	if auth.Refused(err) {
		if in.Direct && in.Authentic {
			c.reply(ctx, ch, in, "I don't know you yet. Ask my owner for a code (`a2a-serve pair invite <you>`), then send: join <code>")
		}
		return
	}
	if err != nil {
		slog.Error("authenticating messenger message", "error", err)
		return
	}
	// --public-allow-incoming anyone admits unlinked senders in direct chats only;
	// groups keep ignoring them.
	if p.Role == auth.RoleGuest && !in.Direct {
		return
	}
	if !c.firstSeen(ctx, in) {
		return
	}
	if in.Prepare != nil && !c.prepare(ctx, ch, &in, p) {
		return
	}
	// Commands that don't wait for the turn, such as /stop, and answers
	// must not wait for the turn they stop or unblock, which holds the
	// conversation.
	if cmd, arg, ok := parseCommand(in.Text); ok {
		if cmd.waits {
			<-t.turn
		} else {
			c.queue.release(t)
		}
		cmd.handle(c, ctx, ch, in, p, arg)
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

	batch := c.queue.waitTurn(t, &waitingMessage{in: in, p: p})
	if batch == nil {
		return
	}
	contextID, err := c.ContextOf(ctx, in.Messenger, in.Conv)
	if err != nil {
		slog.Error("resolving conversation context", "error", err)
		return
	}
	last := batch[len(batch)-1].in
	msg := a2a.NewMessage(a2a.MessageRoleUser, messageParts(batch)...)
	msg.ID = in.Messenger + ":" + in.MessageID
	msg.ContextID = contextID
	msg.Metadata = map[string]any{"messenger": in.Messenger, "identity": in.Identity, "sender": in.Name}
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
	c.stream(ctx, ch, in.Conv, messenger.Ref{Conv: in.Conv, ID: last.MessageID}, p, &a2a.SendMessageRequest{Message: msg})
}

// waitingMessage is a chat message that waits for the turn of its
// conversation.
type waitingMessage struct {
	in messenger.Inbound
	p  auth.Principal
}

// joins reports whether next may reach the agent in the same turn as m:
// the same person sent it to the same conversation, which a message that
// started a thread of its own is not.
func (m *waitingMessage) joins(next *waitingMessage) bool {
	return next.in.Conv == m.in.Conv && next.in.Identity == m.in.Identity && next.p.ID == m.p.ID && next.p.Role == m.p.Role
}

// prepare finishes an inbound message and tells its sender what couldn't be
// done. It reports whether the message is still to be handled. A guest's
// message is refused first if the budget of guests is used up, so that it
// costs no download.
func (c *ChatTurns) prepare(ctx context.Context, ch messenger.Messenger, in *messenger.Inbound, p auth.Principal) bool {
	if p.Role == auth.RoleGuest {
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
	slog.Warn("preparing messenger message", "messenger", in.Messenger, "error", err)
	var dropped *messenger.DroppedError
	if errors.As(err, &dropped) {
		c.reply(ctx, ch, *in, dropped.Reason)
		return false
	}
	failed := messenger.FailedAttachments(err)
	if len(failed) == 0 {
		return true
	}
	names := make([]string, len(failed))
	for i, f := range failed {
		names[i] = f.Name
		var tooLarge *messenger.TooLargeError
		if errors.As(f, &tooLarge) {
			names[i] += " (" + tooLarge.Over() + ")"
		}
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
// pending task, and a guest's message doesn't answer an
// approval. approval is the permission question the task waits on, which
// only an answer continues.
func (c *ChatTurns) canContinue(ctx context.Context, p auth.Principal, taskID a2a.TaskID) (approval *approvals.Question, ok bool) {
	stored, err := c.tasks.Header(auth.WithPrincipal(ctx, auth.Owner()), taskID)
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

// ContextOf returns the context of the conversation conv of the messenger
// ch, which starts a new one if the conversation has none.
func (c *ChatTurns) ContextOf(ctx context.Context, ch string, conv messenger.ConvKey) (string, error) {
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

func (c *ChatTurns) stop(ctx context.Context, ch messenger.Messenger, in messenger.Inbound, p auth.Principal) {
	contextID, err := c.store.ContextOf(ctx, in.Messenger, string(in.Conv))
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

// voiceReplyModes are the output modes of a turn whose reply is spoken.
var voiceReplyModes = []string{"text/plain", speech.MediaOgg}

// withVoiceReply returns req asking for the reply as audio too.
func withVoiceReply(req *a2a.SendMessageRequest) *a2a.SendMessageRequest {
	out := *req
	cfg := a2a.SendMessageConfig{}
	if req.Config != nil {
		cfg = *req.Config
	}
	cfg.AcceptedOutputModes = voiceReplyModes
	out.Config = &cfg
	return &out
}

// stream sends a message to the agent and renders the task in the conversation.
func (c *ChatTurns) stream(ctx context.Context, ch messenger.Messenger, conv messenger.ConvKey, replyTo messenger.Ref, p auth.Principal, req *a2a.SendMessageRequest) {
	c.streamThen(ctx, ch, conv, replyTo, p, req, false, func(error) {})
}

// streamThen is stream which calls accepted once the agent accepted the
// message, with its first event, or with the error it was refused with.
// With quiet, a refusal isn't shown in the chat.
func (c *ChatTurns) streamThen(ctx context.Context, ch messenger.Messenger, conv messenger.ConvKey, replyTo messenger.Ref, p auth.Principal, req *a2a.SendMessageRequest, quiet bool, accepted func(error)) {
	if c.voiceReplies(ctx, p) {
		req = withVoiceReply(req)
	}
	rd := newRenderer(c.chat, ch, conv, replyTo, p)
	rd.onWaitForOwner = func(taskID a2a.TaskID) { c.chat.followOwnerAnswer(ctx, ch, conv, p, taskID) }
	first := true
	for ev, err := range c.sender.SendStreamingMessage(auth.InProcess(ctx, p), req) {
		if first {
			first = false
			accepted(err)
			if err != nil && quiet {
				return
			}
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
