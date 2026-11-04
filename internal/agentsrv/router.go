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
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/a2aproject/a2a-serve/internal/agentsrv/approvals"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/channel"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/manage"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// Router turns channel messages into A2A tasks of the agent and renders
// task events back into the conversation.
type Router struct {
	store     *store.Store
	tasks     *store.TaskStore
	chain     *auth.Chain
	approvals *approvals.Service
	bus       *eventBus
	agentName func() string
	// ownerChat returns the --owner-chat setting.
	ownerChat func() string
	logf      func(format string, args ...any)
	// handler is the agent's A2A handler. It is set before the router starts.
	handler a2asrv.RequestHandler

	// ctx ends when the server stops; channels and turns run in it.
	ctx context.Context
	in  chan channel.Inbound

	mu    sync.Mutex
	chans map[string]*runningChannel
	convs map[string][]*convTicket
	// unattended are the contexts of turns posted with no conversation,
	// whose tasks nobody watches.
	unattended map[string]bool
}

type runningChannel struct {
	ch     channel.Channel
	cancel context.CancelFunc
	done   chan struct{}
	// err is the last error of Run, cleared when it runs again.
	err error
}

// convTicket is a place in the line of a conversation. turn is closed when
// the ticket is first in line.
type convTicket struct {
	key  string
	turn chan struct{}
	once sync.Once
}

// Channel restarts back off from channelRetryMin to channelRetryMax.
var (
	channelRetryMin = 5 * time.Second
	channelRetryMax = 5 * time.Minute
)

// start dispatches channel messages until ctx ends.
func (r *Router) start(ctx context.Context) {
	r.ctx = ctx
	r.in = make(chan channel.Inbound, 64)
	r.chans = map[string]*runningChannel{}
	r.convs = map[string][]*convTicket{}
	go r.dispatch(ctx)
}

// add starts a channel, replacing a running one of the same name after it
// stopped. A failing channel is restarted with backoff rather than stopping
// the server, so that a messenger outage doesn't take the agent down.
func (r *Router) add(ch channel.Channel) {
	r.remove(ch.Name())
	cctx, cancel := context.WithCancel(r.ctx)
	rc := &runningChannel{ch: ch, cancel: cancel, done: make(chan struct{})}
	r.mu.Lock()
	r.chans[ch.Name()] = rc
	r.mu.Unlock()
	go r.run(cctx, rc)
}

func (r *Router) run(ctx context.Context, rc *runningChannel) {
	defer close(rc.done)
	delay := channelRetryMin
	for {
		err := rc.ch.Run(ctx, r.in)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			err = errors.New("stopped unexpectedly")
		}
		slog.Error("channel stopped; restarting", "channel", rc.ch.Name(), "in", delay, "error", err)
		r.mu.Lock()
		rc.err = err
		r.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(delay*2, channelRetryMax)
		r.mu.Lock()
		rc.err = nil
		r.mu.Unlock()
	}
}

// remove stops a channel and waits until it stopped, so that a replacement
// never polls the same account at the same time.
func (r *Router) remove(name string) {
	r.mu.Lock()
	rc := r.chans[name]
	delete(r.chans, name)
	r.mu.Unlock()
	if rc == nil {
		return
	}
	rc.cancel()
	<-rc.done
}

// stopAll stops every channel and waits until they stopped or ctx ends.
func (r *Router) stopAll(ctx context.Context) error {
	r.mu.Lock()
	running := make([]*runningChannel, 0, len(r.chans))
	for name, rc := range r.chans {
		running = append(running, rc)
		delete(r.chans, name)
	}
	r.mu.Unlock()
	for _, rc := range running {
		rc.cancel()
	}
	for _, rc := range running {
		select {
		case <-rc.done:
		case <-ctx.Done():
			return fmt.Errorf("waiting for %s to stop: %w", rc.ch.Name(), ctx.Err())
		}
	}
	return nil
}

func (r *Router) channel(name string) channel.Channel {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rc := r.chans[name]; rc != nil {
		return rc.ch
	}
	return nil
}

func (r *Router) channels() []channel.Channel {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]channel.Channel, 0, len(r.chans))
	for _, rc := range r.chans {
		out = append(out, rc.ch)
	}
	slices.SortFunc(out, func(a, b channel.Channel) int { return strings.Compare(a.Name(), b.Name()) })
	return out
}

func (r *Router) status() []controlapi.ChannelStatus {
	var out []controlapi.ChannelStatus
	for _, ch := range r.channels() {
		out = append(out, *r.channelStatus(ch, ""))
	}
	return out
}

func (r *Router) channelStatus(ch channel.Channel, hint string) *controlapi.ChannelStatus {
	st := &controlapi.ChannelStatus{Name: ch.Name()}
	if d, ok := ch.(channel.Describer); ok {
		st.Account = d.Describe()
	}
	if hint != "" {
		st.Account = strings.TrimSpace(st.Account + "\n" + hint)
	}
	r.mu.Lock()
	if rc := r.chans[ch.Name()]; rc != nil && rc.err != nil {
		st.Error = rc.err.Error()
	}
	r.mu.Unlock()
	return st
}

// announce prints the channels, and how to link the owner if nobody is
// linked as owner yet. One owner code serves every channel.
func (r *Router) announce(ctx context.Context) error {
	code := ""
	for _, ch := range r.channels() {
		line := "✓ " + ch.Name()
		if d, ok := ch.(channel.Describer); ok {
			line += " " + d.Describe()
		}
		r.logf("%s", line)
		if hint := r.ownerHintWith(ctx, ch, &code); hint != "" {
			r.logf("%s", hint)
		}
	}
	return nil
}

// ownerHint mints an owner code for a channel nobody is linked on as owner.
func (r *Router) ownerHint(ctx context.Context, ch channel.Channel) string {
	code := ""
	return r.ownerHintWith(ctx, ch, &code)
}

// ownerHintWith is ownerHint which mints *code if it is empty, and reuses it
// otherwise. A new owner code replaces the pending one, so that restarts
// don't pile up codes.
func (r *Router) ownerHintWith(ctx context.Context, ch channel.Channel, code *string) string {
	links, err := r.store.LinksOf(ctx, auth.LocalPairing)
	if err != nil {
		slog.Error("listing owner links", "error", err)
		return ""
	}
	if slices.ContainsFunc(links, func(l store.Link) bool { return l.Channel == ch.Name() }) {
		return ""
	}
	d, ok := ch.(channel.Describer)
	if !ok {
		return ""
	}
	if *code == "" {
		c, _, err := auth.MintSoleCode(ctx, r.store, auth.LocalPairing)
		if err != nil {
			slog.Error("minting owner code", "error", err)
			return ""
		}
		*code = c
	}
	return fmt.Sprintf("→ link yourself as owner: %s   (%d min)", d.LinkHint(*code), int(auth.CodeTTL.Minutes()))
}

func (r *Router) linkHints(code string) []string {
	var out []string
	for _, ch := range r.channels() {
		if d, ok := ch.(channel.Describer); ok {
			out = append(out, ch.Name()+": "+d.LinkHint(code))
		}
	}
	return out
}

func (r *Router) dispatch(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case in := <-r.in:
			// Messages take their place in line on arrival, so that a slow
			// Prepare doesn't let a later message overtake them.
			var t *convTicket
			if in.Action == nil {
				t = r.enqueue(in.Channel, in.Conv)
			}
			go r.handle(ctx, in, t)
		}
	}
}

// enqueue puts a ticket at the end of the line of a conversation.
func (r *Router) enqueue(ch string, conv channel.ConvKey) *convTicket {
	t := &convTicket{key: ch + "\x00" + string(conv), turn: make(chan struct{})}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.convs[t.key] = append(r.convs[t.key], t)
	if len(r.convs[t.key]) == 1 {
		close(t.turn)
	}
	return t
}

// release takes a ticket out of line, wherever it is, and lets the next one
// go if it was first. Releasing twice is a no-op.
func (r *Router) release(t *convTicket) {
	t.once.Do(func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		line := r.convs[t.key]
		i := slices.Index(line, t)
		if i < 0 {
			return
		}
		line = slices.Delete(line, i, i+1)
		if len(line) == 0 {
			delete(r.convs, t.key)
			return
		}
		r.convs[t.key] = line
		if i == 0 {
			close(line[0].turn)
		}
	})
}

// lockConv serializes the handling of one conversation's messages in the
// order they lock it. The returned function unlocks it.
func (r *Router) lockConv(ch string, conv channel.ConvKey) func() {
	t := r.enqueue(ch, conv)
	<-t.turn
	return func() { r.release(t) }
}

func (r *Router) reply(ctx context.Context, ch channel.Channel, in channel.Inbound, text string) {
	if _, err := ch.Send(ctx, channel.Outbound{Conv: in.Conv, Text: text, ReplyTo: channel.Ref{Conv: in.Conv, ID: in.MessageID}}); err != nil {
		slog.Warn("replying in channel", "channel", ch.Name(), "error", err)
	}
}

func (r *Router) handle(ctx context.Context, in channel.Inbound, t *convTicket) {
	if t != nil {
		defer r.release(t)
	}
	ch := r.channel(in.Channel)
	if ch == nil {
		return
	}
	first, err := r.store.FirstSeen(ctx, in.Channel+"|"+in.MessageID)
	if err != nil {
		slog.Error("dedup", "error", err)
		return
	}
	if !first {
		return
	}
	if in.Action != nil {
		r.handleAction(ctx, ch, in)
		return
	}
	if code, ok := auth.LinkCode(in.Text); ok {
		p, err := auth.Link(ctx, r.store, &in, code)
		if err != nil {
			r.reply(ctx, ch, in, "That code is invalid or has expired. Ask the owner for a new one.")
			return
		}
		r.reply(ctx, ch, in, fmt.Sprintf("Linked as %s. Write to talk to %s.", p.Role, r.agentName()))
		return
	}
	p, err := r.chain.Authenticate(ctx, auth.Request{Channel: &in})
	if refused(err) {
		if in.Direct && in.Authentic {
			r.reply(ctx, ch, in, "I don't know you yet. Ask my owner for a code (`a2a-serve pair invite <you>`), then send: join <code>")
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
	if in.Prepare != nil {
		if err := in.Prepare(ctx, &in); err != nil {
			slog.Warn("preparing channel message", "channel", in.Channel, "error", err)
		}
	}
	// Stops and answers must not wait for the turn they stop or unblock,
	// which holds the conversation.
	if isCommand(in.Text, "stop") {
		r.release(t)
		r.stop(ctx, ch, in, p)
		return
	}
	if rec, q, opt, ok := r.typedAnswer(ctx, in); ok {
		r.release(t)
		r.answerIn(ctx, ch, in, p, rec, q, opt)
		return
	}

	<-t.turn
	switch {
	case isCommand(in.Text, "new"):
		if err := r.store.SetContext(ctx, in.Channel, string(in.Conv), a2a.NewContextID()); err != nil {
			slog.Error("starting a new context", "error", err)
			return
		}
		r.reply(ctx, ch, in, "Started a new conversation.")
		return
	case isCommand(in.Text, "start"):
		r.reply(ctx, ch, in, fmt.Sprintf("Hi! I'm %s. Write what you need; /new starts a new conversation, /stop stops the current task.", r.agentName()))
		return
	}

	contextID, err := r.contextOf(ctx, in.Channel, in.Conv)
	if err != nil {
		slog.Error("resolving conversation context", "error", err)
		return
	}
	msg := a2a.NewMessage(a2a.MessageRoleUser, messageParts(in)...)
	msg.ID = in.Channel + ":" + in.MessageID
	msg.ContextID = contextID
	msg.Metadata = map[string]any{"channel": in.Channel, "identity": in.Identity, "sender": in.Name}
	if last, err := r.tasks.LatestInContext(ctx, contextID); err == nil && last.Status.State == a2a.TaskStateInputRequired {
		if r.canContinue(ctx, p, last.ID) {
			msg.TaskID = last.ID
		} else {
			r.reply(ctx, ch, in, "⏳ Another task in this conversation is waiting for an answer; yours starts after it.")
		}
	}
	r.stream(ctx, ch, in.Conv, channel.Ref{Conv: in.Conv, ID: in.MessageID}, p, &a2a.SendMessageRequest{Message: msg})
}

// canContinue reports whether p's message continues the task: p started it
// or is an owner. Others start a task of their own, so that a group
// member's message doesn't land in another member's pending task.
func (r *Router) canContinue(ctx context.Context, p auth.Principal, taskID a2a.TaskID) bool {
	if p.IsOwner() {
		return true
	}
	stored, err := r.tasks.Get(auth.WithPrincipal(ctx, auth.Owner()), taskID)
	if err != nil {
		slog.Warn("loading the pending task of a conversation", "task", taskID, "error", err)
		return false
	}
	return stored.User == p.ID
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

func (r *Router) contextOf(ctx context.Context, ch string, conv channel.ConvKey) (string, error) {
	id, err := r.store.ContextOf(ctx, ch, string(conv))
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	id = a2a.NewContextID()
	return id, r.store.SetContext(ctx, ch, string(conv), id)
}

func (r *Router) stop(ctx context.Context, ch channel.Channel, in channel.Inbound, p auth.Principal) {
	contextID, err := r.store.ContextOf(ctx, in.Channel, string(in.Conv))
	if err != nil {
		r.reply(ctx, ch, in, "Nothing is running.")
		return
	}
	last, err := r.tasks.LatestInContext(ctx, contextID)
	if err != nil || last.Status.State.Terminal() {
		r.reply(ctx, ch, in, "Nothing is running.")
		return
	}
	if _, err := r.handler.CancelTask(auth.InProcess(ctx, p), &a2a.CancelTaskRequest{ID: last.ID}); err != nil {
		r.reply(ctx, ch, in, "Couldn't stop the task: "+err.Error())
		return
	}
	r.reply(ctx, ch, in, "Stopped.")
}

// stream sends a message to the agent and renders the task in the conversation.
func (r *Router) stream(ctx context.Context, ch channel.Channel, conv channel.ConvKey, replyTo channel.Ref, p auth.Principal, req *a2a.SendMessageRequest) {
	r.streamThen(ctx, ch, conv, replyTo, p, req, func(error) {})
}

// streamThen is stream which calls accepted once the agent accepted the
// message, with its first event, or with the error it was refused with.
func (r *Router) streamThen(ctx context.Context, ch channel.Channel, conv channel.ConvKey, replyTo channel.Ref, p auth.Principal, req *a2a.SendMessageRequest, accepted func(error)) {
	rd := newRenderer(r, ch, conv, replyTo, p)
	first := true
	for ev, err := range r.handler.SendStreamingMessage(auth.InProcess(ctx, p), req) {
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

// restorePending makes a question whose answer wasn't delivered the one a
// typed reply in the conversation answers again.
func (r *Router) restorePending(ctx context.Context, in channel.Inbound, questionID string) {
	if err := r.store.SetPendingQuestion(context.WithoutCancel(ctx), in.Channel, string(in.Conv), questionID); err != nil {
		slog.Warn("restoring the pending question", "error", err)
	}
}

// handleAction answers a question from a button click.
func (r *Router) handleAction(ctx context.Context, ch channel.Channel, in channel.Inbound) {
	rec, err := r.store.QuestionByID(ctx, in.Action.QuestionID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			slog.Error("loading question", "question", in.Action.QuestionID, "error", err)
		}
		return
	}
	if rec.AnsweredBy != "" {
		r.reply(ctx, ch, in, "Already answered.")
		return
	}
	p, err := r.chain.Authenticate(ctx, auth.Request{Channel: &in})
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
	r.answerIn(ctx, ch, in, p, rec, q, q.Options[in.Action.Option])
}

// typedAnswer returns the answer a message types to the question pending
// in its conversation, for channels without buttons and for people who
// type instead of clicking.
func (r *Router) typedAnswer(ctx context.Context, in channel.Inbound) (store.Question, *approvals.Question, approvals.Option, bool) {
	id, err := r.store.PendingQuestion(ctx, in.Channel, string(in.Conv))
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			slog.Warn("loading the pending question", "error", err)
		}
		return store.Question{}, nil, approvals.Option{}, false
	}
	rec, err := r.store.QuestionByID(ctx, id)
	if err != nil || rec.AnsweredBy != "" {
		return store.Question{}, nil, approvals.Option{}, false
	}
	q := approvals.FromRecord(rec)
	opt, ok := q.Match(in.Text)
	return rec, q, opt, ok
}

// answerIn answers a question from a conversation. If the question belongs
// to the task of this conversation, the rest of the task is rendered here.
func (r *Router) answerIn(ctx context.Context, ch channel.Channel, in channel.Inbound, p auth.Principal, rec store.Question, q *approvals.Question, opt approvals.Option) {
	if q.Type == approvals.TypePermission && !approvals.MayAnswer(p, rec.User) {
		r.reply(ctx, ch, in, "Only the person who started the task or an owner can answer this.")
		return
	}
	if err := r.store.ClearPendingQuestion(ctx, in.Channel, string(in.Conv)); err != nil {
		slog.Warn("clearing pending question", "error", err)
	}
	who := in.Name
	if who == "" {
		who = approvals.DisplayPrincipal(p.ID)
	}
	own, err := r.store.ContextOf(ctx, in.Channel, string(in.Conv))
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		slog.Warn("resolving conversation context", "error", err)
	}
	if rec.Contact == "" && rec.ContextID == own && r.convBusy(in.Channel, in.Conv) {
		// A turn queued behind the task holds the conversation, so the rest
		// of the task is rendered without it.
		r.follow(ctx, ch, in.Conv, p, a2a.TaskID(rec.TaskID), func() error { return r.approvals.Answer(ctx, rec, p, opt) }, func(err error) {
			r.answered(ctx, ch, in, rec, opt, who, err)
		})
		return
	}
	if rec.Contact == "" && rec.ContextID == own {
		msg, err := approvals.AnswerMessage(a2a.TaskID(rec.TaskID), rec.ContextID, q.ID, opt.ID)
		if err != nil {
			slog.Error("answering question", "question", rec.ID, "error", err)
			r.reply(ctx, ch, in, "Couldn't send the answer. Try again.")
			return
		}
		if err := r.store.AnswerQuestion(ctx, rec.ID, p.ID, opt.ID); err != nil {
			if !errors.Is(err, store.ErrAnswered) {
				slog.Error("answering question", "question", rec.ID, "error", err)
				r.reply(ctx, ch, in, "Couldn't record the answer. Try again.")
				return
			}
			r.reply(ctx, ch, in, "Already answered.")
			return
		}
		unlock := r.lockConv(in.Channel, in.Conv)
		defer unlock()
		answered := false
		r.streamThen(ctx, ch, in.Conv, channel.Ref{}, p, &a2a.SendMessageRequest{Message: msg}, func(err error) {
			if err == nil {
				answered = true
				r.MarkAnswered(ctx, rec.ID, opt.Label, who)
				return
			}
			if err := r.store.UnanswerQuestion(context.WithoutCancel(ctx), rec.ID, p.ID); err != nil {
				slog.Error("clearing an undelivered answer", "question", rec.ID, "error", err)
			}
		})
		if !answered {
			r.restorePending(ctx, in, rec.ID)
		}
		return
	}
	r.answered(ctx, ch, in, rec, opt, who, r.approvals.Answer(ctx, rec, p, opt))
}

// answered reports the result of answering a question in a conversation.
func (r *Router) answered(ctx context.Context, ch channel.Channel, in channel.Inbound, rec store.Question, opt approvals.Option, who string, err error) {
	switch {
	case errors.Is(err, store.ErrAnswered):
		r.reply(ctx, ch, in, "Already answered.")
	case err != nil:
		r.restorePending(ctx, in, rec.ID)
		r.reply(ctx, ch, in, "Couldn't send the answer: "+err.Error())
	default:
		r.MarkAnswered(ctx, rec.ID, opt.Label, who)
	}
}

// convBusy reports whether a turn holds or waits for the conversation.
func (r *Router) convBusy(ch string, conv channel.ConvKey) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.convs[ch+"\x00"+string(conv)]) > 0
}

// follow renders the rest of a task in a conversation after answer
// delivers an answer to it, and calls done with answer's result.
func (r *Router) follow(ctx context.Context, ch channel.Channel, conv channel.ConvKey, p auth.Principal, taskID a2a.TaskID, answer func() error, done func(error)) {
	events, unsubscribe := r.bus.subscribe(taskID)
	err := answer()
	done(err)
	if err != nil {
		unsubscribe()
		return
	}
	go func() {
		defer unsubscribe()
		rd := newRenderer(r, ch, conv, channel.Ref{}, p)
		timeout := time.NewTimer(25 * time.Hour)
		defer timeout.Stop()
		for {
			ev, ok := events.next(ctx, timeout.C)
			if !ok || rd.onEvent(ctx, ev) {
				return
			}
		}
	}()
}

// MarkAnswered replaces the buttons of a question with who answered it.
func (r *Router) MarkAnswered(ctx context.Context, questionID, label, who string) {
	msgs, err := r.store.TakeQuestionMessages(ctx, questionID)
	if err != nil {
		slog.Warn("loading the messages of an answered question", "question", questionID, "error", err)
		return
	}
	for _, m := range msgs {
		ch := r.channel(m.Channel)
		if ch == nil || !ch.Caps().Edit {
			continue
		}
		text := fmt.Sprintf("%s\n\n✓ %s by %s", m.Text, label, who)
		ref := channel.Ref{Conv: channel.ConvKey(m.Conv), ID: m.MessageID}
		if err := ch.Edit(ctx, ref, channel.Outbound{Conv: ref.Conv, Text: text}); err != nil {
			slog.Debug("updating answered question", "error", err)
		}
	}
}

// Ask sends a question to a chat.
func (r *Router) Ask(ctx context.Context, to approvals.Chat, header string, rec store.Question) {
	ch := r.channel(to.Channel)
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
		out.Text += fmt.Sprintf("\n\nReply %s, or open %s", q.Words(), r.approvals.PageURL(rec))
	}
	ref, err := ch.Send(ctx, out)
	if err != nil {
		slog.Warn("sending question", "channel", ch.Name(), "error", err)
		return
	}
	r.remember(ctx, rec.ID, ch.Name(), to.Conv, ref, text)
}

// ownerChatOrder is the order in which, without --owner-chat, the
// messengers are tried for the owner chat.
var ownerChatOrder = []string{ChannelTelegram, ChannelDiscord, ChannelEmail}

// OwnerChat returns the chat of --owner-chat, or else the direct chat of an
// owner on the first running messenger, in ownerChatOrder.
func (r *Router) OwnerChat(ctx context.Context) (approvals.Chat, bool) {
	if v := r.ownerChat(); v != "" {
		chName, who, _ := strings.Cut(v, ":")
		c, err := r.directChat(ctx, chName, who)
		if err != nil {
			slog.Warn("the owner chat is unavailable", "owner-chat", v, "error", err)
			return approvals.Chat{}, false
		}
		return c, true
	}
	for _, chName := range ownerChatOrder {
		if c, err := r.directChat(ctx, chName, ""); err == nil {
			return c, true
		}
	}
	return approvals.Chat{}, false
}

// directChat returns the direct chat on a running channel of an owner: the
// owner pairing who, or else yours, or else any owner pairing's if who is
// empty or "owner". Only owners can answer the questions sent there.
func (r *Router) directChat(ctx context.Context, chName, who string) (approvals.Chat, error) {
	if r.channel(chName) == nil {
		return approvals.Chat{}, fmt.Errorf("channel %s is not running", chName)
	}
	anyOwner := who == "" || who == auth.OwnerName || who == auth.OwnerID || who == auth.LocalPairing
	if anyOwner {
		conv, err := manage.DirectConv(ctx, r.store, chName, auth.OwnerID)
		if err == nil {
			return approvals.Chat{Channel: chName, Conv: channel.ConvKey(conv)}, nil
		}
		c, ok, ownerErr := r.ownerPairingChat(ctx, chName)
		if ownerErr != nil || ok {
			return c, ownerErr
		}
		return approvals.Chat{}, err
	}
	links, err := r.store.LinksOf(ctx, who)
	if err != nil {
		return approvals.Chat{}, err
	}
	if len(links) > 0 {
		owner, err := r.isOwnerPairing(ctx, who)
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
func (r *Router) ownerPairingChat(ctx context.Context, chName string) (approvals.Chat, bool, error) {
	links, err := r.store.Links(ctx)
	if err != nil {
		return approvals.Chat{}, false, err
	}
	for _, name := range slices.Sorted(maps.Keys(links)) {
		for _, l := range links[name] {
			if l.Channel != chName || l.Conv == "" {
				continue
			}
			owner, err := r.isOwnerPairing(ctx, name)
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
func (r *Router) isOwnerPairing(ctx context.Context, name string) (bool, error) {
	if name == auth.LocalPairing {
		return true, nil
	}
	p, err := r.store.Pairing(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return err == nil && p.Role == auth.RoleOwner, err
}

// Unattended reports whether a turn was posted to the context with no
// conversation, so that nobody watches its tasks.
func (r *Router) Unattended(contextID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.unattended[contextID]
}

func (r *Router) setUnattended(contextID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.unattended == nil {
		r.unattended = map[string]bool{}
	}
	r.unattended[contextID] = true
}

// remember records a message which shows a question, so that it can be
// updated once answered, and makes the question the one a typed reply in
// the conversation answers.
func (r *Router) remember(ctx context.Context, questionID, ch string, conv channel.ConvKey, ref channel.Ref, text string) {
	if err := r.store.AddQuestionMessage(ctx, store.QuestionMessage{QuestionID: questionID, Channel: ch, Conv: string(ref.Conv), MessageID: ref.ID, Text: text}); err != nil {
		slog.Warn("recording a question message", "error", err)
	}
	if err := r.store.SetPendingQuestion(ctx, ch, string(conv), questionID); err != nil {
		slog.Warn("recording pending question", "error", err)
	}
}

// injectTurn posts text into a context as a new turn, rendered in the
// context's conversation if it has one. While the context's latest task
// waits for input, the turn is held back: text sent to that task would
// interrupt it as if it were the answer.
func (r *Router) injectTurn(ctx context.Context, contextID string, p auth.Principal, text string) error {
	chName, conv, err := r.store.ConvOf(ctx, contextID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	ch := r.channel(chName)
	go func() {
		for {
			msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))
			msg.ContextID = contextID
			if ch == nil {
				if r.waitingForInput(ctx, contextID) {
					if !r.waitInput(ctx) {
						return
					}
					continue
				}
				r.setUnattended(contextID)
				if _, err := r.handler.SendMessage(auth.InProcess(ctx, p), &a2a.SendMessageRequest{Message: msg, Config: &a2a.SendMessageConfig{ReturnImmediately: true}}); err != nil {
					slog.Error("posting a turn", "context", contextID, "error", err)
				}
				return
			}
			unlock := r.lockConv(chName, channel.ConvKey(conv))
			if r.waitingForInput(ctx, contextID) {
				unlock()
				if !r.waitInput(ctx) {
					return
				}
				continue
			}
			r.stream(ctx, ch, channel.ConvKey(conv), channel.Ref{}, p, &a2a.SendMessageRequest{Message: msg})
			unlock()
			return
		}
	}()
	return nil
}

// injectRetry is how often a held-back turn checks whether it can be posted.
var injectRetry = 2 * time.Second

func (r *Router) waitingForInput(ctx context.Context, contextID string) bool {
	last, err := r.tasks.LatestInContext(ctx, contextID)
	return err == nil && last.Status.State == a2a.TaskStateInputRequired
}

// waitInput waits before checking a held-back turn again, and reports false
// when ctx ended.
func (r *Router) waitInput(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(injectRetry):
		return true
	}
}
