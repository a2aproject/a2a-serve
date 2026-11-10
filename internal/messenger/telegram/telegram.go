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

// Package telegram is the Telegram messenger of an agent server: a bot reached
// with long polling, so no public URL is needed.
package telegram

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/a2aproject/a2a-serve/internal/messenger"
)

// maxText is Telegram's message limit, minus room for MarkdownV2 escapes.
const maxText = 3500

// maxFile is the largest file a bot can download.
const maxFile = 20 << 20

// defaultServerURL is the Bot API URL of new messengers; empty for Telegram's.
// Only e2e builds set it.
var defaultServerURL string

// Messenger is a Telegram bot.
type Messenger struct {
	token string
	// commands are the bot's command menu.
	commands []messenger.Command
	// ServerURL overrides the Bot API URL, for tests.
	ServerURL       string
	downloadTimeout time.Duration

	connMu sync.Mutex
	bot    *bot.Bot
	me     *models.User

	mu     sync.Mutex
	in     chan<- messenger.Inbound
	report func(error)
	runCtx context.Context
}

var (
	_ messenger.Messenger = (*Messenger)(nil)
	_ messenger.Describer = (*Messenger)(nil)
	_ messenger.Validator = (*Messenger)(nil)
)

// New returns a messenger for the bot with token, whose command menu lists
// commands.
func New(token string, commands []messenger.Command) *Messenger {
	return &Messenger{token: token, commands: commands, ServerURL: defaultServerURL, downloadTimeout: 2 * time.Minute}
}

// Name implements [messenger.Messenger].
func (c *Messenger) Name() string { return "telegram" }

// Caps implements [messenger.Messenger].
func (c *Messenger) Caps() messenger.Caps {
	return messenger.Caps{Edit: true, Buttons: true, Threads: true, Typing: true, MaxText: maxText, Markdown: true, Stream: true}
}

// connect creates the bot client once it succeeds; failures are retried on
// the next call, so that a brief outage doesn't break the messenger for good.
func (c *Messenger) connect(ctx context.Context) error {
	c.connMu.Lock()
	defer c.connMu.Unlock()
	if c.bot != nil {
		return nil
	}
	opts := []bot.Option{
		bot.WithDefaultHandler(c.handle),
		// Handlers run in a goroutine per update otherwise, so that messages
		// of a conversation could reach the router out of order.
		bot.WithNotAsyncHandlers(),
		// The bot confirms the updates it fetched with its next poll. Without
		// a buffer, it polls again only once the handler took all but the
		// last of them, so that a stop leaves the rest to the next run.
		bot.WithUpdatesChannelCap(0),
		bot.WithAllowedUpdates(bot.AllowedUpdates{"message", "callback_query"}),
		bot.WithHTTPClient(50*time.Second, &pollClient{client: &http.Client{Timeout: 70 * time.Second}, polled: func() { c.reportState(nil) }}),
		bot.WithErrorsHandler(c.pollFailed),
		bot.WithSkipGetMe(),
	}
	if c.ServerURL != "" {
		opts = append(opts, bot.WithServerURL(c.ServerURL))
	}
	b, err := bot.New(c.token, opts...)
	if err != nil {
		return fmt.Errorf("connecting to the Telegram bot: %w", err)
	}
	me, err := b.GetMe(ctx)
	if errors.Is(err, bot.ErrorNotFound) || errors.Is(err, bot.ErrorUnauthorized) {
		return errors.New("invalid Telegram bot token")
	}
	if err != nil {
		return fmt.Errorf("telegram getMe: %w", err)
	}
	c.bot, c.me = b, me
	return nil
}

// Validate implements [messenger.Validator]: it checks the bot token.
func (c *Messenger) Validate(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return c.connect(ctx)
}

// Describe implements [messenger.Describer].
func (c *Messenger) Describe() string {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := c.connect(ctx); err != nil {
		return "(" + err.Error() + ")"
	}
	return "@" + c.me.Username
}

// LinkHint implements [messenger.Describer]: a deep link which sends /start <code>.
func (c *Messenger) LinkHint(code string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := c.connect(ctx); err != nil {
		return "send the bot: /start " + code
	}
	return "https://t.me/" + c.me.Username + "?start=" + code
}

// Run implements [messenger.Messenger]. Telegram keeps pending updates for a
// while, so a server that was briefly offline catches up on restart. It
// reports each failed poll, and the next one that succeeds.
func (c *Messenger) Run(ctx context.Context, in chan<- messenger.Inbound, report func(error)) error {
	if err := c.connect(ctx); err != nil {
		return err
	}
	c.mu.Lock()
	c.in, c.report, c.runCtx = in, report, ctx
	c.mu.Unlock()
	menu := make([]models.BotCommand, len(c.commands))
	for i, cmd := range c.commands {
		menu[i] = models.BotCommand{Command: cmd.Name, Description: cmd.Description}
	}
	if _, err := c.bot.SetMyCommands(ctx, &bot.SetMyCommandsParams{Commands: menu}); err != nil {
		slog.Warn("telegram: setting commands", "error", err)
	}
	c.bot.Start(ctx)
	return nil
}

func (c *Messenger) reportState(err error) {
	c.mu.Lock()
	report := c.report
	c.mu.Unlock()
	if report != nil {
		report(err)
	}
}

// pollFailed receives the errors of the bot, which are those of polling for
// updates. The bot keeps polling with backoff.
func (c *Messenger) pollFailed(err error) {
	c.mu.Lock()
	ctx := c.runCtx
	c.mu.Unlock()
	// Updates fetched but not handled when Run stops are fetched again
	// on the next run, so the bot's warning about them is moot.
	if ctx != nil && ctx.Err() != nil {
		return
	}
	slog.Warn("telegram: polling for updates", "error", err)
	c.reportState(fmt.Errorf("telegram: %w", err))
}

// pollClient tells when a poll for updates succeeds, which the bot doesn't.
// A long poll answers when an update arrives or it times out, so an idle
// bot shows that it recovered after up to the poll timeout.
type pollClient struct {
	client *http.Client
	polled func()
}

func (p *pollClient) Do(req *http.Request) (*http.Response, error) {
	resp, err := p.client.Do(req)
	if err == nil && resp.StatusCode == http.StatusOK && strings.HasSuffix(req.URL.Path, "/getUpdates") {
		p.polled()
	}
	return resp, err
}

func (c *Messenger) deliver(ctx context.Context, m messenger.Inbound) {
	c.mu.Lock()
	in := c.in
	c.mu.Unlock()
	select {
	case in <- m:
	case <-ctx.Done():
	}
}

func (c *Messenger) handle(ctx context.Context, b *bot.Bot, u *models.Update) {
	switch {
	case u.CallbackQuery != nil:
		c.handleCallback(ctx, b, u.CallbackQuery)
	case u.Message != nil:
		c.handleMessage(ctx, b, u.Message)
	}
}

func convKey(chatID int64, thread int) messenger.ConvKey {
	if thread != 0 {
		return messenger.ConvKey(strconv.FormatInt(chatID, 10) + ":" + strconv.Itoa(thread))
	}
	return messenger.ConvKey(strconv.FormatInt(chatID, 10))
}

func parseConv(conv messenger.ConvKey) (int64, int, error) {
	chat, thread, _ := strings.Cut(string(conv), ":")
	id, err := strconv.ParseInt(chat, 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid telegram conversation %q", conv)
	}
	t := 0
	if thread != "" {
		if t, err = strconv.Atoi(thread); err != nil {
			return 0, 0, fmt.Errorf("invalid telegram conversation %q", conv)
		}
	}
	return id, t, nil
}

func displayName(u *models.User) string {
	if u.Username != "" {
		return "@" + u.Username
	}
	return strings.TrimSpace(u.FirstName + " " + u.LastName)
}

func (c *Messenger) handleMessage(ctx context.Context, b *bot.Bot, msg *models.Message) {
	if msg.From == nil || msg.From.IsBot {
		return
	}
	text := msg.Text
	if text == "" {
		text = msg.Caption
	}
	direct := msg.Chat.Type == "private"
	if !direct {
		replyToBot := msg.ReplyToMessage != nil && msg.ReplyToMessage.From != nil && msg.ReplyToMessage.From.ID == c.me.ID
		var ok bool
		if text, ok = addressed(text, c.me.Username, replyToBot); !ok {
			return
		}
	}
	thread := 0
	if msg.IsTopicMessage {
		thread = msg.MessageThreadID
	}
	conv := convKey(msg.Chat.ID, thread)
	m := messenger.Inbound{
		Messenger: c.Name(),
		MessageID: fmt.Sprintf("%d:%d", msg.Chat.ID, msg.ID),
		Identity:  "telegram:" + strconv.FormatInt(msg.From.ID, 10),
		Name:      displayName(msg.From),
		Conv:      conv,
		Direct:    direct,
		Text:      text,
		Authentic: true,
	}
	if msg.ReplyToMessage != nil {
		m.ReplyTo = messenger.Ref{Conv: conv, ID: strconv.Itoa(msg.ReplyToMessage.ID)}
	}
	type attachment struct{ id, name, mediaType string }
	var files []attachment
	if msg.Document != nil {
		files = append(files, attachment{msg.Document.FileID, msg.Document.FileName, msg.Document.MimeType})
	}
	if n := len(msg.Photo); n > 0 {
		files = append(files, attachment{msg.Photo[n-1].FileID, "photo.jpg", "image/jpeg"})
	}
	if v := msg.Voice; v != nil {
		files = append(files, attachment{v.FileID, "voice.ogg", cmp.Or(v.MimeType, "audio/ogg")})
	}
	if a := msg.Audio; a != nil {
		files = append(files, attachment{a.FileID, cmp.Or(a.FileName, "audio"), cmp.Or(a.MimeType, "audio/mpeg")})
	}
	if len(files) > 0 {
		m.Prepare = func(ctx context.Context, in *messenger.Inbound) error {
			var errs []error
			for _, a := range files {
				f, err := c.download(ctx, b, a.id, a.name, a.mediaType)
				if err != nil {
					errs = append(errs, &messenger.AttachmentError{Name: a.name, Err: err})
					continue
				}
				in.Files = append(in.Files, f)
			}
			return errors.Join(errs...)
		}
	}
	c.deliver(ctx, m)
}

// addressed returns the text of a group message meant for the bot, so that
// it doesn't treat every message of a linked user as a task: a command
// without a bot suffix or with its own, a mention or a reply to the bot.
func addressed(text, username string, replyToBot bool) (string, bool) {
	if cmd, ok := strings.CutPrefix(text, "/"); ok {
		name, args, _ := strings.Cut(cmd, " ")
		name, target, hasTarget := strings.Cut(name, "@")
		if hasTarget && !strings.EqualFold(target, username) {
			return "", false
		}
		return strings.TrimSpace("/" + name + " " + args), true
	}
	mention := "@" + username
	switch {
	case strings.Contains(text, mention):
		return strings.TrimSpace(strings.ReplaceAll(text, mention, "")), true
	case replyToBot:
		return text, true
	default:
		return "", false
	}
}

func (c *Messenger) download(ctx context.Context, b *bot.Bot, fileID, name, mediaType string) (messenger.File, error) {
	f, err := b.GetFile(ctx, &bot.GetFileParams{FileID: fileID})
	if err != nil {
		return messenger.File{}, err
	}
	if f.FileSize > maxFile {
		return messenger.File{}, &messenger.TooLargeError{Limit: maxFile}
	}
	ctx, cancel := context.WithTimeout(ctx, c.downloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.FileDownloadLink(f), nil)
	if err != nil {
		return messenger.File{}, c.redact(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return messenger.File{}, c.redact(err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			slog.Debug("telegram: closing download", "error", err)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return messenger.File{}, fmt.Errorf("the server answered %s", resp.Status)
	}
	data, err := messenger.ReadLimited(resp.Body, maxFile)
	if err != nil {
		return messenger.File{}, c.redact(err)
	}
	return messenger.File{Name: name, MediaType: mediaType, Data: data}, nil
}

// redact hides the bot token in the URL of a failed request, which is part of
// the file download link.
func (c *Messenger) redact(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		urlErr.URL = strings.ReplaceAll(urlErr.URL, c.token, "***")
	}
	return err
}

func (c *Messenger) handleCallback(ctx context.Context, b *bot.Bot, q *models.CallbackQuery) {
	if _, err := b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: q.ID}); err != nil {
		slog.Debug("telegram: answering callback", "error", err)
	}
	msg := q.Message.Message
	if msg == nil {
		return
	}
	action, err := messenger.ParseActionData(q.Data)
	if err != nil {
		return
	}
	thread := 0
	if msg.IsTopicMessage {
		thread = msg.MessageThreadID
	}
	conv := convKey(msg.Chat.ID, thread)
	c.deliver(ctx, messenger.Inbound{
		Messenger: c.Name(),
		MessageID: "cb:" + q.ID,
		Identity:  "telegram:" + strconv.FormatInt(q.From.ID, 10),
		Name:      displayName(&q.From),
		Conv:      conv,
		Direct:    msg.Chat.Type == "private",
		Action:    action,
		Authentic: true,
		ReplyTo:   messenger.Ref{Conv: conv, ID: strconv.Itoa(msg.ID)},
	})
}

func keyboard(q *messenger.Question) models.ReplyMarkup {
	if q == nil {
		return &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{}}
	}
	// Buttons with short labels share the first row, and each long label
	// gets a row of its own below them, so that Telegram doesn't cut it.
	var short []models.InlineKeyboardButton
	var long [][]models.InlineKeyboardButton
	for i, label := range q.Options {
		b := models.InlineKeyboardButton{Text: label, CallbackData: messenger.ActionData(q.ID, i)}
		if utf8.RuneCountInString(label) > maxSharedLabel {
			long = append(long, []models.InlineKeyboardButton{b})
			continue
		}
		short = append(short, b)
	}
	rows := long
	if len(short) > 0 {
		rows = append([][]models.InlineKeyboardButton{short}, long...)
	}
	return &models.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// maxSharedLabel is the longest label of a button that shares a row.
const maxSharedLabel = 16

// isFormatError reports whether Telegram rejected the MarkdownV2 text: its
// entities don't parse or the escaped text is too long. The same text sent
// plain fits the limit.
func isFormatError(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "can't parse entities") || strings.Contains(err.Error(), "message is too long"))
}

// apiError wraps a Bot API error, as a [messenger.RateLimitError] if Telegram
// asks to wait.
func apiError(op string, err error) error {
	var limit *bot.TooManyRequestsError
	if errors.As(err, &limit) {
		return &messenger.RateLimitError{RetryAfter: time.Duration(limit.RetryAfter) * time.Second, Err: fmt.Errorf("telegram %s: %w", op, err)}
	}
	return fmt.Errorf("telegram %s: %w", op, err)
}

func isNotModified(err error) bool {
	return err != nil && strings.Contains(err.Error(), "message is not modified")
}

// Send implements [messenger.Messenger].
func (c *Messenger) Send(ctx context.Context, m messenger.Outbound) (messenger.Ref, error) {
	if err := c.connect(ctx); err != nil {
		return messenger.Ref{}, err
	}
	chat, thread, err := parseConv(m.Conv)
	if err != nil {
		return messenger.Ref{}, err
	}
	var ref messenger.Ref
	for _, f := range m.Files {
		msg, err := c.sendFile(ctx, chat, thread, f)
		if err != nil {
			return ref, err
		}
		ref = messenger.Ref{Conv: m.Conv, ID: strconv.Itoa(msg.ID)}
	}
	pieces := messenger.Split(m.Text, maxText)
	for i, piece := range pieces {
		if piece == "" {
			continue
		}
		params := &bot.SendMessageParams{
			ChatID: chat, MessageThreadID: thread, Text: piece,
			LinkPreviewOptions: &models.LinkPreviewOptions{IsDisabled: bot.True()},
		}
		if !m.Literal {
			params.Text, params.ParseMode = ToMarkdownV2(piece), models.ParseModeMarkdown
		}
		if i == len(pieces)-1 && m.Question != nil {
			params.ReplyMarkup = keyboard(m.Question)
		}
		msg, err := c.bot.SendMessage(ctx, params)
		if isFormatError(err) {
			params.Text, params.ParseMode = piece, ""
			msg, err = c.bot.SendMessage(ctx, params)
		}
		if err != nil {
			return ref, apiError("sendMessage", err)
		}
		ref = messenger.Ref{Conv: m.Conv, ID: strconv.Itoa(msg.ID)}
	}
	return ref, nil
}

// sendFile sends f, as a voice note that plays in the chat if it is Ogg
// Opus or MP3 audio.
func (c *Messenger) sendFile(ctx context.Context, chat int64, thread int, f messenger.File) (*models.Message, error) {
	if f.MediaType == "audio/ogg" || f.MediaType == "audio/mpeg" {
		msg, err := c.bot.SendVoice(ctx, &bot.SendVoiceParams{
			ChatID: chat, MessageThreadID: thread,
			Voice: &models.InputFileUpload{Filename: f.Name, Data: bytes.NewReader(f.Data)},
		})
		var limit *bot.TooManyRequestsError
		if err == nil || errors.As(err, &limit) {
			return msg, wrapAPIError("sendVoice", err)
		}
		// People can refuse voice notes in their privacy settings.
		slog.Debug("telegram: sending a voice note; sending it as a file", "error", err)
	}
	msg, err := c.bot.SendDocument(ctx, &bot.SendDocumentParams{
		ChatID: chat, MessageThreadID: thread,
		Document: &models.InputFileUpload{Filename: f.Name, Data: bytes.NewReader(f.Data)},
	})
	return msg, wrapAPIError("sendDocument", err)
}

// wrapAPIError is [apiError], or nil for no error.
func wrapAPIError(op string, err error) error {
	if err == nil {
		return nil
	}
	return apiError(op, err)
}

// Edit implements [messenger.Messenger].
func (c *Messenger) Edit(ctx context.Context, ref messenger.Ref, m messenger.Outbound) error {
	if err := c.connect(ctx); err != nil {
		return err
	}
	chat, _, err := parseConv(ref.Conv)
	if err != nil {
		return err
	}
	id, err := strconv.Atoi(ref.ID)
	if err != nil {
		return fmt.Errorf("invalid telegram message id %q", ref.ID)
	}
	text := messenger.Truncate(m.Text, maxText)
	params := &bot.EditMessageTextParams{
		ChatID: chat, MessageID: id, Text: text,
		ReplyMarkup:        keyboard(m.Question),
		LinkPreviewOptions: &models.LinkPreviewOptions{IsDisabled: bot.True()},
	}
	if !m.Literal {
		params.Text, params.ParseMode = ToMarkdownV2(text), models.ParseModeMarkdown
	}
	_, err = c.bot.EditMessageText(ctx, params)
	if isFormatError(err) {
		params.Text, params.ParseMode = text, ""
		_, err = c.bot.EditMessageText(ctx, params)
	}
	if err != nil && !isNotModified(err) {
		return apiError("editMessageText", err)
	}
	return nil
}
