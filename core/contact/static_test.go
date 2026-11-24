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

package contact

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/a2aproject/a2a-serve/internal/a2atext"
)

const secretAuth = "Bearer s3cret-token"

// syncBuffer is a buffer that the logger and the test share.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// startTLSContact starts a fake contact over https.
func startTLSContact(t *testing.T) *fakeContact {
	t.Helper()
	srv := httptest.NewUnstartedServer(nil)
	fc := newFakeContact(t, srv, nil)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	fc.URL = srv.URL
	fc.setCard(fakeCard(srv.URL))
	return fc
}

// startUnixContact starts a fake contact on a Unix socket, and returns it
// with the socket's URL.
func startUnixContact(t *testing.T) (*fakeContact, string) {
	t.Helper()
	// Socket paths are short, so the socket goes in a short temporary
	// directory.
	dir, err := os.MkdirTemp("", "ct")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("removing %s: %v", dir, err)
		}
	})
	path := filepath.Join(dir, "c.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(nil)
	srv.Listener = ln
	fc := newFakeContact(t, srv, nil)
	srv.Start()
	t.Cleanup(srv.Close)
	fc.setCard(fakeCard("http://planner.internal"))
	return fc, "unix://" + path
}

func newTestStatic(t *testing.T, cfg StaticConfig) (*Static, *syncBuffer) {
	t.Helper()
	logs := &syncBuffer{}
	cfg.Log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	s, err := NewStatic(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewStatic() error = %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Static.Close() error = %v", err)
		}
	})
	return s, logs
}

func TestStaticCallsContactsThroughTheProxy(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		start func(t *testing.T) (*fakeContact, Entry, http.RoundTripper)
	}{
		{name: "https contact", start: func(t *testing.T) (*fakeContact, Entry, http.RoundTripper) {
			fc := startTLSContact(t)
			return fc, Entry{Name: "reviewer", URL: fc.URL, Auth: secretAuth}, fc.srv.Client().Transport
		}},
		{name: "unix contact", start: func(t *testing.T) (*fakeContact, Entry, http.RoundTripper) {
			fc, u := startUnixContact(t)
			return fc, Entry{Name: "reviewer", URL: u, Auth: secretAuth}, nil
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			fc, entry, rt := tc.start(t)
			s, logs := newTestStatic(t, StaticConfig{Contacts: []Entry{entry}, Transport: rt})
			p, err := New(Config{Turns: fakeTurns{turn: Turn{ContextID: "outer"}}, Directory: s})
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			srv := httptest.NewServer(p)
			t.Cleanup(srv.Close)

			wantStatus := []Status{{Name: "reviewer", Ready: true, Card: shorten(fc.Card)}}
			if diff := cmp.Diff(wantStatus, s.Status(), cmpopts.IgnoreFields(Status{}, "Checked")); diff != "" {
				t.Fatalf("Static.Status() wrong result (-want +got) diff = %s", diff)
			}
			_, card, _ := getCard(t, srv.Client(), srv.URL+"/reviewer")
			if want := `reviewer, called through the server hosting you. It says of itself: "Reviews code."`; card.Description != want {
				t.Fatalf("card description = %q, want %q", card.Description, want)
			}
			client := proxyClient(t, srv.Client(), srv.URL+"/reviewer", a2a.TransportProtocolJSONRPC)
			res, err := client.SendMessage(ctx, &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(promptDone))})
			if err != nil {
				t.Fatalf("SendMessage() error = %v", err)
			}
			task, ok := res.(*a2a.Task)
			if !ok || a2atext.Result(task) != fakeResult {
				t.Fatalf("SendMessage() = %+v, want the contact's result", res)
			}
			for _, r := range fc.received() {
				if r.Authorization != secretAuth {
					t.Fatalf("request to the contact at %s has Authorization %q, want the auth of the contact", r.Path, r.Authorization)
				}
			}
			if strings.Contains(logs.String(), "s3cret") {
				t.Fatalf("logs = %s, want no auth header", logs)
			}
		})
	}
}

func TestStaticSendsAuthOnlyToTheCardsHost(t *testing.T) {
	t.Parallel()
	elsewhere := startFakeContact(t)
	fc := startTLSContact(t)
	fc.setCard(fakeCard(elsewhere.URL))
	s, _ := newTestStatic(t, StaticConfig{Contacts: []Entry{{Name: "reviewer", URL: fc.URL, Auth: secretAuth}}, Transport: fc.srv.Client().Transport})
	target, err := s.Target(context.Background(), Turn{}, "reviewer")
	if err != nil {
		t.Fatalf("Static.Target() error = %v", err)
	}

	if _, err := Dial(context.Background(), Turn{}, target, a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(promptDone))); err != nil {
		t.Fatalf("Dial() error = %v", err)
	}

	got := elsewhere.received()
	if len(got) == 0 {
		t.Fatalf("requests to the card's interface = none, want the call")
	}
	for _, r := range got {
		if r.Authorization != "" {
			t.Fatalf("request to another host has Authorization %q, want none", r.Authorization)
		}
	}
}

func TestStaticRefusesAContactWhoseCardFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fc := startTLSContact(t)
	fc.setCard(nil)
	s, logs := newTestStatic(t, StaticConfig{
		Contacts:  []Entry{{Name: "reviewer", URL: fc.URL + "/card.json?token=url-secret", Auth: secretAuth}},
		Transport: fc.srv.Client().Transport,
		Retry:     10 * time.Millisecond,
	})

	st := s.Status()
	if len(st) != 1 || st[0].Ready || !strings.Contains(st[0].Reason, "503") {
		t.Fatalf("Static.Status() = %+v, want the contact failed with its status", st)
	}
	if strings.Contains(st[0].Reason, "url-secret") {
		t.Fatalf("Static.Status() reason = %q, want the URL's query left out", st[0].Reason)
	}
	if _, err := s.Card(ctx, Turn{}, "reviewer"); !errors.Is(err, ErrRefused) {
		t.Fatalf("Static.Card() of a failed contact error = %v, want %v", err, ErrRefused)
	}
	if _, err := s.Target(ctx, Turn{}, "reviewer"); !errors.Is(err, ErrRefused) {
		t.Fatalf("Static.Target() of a failed contact error = %v, want %v", err, ErrRefused)
	}

	fc.setCard(fakeCard(fc.URL))

	waitFor(t, "the card to be fetched again", func() bool { return s.Status()[0].Ready })
	if _, err := s.Card(ctx, Turn{}, "reviewer"); err != nil {
		t.Fatalf("Static.Card() after a fetch succeeded error = %v", err)
	}
	if l := logs.String(); !strings.Contains(l, "fetching the card of a contact") || strings.Contains(l, "s3cret") || strings.Contains(l, "url-secret") {
		t.Fatalf("logs = %s, want the failed fetch without secrets", l)
	}
}

func TestStaticRefreshesCards(t *testing.T) {
	t.Parallel()
	fc := startTLSContact(t)
	s, _ := newTestStatic(t, StaticConfig{Contacts: []Entry{{Name: "reviewer", URL: fc.URL}}, Transport: fc.srv.Client().Transport, Refresh: 10 * time.Millisecond})
	card := fakeCard(fc.URL)
	card.Description = "Plans work."

	fc.setCard(card)

	waitFor(t, "the new card", func() bool {
		st := s.Status()[0]
		return st.Card != nil && st.Card.Description == "Plans work."
	})
}

func TestStaticRefusesUnknownNames(t *testing.T) {
	t.Parallel()
	s, _ := newTestStatic(t, StaticConfig{})

	_, err := s.Card(context.Background(), Turn{}, "nobody")

	if !errors.Is(err, ErrRefused) {
		t.Fatalf("Static.Card() of an unknown name error = %v, want %v", err, ErrRefused)
	}
}

func TestNewStaticRefusesInvalidContacts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		contacts []Entry
		want     string
	}{
		{name: "plain http", contacts: []Entry{{Name: "a", URL: "http://a.example"}}, want: "must start with https://, loopback http://, or unix:///"},
		{name: "file", contacts: []Entry{{Name: "a", URL: "file:///etc/card.json"}}, want: "must start with https://, loopback http://, or unix:///"},
		{name: "relative socket", contacts: []Entry{{Name: "a", URL: "unix://run/a.sock"}}, want: "must start with https://, loopback http://, or unix:///"},
		{name: "invalid name", contacts: []Entry{{Name: "a b", URL: "https://a.example"}}, want: "invalid name"},
		{name: "same name twice", contacts: []Entry{{Name: "a", URL: "https://a.example"}, {Name: "A", URL: "https://b.example"}}, want: "listed twice"},
		{name: "auth with a line break", contacts: []Entry{{Name: "a", URL: "https://a.example", Auth: "Bearer x\r\nX-Evil: 1"}}, want: "line break"},
		{name: "header with a line break", contacts: []Entry{{Name: "a", URL: "http://127.0.0.1:8080", Headers: http.Header{"A2A-Sidecar-Token": {"x\r\nX-Evil: 1"}}}}, want: "line break"},
		{name: "too many", contacts: make([]Entry, MaxContacts+1), want: "at most"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewStatic(context.Background(), StaticConfig{Contacts: tc.contacts})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("NewStatic() error = %v, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "X-Evil") {
				t.Fatalf("NewStatic() error = %v, want the auth value left out", err)
			}
		})
	}
}

// authRecorder answers every request and records its Authorization header.
type authRecorder struct {
	mu   sync.Mutex
	auth map[string]string
}

func (r *authRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.auth[req.URL.String()] = req.Header.Get("Authorization")
	r.mu.Unlock()
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: req}, nil
}

func TestStaticSendsAuthOnlyOverHTTPSToTheCardsHost(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		contact string
		request string
		want    string
	}{
		{name: "card's host", contact: "https://a.example/card.json", request: "https://a.example/", want: secretAuth},
		{name: "explicit default port", contact: "https://a.example:443/card.json", request: "https://a.example/", want: secretAuth},
		{name: "explicit default port of the request", contact: "https://a.example/card.json", request: "https://a.example:443/", want: secretAuth},
		{name: "plain http to the card's host", contact: "https://a.example/card.json", request: "http://a.example/"},
		{name: "plain http on the https port", contact: "https://a.example/card.json", request: "http://a.example:443/"},
		{name: "another port", contact: "https://a.example/card.json", request: "https://a.example:8443/"},
		{name: "another host", contact: "https://a.example/card.json", request: "https://b.example/"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := &authRecorder{auth: map[string]string{}}
			c, err := newStaticContact(Entry{Name: "a", URL: tc.contact, Auth: secretAuth}, rec)
			if err != nil {
				t.Fatalf("newStaticContact() error = %v", err)
			}
			req, err := http.NewRequest(http.MethodGet, tc.request, nil)
			if err != nil {
				t.Fatal(err)
			}

			resp, err := c.http.Do(req)
			if err != nil {
				t.Fatalf("GET %s error = %v", tc.request, err)
			}
			if err := resp.Body.Close(); err != nil {
				t.Fatal(err)
			}

			if got := rec.auth[tc.request]; got != tc.want {
				t.Fatalf("Authorization of GET %s = %q, want %q", tc.request, got, tc.want)
			}
		})
	}
}

func TestStaticSendsHeadersOnlyToLoopbackContactsOrigin(t *testing.T) {
	t.Parallel()
	const token = "loopback-host-token"
	tests := []struct {
		name    string
		contact string
		request string
		want    string
	}{
		{name: "same loopback origin", contact: "http://127.0.0.1:8080/card.json", request: "http://127.0.0.1:8080/", want: token},
		{name: "different port", contact: "http://127.0.0.1:8080/card.json", request: "http://127.0.0.1:9090/"},
		{name: "remote host", contact: "http://127.0.0.1:8080/card.json", request: "https://b.example/"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got string
			rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
				got = req.Header.Get("A2A-Sidecar-Token")
				return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: req}, nil
			})
			c, err := newStaticContact(Entry{
				Name:    "host",
				URL:     tc.contact,
				Headers: http.Header{"A2A-Sidecar-Token": {token}},
			}, rt)
			if err != nil {
				t.Fatalf("newStaticContact() error = %v", err)
			}
			req, err := http.NewRequest(http.MethodGet, tc.request, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := c.http.Do(req)
			if err != nil {
				t.Fatalf("GET %s error = %v", tc.request, err)
			}
			if err := resp.Body.Close(); err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("A2A-Sidecar-Token of GET %s = %q, want %q", tc.request, got, tc.want)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
