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

package email

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/messenger"
	"github.com/a2aproject/a2a-serve/internal/messenger/messengertest"
)

func TestAuthenticate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		results       []string
		want          bool
		wantTemporary bool
	}{
		{name: "dmarc pass", results: []string{"mx.example.com; dkim=pass header.d=other.org; dmarc=pass header.from=alice.org"}, want: true},
		{name: "dmarc pass with uppercase domain", results: []string{"mx.example.com; dmarc=pass header.from=Alice.ORG"}, want: true},
		{name: "dkim of the sender's domain without dmarc", results: []string{"mx.example.com; dkim=pass header.d=alice.org"}, want: true},
		{name: "dkim of the sender's domain with dmarc none", results: []string{"mx.example.com; dkim=pass header.d=alice.org; dmarc=none"}, want: true},
		{name: "dkim of a subdomain", results: []string{"mx.example.com; dkim=pass header.d=mail.alice.org"}, want: false},
		{name: "dkim of a parent domain", results: []string{"mx.example.com; dkim=pass header.d=org"}, want: false},
		{name: "dkim with header.i only", results: []string{"mx.example.com; dkim=pass header.i=@alice.org; dmarc=none"}, want: false},
		{name: "dmarc fail with dkim of the sender's domain", results: []string{"mx.example.com; dmarc=fail header.from=alice.org; dkim=pass header.d=alice.org"}, want: false},
		{name: "dkim before dmarc fail", results: []string{"mx.example.com; dkim=pass header.d=alice.org; dmarc=fail header.from=alice.org"}, want: false},
		{name: "dmarc quarantine", results: []string{"mx.example.com; dmarc=quarantine header.from=alice.org; dkim=pass header.d=alice.org"}, want: false},
		{name: "unaligned dkim", results: []string{"mx.example.com; dkim=pass header.d=evil.com; dmarc=fail"}, want: false},
		{name: "untrusted server", results: []string{"evil.com; dmarc=pass header.from=alice.org"}, want: false},
		{name: "dmarc pass for another domain", results: []string{"mx.example.com; dmarc=pass header.from=evil.com"}, want: false},
		{name: "dmarc pass without header.from", results: []string{"mx.example.com; dmarc=pass"}, want: false},
		{name: "forged header below the trusted one", results: []string{
			"mx.example.com; dmarc=fail header.from=alice.org",
			"mx.example.com; dmarc=pass header.from=alice.org",
		}, want: false},
		{name: "no results", want: false},
		{name: "version and comments", results: []string{"mx.example.com 1 (Postfix); dmarc=pass (p=reject) header.from=alice.org"}, want: true},
		{name: "quoted reason", results: []string{`mx.example.com; dmarc=pass reason="ok; aligned" header.from=alice.org`}, want: true},
		{name: "quoted mailfrom spoofing a result", results: []string{
			`mx.example.com; spf=pass smtp.mailfrom="x;dmarc=pass header.from=alice.org y"@evil.com; dmarc=fail header.from=alice.org`,
		}, want: false},
		{name: "comment spoofing a result", results: []string{
			"mx.example.com; spf=pass (sender x;dmarc=pass header.from=alice.org;) smtp.mailfrom=evil.com; dmarc=fail header.from=alice.org",
		}, want: false},
		{name: "comment spoofing a property", results: []string{"mx.example.com; dmarc=fail (header.from=alice.org dmarc=pass) header.from=evil.com"}, want: false},
		{name: "unterminated quote", results: []string{`mx.example.com; dmarc=pass header.from=alice.org reason="x`}, want: false},
		{name: "Gmail bestguesspass", results: []string{"mx.example.com; dkim=pass header.i=@alice.org; spf=pass smtp.mailfrom=alice.org; dmarc=bestguesspass header.from=alice.org"}, want: true},
		{name: "Microsoft bestguesspass", results: []string{"mx.example.com; spf=pass smtp.mailfrom=alice.org; dkim=pass header.d=alice.org; dmarc=bestguesspass action=none header.from=alice.org; compauth=pass reason=109"}, want: true},
		{name: "Microsoft pass with action", results: []string{"mx.example.com; dmarc=pass action=none header.from=alice.org; compauth=pass reason=100"}, want: true},
		{name: "bestguesspass for another domain", results: []string{"mx.example.com; dmarc=bestguesspass header.from=evil.com"}, want: false},
		{name: "dmarc temperror", results: []string{"mx.example.com; dmarc=temperror header.from=alice.org; dkim=pass header.d=alice.org"}, want: false, wantTemporary: true},
		{name: "dmarc permerror", results: []string{"mx.example.com; dmarc=permerror header.from=alice.org; dkim=pass header.d=alice.org"}, want: false},
		{name: "dkim temperror without dmarc", results: []string{"mx.example.com; dkim=temperror header.d=alice.org"}, want: false, wantTemporary: true},
		{name: "dkim permerror without dmarc", results: []string{"mx.example.com; dkim=permerror header.d=alice.org"}, want: false},
		{name: "Microsoft fail with action", results: []string{"mx.example.com; dmarc=fail action=oreject header.from=alice.org; dkim=pass header.d=alice.org"}, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := Authenticate(tc.results, "mx.example.com", "alice.org")
			if got := err == nil; got != tc.want {
				t.Fatalf("Authenticate(%q) error = %v, want accepted %v", tc.results, err, tc.want)
			}
			if got := errors.Is(err, ErrTemporary); got != tc.wantTemporary {
				t.Fatalf("Authenticate(%q) error = %v, want temporary %v", tc.results, err, tc.wantTemporary)
			}
		})
	}
}

func TestStripQuoted(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		text  string
		reply bool
		want  string
	}{
		{name: "attribution", text: "approve\r\n\r\nOn Mon, 1 Jan 2026, coder wrote:\r\n> coder wants to run git push\r\n", reply: true, want: "approve"},
		{name: "iPhone trailer", text: "yes\n\nSent from my iPhone", reply: true, want: "yes"},
		{name: "Outlook mobile trailer", text: "deny\n\nGet Outlook for Android", reply: true, want: "deny"},
		{name: "German trailer", text: "ok\n\nVon meinem iPhone gesendet", reply: true, want: "ok"},
		{name: "signature", text: "approve\n-- \nAlice\nACME", reply: true, want: "approve"},
		{name: "Outlook original message", text: "approve\n\n-----Original Message-----\nFrom: coder\nwants to push", reply: true, want: "approve"},
		{name: "Outlook separator", text: "approve\n\n________________________________\nFrom: coder <coder@example.com>\nSent: Monday\n\nwants to push", reply: true, want: "approve"},
		{name: "Outlook header block", text: "approve\n\nFrom: coder <coder@example.com>\nSent: Monday, January 1, 2026 10:00 AM\nTo: Alice\nSubject: push?\n\nwants to push", reply: true, want: "approve"},
		{name: "localized Outlook header block", text: "approve\n\nVon: coder <coder@example.com>\nGesendet: Montag, 1. Januar 2026 10:00\n\nwants to push", reply: true, want: "approve"},
		{name: "wrapped Gmail attribution", text: "approve\n\nOn Mon, Jan 1, 2026 at 10:00 AM coder <\ncoder@example.com> wrote:\n\n> wants to push", reply: true, want: "approve"},
		{name: "German attribution", text: "approve\n\nAm Mo., 1. Jan. 2026 um 10:00 Uhr schrieb coder <coder@example.com>:\n> wants to push", reply: true, want: "approve"},
		{name: "French attribution", text: "approve\n\nLe lun. 1 janv. 2026 à 10:00, coder <coder@example.com> a écrit :\n> wants to push", reply: true, want: "approve"},
		{name: "unknown attribution before quoted text", text: "approve\n\n2026-01-01 10:00 GMT, coder <coder@example.com>:\n\n> wants to push", reply: true, want: "approve"},
		{name: "inline answers", text: "See below:\n> run tests?\nyes\n> push?\nno", reply: true, want: "See below:\nyes\nno"},
		{name: "remaining text", text: "yes\n\nbut only in tests/\n\n> wants to push", reply: true, want: "yes\n\nbut only in tests/"},
		{name: "forward keeps the header block", text: "Have a look.\n\nFrom: bob@example.com\nSent: Monday\n\nThe build fails.", want: "Have a look.\n\nFrom: bob@example.com\nSent: Monday\n\nThe build fails."},
		{name: "new mail keeps the separator", text: "Fix it.\n\n________________________________\nlog", want: "Fix it.\n\n________________________________\nlog"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := StripQuoted(tc.text, tc.reply); got != tc.want {
				t.Fatalf("StripQuoted(%q) = %q, want %q", tc.text, got, tc.want)
			}
		})
	}
}

func TestHTMLText(t *testing.T) {
	t.Parallel()
	in := `<html><head><style>p{}</style></head><body><div dir="ltr">Approve&nbsp;<b>it</b></div><br>` +
		`<div class="gmail_quote"><div class="gmail_attr">On Mon, Jan 1, 2026 at 10:00 AM coder wrote:<br></div>` +
		`<blockquote class="gmail_quote"><p>coder wants to run git push</p></blockquote></div></body></html>`
	got, err := htmlText(strings.NewReader(in))
	if err != nil {
		t.Fatalf("htmlText() error = %v", err)
	}
	if got := StripQuoted(got, true); got != "Approve\u00a0it" {
		t.Fatalf("StripQuoted(htmlText()) = %q, want %q", got, "Approve\u00a0it")
	}
}

const rawMail = "From: Alice <Alice@alice.org>\r\n" +
	"To: coder@example.com\r\n" +
	"Subject: Re: fix the test\r\n" +
	"Message-ID: <m2@alice.org>\r\n" +
	"In-Reply-To: <m1@example.com>\r\n" +
	"References: <root@alice.org> <m1@example.com>\r\n" +
	"Authentication-Results: mx.example.com; dmarc=pass header.from=alice.org\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"Please push it.\r\n" +
	"> quoted\r\n"

func newTestMessenger(t *testing.T) *Messenger {
	t.Helper()
	return New(Config{Address: "coder@example.com", TrustedAuthServ: "mx.example.com"}, messengertest.NewKV(nil))
}

// parse reads a whole mail as the IMAP fetch does in two steps.
func parse(ctx context.Context, c *Messenger, raw string) (messenger.Inbound, error) {
	env, err := c.envelope([]byte(raw))
	if err != nil {
		return messenger.Inbound{}, err
	}
	in, ok, err := c.inbound(ctx, env, []byte(raw))
	if err == nil && !ok {
		err = errors.New("dropped")
	}
	return in, err
}

func TestParse(t *testing.T) {
	t.Parallel()
	c := newTestMessenger(t)
	got, err := parse(t.Context(), c, rawMail)
	if err != nil {
		t.Fatalf("parse() error = %v, want a message", err)
	}
	want := messenger.Inbound{
		Messenger: "email", MessageID: "alice@alice.org|m2@alice.org", Identity: "alice@alice.org", Name: "Alice",
		Conv: ConvKey("alice@alice.org", "root@alice.org"), Direct: true, Authentic: true,
		Text:    "Please push it.",
		ReplyTo: messenger.Ref{Conv: ConvKey("alice@alice.org", "root@alice.org"), ID: "m1@example.com"},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("parse() wrong result (-want +got) diff = %s", diff)
	}
}

func TestParseDropsMail(t *testing.T) {
	t.Parallel()
	replace := func(old, new string) string { return strings.Replace(rawMail, old, new, 1) }
	tests := []struct {
		name string
		raw  string
	}{
		{name: "auto-reply", raw: "Auto-Submitted: auto-replied\r\n" + rawMail},
		{name: "list", raw: "List-Id: <dev.lists.example.com>\r\n" + rawMail},
		{name: "own mail", raw: replace("Alice <Alice@alice.org>", "coder@example.com")},
		{name: "several senders", raw: replace("Alice <Alice@alice.org>", "Alice <alice@alice.org>, x@evil.com")},
		{name: "two From headers", raw: "From: mallory@evil.com\r\n" + rawMail},
		{name: "two Sender headers", raw: "Sender: a@alice.org\r\nSender: b@alice.org\r\n" + rawMail},
		{name: "Sender with two addresses", raw: "Sender: a@alice.org, b@alice.org\r\n" + rawMail},
		{name: "two Subject headers", raw: "Subject: other\r\n" + rawMail},
		{name: "two Message-ID headers", raw: "Message-ID: <x@alice.org>\r\n" + rawMail},
		{name: "no Message-ID", raw: replace("Message-ID: <m2@alice.org>\r\n", "")},
		{name: "dmarc fail", raw: replace("dmarc=pass", "dmarc=fail")},
		{name: "no Authentication-Results", raw: replace("Authentication-Results: mx.example.com; dmarc=pass header.from=alice.org\r\n", "")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got, err := parse(t.Context(), newTestMessenger(t), tc.raw); err == nil {
				t.Fatalf("parse() = %+v, want dropped", got)
			}
		})
	}
}

func TestParseAcceptsOneSender(t *testing.T) {
	t.Parallel()
	if _, err := parse(t.Context(), newTestMessenger(t), "Sender: secretary@alice.org\r\n"+rawMail); err != nil {
		t.Fatalf("parse() error = %v, want a message", err)
	}
}

func TestParseTypedApproval(t *testing.T) {
	t.Parallel()
	header := "From: Alice <alice@alice.org>\r\n" +
		"Subject: Re: coder wants to push\r\n" +
		"Message-ID: <m3@alice.org>\r\n" +
		"In-Reply-To: <q1@example.com>\r\n" +
		"Authentication-Results: mx.example.com; dmarc=pass header.from=alice.org\r\n"
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "plain text with trailer",
			raw:  header + "Content-Type: text/plain\r\n\r\nyes\r\n\r\nSent from my iPhone\r\n",
			want: "yes",
		},
		{
			name: "HTML only",
			raw: header + "Content-Type: text/html\r\n\r\n<div>approve</div><hr><div id=\"divRplyFwdMsg\"><b>From:</b> coder<br>" +
				"<b>Sent:</b> Monday</div><div>coder wants to push</div>\r\n",
			want: "approve",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parse(t.Context(), newTestMessenger(t), tc.raw)
			if err != nil {
				t.Fatalf("parse() error = %v, want a message", err)
			}
			if got.Text != tc.want {
				t.Fatalf("parse().Text = %q, want %q", got.Text, tc.want)
			}
		})
	}
}

func TestWriteMessage(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := writeMessage(&buf, mailHeader("coder@example.com", "alice@alice.org"), messenger.Outbound{
		Text: "Done.", Files: []messenger.File{{Name: "a.txt", MediaType: "text/plain", Data: []byte("x")}},
	}); err != nil {
		t.Fatalf("writeMessage() error = %v", err)
	}
	for _, want := range []string{"Done.", `filename=a.txt`, "To: <alice@alice.org>"} {
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("writeMessage() = %s, want it to contain %q", buf.String(), want)
		}
	}
}

func TestParseReplyKeepsConversation(t *testing.T) {
	t.Parallel()
	first := func(subject string) string {
		return "From: Alice <alice@alice.org>\r\n" +
			"To: coder@example.com\r\n" +
			subject +
			"Message-ID: <root@alice.org>\r\n" +
			"Authentication-Results: mx.example.com; dmarc=pass header.from=alice.org\r\n" +
			"Content-Type: text/plain; charset=utf-8\r\n" +
			"\r\n" +
			"Please fix it.\r\n"
	}
	tests := []struct {
		first, reply string
	}{
		{first: "Subject: fix the test\r\n", reply: "Re: fix the test"},
		{first: "Subject: fix the test\r\n", reply: "RE: Re: fix the test"},
		{first: "Subject: fix the test\r\n", reply: "Antw: fix the test"},
		{first: "Subject: fix the test\r\n", reply: "回复: fix the test"},
		{first: "Subject: fix the test\r\n", reply: "RES: fix the test"},
		{first: "Subject: fix the test\r\n", reply: "Odp: fix the test"},
		{first: "Subject: fix the test\r\n", reply: "a new subject"},
		{first: "", reply: "Re: "},
	}
	for _, tc := range tests {
		t.Run(tc.reply, func(t *testing.T) {
			t.Parallel()
			c := newTestMessenger(t)
			want, err := parse(t.Context(), c, first(tc.first))
			if err != nil {
				t.Fatalf("parse(first) error = %v, want a message", err)
			}
			reply := "From: Alice <alice@alice.org>\r\n" +
				"To: coder@example.com\r\n" +
				"Subject: " + tc.reply + "\r\n" +
				"Message-ID: <m3@alice.org>\r\n" +
				"In-Reply-To: <m2@example.com>\r\n" +
				"References: <root@alice.org> <m2@example.com>\r\n" +
				"Authentication-Results: mx.example.com; dmarc=pass header.from=alice.org\r\n" +
				"Content-Type: text/plain; charset=utf-8\r\n" +
				"\r\n" +
				"approve\r\n"
			got, err := parse(t.Context(), c, reply)
			if err != nil {
				t.Fatalf("parse(reply) error = %v, want a message", err)
			}
			if got.Conv != want.Conv {
				t.Fatalf("parse(reply).Conv = %q, want %q", got.Conv, want.Conv)
			}
		})
	}
}

func TestParseReplyKeepsLegacyConversation(t *testing.T) {
	t.Parallel()
	reply := func(id, subject string) string {
		return "From: Alice <alice@alice.org>\r\n" +
			"Subject: " + subject + "\r\n" +
			"Message-ID: <" + id + "@alice.org>\r\n" +
			"References: <root@alice.org> <m1@example.com>\r\n" +
			"Authentication-Results: mx.example.com; dmarc=pass header.from=alice.org\r\n" +
			"\r\n" +
			"approve\r\n"
	}
	tests := []struct {
		name string
		kv   map[string]string
		want messenger.ConvKey
	}{
		{
			name: "mailbox read before the upgrade",
			kv:   map[string]string{uidValidityKey: "7", lastUIDKey: "41"},
			want: "alice@alice.org\troot@alice.org\tfix the test",
		},
		{
			name: "new mailbox",
			want: ConvKey("alice@alice.org", "root@alice.org"),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := New(Config{Address: "coder@example.com", TrustedAuthServ: "mx.example.com"}, messengertest.NewKV(tc.kv))
			for _, m := range []struct{ id, subject string }{{"m2", "Re: fix the test"}, {"m3", "Antw: fix the test"}} {
				got, err := parse(t.Context(), c, reply(m.id, m.subject))
				if err != nil {
					t.Fatalf("parse(%s) error = %v, want a message", m.subject, err)
				}
				if got.Conv != tc.want {
					t.Fatalf("parse(%s).Conv = %q, want %q", m.subject, got.Conv, tc.want)
				}
			}
		})
	}
}

func TestNewMailboxIsNotLegacyAfterItsFirstSession(t *testing.T) {
	t.Parallel()
	c := newTestMessenger(t)
	if err := c.threads.load(t.Context()); err != nil {
		t.Fatalf("threads.load() error = %v", err)
	}
	if err := c.checkValidity(t.Context(), &imap.SelectData{UIDValidity: 7, UIDNext: 10}, nil); err != nil {
		t.Fatalf("checkValidity() error = %v", err)
	}
	got, err := parse(t.Context(), New(Config{Address: "coder@example.com", TrustedAuthServ: "mx.example.com"}, c.kv), rawMail)
	if err != nil {
		t.Fatalf("parse() error = %v, want a message", err)
	}
	if want := ConvKey("alice@alice.org", "root@alice.org"); got.Conv != want {
		t.Fatalf("parse().Conv = %q, want %q", got.Conv, want)
	}
}

func TestSendThreadsReplies(t *testing.T) {
	t.Parallel()
	srv := startSMTP(t)
	kv := messengertest.NewKV(nil)
	c := New(Config{Address: "coder@example.com", SMTP: srv.addr, TrustedAuthServ: "mx.example.com"}, kv)
	first := strings.Replace(rawMail, "Subject: Re: fix the test", "Subject: AW: fix the test", 1)
	in, err := parse(t.Context(), c, first)
	if err != nil {
		t.Fatalf("parse() error = %v, want a message", err)
	}
	ref, err := c.Send(t.Context(), messenger.Outbound{Conv: in.Conv, Text: "Done."})
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	msgs := srv.messages()
	if len(msgs) != 1 || !strings.Contains(msgs[0], "Subject: Re: fix the test\r\n") {
		t.Fatalf("Send() sent %q, want one mail with the subject %q", msgs, "Re: fix the test")
	}

	reply := "From: Alice <alice@alice.org>\r\n" +
		"Message-ID: <m9@alice.org>\r\n" +
		"In-Reply-To: <" + ref.ID + ">\r\n" +
		"Authentication-Results: mx.example.com; dmarc=pass header.from=alice.org\r\n" +
		"\r\n" +
		"thanks\r\n"
	got, err := parse(t.Context(), New(Config{Address: "coder@example.com", TrustedAuthServ: "mx.example.com"}, kv), reply)
	if err != nil {
		t.Fatalf("parse(reply) error = %v, want a message", err)
	}
	if got.Conv != in.Conv {
		t.Fatalf("parse(reply).Conv = %q, want %q", got.Conv, in.Conv)
	}
}

func TestSendToLegacyConversation(t *testing.T) {
	t.Parallel()
	srv := startSMTP(t)
	c := New(Config{Address: "coder@example.com", SMTP: srv.addr}, messengertest.NewKV(nil))
	if _, err := c.Send(t.Context(), messenger.Outbound{Conv: "alice@alice.org\troot@alice.org\tfix the test", Text: "Done."}); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	msgs := srv.messages()
	for _, want := range []string{"To: <alice@alice.org>", "Subject: Re: fix the test", "References: <root@alice.org>"} {
		if len(msgs) != 1 || !strings.Contains(msgs[0], want) {
			t.Fatalf("Send() sent %q, want one mail containing %q", msgs, want)
		}
	}
}

func TestCheckValidity(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		kv      map[string]string
		sel     imap.SelectData
		highest imap.UID
		want    map[string]string
	}{
		{
			name: "new mailbox",
			sel:  imap.SelectData{UIDValidity: 7, UIDNext: 10, NumMessages: 3},
			want: map[string]string{uidValidityKey: "7", lastUIDKey: "9"},
		},
		{
			name:    "new mailbox without UIDNEXT",
			sel:     imap.SelectData{UIDValidity: 7, NumMessages: 3},
			highest: 42,
			want:    map[string]string{uidValidityKey: "7", lastUIDKey: "42"},
		},
		{
			name: "empty mailbox without UIDNEXT",
			sel:  imap.SelectData{UIDValidity: 7},
			want: map[string]string{uidValidityKey: "7", lastUIDKey: "0"},
		},
		{
			name: "recreated mailbox",
			kv:   map[string]string{uidValidityKey: "6", lastUIDKey: "100"},
			sel:  imap.SelectData{UIDValidity: 7, UIDNext: 5, NumMessages: 3},
			want: map[string]string{uidValidityKey: "7", lastUIDKey: "4"},
		},
		{
			name: "known mailbox",
			kv:   map[string]string{uidValidityKey: "7", lastUIDKey: "3"},
			sel:  imap.SelectData{UIDValidity: 7, UIDNext: 10, NumMessages: 3},
			want: map[string]string{uidValidityKey: "7", lastUIDKey: "3"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			kv := messengertest.NewKV(tc.kv)
			c := New(Config{Address: "coder@example.com"}, kv)
			highest := func() (imap.UID, error) {
				if tc.highest == 0 {
					return 0, errors.New("unexpected lookup of the highest UID")
				}
				return tc.highest, nil
			}
			if err := c.checkValidity(t.Context(), &tc.sel, highest); err != nil {
				t.Fatalf("checkValidity() error = %v, want nil", err)
			}
			if diff := cmp.Diff(tc.want, kv.All()); diff != "" {
				t.Fatalf("checkValidity() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}
