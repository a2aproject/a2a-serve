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

// Package discord is the Discord messenger of an agent server: a bot on the
// outbound gateway WebSocket, with application commands, buttons and a thread
// per conversation in guild channels.
package discord

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/a2aproject/a2a-serve/internal/messenger"
)

const (
	maxText = 2000
	maxFile = 20 << 20
	// catchUpLimit is Discord's page size for message history.
	catchUpLimit = 50
	// maxCatchUpMessages bounds the messages read per channel when catching
	// up after an outage; the newest are read.
	maxCatchUpMessages = 1000
	// maxCatchUpRetry caps the delay between attempts to catch up.
	maxCatchUpRetry = time.Minute
	// convsKey lists the conversations to catch up on after a reconnect.
	convsKey = "discord:convs"
	// maxConvs bounds the conversations kept; the least recently active
	// ones are forgotten.
	maxConvs = 200
)

// Messenger is a Discord bot.
type Messenger struct {
	token string
	kv    messenger.KV

	connMu sync.Mutex
	s      *discordgo.Session
	me     *discordgo.User

	// queue hands each conversation's events to the router in the order the
	// gateway received them.
	queue *orderedQueue
	// threadRetry is the first delay before starting a thread is tried again.
	threadRetry time.Duration
	// catchUpRetry is the first delay before a failed catch-up is tried
	// again; it doubles up to maxCatchUpRetry.
	catchUpRetry time.Duration

	mu     sync.Mutex
	in     chan<- messenger.Inbound
	ctx    context.Context
	report func(error)
	// convs are the conversations to catch up on, least recently active
	// first.
	convs []string
	// catching maps a conversation whose catch-up hasn't succeeded yet to
	// the ID of that catch-up. Live messages of such a conversation don't
	// move its cursor, which would skip the messages it hasn't caught up on.
	catching map[string]uint64
	// catchUps counts the catch-ups started.
	catchUps uint64

	// cursorMu serializes updates of the last message seen of conversations.
	cursorMu sync.Mutex
}

var (
	_ messenger.Messenger = (*Messenger)(nil)
	_ messenger.Describer = (*Messenger)(nil)
	_ messenger.Validator = (*Messenger)(nil)
)

// noMentions keeps agent output from pinging users, roles or @everyone.
var noMentions = &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}}

// New returns a messenger for the bot with token. kv keeps the last message
// seen per conversation.
func New(token string, kv messenger.KV) *Messenger {
	return newMessenger(token, kv, time.Second)
}

func newMessenger(token string, kv messenger.KV, catchUpRetry time.Duration) *Messenger {
	return &Messenger{
		token: token, kv: kv, queue: newOrderedQueue(),
		threadRetry: 500 * time.Millisecond, catchUpRetry: catchUpRetry,
		catching: map[string]uint64{},
	}
}

// Name implements [messenger.Messenger].
func (c *Messenger) Name() string { return "discord" }

// Caps implements [messenger.Messenger].
func (c *Messenger) Caps() messenger.Caps {
	return messenger.Caps{Edit: true, Buttons: true, Threads: true, Typing: true, MaxText: maxText, Markdown: true, Stream: true}
}

// connect creates the session once it succeeds; failures are retried on the
// next call, so that a brief outage doesn't break the messenger for good.
func (c *Messenger) connect() error {
	c.connMu.Lock()
	defer c.connMu.Unlock()
	if c.s != nil {
		return nil
	}
	s, err := discordgo.New("Bot " + c.token)
	if err != nil {
		return fmt.Errorf("discord: %w", err)
	}
	me, err := s.User("@me")
	if err != nil {
		return fmt.Errorf("discord: checking the bot token: %w", err)
	}
	s.Identify.Intents = discordgo.IntentsGuildMessages | discordgo.IntentsDirectMessages | discordgo.IntentMessageContent
	// Handlers run one at a time in the order of the events, so they must
	// not block: they queue the work, which runs in order per conversation.
	s.SyncEvents = true
	s.AddHandler(c.onReady)
	s.AddHandler(c.onConnect)
	s.AddHandler(c.onDisconnect)
	s.AddHandler(c.onMessage)
	s.AddHandler(c.onInteraction)
	c.s, c.me = s, me
	return nil
}

// Validate implements [messenger.Validator]: it checks the bot token.
func (c *Messenger) Validate(context.Context) error { return c.connect() }

// Describe implements [messenger.Describer].
func (c *Messenger) Describe() string {
	if err := c.connect(); err != nil {
		return "(" + err.Error() + ")"
	}
	return "@" + c.me.Username
}

// LinkHint implements [messenger.Describer].
func (c *Messenger) LinkHint(code string) string {
	return fmt.Sprintf("send %s a direct message: /join %s", c.Describe(), code)
}

// Run implements [messenger.Messenger]. The session reconnects by itself, and
// reports when it disconnects and connects.
func (c *Messenger) Run(ctx context.Context, in chan<- messenger.Inbound, report func(error)) error {
	if err := c.connect(); err != nil {
		return err
	}
	v, ok, err := c.kv.Get(ctx, convsKey)
	if err != nil {
		return fmt.Errorf("discord: loading conversations: %w", err)
	}
	c.mu.Lock()
	c.in, c.ctx, c.report = in, ctx, report
	if ok && v != "" {
		c.convs = strings.Split(v, ",")
	}
	c.mu.Unlock()
	if err := c.s.Open(); err != nil {
		return fmt.Errorf("discord gateway: %w", err)
	}
	<-ctx.Done()
	return c.s.Close()
}

// deliver hands m to the router and reports whether it did before the
// messenger stopped.
func (c *Messenger) deliver(m messenger.Inbound) bool {
	c.mu.Lock()
	in, ctx := c.in, c.ctx
	c.mu.Unlock()
	select {
	case in <- m:
		return true
	case <-ctx.Done():
		return false
	}
}

func (c *Messenger) reportState(err error) {
	c.mu.Lock()
	report := c.report
	c.mu.Unlock()
	if report != nil {
		report(err)
	}
}

func (c *Messenger) onConnect(*discordgo.Session, *discordgo.Connect) { c.reportState(nil) }

func (c *Messenger) onDisconnect(*discordgo.Session, *discordgo.Disconnect) {
	c.reportState(errors.New("discord: disconnected from the gateway; reconnecting"))
}

func (c *Messenger) onReady(s *discordgo.Session, r *discordgo.Ready) { c.ready(s, s.State, r.User.ID) }

// readyAPI is the part of the Discord REST API that a new gateway session
// uses.
type readyAPI interface {
	restAPI
	ApplicationCommandBulkOverwrite(appID string, guildID string, commands []*discordgo.ApplicationCommand, options ...discordgo.RequestOption) ([]*discordgo.ApplicationCommand, error)
}

// ready catches up on messages missed while disconnected, since the gateway
// doesn't replay them, and registers the application commands. The catch-up
// starts from the cursors as they are now and is queued before the live
// messages of its conversation. A failed catch-up is tried again with a
// growing delay, and until one succeeds the live messages of its conversation
// don't move the cursor.
func (c *Messenger) ready(api readyAPI, st *discordgo.State, appID string) {
	c.mu.Lock()
	convs, ctx := slices.Clone(c.convs), c.ctx
	c.mu.Unlock()
	for _, conv := range convs {
		last, ok, err := c.kv.Get(ctx, lastKey(conv))
		if err != nil {
			slog.Warn("discord: loading the last message seen", "messenger", conv, "error", err)
			continue
		}
		if !ok || last == "" {
			continue
		}
		id := c.beginCatchUp(conv)
		c.queue.do(conv, func() { c.runCatchUp(api, st, conv, last, id, c.catchUpRetry) })
	}
	go registerCommands(api, appID)
}

func registerCommands(api readyAPI, appID string) {
	cmds := []*discordgo.ApplicationCommand{
		{Name: "join", Description: "Join with a pairing code from the agent's owner", Options: []*discordgo.ApplicationCommandOption{
			{Type: discordgo.ApplicationCommandOptionString, Name: "code", Description: "The pairing code", Required: true},
		}},
		{Name: "new", Description: "Start a new conversation"},
		{Name: "stop", Description: "Stop the current task"},
		{Name: "allowed", Description: "Show and remove what runs without asking for you"},
	}
	if _, err := api.ApplicationCommandBulkOverwrite(appID, "", cmds); err != nil {
		slog.Warn("discord: registering commands", "error", err)
	}
}

func lastKey(conv string) string { return "discord:last:" + conv }

// restAPI is the part of the Discord REST API that catching up uses.
type restAPI interface {
	threadStarter
	ChannelMessages(channelID string, limit int, beforeID, afterID, aroundID string, options ...discordgo.RequestOption) ([]*discordgo.Message, error)
	Channel(channelID string, options ...discordgo.RequestOption) (*discordgo.Channel, error)
}

type threadStarter interface {
	MessageThreadStartComplex(channelID, messageID string, data *discordgo.ThreadStart, options ...discordgo.RequestOption) (*discordgo.Channel, error)
}

// beginCatchUp marks conv as catching up and returns the ID of the catch-up,
// which a newer one of conv supersedes.
func (c *Messenger) beginCatchUp(conv string) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.catchUps++
	c.catching[conv] = c.catchUps
	return c.catchUps
}

func (c *Messenger) catchUpCurrent(conv string, id uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.catching[conv] == id
}

func (c *Messenger) endCatchUp(conv string, id uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.catching[conv] == id {
		delete(c.catching, conv)
	}
}

func (c *Messenger) isCatchingUp(conv string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.catching[conv]
	return ok
}

// runCatchUp catches up on conv from last. If that fails for a reason that
// may pass, it queues another attempt after delay, from the cursor as it is
// then.
func (c *Messenger) runCatchUp(api restAPI, st *discordgo.State, conv, last string, id uint64, delay time.Duration) {
	err := c.catchUp(api, st, conv, last)
	if err == nil {
		c.endCatchUp(conv, id)
		return
	}
	c.mu.Lock()
	ctx := c.ctx
	c.mu.Unlock()
	if ctx.Err() != nil || permanent(err) {
		slog.Warn("discord: catching up", "messenger", conv, "error", err)
		c.endCatchUp(conv, id)
		return
	}
	slog.Warn("discord: catching up failed; trying again", "messenger", conv, "error", err, "delay", delay)
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		c.queue.do(conv, func() {
			if !c.catchUpCurrent(conv, id) {
				return
			}
			next, ok, err := c.kv.Get(ctx, lastKey(conv))
			if err != nil || !ok || next == "" {
				slog.Warn("discord: loading the last message seen", "messenger", conv, "error", err)
				c.endCatchUp(conv, id)
				return
			}
			c.runCatchUp(api, st, conv, next, id, min(delay*2, maxCatchUpRetry))
		})
	}()
}

// permanent reports whether err is Discord refusing the request in a way
// that trying again doesn't change, such as a deleted channel. Rate limits,
// server errors and network errors may pass.
func permanent(err error) bool {
	var restErr *discordgo.RESTError
	if !errors.As(err, &restErr) || restErr.Response == nil {
		return false
	}
	code := restErr.Response.StatusCode
	return code >= 400 && code < 500 && code != http.StatusTooManyRequests
}

// catchUp delivers the messages of conv after last. It returns an error if it
// couldn't read the messages or tell what kind of channel conv is, and
// errStopped if the messenger stopped first.
func (c *Messenger) catchUp(s restAPI, st *discordgo.State, conv, last string) error {
	msgs, skipped, err := missed(s, conv, last)
	if err != nil {
		return err
	}
	if skipped > 0 {
		slog.Warn("discord: catching up on the newest missed messages only; older ones are skipped",
			"messenger", conv, "read", len(msgs), "skipped_at_least", skipped)
	}
	if len(msgs) == 0 {
		return nil
	}
	direct, err := isDirect(s, st, conv)
	if err != nil {
		return err
	}
	for _, m := range msgs {
		if !c.handleMessage(s, m, direct) {
			return errStopped
		}
		c.setCursor(conv, m.ID, newer(m.ID))
		c.remember(conv)
	}
	return nil
}

var errStopped = errors.New("discord: stopped")

// missed returns the newest messages of conv after last, up to
// maxCatchUpMessages, oldest first. If older ones are left unread, it also
// returns how many of them it saw. It reads backwards from the newest message,
// so on an error it returns none: the cursor must not pass messages that
// weren't read.
func missed(api restAPI, conv, last string) ([]*discordgo.Message, int, error) {
	var all []*discordgo.Message
	before := ""
	for len(all) < maxCatchUpMessages {
		limit := min(catchUpLimit, maxCatchUpMessages-len(all))
		page, err := api.ChannelMessages(conv, limit, before, "", "")
		if err != nil {
			return nil, 0, err
		}
		newer := newerThan(page, last)
		all = append(all, newer...)
		if len(newer) < len(page) || len(page) < limit {
			return sortedByID(all), 0, nil
		}
		before = oldestID(page)
	}
	probe, err := api.ChannelMessages(conv, catchUpLimit, before, "", "")
	if err != nil {
		slog.Warn("discord: checking for more missed messages", "messenger", conv, "error", err)
		return sortedByID(all), 0, nil
	}
	return sortedByID(all), len(newerThan(probe, last)), nil
}

func newerThan(page []*discordgo.Message, last string) []*discordgo.Message {
	return slices.DeleteFunc(slices.Clone(page), func(m *discordgo.Message) bool { return compareSnowflakes(m.ID, last) <= 0 })
}

func oldestID(page []*discordgo.Message) string {
	return slices.MinFunc(page, func(a, b *discordgo.Message) int { return compareSnowflakes(a.ID, b.ID) }).ID
}

func sortedByID(msgs []*discordgo.Message) []*discordgo.Message {
	slices.SortFunc(msgs, func(a, b *discordgo.Message) int { return compareSnowflakes(a.ID, b.ID) })
	return msgs
}

func compareSnowflakes(a, b string) int {
	if c := cmp.Compare(len(a), len(b)); c != 0 {
		return c
	}
	return strings.Compare(a, b)
}

// isDirect reports whether conv is a direct message channel. Messages read
// over REST have no guild_id and the state may not know the channel yet, so
// it asks Discord; a channel that can't be found counts as a guild channel,
// where the bot only answers when mentioned. Other errors may pass and are
// returned, since a direct message taken for a guild message is dropped.
func isDirect(api restAPI, st *discordgo.State, conv string) (bool, error) {
	ch, err := st.Channel(conv)
	if err != nil {
		if ch, err = api.Channel(conv); err != nil {
			if permanent(err) {
				slog.Warn("discord: looking up messenger", "messenger", conv, "error", err)
				return false, nil
			}
			return false, fmt.Errorf("discord: looking up channel: %w", err)
		}
	}
	return ch.Type == discordgo.ChannelTypeDM || ch.Type == discordgo.ChannelTypeGroupDM, nil
}

// trackInbound moves the catch-up cursor of a conversation to a live message
// the bot received, unless it is already past it or the conversation hasn't
// caught up on the messages before it.
func (c *Messenger) trackInbound(conv, msgID string) {
	if !c.isCatchingUp(conv) {
		c.setCursor(conv, msgID, newer(msgID))
	}
	c.remember(conv)
}

func newer(msgID string) func(old string, ok bool) bool {
	return func(old string, ok bool) bool { return !ok || compareSnowflakes(msgID, old) > 0 }
}

// trackSent starts the catch-up cursor of a conversation with a message the
// bot sent. It never moves a cursor: messages that arrived while the bot was
// disconnected are older than its replies and must still be caught up on.
func (c *Messenger) trackSent(conv, msgID string) {
	c.setCursor(conv, msgID, func(_ string, ok bool) bool { return !ok })
	c.remember(conv)
}

func (c *Messenger) setCursor(conv, msgID string, replace func(old string, ok bool) bool) {
	ctx := context.Background()
	c.cursorMu.Lock()
	defer c.cursorMu.Unlock()
	old, ok, err := c.kv.Get(ctx, lastKey(conv))
	if err != nil {
		slog.Warn("discord: loading the last message seen", "messenger", conv, "error", err)
		return
	}
	if !replace(old, ok && old != "") {
		return
	}
	if err := c.kv.Set(ctx, lastKey(conv), msgID); err != nil {
		slog.Warn("discord: recording last message", "error", err)
	}
}

// remember marks conv as the most recently active conversation, and
// forgets the least recently active ones beyond maxConvs, with their rows.
func (c *Messenger) remember(conv string) {
	ctx := context.Background()
	c.mu.Lock()
	defer c.mu.Unlock()
	if n := len(c.convs); n > 0 && c.convs[n-1] == conv {
		return
	}
	c.convs = append(slices.DeleteFunc(c.convs, func(x string) bool { return x == conv }), conv)
	var evicted []string
	if len(c.convs) > maxConvs {
		evicted = slices.Clone(c.convs[:len(c.convs)-maxConvs])
		c.convs = slices.Delete(c.convs, 0, len(evicted))
		for _, old := range evicted {
			delete(c.catching, old)
		}
	}
	if err := c.kv.Set(ctx, convsKey, strings.Join(c.convs, ",")); err != nil {
		slog.Warn("discord: recording conversations", "error", err)
		return
	}
	for _, old := range evicted {
		for _, key := range []string{lastKey(old), threadKey(old)} {
			if err := c.kv.Delete(ctx, key); err != nil {
				slog.Warn("discord: forgetting a conversation", "messenger", old, "error", err)
			}
		}
	}
}

func threadKey(id string) string { return "discord:thread:" + id }

func (c *Messenger) ownThread(id string) bool {
	v, ok, err := c.kv.Get(context.Background(), threadKey(id))
	if err != nil {
		slog.Warn("discord: looking up thread", "messenger", id, "error", err)
		return false
	}
	return ok && v == "1"
}

// onMessage queues the message, so that the gateway isn't held up while the
// router is busy.
func (c *Messenger) onMessage(s *discordgo.Session, m *discordgo.MessageCreate) {
	c.queue.do(m.ChannelID, func() { c.handleMessage(s, m.Message, m.GuildID == "") })
}

// handleMessage delivers m if it is meant for the bot, and moves the cursor
// of its conversation past it once delivered. It reports false if the
// messenger stopped first.
func (c *Messenger) handleMessage(s threadStarter, m *discordgo.Message, direct bool) bool {
	if m.Author == nil || m.Author.Bot || m.Author.ID == c.me.ID {
		return true
	}
	text := m.Content
	// In guild channels the bot answers when mentioned and moves the
	// conversation into a thread of its own, once the sender is allowed in.
	newThread := !direct && !c.ownThread(m.ChannelID)
	if newThread && !slices.ContainsFunc(m.Mentions, func(u *discordgo.User) bool { return u.ID == c.me.ID }) {
		return true
	}
	text = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(text, "<@"+c.me.ID+">", ""), "<@!"+c.me.ID+">", ""))
	in := messenger.Inbound{
		Messenger: c.Name(),
		MessageID: m.ID,
		Identity:  "discord:" + m.Author.ID,
		Name:      m.Author.Username,
		Conv:      messenger.ConvKey(m.ChannelID),
		Direct:    direct,
		Text:      text,
		Authentic: true,
	}
	if ref := m.MessageReference; ref != nil && ref.Type == discordgo.MessageReferenceTypeDefault {
		in.ReplyTo = messenger.Ref{Conv: in.Conv, ID: ref.MessageID}
	}
	if newThread || len(m.Attachments) > 0 {
		in.Prepare = func(ctx context.Context, in *messenger.Inbound) error {
			var errs []error
			if newThread {
				th, err := c.startThread(ctx, s, m)
				if err != nil {
					return &messenger.DroppedError{
						Reason: "⚠ I couldn't start a thread for this conversation, so I ignored your message. Check that I may create threads in this messenger, then try again.",
						Err:    err,
					}
				}
				in.Conv = messenger.ConvKey(th.ID)
				c.remember(th.ID)
				if err := c.kv.Set(ctx, threadKey(th.ID), "1"); err != nil {
					errs = append(errs, fmt.Errorf("discord: recording thread: %w", err))
				}
			}
			for _, a := range m.Attachments {
				f, err := download(ctx, a)
				if err != nil {
					errs = append(errs, &messenger.AttachmentError{Name: a.Filename, Err: err})
					continue
				}
				in.Files = append(in.Files, f)
			}
			return errors.Join(errs...)
		}
	}
	if !c.deliver(in) {
		return false
	}
	c.trackInbound(m.ChannelID, m.ID)
	return true
}

// startThread starts the thread of a conversation under m, trying again a
// few times: a conversation that fell back to its channel would be shared
// with everyone who writes there.
func (c *Messenger) startThread(ctx context.Context, s threadStarter, m *discordgo.Message) (*discordgo.Channel, error) {
	const attempts = 3
	delay := c.threadRetry
	var err error
	for attempt := range attempts {
		var th *discordgo.Channel
		th, err = s.MessageThreadStartComplex(m.ChannelID, m.ID, &discordgo.ThreadStart{
			Name: threadName(m.Content, c.me.ID), AutoArchiveDuration: 1440,
		}, discordgo.WithContext(ctx))
		if err == nil {
			return th, nil
		}
		if attempt == attempts-1 {
			break
		}
		select {
		case <-ctx.Done():
			return nil, errors.Join(err, ctx.Err())
		case <-time.After(delay):
		}
		delay *= 2
	}
	return nil, fmt.Errorf("discord: starting thread: %w", err)
}

func threadName(text, botID string) string {
	name := strings.TrimSpace(strings.ReplaceAll(text, "<@"+botID+">", ""))
	if name == "" {
		name = "conversation"
	}
	if r := []rune(name); len(r) > 80 {
		name = string(r[:80])
	}
	return name
}

func download(ctx context.Context, a *discordgo.MessageAttachment) (messenger.File, error) {
	if a.Size > maxFile {
		return messenger.File{}, fmt.Errorf("attachment %s is too large", a.Filename)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return messenger.File{}, redactQuery(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return messenger.File{}, redactQuery(err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			slog.Debug("discord: closing download", "error", err)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return messenger.File{}, fmt.Errorf("the server answered %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxFile))
	if err != nil {
		return messenger.File{}, err
	}
	return messenger.File{Name: a.Filename, MediaType: a.ContentType, Data: data}, nil
}

// redactQuery drops the query of the URL in err, where Discord puts the
// signature of an attachment link.
func redactQuery(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		urlErr.URL, _, _ = strings.Cut(urlErr.URL, "?")
	}
	return err
}

func (c *Messenger) onInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
	user := i.User
	if i.Member != nil && i.Member.User != nil {
		user = i.Member.User
	}
	if user == nil {
		return
	}
	in := messenger.Inbound{
		Messenger: c.Name(),
		MessageID: "int:" + i.ID,
		Identity:  "discord:" + user.ID,
		Name:      user.Username,
		Conv:      messenger.ConvKey(i.ChannelID),
		Direct:    i.GuildID == "",
		Authentic: true,
	}
	switch i.Type {
	case discordgo.InteractionApplicationCommand:
		data := i.ApplicationCommandData()
		in.Text = "/" + data.Name
		if data.Name == "join" && len(data.Options) > 0 {
			in.Text = "join " + data.Options[0].StringValue()
		}
		go respond(s, i, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{Content: "…", Flags: discordgo.MessageFlagsEphemeral},
		})
	case discordgo.InteractionMessageComponent:
		action, err := messenger.ParseActionData(i.MessageComponentData().CustomID)
		if err != nil {
			return
		}
		in.Action = action
		if i.Message != nil {
			in.ReplyTo = messenger.Ref{Conv: in.Conv, ID: i.Message.ID}
		}
		go respond(s, i, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseDeferredMessageUpdate})
	default:
		return
	}
	c.queue.do(i.ChannelID, func() { c.deliver(in) })
}

// respond acknowledges an interaction, which Discord fails unless it is
// acknowledged within 3 seconds. Handlers call it on a goroutine of its own,
// so that it doesn't wait for the events before it.
func respond(s *discordgo.Session, i *discordgo.InteractionCreate, r *discordgo.InteractionResponse) {
	if err := s.InteractionRespond(i.Interaction, r); err != nil {
		slog.Debug("discord: responding to interaction", "error", err)
	}
}

// maxButtonLabel is the longest label Discord takes for a button.
const maxButtonLabel = 80

func components(q *messenger.Question) []discordgo.MessageComponent {
	if q == nil {
		return []discordgo.MessageComponent{}
	}
	var rows []discordgo.MessageComponent
	var row []discordgo.MessageComponent
	for i, label := range q.Options {
		style := discordgo.SecondaryButton
		switch i {
		case 0:
			style = discordgo.PrimaryButton
		case len(q.Options) - 1:
			style = discordgo.DangerButton
		}
		if r := []rune(label); len(r) > maxButtonLabel {
			label = string(r[:maxButtonLabel-1]) + "…"
		}
		row = append(row, discordgo.Button{Label: label, Style: style, CustomID: messenger.ActionData(q.ID, i)})
		if len(row) == 5 {
			rows = append(rows, discordgo.ActionsRow{Components: row})
			row = nil
		}
	}
	if len(row) > 0 {
		rows = append(rows, discordgo.ActionsRow{Components: row})
	}
	return rows
}

// markdownChars are the characters that Discord formats or turns into links,
// spoilers and quotes.
const markdownChars = "\\*_~|`>#[]()<"

// escapeMarkdown makes Discord show text as written.
func escapeMarkdown(text string) string {
	var sb strings.Builder
	for _, r := range text {
		if strings.ContainsRune(markdownChars, r) {
			sb.WriteByte('\\')
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

// escapeWithin is escapeMarkdown, with text cut so that the result has at most
// limit bytes.
func escapeWithin(text string, limit int) string {
	escaped := escapeMarkdown(text)
	for len(escaped) > limit {
		text = messenger.Truncate(text, max(len(text)-(len(escaped)-limit), 0))
		escaped = escapeMarkdown(text)
	}
	return escaped
}

// messagePieces splits the text of m into messages.
func messagePieces(m messenger.Outbound) []string {
	if !m.Literal {
		return messenger.Split(m.Text, maxText)
	}
	// Escaping at most doubles the size.
	pieces := messenger.Split(m.Text, maxText/2)
	for i, p := range pieces {
		pieces[i] = escapeMarkdown(p)
	}
	return pieces
}

// Send implements [messenger.Messenger].
func (c *Messenger) Send(ctx context.Context, m messenger.Outbound) (messenger.Ref, error) {
	if err := c.connect(); err != nil {
		return messenger.Ref{}, err
	}
	var ref messenger.Ref
	pieces := messagePieces(m)
	for i, piece := range pieces {
		send := &discordgo.MessageSend{Content: piece, AllowedMentions: noMentions}
		last := i == len(pieces)-1
		if last && m.Question != nil {
			send.Components = components(m.Question)
		}
		if last {
			for _, f := range m.Files {
				send.Files = append(send.Files, &discordgo.File{Name: f.Name, ContentType: f.MediaType, Reader: bytes.NewReader(f.Data)})
			}
		}
		if send.Content == "" && len(send.Files) == 0 {
			continue
		}
		msg, err := c.s.ChannelMessageSendComplex(string(m.Conv), send, discordgo.WithContext(ctx))
		if err != nil {
			return ref, fmt.Errorf("discord send: %w", err)
		}
		ref = messenger.Ref{Conv: m.Conv, ID: msg.ID}
		c.trackSent(string(m.Conv), msg.ID)
	}
	return ref, nil
}

// Edit implements [messenger.Messenger].
func (c *Messenger) Edit(ctx context.Context, ref messenger.Ref, m messenger.Outbound) error {
	if err := c.connect(); err != nil {
		return err
	}
	text := messenger.Truncate(m.Text, maxText)
	if m.Literal {
		text = escapeWithin(m.Text, maxText)
	}
	comps := components(m.Question)
	_, err := c.s.ChannelMessageEditComplex(&discordgo.MessageEdit{
		ID: ref.ID, Channel: string(ref.Conv), Content: &text, Components: &comps, AllowedMentions: noMentions,
	}, discordgo.WithContext(ctx))
	if err != nil {
		return fmt.Errorf("discord edit: %w", err)
	}
	return nil
}
