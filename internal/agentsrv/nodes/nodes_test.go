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

package nodes

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-serve/internal/egress"
	"github.com/a2aproject/a2a-serve/internal/mailboxapi"
	"github.com/a2aproject/a2a-serve/internal/store"
)

func TestInstructionsKeepTheNodeToken(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{mailboxapi.KindTask, mailboxapi.KindMessage} {
		got := instructions(mailboxapi.Envelope{Kind: kind, TaskID: "t1", ReplyURL: "https://agent.example.com/mailbox/api/tasks/t1/events"})
		if strings.Contains(got, "token") || strings.Contains(got, "replyUrl") {
			t.Fatalf("instructions(%s) = %q, want no mention of the node token or replyUrl", kind, got)
		}
		if !strings.Contains(got, "mailbox reply t1") {
			t.Fatalf("instructions(%s) = %q, want the reply command", kind, got)
		}
	}
}

func TestRepeatedEventInLaterTurn(t *testing.T) {
	t.Parallel()
	h := New(t.Context(), Config{})
	node := store.Node{ID: "grok"}
	ev := mailboxapi.Event{IdempotencyKey: "k1", State: "input-required", Text: "which one?"}

	post := func() mailboxapi.EventResult {
		t.Helper()
		body, err := json.Marshal(ev)
		if err != nil {
			t.Fatalf("json.Marshal() error = %v", err)
		}
		r := httptest.NewRequest(http.MethodPost, "/mailbox/api/tasks/t1/events", bytes.NewReader(body))
		r.SetPathValue("id", "t1")
		w := httptest.NewRecorder()
		h.handleEvent(w, r, "", node)
		if w.Code != http.StatusOK {
			t.Fatalf("handleEvent() status = %d, want %d: %s", w.Code, http.StatusOK, w.Body)
		}
		var res mailboxapi.EventResult
		if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
			t.Fatalf("json.Unmarshal() error = %v", err)
		}
		return res
	}
	turn := func(wantDuplicate bool) {
		t.Helper()
		w := h.openTask("t1", node.ID)
		defer h.closeTask("t1", w)
		go func() {
			select {
			case d := <-w.events:
				d.result <- nil
			case <-w.done:
			}
		}()
		if got := post(); got.Duplicate {
			t.Fatalf("handleEvent() Duplicate = %v, want false", got.Duplicate)
		}
		if got := post(); got.Duplicate != wantDuplicate {
			t.Fatalf("handleEvent() retried Duplicate = %v, want %v", got.Duplicate, wantDuplicate)
		}
	}
	turn(true)
	turn(true)
}

func TestPostWebhookErrorHidesSecrets(t *testing.T) {
	t.Parallel()
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad key s3cret", http.StatusUnauthorized)
	}))
	t.Cleanup(failing.Close)
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	tests := []struct {
		name    string
		webhook string
	}{
		{name: "error status", webhook: failing.URL + "/hook?key=s3cret"},
		{name: "unreachable", webhook: closed.URL + "/hook?key=s3cret"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := postWebhook(t.Context(), store.Node{ID: "grok", Webhook: tc.webhook}, "wake_1", []byte("{}"))
			if err == nil || strings.Contains(err.Error(), "s3cret") {
				t.Fatalf("postWebhook() error = %v, want an error without the secret", err)
			}
		})
	}
}

func TestPostWebhookRedirects(t *testing.T) {
	t.Parallel()
	type result struct {
		header http.Header
		body   string
	}
	target := func(t *testing.T) (*httptest.Server, chan result) {
		t.Helper()
		got := make(chan result, 1)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("reading the redirected wake: %v", err)
			}
			got <- result{header: r.Header.Clone(), body: string(body)}
		}))
		t.Cleanup(srv.Close)
		return srv, got
	}
	tests := []struct {
		name        string
		sameHost    bool
		wantHeaders bool
	}{
		{name: "same host keeps the headers", sameHost: true, wantHeaders: true},
		{name: "other host drops the headers"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			final, got := target(t)
			mux := http.NewServeMux()
			origin := httptest.NewServer(mux)
			t.Cleanup(origin.Close)
			mux.HandleFunc("/hook", func(w http.ResponseWriter, r *http.Request) {
				dest := final.URL + "/final"
				if tc.sameHost {
					dest = origin.URL + "/final"
				}
				http.Redirect(w, r, dest, http.StatusTemporaryRedirect)
			})
			mux.HandleFunc("/final", func(w http.ResponseWriter, r *http.Request) {
				final.Config.Handler.ServeHTTP(w, r)
			})
			node := store.Node{ID: "grok", Secret: "s", Webhook: origin.URL + "/hook", Headers: map[string]string{"Authorization": "Bearer hook", "X-Api-Key": "k"}}

			if err := postWebhook(t.Context(), node, "wake_1", []byte(`{"a":1}`)); err != nil {
				t.Fatalf("postWebhook() error = %v", err)
			}

			res := <-got
			if res.body != `{"a":1}` {
				t.Fatalf("redirected wake body = %q, want it kept", res.body)
			}
			if got := res.header.Get(mailboxapi.WakeHeader); got != "wake_1" {
				t.Fatalf("redirected wake %s = %q, want wake_1", mailboxapi.WakeHeader, got)
			}
			for _, name := range []string{"Authorization", "X-Api-Key", mailboxapi.SignatureHeader} {
				if has := res.header.Get(name) != ""; has != tc.wantHeaders {
					t.Fatalf("redirected wake has %s = %v, want %v", name, has, tc.wantHeaders)
				}
			}
		})
	}
}

func TestWakeClientRefusesRedirectFromHTTPSToHTTP(t *testing.T) {
	t.Parallel()
	req := func(u string) *http.Request {
		t.Helper()
		parsed, err := url.Parse(u)
		if err != nil {
			t.Fatalf("url.Parse(%q) error = %v", u, err)
		}
		return &http.Request{URL: parsed, Header: http.Header{}}
	}
	c := wakeClient(store.Node{ID: "grok", Webhook: "https://hook.example/wake"})

	err := c.CheckRedirect(req("http://hook.example/wake"), []*http.Request{req("https://hook.example/wake")})

	if !errors.Is(err, egress.ErrInsecureRedirect) {
		t.Fatalf("wakeClient().CheckRedirect() error = %v, want %v", err, egress.ErrInsecureRedirect)
	}
}

func TestWakeLastErrorIsGeneric(t *testing.T) {
	t.Parallel()
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad key", http.StatusUnauthorized)
	}))
	t.Cleanup(failing.Close)
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	tests := []struct {
		name    string
		webhook string
		want    string
	}{
		{name: "error status", webhook: failing.URL + "/hook", want: "webhook returned 401 Unauthorized"},
		{name: "unreachable", webhook: closed.URL + "/hook", want: errWakeUnreachable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			st, err := store.Open(filepath.Join(t.TempDir(), "server.db"))
			if err != nil {
				t.Fatalf("store.Open() error = %v", err)
			}
			t.Cleanup(func() {
				if err := st.Close(); err != nil {
					t.Errorf("Close() error = %v", err)
				}
			})
			if err := st.PutNode(t.Context(), store.Node{ID: "grok", Name: "grok", Webhook: tc.webhook, Secret: "s"}); err != nil {
				t.Fatalf("store.PutNode() error = %v", err)
			}
			h := New(t.Context(), Config{Store: st, WebhookRetries: []time.Duration{0}, BaseURL: func() string { return "http://agent.example" }})

			entry := h.wake("grok", mailboxapi.Envelope{Kind: mailboxapi.KindPing})

			deadline := time.Now().Add(10 * time.Second)
			for {
				h.mu.Lock()
				attempts, got := entry.Attempts, entry.LastError
				h.mu.Unlock()
				if attempts > 0 {
					if got != tc.want {
						t.Fatalf("wake LastError = %q, want %q", got, tc.want)
					}
					return
				}
				if time.Now().After(deadline) {
					t.Fatalf("wake made no attempt within 10s")
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}
