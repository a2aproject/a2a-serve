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
	"errors"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/channel"
	"github.com/a2aproject/a2a-serve/internal/channel/channeltest"
)

func TestAuthentic(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		results []string
		want    bool
	}{
		{name: "dmarc pass", results: []string{"mx.example.com; dkim=pass header.d=other.org; dmarc=pass header.from=alice.org"}, want: true},
		{name: "aligned subdomain dkim", results: []string{"mx.example.com; dkim=pass header.d=mail.alice.org"}, want: true},
		{name: "aligned dkim", results: []string{"mx.example.com; dkim=pass header.d=alice.org; dmarc=none"}, want: true},
		{name: "dkim of a parent domain", results: []string{"mx.example.com; dkim=pass header.d=org"}, want: false},
		{name: "unaligned dkim", results: []string{"mx.example.com; dkim=pass header.d=evil.com; dmarc=fail"}, want: false},
		{name: "untrusted server", results: []string{"evil.com; dmarc=pass"}, want: false},
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
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Authentic(tc.results, "mx.example.com", "alice.org"); got != tc.want {
				t.Fatalf("Authentic(%q) = %v, want %v", tc.results, got, tc.want)
			}
		})
	}
}

func TestStripQuoted(t *testing.T) {
	t.Parallel()
	in := "approve\r\n\r\nOn Mon, 1 Jan 2026, coder wrote:\r\n> coder wants to run git push\r\n"
	if got := StripQuoted(in); got != "approve" {
		t.Fatalf("StripQuoted() = %q, want %q", got, "approve")
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

func TestParse(t *testing.T) {
	t.Parallel()
	c := New(Config{Address: "coder@example.com", TrustedAuthServ: "mx.example.com"}, nil)
	got, ok, err := c.parse([]byte(rawMail))
	if err != nil || !ok {
		t.Fatalf("parse() = %v, %v, want a message", ok, err)
	}
	want := channel.Inbound{
		Channel: "email", MessageID: "alice@alice.org|m2@alice.org", Identity: "alice@alice.org", Name: "Alice",
		Conv: ConvKey("alice@alice.org", "root@alice.org", "Re: fix the test"), Direct: true, Authentic: true,
		Text: "Please push it.",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("parse() wrong result (-want +got) diff = %s", diff)
	}

	for name, header := range map[string]string{
		"auto-reply":      "Auto-Submitted: auto-replied\r\n",
		"list":            "List-Id: <dev.lists.example.com>\r\n",
		"own mail":        "",
		"several senders": "",
	} {
		raw := header + rawMail
		switch name {
		case "own mail":
			raw = strings.Replace(rawMail, "Alice <Alice@alice.org>", "coder@example.com", 1)
		case "several senders":
			raw = strings.Replace(rawMail, "Alice <Alice@alice.org>", "Alice <alice@alice.org>, x@evil.com", 1)
		}
		if _, ok, err := c.parse([]byte(raw)); err != nil || ok {
			t.Errorf("parse(%s) = %v, %v, want dropped", name, ok, err)
		}
	}
}

func TestWriteMessage(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := writeMessage(&buf, mailHeader("coder@example.com", "alice@alice.org"), channel.Outbound{
		Text: "Done.", Files: []channel.File{{Name: "a.txt", MediaType: "text/plain", Data: []byte("x")}},
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
	c := New(Config{Address: "coder@example.com", TrustedAuthServ: "mx.example.com"}, nil)
	first := "From: Alice <alice@alice.org>\r\n" +
		"To: coder@example.com\r\n" +
		"Subject: fix the test\r\n" +
		"Message-ID: <root@alice.org>\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"Please fix it.\r\n"
	want, ok, err := c.parse([]byte(first))
	if err != nil || !ok {
		t.Fatalf("parse(first) = %v, %v, want a message", ok, err)
	}
	for _, subject := range []string{
		"Re: fix the test",
		"RE: Re: fix the test",
		"Re[2]: fix the test",
		"Re:fix the test",
		"Fwd: fix the test",
		"FW: fix the test",
		"AW: fix the test",
		"Aw: WG: fix the test",
		"SV: fix the test",
	} {
		t.Run(subject, func(t *testing.T) {
			t.Parallel()
			reply := "From: Alice <alice@alice.org>\r\n" +
				"To: coder@example.com\r\n" +
				"Subject: " + subject + "\r\n" +
				"Message-ID: <m3@alice.org>\r\n" +
				"In-Reply-To: <m2@example.com>\r\n" +
				"References: <root@alice.org> <m2@example.com>\r\n" +
				"Content-Type: text/plain; charset=utf-8\r\n" +
				"\r\n" +
				"approve\r\n"
			got, ok, err := c.parse([]byte(reply))
			if err != nil || !ok {
				t.Fatalf("parse(reply) = %v, %v, want a message", ok, err)
			}
			if got.Conv != want.Conv {
				t.Fatalf("parse(reply).Conv = %q, want %q", got.Conv, want.Conv)
			}
		})
	}
}

func TestParseDedupKey(t *testing.T) {
	t.Parallel()
	c := New(Config{Address: "coder@example.com", TrustedAuthServ: "mx.example.com"}, nil)
	genuine, ok, err := c.parse([]byte(rawMail))
	if err != nil || !ok {
		t.Fatalf("parse(genuine) = %v, %v, want a message", ok, err)
	}
	tests := []struct {
		name string
		raw  string
	}{
		{name: "forged sender", raw: strings.Replace(rawMail, "dmarc=pass", "dmarc=fail", 1)},
		{name: "another sender", raw: strings.NewReplacer("Alice@alice.org", "mallory@evil.com", "alice.org\r\n", "evil.com\r\n").Replace(rawMail)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok, err := c.parse([]byte(tc.raw))
			if err != nil || !ok {
				t.Fatalf("parse() = %v, %v, want a message", ok, err)
			}
			if got.MessageID == genuine.MessageID {
				t.Fatalf("parse().MessageID = %q, want it to differ from the genuine message's", got.MessageID)
			}
		})
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
			kv := channeltest.NewKV(tc.kv)
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
