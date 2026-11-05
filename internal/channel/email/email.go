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

// Package email is the email channel of an agent server: IMAP IDLE on a
// dedicated mailbox and SMTP for replies, so no MX record or public URL is
// needed. Only mail which passed DMARC, or DKIM of the sender's domain, at
// the trusted receiving server is read; other mail is dropped unanswered.
package email

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/smtp"
	"net/textproto"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset" // decodes non-UTF-8 mail
	"github.com/emersion/go-message/mail"
	mtextproto "github.com/emersion/go-message/textproto"
	"golang.org/x/oauth2"

	"github.com/a2aproject/a2a-serve/internal/backoff"
	"github.com/a2aproject/a2a-serve/internal/channel"
	"github.com/a2aproject/a2a-serve/internal/cryptoutil"
)

// Config configures the mailbox of the agent.
type Config struct {
	Address string
	// IMAP and SMTP are host:port. Ports 993 and 465 use implicit TLS, others STARTTLS.
	IMAP     string
	SMTP     string
	Username string
	Password string
	// OAuth, if set, signs in with XOAUTH2, and Password is unused.
	OAuth   *OAuthConfig
	Mailbox string
	// TrustedAuthServ is the authserv-id of the receiving server whose
	// Authentication-Results headers are trusted.
	TrustedAuthServ string
	// MaxMessage is the size of the largest mail read, 64 MiB if zero.
	MaxMessage int64
}

const (
	maxText     = 100_000
	idleLimit   = 25 * time.Minute
	smtpTimeout = 2 * time.Minute
	dialTimeout = 30 * time.Second
	// maxHeader bounds the header read to authenticate a mail. The trusted
	// Authentication-Results header is on top, so a cut header still has it.
	maxHeader         = 256 << 10
	maxPart           = 20 << 20
	defaultMaxMessage = 64 << 20
	fetchBatch        = 20
	sendAttempts      = 4
)

// Channel is a mailbox.
type Channel struct {
	cfg     Config
	kv      channel.KV
	tokens  oauth2.TokenSource
	threads *threads

	mu sync.Mutex
}

var (
	_ channel.Channel   = (*Channel)(nil)
	_ channel.Describer = (*Channel)(nil)
)

// New returns a channel for the mailbox. kv keeps the last UID processed
// and the state of recent threads.
func New(cfg Config, kv channel.KV) *Channel {
	if cfg.Mailbox == "" {
		cfg.Mailbox = "INBOX"
	}
	if cfg.Username == "" {
		cfg.Username = cfg.Address
	}
	if cfg.MaxMessage == 0 {
		cfg.MaxMessage = defaultMaxMessage
	}
	c := &Channel{cfg: cfg, kv: kv, threads: &threads{kv: kv}}
	if cfg.OAuth != nil {
		c.tokens = newTokenSource(*cfg.OAuth)
	}
	return c
}

// Name implements [channel.Channel].
func (c *Channel) Name() string { return "email" }

// Caps implements [channel.Channel]. Email sends questions and the final
// result, not a stream.
func (c *Channel) Caps() channel.Caps { return channel.Caps{MaxText: maxText} }

// Describe implements [channel.Describer].
func (c *Channel) Describe() string { return c.cfg.Address }

// LinkHint implements [channel.Describer].
func (c *Channel) LinkHint(code string) string {
	return fmt.Sprintf("email %s with the text: join %s", c.cfg.Address, code)
}

// Run implements [channel.Channel]. It reconnects with backoff.
func (c *Channel) Run(ctx context.Context, in chan<- channel.Inbound) error {
	if c.cfg.TrustedAuthServ == "" {
		return errors.New("email: trusted_authserv must be set, or no mail could be trusted")
	}
	retry := backoff.New(time.Second, 5*time.Minute, time.Minute)
	for ctx.Err() == nil {
		started := time.Now()
		err := c.session(ctx, in)
		if ctx.Err() != nil {
			return nil
		}
		delay := retry.Next(time.Since(started))
		slog.Warn("email: IMAP session ended, reconnecting", "error", err, "in", delay)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
	}
	return nil
}

// dialIMAP connects to the IMAP server. Ending ctx aborts the connection.
func (c *Channel) dialIMAP(ctx context.Context, opts *imapclient.Options) (*imapclient.Client, error) {
	host, port, err := net.SplitHostPort(c.cfg.IMAP)
	if err != nil {
		return nil, fmt.Errorf("email: imap must be host:port: %w", err)
	}
	if opts == nil {
		opts = &imapclient.Options{}
	}
	opts.TLSConfig = &tls.Config{ServerName: host}
	d := &net.Dialer{Timeout: dialTimeout}
	if port == "993" {
		conn, err := (&tls.Dialer{NetDialer: d, Config: &tls.Config{ServerName: host, NextProtos: []string{"imap"}}}).DialContext(ctx, "tcp", c.cfg.IMAP)
		if err != nil {
			return nil, err
		}
		return imapclient.New(conn, opts), nil
	}
	conn, err := d.DialContext(ctx, "tcp", c.cfg.IMAP)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { closeConn(conn) })
	client, err := imapclient.NewStartTLS(conn, opts)
	if !stop() {
		if err == nil {
			closeIMAP(client)
		}
		return nil, ctx.Err()
	}
	return client, err
}

func closeConn(conn net.Conn) {
	if err := conn.Close(); err != nil {
		slog.Debug("email: closing connection", "error", err)
	}
}

func closeIMAP(client *imapclient.Client) {
	if err := client.Close(); err != nil {
		slog.Debug("email: closing IMAP", "error", err)
	}
}

// login signs in with the password, or with an OAuth access token.
func (c *Channel) login(ctx context.Context, client *imapclient.Client) error {
	if c.tokens == nil {
		return client.Login(c.cfg.Username, c.cfg.Password).Wait()
	}
	tok, err := c.accessToken(ctx)
	if err != nil {
		return err
	}
	return client.Authenticate(xoauth2Client{user: c.cfg.Username, token: tok})
}

// session processes new mail, then idles until more arrives. Ending ctx
// closes the connection, which unblocks any command in flight.
func (c *Channel) session(ctx context.Context, in chan<- channel.Inbound) error {
	wake := make(chan struct{}, 1)
	client, err := c.dialIMAP(ctx, &imapclient.Options{
		UnilateralDataHandler: &imapclient.UnilateralDataHandler{
			Mailbox: func(data *imapclient.UnilateralDataMailbox) {
				if data.NumMessages != nil {
					select {
					case wake <- struct{}{}:
					default:
					}
				}
			},
		},
	})
	if err != nil {
		return err
	}
	closeOnce := sync.OnceFunc(func() { closeIMAP(client) })
	stop := context.AfterFunc(ctx, closeOnce)
	defer stop()
	defer closeOnce()
	if err := c.login(ctx, client); err != nil {
		return fmt.Errorf("IMAP login: %w", err)
	}
	sel, err := client.Select(c.cfg.Mailbox, nil).Wait()
	if err != nil {
		return fmt.Errorf("selecting %s: %w", c.cfg.Mailbox, err)
	}
	if err := c.threads.load(ctx); err != nil {
		return err
	}
	if err := c.checkValidity(ctx, sel, func() (imap.UID, error) { return highestUID(client, sel.NumMessages) }); err != nil {
		return err
	}
	for {
		if err := c.fetchNew(ctx, client, in); err != nil {
			return err
		}
		idle, err := client.Idle()
		if err != nil {
			return fmt.Errorf("IMAP IDLE: %w", err)
		}
		select {
		case <-ctx.Done():
		case <-wake:
		case <-time.After(idleLimit):
		}
		if err := idle.Close(); err != nil {
			return err
		}
		if err := idle.Wait(); err != nil {
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
	}
}

const (
	uidValidityKey = "email:uidvalidity"
	lastUIDKey     = "email:lastuid"
)

// checkValidity starts from the newest message when the mailbox is new to
// the agent or was recreated, instead of answering old mail. highest looks up
// the newest UID for servers which don't report UIDNEXT.
func (c *Channel) checkValidity(ctx context.Context, sel *imap.SelectData, highest func() (imap.UID, error)) error {
	v := strconv.FormatUint(uint64(sel.UIDValidity), 10)
	cur, ok, err := c.kv.Get(ctx, uidValidityKey)
	if err != nil {
		return err
	}
	if ok && cur == v {
		return nil
	}
	var last imap.UID
	switch {
	case sel.UIDNext > 0:
		last = sel.UIDNext - 1
	case sel.NumMessages > 0:
		if last, err = highest(); err != nil {
			return fmt.Errorf("finding the newest message: %w", err)
		}
	}
	if err := c.kv.Set(ctx, lastUIDKey, strconv.FormatUint(uint64(last), 10)); err != nil {
		return err
	}
	return c.kv.Set(ctx, uidValidityKey, v)
}

// highestUID returns the UID of message n, the newest of the mailbox.
func highestUID(client *imapclient.Client, n uint32) (imap.UID, error) {
	msgs, err := client.Fetch(imap.SeqSetNum(n), &imap.FetchOptions{UID: true}).Collect()
	if err != nil {
		return 0, err
	}
	if len(msgs) == 0 {
		return 0, fmt.Errorf("message %d not found", n)
	}
	return msgs[0].UID, nil
}

var (
	headerSection = &imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader, Peek: true, Partial: &imap.SectionPartial{Size: maxHeader}}
	fullSection   = &imap.FetchItemBodySection{Peek: true}
)

// fetchNew processes the mail after the last UID processed. It lists UIDs
// and sizes first, because "UID last+1:*" also returns the newest message
// when there is no new one. Then it reads the headers of a batch, and the
// body of each authenticated mail on its own.
func (c *Channel) fetchNew(ctx context.Context, client *imapclient.Client, in chan<- channel.Inbound) error {
	lastStr, ok, err := c.kv.Get(ctx, lastUIDKey)
	if err != nil {
		return err
	}
	var last imap.UID
	if ok {
		n, err := strconv.ParseUint(lastStr, 10, 32)
		if err != nil {
			return fmt.Errorf("email: invalid last UID %q: %w", lastStr, err)
		}
		last = imap.UID(n)
	}
	var set imap.UIDSet
	set.AddRange(last+1, 0)
	listed, err := client.Fetch(set, &imap.FetchOptions{UID: true, RFC822Size: true}).Collect()
	if err != nil {
		return fmt.Errorf("IMAP fetch: %w", err)
	}
	listed = slices.DeleteFunc(listed, func(m *imapclient.FetchMessageBuffer) bool { return m.UID <= last })
	slices.SortFunc(listed, func(a, b *imapclient.FetchMessageBuffer) int { return cmp.Compare(a.UID, b.UID) })
	for batch := range slices.Chunk(listed, fetchBatch) {
		uids := make([]imap.UID, len(batch))
		for i, m := range batch {
			uids[i] = m.UID
		}
		headers, err := fetchSection(client, uids, headerSection, maxHeader)
		if err != nil {
			return fmt.Errorf("IMAP fetch: %w", err)
		}
		for _, m := range batch {
			if err := c.process(ctx, client, m.UID, m.RFC822Size, headers[m.UID], in); err != nil {
				return err
			}
			if ctx.Err() != nil {
				return nil
			}
			if err := c.kv.Set(ctx, lastUIDKey, strconv.FormatUint(uint64(m.UID), 10)); err != nil {
				return err
			}
		}
	}
	return nil
}

// fetchSection fetches a section of each message, reading at most limit
// bytes of it. Longer sections are left out.
func fetchSection(client *imapclient.Client, uids []imap.UID, section *imap.FetchItemBodySection, limit int64) (map[imap.UID][]byte, error) {
	cmd := client.Fetch(imap.UIDSetNum(uids...), &imap.FetchOptions{UID: true, BodySection: []*imap.FetchItemBodySection{section}})
	out := map[imap.UID][]byte{}
	for msg := cmd.Next(); msg != nil; msg = cmd.Next() {
		var (
			uid  imap.UID
			data []byte
		)
		for item := msg.Next(); item != nil; item = msg.Next() {
			switch item := item.(type) {
			case imapclient.FetchItemDataUID:
				uid = item.UID
			case imapclient.FetchItemDataBodySection:
				if item.Literal == nil {
					continue
				}
				b, err := io.ReadAll(io.LimitReader(item.Literal, limit+1))
				if err != nil {
					return nil, errors.Join(err, cmd.Close())
				}
				if int64(len(b)) <= limit {
					data = b
				}
			}
		}
		if uid != 0 && data != nil {
			out[uid] = data
		}
	}
	return out, cmd.Close()
}

// process reads a listed message and delivers it to in. It drops mail which
// is malformed, unauthenticated or too large, and only returns errors of the
// IMAP connection and the channel's state.
func (c *Channel) process(ctx context.Context, client *imapclient.Client, uid imap.UID, size int64, header []byte, in chan<- channel.Inbound) error {
	if len(header) >= maxHeader {
		slog.Info("email: dropped mail", "uid", uid, "reason", "header too large")
		return nil
	}
	env, err := c.envelope(header)
	if errors.Is(err, ErrTemporary) {
		slog.Warn("email: dropped mail because the mail server couldn't check its sender; ask the sender to send it again", "uid", uid, "reason", err)
		return nil
	}
	if err != nil {
		slog.Info("email: dropped mail", "uid", uid, "reason", err)
		return nil
	}
	if size > c.cfg.MaxMessage {
		slog.Warn("email: dropped mail larger than the limit", "uid", uid, "from", env.sender, "size", size, "limit", c.cfg.MaxMessage)
		return nil
	}
	bodies, err := fetchSection(client, []imap.UID{uid}, fullSection, c.cfg.MaxMessage)
	if err != nil {
		return fmt.Errorf("IMAP fetch: %w", err)
	}
	raw, ok := bodies[uid]
	if !ok {
		slog.Warn("email: dropped mail whose body couldn't be read", "uid", uid, "from", env.sender)
		return nil
	}
	msg, ok, err := c.inbound(ctx, env, raw)
	if err != nil || !ok {
		return err
	}
	select {
	case in <- msg:
	case <-ctx.Done():
	}
	return nil
}

// envelope is what the header of an authenticated mail tells.
type envelope struct {
	sender, name, msgID, subject string
	references, inReplyTo        []string
}

// reply reports whether the mail answers another one, and isn't a forward.
func (e envelope) reply() bool {
	return (len(e.references) > 0 || len(e.inReplyTo) > 0) && !forwardPrefix.MatchString(e.subject)
}

// envelope parses the header of a mail. It returns an error for mail the
// agent must not read: malformed, unauthenticated, its own, auto-replies
// and lists.
func (c *Channel) envelope(raw []byte) (envelope, error) {
	th, err := mtextproto.ReadHeader(bufio.NewReader(bytes.NewReader(raw)))
	if err != nil {
		return envelope{}, fmt.Errorf("malformed header: %w", err)
	}
	h := mail.Header{Header: message.Header{Header: th}}
	// DMARC checks a single From domain, and receiving servers may check
	// another copy of a repeated header than the one read here.
	for _, f := range []string{"From", "Message-Id"} {
		if n := len(h.Values(f)); n != 1 {
			return envelope{}, fmt.Errorf("%d %s headers, want one", n, f)
		}
	}
	for _, f := range []string{"Sender", "Subject"} {
		if n := len(h.Values(f)); n > 1 {
			return envelope{}, fmt.Errorf("%d %s headers, want at most one", n, f)
		}
	}
	from, err := h.AddressList("From")
	if err != nil || len(from) != 1 {
		return envelope{}, fmt.Errorf("%d From addresses, want one (%v)", len(from), err)
	}
	if h.Has("Sender") {
		if s, err := h.AddressList("Sender"); err != nil || len(s) != 1 {
			return envelope{}, fmt.Errorf("%d Sender addresses, want one (%v)", len(s), err)
		}
	}
	sender := strings.ToLower(from[0].Address)
	if strings.EqualFold(sender, c.cfg.Address) {
		return envelope{}, errors.New("sent by the agent")
	}
	if auto := strings.ToLower(h.Get("Auto-Submitted")); auto != "" && auto != "no" {
		return envelope{}, errors.New("auto-submitted")
	}
	switch strings.ToLower(h.Get("Precedence")) {
	case "bulk", "list", "junk", "auto_reply":
		return envelope{}, errors.New("bulk mail")
	}
	if h.Get("List-Id") != "" || h.Get("List-Unsubscribe") != "" {
		return envelope{}, errors.New("list mail")
	}
	msgID, err := h.MessageID()
	if err != nil || msgID == "" {
		return envelope{}, fmt.Errorf("no Message-ID (%v)", err)
	}
	if err := Authenticate(h.Values("Authentication-Results"), c.cfg.TrustedAuthServ, domainOf(sender)); err != nil {
		return envelope{}, fmt.Errorf("unauthenticated sender %s: %w", sender, err)
	}
	subject, err := h.Subject()
	if err != nil {
		slog.Debug("email: undecodable subject", "error", err)
		subject = ""
	}
	env := envelope{sender: sender, name: from[0].Name, msgID: msgID, subject: subject}
	if env.references, err = h.MsgIDList("References"); err != nil {
		slog.Debug("email: malformed References", "error", err)
	}
	if env.inReplyTo, err = h.MsgIDList("In-Reply-To"); err != nil {
		slog.Debug("email: malformed In-Reply-To", "error", err)
	}
	return env, nil
}

// conv returns the conversation of a mail: the one in which the agent sent
// a message the mail references, or else the thread root with the sender.
func (c *Channel) conv(ctx context.Context, env envelope) (channel.ConvKey, error) {
	root := env.msgID
	switch {
	case len(env.references) > 0:
		root = env.references[0]
	case len(env.inReplyTo) > 0:
		root = env.inReplyTo[0]
	}
	return c.threads.convOf(ctx, env.sender, root, slices.Concat(env.references, env.inReplyTo), root != env.msgID, env.subject)
}

// inbound turns an authenticated mail into a message. It drops a mail whose
// body can't be parsed, and returns errors of the channel's state.
func (c *Channel) inbound(ctx context.Context, env envelope, raw []byte) (channel.Inbound, bool, error) {
	text, files, err := body(raw, env.reply())
	if err != nil {
		slog.Warn("email: dropped mail whose body couldn't be parsed", "from", env.sender, "error", err)
		return channel.Inbound{}, false, nil
	}
	conv, err := c.conv(ctx, env)
	if err != nil {
		return channel.Inbound{}, false, err
	}
	if err := c.threads.received(ctx, conv, env.subject); err != nil {
		return channel.Inbound{}, false, err
	}
	return channel.Inbound{
		Channel:   c.Name(),
		MessageID: env.sender + "|" + env.msgID,
		Identity:  env.sender,
		Name:      env.name,
		Conv:      conv,
		Direct:    true,
		Authentic: true,
		Text:      text,
		Files:     files,
	}, true, nil
}

// body returns the new text of a mail and its attachments. Mail without a
// text/plain part falls back to its text/html part.
func body(raw []byte, reply bool) (string, []channel.File, error) {
	r, err := mail.CreateReader(bytes.NewReader(raw))
	if err != nil {
		return "", nil, err
	}
	var (
		plain, html string
		files       []channel.File
	)
	for {
		p, err := r.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", nil, err
		}
		data, err := io.ReadAll(io.LimitReader(p.Body, maxPart))
		if err != nil {
			return "", nil, err
		}
		switch ph := p.Header.(type) {
		case *mail.InlineHeader:
			ct, _, err := ph.ContentType()
			if err != nil {
				slog.Debug("email: inline part without a valid content type", "error", err)
				continue
			}
			switch {
			case ct == "text/plain" && plain == "":
				plain = string(data)
			case ct == "text/html" && html == "":
				html = string(data)
			}
		case *mail.AttachmentHeader:
			name, err := ph.Filename()
			if err != nil {
				slog.Debug("email: attachment without a valid filename", "error", err)
			}
			ct, _, err := ph.ContentType()
			if err != nil {
				slog.Debug("email: attachment without a valid content type", "error", err)
				ct = "application/octet-stream"
			}
			files = append(files, channel.File{Name: name, MediaType: ct, Data: data})
		}
	}
	if strings.TrimSpace(plain) == "" && html != "" {
		if plain, err = htmlText(strings.NewReader(html)); err != nil {
			return "", nil, fmt.Errorf("reading the HTML part: %w", err)
		}
	}
	return StripQuoted(plain, reply), files, nil
}

// ConvKey identifies an email thread: the correspondent and the thread
// root, separated by a tab. Keys made before also had the base subject as
// a third field, which [parseConv] still accepts.
func ConvKey(addr, root string) channel.ConvKey {
	return channel.ConvKey(addr + "\t" + root)
}

// legacyConvKey is the key of a conversation started before the subject
// left the key: the correspondent, the thread root and the subject without
// the prefixes the old pattern stripped.
func legacyConvKey(addr, root, subject string) channel.ConvKey {
	for {
		loc := legacySubjectPrefix.FindStringIndex(subject)
		if loc == nil {
			break
		}
		subject = subject[loc[1]:]
	}
	return channel.ConvKey(addr + "\t" + root + "\t" + strings.ReplaceAll(strings.TrimSpace(subject), "\t", " "))
}

var legacySubjectPrefix = regexp.MustCompile(`(?i)^\s*(re|fwd?|aw|wg|sv)(\[\d+\])?\s*:\s*`)

// subjectPrefix matches one reply or forward prefix, such as "Re:", "RE[2]:",
// "Fwd:", the German "AW:" and "WG:", the Swedish "SV:" or the Chinese "回复：".
var subjectPrefix = regexp.MustCompile(`(?i)^\s*(re|fwd?|aw|wg|sv|antw|res|odp|tr|rv|回复|答复|转发)(\[\d+\])?\s*[:：]\s*`)

// forwardPrefix matches a forward prefix at the start of a subject.
var forwardPrefix = regexp.MustCompile(`(?i)^\s*(fwd?|wg|tr|rv|i|转发)(\[\d+\])?\s*[:：]`)

// baseSubject strips the reply and forward prefixes of a subject.
func baseSubject(subject string) string {
	for {
		loc := subjectPrefix.FindStringIndex(subject)
		if loc == nil {
			return strings.TrimSpace(subject)
		}
		subject = subject[loc[1]:]
	}
}

func parseConv(conv channel.ConvKey) (addr, root, subject string) {
	parts := strings.SplitN(string(conv), "\t", 3)
	for len(parts) < 3 {
		parts = append(parts, "")
	}
	return parts[0], parts[1], parts[2]
}

func domainOf(addr string) string {
	_, d, _ := strings.Cut(addr, "@")
	return strings.ToLower(d)
}

// Send implements [channel.Channel].
func (c *Channel) Send(ctx context.Context, m channel.Outbound) (channel.Ref, error) {
	to, root, keySubject := parseConv(m.Conv)
	if to == "" {
		return channel.Ref{}, fmt.Errorf("email: no recipient in conversation %q", m.Conv)
	}
	subject, err := c.threads.subject(ctx, m.Conv)
	if err != nil {
		return channel.Ref{}, err
	}
	for _, s := range []string{keySubject, m.Subject} {
		if subject == "" {
			subject = s
		}
	}
	if root != "" {
		subject = "Re: " + baseSubject(subject)
	}
	msgID := cryptoutil.RandomString(24, cryptoutil.Alphanumeric) + "@" + domainOf(c.cfg.Address)
	h := mailHeader(c.cfg.Address, to)
	h.SetSubject(subject)
	h.SetMessageID(msgID)
	h.Set("Auto-Submitted", "auto-replied")
	if root != "" {
		h.SetMsgIDList("In-Reply-To", []string{root})
		h.SetMsgIDList("References", []string{root})
	}
	var buf bytes.Buffer
	if err := writeMessage(&buf, h, m); err != nil {
		return channel.Ref{}, err
	}
	if err := c.threads.sent(ctx, m.Conv, msgID); err != nil {
		return channel.Ref{}, err
	}
	if err := c.deliver(ctx, to, buf.Bytes()); err != nil {
		return channel.Ref{}, err
	}
	return channel.Ref{Conv: m.Conv, ID: msgID}, nil
}

// deliver sends msg, retrying with backoff while ctx lasts. A 5xx reply
// isn't retried, because the server would refuse the message again.
func (c *Channel) deliver(ctx context.Context, to string, msg []byte) error {
	retry := backoff.New(time.Second, 30*time.Second, time.Hour)
	for attempt := 1; ; attempt++ {
		err := c.sendOnce(ctx, to, msg)
		if err == nil || attempt == sendAttempts || permanent(err) || ctx.Err() != nil {
			return err
		}
		delay := retry.Next(0)
		slog.Warn("email: sending failed, retrying", "error", err, "in", delay)
		select {
		case <-ctx.Done():
			return err
		case <-time.After(delay):
		}
	}
}

func (c *Channel) sendOnce(ctx context.Context, to string, msg []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sendSMTP(ctx, to, msg)
}

func permanent(err error) bool {
	var tp *textproto.Error
	return errors.As(err, &tp) && tp.Code >= 500
}

func mailHeader(from, to string) mail.Header {
	var h mail.Header
	h.SetDate(time.Now())
	h.SetAddressList("From", []*mail.Address{{Address: from}})
	h.SetAddressList("To", []*mail.Address{{Address: to}})
	return h
}

func writeMessage(w io.Writer, h mail.Header, m channel.Outbound) error {
	mw, err := mail.CreateWriter(w, h)
	if err != nil {
		return err
	}
	tw, err := mw.CreateInline()
	if err != nil {
		return err
	}
	var th mail.InlineHeader
	th.Set("Content-Type", "text/plain; charset=utf-8")
	pw, err := tw.CreatePart(th)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(pw, m.Text); err != nil {
		return err
	}
	if err := errors.Join(pw.Close(), tw.Close()); err != nil {
		return err
	}
	for _, f := range m.Files {
		var ah mail.AttachmentHeader
		mt := f.MediaType
		if mt == "" {
			mt = "application/octet-stream"
		}
		ah.Set("Content-Type", mt)
		ah.SetFilename(f.Name)
		aw, err := mw.CreateAttachment(ah)
		if err != nil {
			return err
		}
		if _, err := aw.Write(f.Data); err != nil {
			return err
		}
		if err := aw.Close(); err != nil {
			return err
		}
	}
	return mw.Close()
}

// Validate implements [channel.Validator]: it logs in to the IMAP and SMTP
// servers.
func (c *Channel) Validate(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	client, err := c.dialIMAP(ctx, nil)
	if err != nil {
		return fmt.Errorf("IMAP %s: %w", c.cfg.IMAP, err)
	}
	closeOnce := sync.OnceFunc(func() { closeIMAP(client) })
	stop := context.AfterFunc(ctx, closeOnce)
	defer stop()
	defer closeOnce()
	if err := c.login(ctx, client); err != nil {
		return fmt.Errorf("IMAP login: %w", err)
	}
	if err := client.Logout().Wait(); err != nil {
		slog.Debug("email: IMAP logout", "error", err)
	}
	smtpClient, err := c.dialSMTP(ctx)
	if err != nil {
		return err
	}
	return smtpClient.Quit()
}

func (c *Channel) sendSMTP(ctx context.Context, to string, msg []byte) error {
	client, err := c.dialSMTP(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if err := client.Close(); err != nil {
			slog.Debug("email: closing SMTP", "error", err)
		}
	}()
	if err := client.Mail(c.cfg.Address); err != nil {
		return err
	}
	if err := client.Rcpt(to); err != nil {
		return err
	}
	w, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return errors.Join(err, w.Close())
	}
	if err := w.Close(); err != nil {
		return err
	}
	// The server accepted the message; failing here would send it again.
	if err := client.Quit(); err != nil {
		slog.Debug("email: SMTP quit", "error", err)
	}
	return nil
}

// dialSMTP connects and logs in to the SMTP server.
func (c *Channel) dialSMTP(ctx context.Context) (*smtp.Client, error) {
	host, _, err := net.SplitHostPort(c.cfg.SMTP)
	if err != nil {
		return nil, fmt.Errorf("email: smtp must be host:port: %w", err)
	}
	d := net.Dialer{Timeout: dialTimeout}
	var conn net.Conn
	if strings.HasSuffix(c.cfg.SMTP, ":465") {
		conn, err = (&tls.Dialer{NetDialer: &d, Config: &tls.Config{ServerName: host}}).DialContext(ctx, "tcp", c.cfg.SMTP)
	} else {
		conn, err = d.DialContext(ctx, "tcp", c.cfg.SMTP)
	}
	if err != nil {
		return nil, fmt.Errorf("email: SMTP: %w", err)
	}
	// A stalled server would otherwise block every later reply, which wait
	// for c.mu.
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(smtpTimeout)
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, errors.Join(fmt.Errorf("email: SMTP: %w", err), conn.Close())
	}
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("email: SMTP: %w", err), conn.Close())
	}
	_, implicitTLS := conn.(*tls.Conn)
	switch ok, _ := client.Extension("STARTTLS"); {
	case ok:
		if err := client.StartTLS(&tls.Config{ServerName: host}); err != nil {
			return nil, errors.Join(fmt.Errorf("email: STARTTLS: %w", err), client.Close())
		}
	case !implicitTLS && !isLoopback(host):
		return nil, errors.Join(fmt.Errorf("email: %s doesn't offer STARTTLS; refusing to send the password unencrypted", c.cfg.SMTP), client.Close())
	}
	a, err := c.smtpAuth(ctx, host)
	if err != nil {
		return nil, errors.Join(err, client.Close())
	}
	if err := client.Auth(a); err != nil {
		return nil, errors.Join(fmt.Errorf("email: SMTP auth: %w", err), client.Close())
	}
	return client, nil
}

// smtpAuth returns the SMTP login with the password, or with an OAuth
// access token.
func (c *Channel) smtpAuth(ctx context.Context, host string) (smtp.Auth, error) {
	if c.tokens == nil {
		return smtp.PlainAuth("", c.cfg.Username, c.cfg.Password, host), nil
	}
	tok, err := c.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	return xoauth2Auth{user: c.cfg.Username, token: tok}, nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Edit implements [channel.Channel]. Email can't edit sent messages.
func (c *Channel) Edit(context.Context, channel.Ref, channel.Outbound) error {
	return errors.New("email messages can't be edited")
}
