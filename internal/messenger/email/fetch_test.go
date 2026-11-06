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
	"context"
	"errors"
	"net"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/messenger"
	"github.com/a2aproject/a2a-serve/internal/messenger/messengertest"
)

func mailWithBody(from, results, body string) string {
	return "From: " + from + "\r\n" +
		"Subject: hello\r\n" +
		"Message-ID: <" + strings.ToLower(body[:min(len(body), 10)]) + "@x>\r\n" +
		"Authentication-Results: " + results + "\r\n" +
		"\r\n" +
		body + "\r\n"
}

func TestFetchNew(t *testing.T) {
	t.Parallel()
	srv := startIMAP(t, "coder@example.com", "pw", "")
	srv.deliver(t, mailWithBody("alice@alice.org", "mx.example.com; dmarc=pass header.from=alice.org", "GENUINE1"))
	srv.deliver(t, mailWithBody("alice@alice.org", "mx.example.com; dmarc=fail header.from=alice.org", "FORGEDBODY"))
	srv.deliver(t, mailWithBody("alice@alice.org", "mx.example.com; dmarc=pass header.from=alice.org", "HUGEBODY"+strings.Repeat("x", 2000)))
	srv.deliver(t, mailWithBody("alice@alice.org", "mx.example.com; dmarc=pass header.from=alice.org", "GENUINE2"))
	kv := messengertest.NewKV(map[string]string{lastUIDKey: "0"})
	c := New(Config{Address: "coder@example.com", Password: "pw", TrustedAuthServ: "mx.example.com", MaxMessage: 1000}, kv)
	client := srv.dial(t)
	if err := c.login(t.Context(), client); err != nil {
		t.Fatalf("login() error = %v", err)
	}
	if _, err := client.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	in := make(chan messenger.Inbound, 10)

	for range 2 {
		if err := c.fetchNew(t.Context(), client, in); err != nil {
			t.Fatalf("fetchNew() error = %v", err)
		}
	}

	close(in)
	var got []string
	for m := range in {
		got = append(got, m.Text)
	}
	if diff := cmp.Diff([]string{"GENUINE1", "GENUINE2"}, got); diff != "" {
		t.Fatalf("fetchNew() delivered wrong messages (-want +got) diff = %s", diff)
	}
	if diff := cmp.Diff(map[string]string{lastUIDKey: "4"}, onlyKey(kv.All(), lastUIDKey)); diff != "" {
		t.Fatalf("fetchNew() wrong last UID (-want +got) diff = %s", diff)
	}
	for body, want := range map[string]int{"FORGEDBODY": 0, "HUGEBODY": 0, "GENUINE2": 1} {
		if got := srv.sentCount(body); got != want {
			t.Fatalf("fetchNew() read the body %s %d times, want %d", body, got, want)
		}
	}
}

func onlyKey(m map[string]string, key string) map[string]string {
	return map[string]string{key: m[key]}
}

func TestLoginXOAUTH2(t *testing.T) {
	t.Parallel()
	srv := startIMAP(t, "coder@example.com", "", "access-1")
	tokens := tokenServer(t, "refresh-1", "access-1")
	c := New(Config{
		Address: "coder@example.com", TrustedAuthServ: "mx.example.com",
		OAuth: &OAuthConfig{TokenURL: tokens.URL, ClientID: "id", ClientSecret: "secret", RefreshToken: "refresh-1"},
	}, messengertest.NewKV(nil))
	client := srv.dial(t)

	if err := c.login(t.Context(), client); err != nil {
		t.Fatalf("login() error = %v", err)
	}
	if _, err := client.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("Select() after login error = %v", err)
	}
}

func TestLoginXOAUTH2SavesRotatedRefreshToken(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		rotated string
		want    []string
	}{
		{name: "rotated token", rotated: "refresh-2", want: []string{"refresh-2"}},
		{name: "same token", rotated: "refresh-1"},
		{name: "no new token"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := startIMAP(t, "coder@example.com", "", "access-1")
			tokens := rotatingTokenServer(t, "refresh-1", "access-1", tc.rotated)
			var saved []string
			c := New(Config{
				Address: "coder@example.com", TrustedAuthServ: "mx.example.com",
				OAuth: &OAuthConfig{
					TokenURL: tokens.URL, ClientID: "id", RefreshToken: "refresh-1",
					SaveRefreshToken: func(tok string) error {
						saved = append(saved, tok)
						return nil
					},
				},
			}, messengertest.NewKV(nil))

			for range 2 {
				if err := c.login(t.Context(), srv.dial(t)); err != nil {
					t.Fatalf("login() error = %v", err)
				}
			}

			if diff := cmp.Diff(tc.want, saved); diff != "" {
				t.Fatalf("login() saved refresh tokens wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestLoginXOAUTH2RetriesSavingRotatedRefreshToken(t *testing.T) {
	t.Parallel()
	srv := startIMAP(t, "coder@example.com", "", "access-1")
	tokens := rotatingTokenServer(t, "refresh-1", "access-1", "refresh-2")
	var attempts int
	c := New(Config{
		Address: "coder@example.com", TrustedAuthServ: "mx.example.com",
		OAuth: &OAuthConfig{
			TokenURL: tokens.URL, ClientID: "id", RefreshToken: "refresh-1",
			SaveRefreshToken: func(string) error {
				attempts++
				if attempts == 1 {
					return errors.New("disk full")
				}
				return nil
			},
		},
	}, messengertest.NewKV(nil))

	for range 3 {
		if err := c.login(t.Context(), srv.dial(t)); err != nil {
			t.Fatalf("login() error = %v", err)
		}
	}

	if attempts != 2 {
		t.Fatalf("SaveRefreshToken() calls = %d, want 2", attempts)
	}
}

func TestLoginXOAUTH2RefusedToken(t *testing.T) {
	t.Parallel()
	srv := startIMAP(t, "coder@example.com", "", "access-1")
	tokens := tokenServer(t, "refresh-1", "stale")
	c := New(Config{
		Address: "coder@example.com", TrustedAuthServ: "mx.example.com",
		OAuth: &OAuthConfig{TokenURL: tokens.URL, ClientID: "id", RefreshToken: "refresh-1"},
	}, messengertest.NewKV(nil))

	if err := c.login(t.Context(), srv.dial(t)); err == nil {
		t.Fatalf("login() error = nil, want the refused token")
	}
}

// silentServer accepts connections and never answers.
func silentServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	var conns []net.Conn
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conns = append(conns, conn)
		}
	}()
	t.Cleanup(func() {
		if err := ln.Close(); err != nil {
			t.Logf("closing the listener: %v", err)
		}
		<-done
		for _, c := range conns {
			if err := c.Close(); err != nil {
				slog.Debug("closing a connection", "error", err)
			}
		}
	})
	return ln.Addr().String()
}

func TestSessionEndsWithContext(t *testing.T) {
	t.Parallel()
	c := New(Config{Address: "coder@example.com", IMAP: silentServer(t), TrustedAuthServ: "mx.example.com"}, messengertest.NewKV(nil))
	ctx, cancel := context.WithCancel(t.Context())
	errc := make(chan error, 1)
	go func() { errc <- c.session(ctx, make(chan messenger.Inbound), func(error) {}) }()
	time.Sleep(50 * time.Millisecond)

	cancel()

	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Logf("session() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("session() still runs 5s after its context ended")
	}
}

func TestRunReportsFailedSessions(t *testing.T) {
	t.Parallel()
	ln := hangUpListener(t)
	c := New(Config{Address: "coder@example.com", IMAP: ln.Addr().String(), TrustedAuthServ: "mx.example.com"}, messengertest.NewKV(nil))
	ctx, cancel := context.WithCancel(t.Context())
	reports := make(chan error, 1)
	done := make(chan error, 1)

	go func() {
		done <- c.Run(ctx, make(chan messenger.Inbound), func(err error) {
			select {
			case reports <- err:
			default:
			}
		})
	}()

	select {
	case err := <-reports:
		if err == nil {
			t.Fatalf("Run() reported %v, want the error of the session that the server hung up on", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() reported nothing after a failed session")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run() error = %v", err)
	}
}

// hangUpListener closes every connection it accepts. Its port stays taken
// until the test ends.
func hangUpListener(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	t.Cleanup(func() {
		if err := ln.Close(); err != nil {
			t.Errorf("closing the listener: %v", err)
		}
	})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			if err := conn.Close(); err != nil {
				slog.Debug("closing a connection", "error", err)
			}
		}
	}()
	return ln
}

func TestSendRetries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		refusals []string
		wantErr  bool
		wantMsgs int
	}{
		{name: "temporary failure", refusals: []string{"421 try again later"}, wantMsgs: 1},
		{name: "permanent failure", refusals: []string{"550 mailbox unavailable", ""}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := startSMTP(t, tc.refusals...)
			c := New(Config{Address: "coder@example.com", SMTP: srv.addr}, messengertest.NewKV(nil))

			_, err := c.Send(t.Context(), messenger.Outbound{Conv: ConvKey("alice@alice.org", ""), Text: "Done."})

			if gotErr := err != nil; gotErr != tc.wantErr {
				t.Fatalf("Send() error = %v, want error %v", err, tc.wantErr)
			}
			if got := len(srv.messages()); got != tc.wantMsgs {
				t.Fatalf("Send() delivered %d mails, want %d", got, tc.wantMsgs)
			}
		})
	}
}

func TestSendStopsRetryingWithContext(t *testing.T) {
	t.Parallel()
	srv := startSMTP(t, "421 a", "421 b", "421 c", "421 d")
	c := New(Config{Address: "coder@example.com", SMTP: srv.addr}, messengertest.NewKV(nil))
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()

	if _, err := c.Send(ctx, messenger.Outbound{Conv: ConvKey("alice@alice.org", ""), Text: "Done."}); err == nil {
		t.Fatalf("Send() error = nil, want the refusal")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Send() returned after %v, want soon after its context ended", d)
	}
}

func TestSendXOAUTH2(t *testing.T) {
	t.Parallel()
	srv := startSMTP(t)
	tokens := tokenServer(t, "refresh-1", "access-1")
	c := New(Config{
		Address: "coder@example.com", SMTP: srv.addr, Password: "unused",
		OAuth: &OAuthConfig{TokenURL: tokens.URL, ClientID: "id", RefreshToken: "refresh-1"},
	}, messengertest.NewKV(nil))

	if _, err := c.Send(t.Context(), messenger.Outbound{Conv: ConvKey("alice@alice.org", ""), Text: "Done."}); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	want := []string{"XOAUTH2 " + string(xoauth2Response("coder@example.com", "access-1"))}
	if diff := cmp.Diff(want, srv.authentications()); diff != "" {
		t.Fatalf("Send() wrong authentication (-want +got) diff = %s", diff)
	}
}

func TestSendPassword(t *testing.T) {
	t.Parallel()
	srv := startSMTP(t)
	c := New(Config{Address: "coder@example.com", SMTP: srv.addr, Password: "pw"}, messengertest.NewKV(nil))

	if _, err := c.Send(t.Context(), messenger.Outbound{Conv: ConvKey("alice@alice.org", ""), Text: "Done."}); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	want := []string{"PLAIN \x00coder@example.com\x00pw"}
	if diff := cmp.Diff(want, srv.authentications()); diff != "" {
		t.Fatalf("Send() wrong authentication (-want +got) diff = %s", diff)
	}
}
