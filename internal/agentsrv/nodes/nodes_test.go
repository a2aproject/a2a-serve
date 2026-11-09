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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/a2aproject/a2a-serve/internal/a2alimit"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/egress"
	"github.com/a2aproject/a2a-serve/internal/mailboxapi"
	"github.com/a2aproject/a2a-serve/internal/store"
)

func viewer(ctx context.Context) (string, bool, error) {
	if p, ok := auth.Caller(ctx); ok {
		return p.ID, p.IsOwner(), nil
	}
	return "", true, nil
}

func newHost(t *testing.T) *Host {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	h := New(t.Context(), Config{
		Store:          st,
		Viewer:         viewer,
		Runtime:        agentdir.NewRuntime(agentdir.Dir{}, &agentdir.Config{Name: "assistant"}, nil),
		BaseURL:        func() string { return "https://assistant.example.com" },
		WebhookRetries: []time.Duration{0},
		Advertised:     func(string) bool { return true },
	})
	t.Cleanup(h.Stop)
	return h
}

func openTask(t *testing.T, h *Host, id a2a.TaskID) *nodeWaiter {
	t.Helper()
	w, err := h.openTask(id, "grok", "", false)
	if err != nil {
		t.Fatalf("openTask() error = %v", err)
	}
	return w
}

func postEvent(t *testing.T, h *Host, node string, taskID a2a.TaskID, ev mailboxapi.Event) (int, mailboxapi.EventResult) {
	t.Helper()
	body, err := json.Marshal(ev)
	if err != nil {
		t.Errorf("json.Marshal() error = %v", err)
		return 0, mailboxapi.EventResult{}
	}
	r := httptest.NewRequest(http.MethodPost, "/mailbox/api/tasks/"+string(taskID)+"/events", bytes.NewReader(body))
	r.SetPathValue("id", string(taskID))
	w := httptest.NewRecorder()
	h.handleEvent(w, r, "", store.Node{ID: node})
	var res mailboxapi.EventResult
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
			t.Errorf("json.Unmarshal() error = %v", err)
		}
	}
	return w.Code, res
}

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

func TestEventOverMessageLimitsRefused(t *testing.T) {
	t.Parallel()
	manyParts := make([]*a2a.Part, a2alimit.MaxParts)
	for i := range manyParts {
		manyParts[i] = a2a.NewTextPart("p")
	}
	tests := []struct {
		name    string
		ev      mailboxapi.Event
		wantErr string
	}{
		{
			name:    "too many parts",
			ev:      mailboxapi.Event{State: "completed", Text: "done", Parts: manyParts},
			wantErr: fmt.Sprintf("a reply may have at most %d parts, got %d", a2alimit.MaxParts, a2alimit.MaxParts+1),
		},
		{
			name:    "too much text in artifacts",
			ev:      mailboxapi.Event{State: "working", Artifacts: []*a2a.Artifact{{Parts: []*a2a.Part{a2a.NewTextPart(strings.Repeat("a", a2alimit.MaxText))}}, {Parts: []*a2a.Part{a2a.NewTextPart("b")}}}},
			wantErr: fmt.Sprintf("the text of a reply may have at most %d bytes, got %d", a2alimit.MaxText, a2alimit.MaxText+1),
		},
		{
			name:    "too much metadata",
			ev:      mailboxapi.Event{State: "working", Artifacts: []*a2a.Artifact{{Parts: []*a2a.Part{a2a.NewTextPart("a")}, Metadata: map[string]any{"k": strings.Repeat("m", a2alimit.MaxMetadata)}}}},
			wantErr: fmt.Sprintf("the metadata of a reply and its parts may have at most %d bytes, got %d", a2alimit.MaxMetadata, a2alimit.MaxMetadata+8),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := New(t.Context(), Config{})
			body, err := json.Marshal(tc.ev)
			if err != nil {
				t.Fatalf("json.Marshal() error = %v", err)
			}
			r := httptest.NewRequest(http.MethodPost, "/mailbox/api/tasks/t1/events", bytes.NewReader(body))
			r.SetPathValue("id", "t1")
			w := httptest.NewRecorder()

			h.handleEvent(w, r, "", store.Node{ID: "grok"})

			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), tc.wantErr) {
				t.Fatalf("handleEvent() = %d %s, want %d with %q", w.Code, w.Body, http.StatusBadRequest, tc.wantErr)
			}
		})
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
		w := openTask(t, h, "t1")
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

func TestTasksFailsInterruptedTasksOfCanceledRequest(t *testing.T) {
	t.Parallel()
	h := newHost(t)
	owner := auth.WithPrincipal(t.Context(), auth.Owner())
	task := &a2a.Task{ID: "t1", ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateWorking}}
	if _, err := h.cfg.Store.Tasks(nodeScope("grok"), viewer).Create(owner, task); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	h.tasks(ctx, "grok")

	got, err := h.tasks(t.Context(), "grok").Get(owner, "t1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Task.Status.State != a2a.TaskStateFailed {
		t.Fatalf("Get() state = %v, want %v", got.Task.Status.State, a2a.TaskStateFailed)
	}
}

func TestRetryWaitsForFirstEvent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		firstErr      error
		wantFirst     int
		wantDuplicate bool
		wantApplied   int
	}{
		{name: "first applied", wantFirst: http.StatusOK, wantDuplicate: true, wantApplied: 1},
		{name: "first failed", firstErr: errors.New("boom"), wantFirst: http.StatusConflict, wantApplied: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := New(t.Context(), Config{})
			w := openTask(t, h, "t1")
			defer h.closeTask("t1", w)
			ev := mailboxapi.Event{IdempotencyKey: "k1", State: "working", Text: "on it"}

			first := make(chan int, 1)
			go func() {
				code, _ := postEvent(t, h, "grok", "t1", ev)
				first <- code
			}()
			d := <-w.events
			type result struct {
				code int
				res  mailboxapi.EventResult
			}
			retry := make(chan result, 1)
			go func() {
				code, res := postEvent(t, h, "grok", "t1", ev)
				retry <- result{code, res}
			}()
			select {
			case r := <-retry:
				t.Fatalf("handleEvent() retry = %d, %+v before the first event was applied, want it to wait", r.code, r.res)
			case <-time.After(100 * time.Millisecond):
			}
			d.result <- tc.firstErr
			if got := <-first; got != tc.wantFirst {
				t.Fatalf("handleEvent() first status = %d, want %d", got, tc.wantFirst)
			}
			applied := 1
			select {
			case d := <-w.events:
				applied++
				d.result <- nil
			case r := <-retry:
				retry <- r
			}
			r := <-retry
			if r.code != http.StatusOK || r.res.Duplicate != tc.wantDuplicate {
				t.Fatalf("handleEvent() retry = %d, Duplicate %v, want %d, Duplicate %v", r.code, r.res.Duplicate, http.StatusOK, tc.wantDuplicate)
			}
			if applied != tc.wantApplied {
				t.Fatalf("events applied = %d, want %d", applied, tc.wantApplied)
			}
		})
	}
}

func TestForgetDropsNode(t *testing.T) {
	t.Parallel()
	h := newHost(t)
	handler := h.Handler("grok")
	h.wake("grok", mailboxapi.Envelope{Kind: mailboxapi.KindPing})
	w := openTask(t, h, "t1")
	defer h.closeTask("t1", w)

	h.Forget("grok")

	select {
	case <-w.done:
	default:
		t.Fatalf("Forget() kept the open task waiting")
	}
	if waiting, open := h.Mailbox("grok"); waiting != 0 || open != 0 {
		t.Fatalf("Mailbox() after Forget() = %d, %d, want 0, 0", waiting, open)
	}
	r := httptest.NewRequest(http.MethodGet, "/mailbox/api/inbox?all=1", nil)
	rec := httptest.NewRecorder()
	h.handleInbox(rec, r, "", store.Node{ID: "grok"})
	var inbox mailboxapi.Inbox
	if err := json.Unmarshal(rec.Body.Bytes(), &inbox); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if len(inbox.Entries) != 0 {
		t.Fatalf("handleInbox() after Forget() = %+v, want no entries", inbox.Entries)
	}
	if h.Handler("grok") == handler {
		t.Fatalf("Handler() after Forget() returned the handler of the removed node")
	}
}

func TestOpenTaskCaps(t *testing.T) {
	t.Parallel()
	h := newHost(t)
	send := func(p auth.Principal) error {
		ctx := auth.WithPrincipal(t.Context(), p)
		msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi"))
		_, err := h.Handler("grok").SendMessage(ctx, &a2a.SendMessageRequest{Message: msg, Config: &a2a.SendMessageConfig{ReturnImmediately: true}})
		return err
	}
	caller := func(i int) auth.Principal {
		return auth.Principal{ID: fmt.Sprintf("pair:caller%d", i), Role: auth.RoleMember}
	}
	for i := range maxNodeOpenTasks / maxCallerOpenTasks {
		for range maxCallerOpenTasks {
			if err := send(caller(i)); err != nil {
				t.Fatalf("SendMessage() as %s error = %v", caller(i).ID, err)
			}
		}
		if err := send(caller(i)); err == nil {
			t.Fatalf("SendMessage() as %s past its cap error = nil, want an error", caller(i).ID)
		}
	}
	if err := send(caller(maxNodeOpenTasks)); err == nil {
		t.Fatalf("SendMessage() past the node cap error = nil, want an error")
	}
	if err := send(auth.Owner()); err != nil {
		t.Fatalf("SendMessage() as owner error = %v", err)
	}

	var finished a2a.TaskID
	h.mu.Lock()
	for id, w := range h.open {
		if w.caller == caller(0).ID {
			finished = id
			break
		}
	}
	h.mu.Unlock()
	if code, _ := postEvent(t, h, "grok", finished, mailboxapi.Event{State: "completed", Text: "done"}); code != http.StatusOK {
		t.Fatalf("handleEvent() status = %d, want %d", code, http.StatusOK)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := send(caller(0))
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("SendMessage() after a task finished error = %v, want nil", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, open := h.Mailbox("grok"); open != maxNodeOpenTasks+1 {
		t.Fatalf("Mailbox() open = %d, want %d", open, maxNodeOpenTasks+1)
	}
}

// unreachableWebhook is a URL nothing answers at. The port of a closed
// server can go to a listener of another test, so it is port 1, which
// nothing listens on.
const unreachableWebhook = "http://127.0.0.1:1"

func TestPostWebhookErrorHidesSecrets(t *testing.T) {
	t.Parallel()
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad key s3cret", http.StatusUnauthorized)
	}))
	t.Cleanup(failing.Close)
	tests := []struct {
		name    string
		webhook string
	}{
		{name: "error status", webhook: failing.URL + "/hook?key=s3cret"},
		{name: "unreachable", webhook: unreachableWebhook + "/hook?key=s3cret"},
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
	tests := []struct {
		name    string
		webhook string
		want    string
	}{
		{name: "error status", webhook: failing.URL + "/hook", want: "webhook returned 401 Unauthorized"},
		{name: "unreachable", webhook: unreachableWebhook + "/hook", want: errWakeUnreachable},
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
			t.Cleanup(h.Stop)

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

func TestStopWaitsForWakeDeliveries(t *testing.T) {
	t.Parallel()
	arrived := make(chan struct{})
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Errorf("reading the wake error = %v", err)
		}
		close(arrived)
		<-r.Context().Done()
	}))
	defer hook.Close()
	st, err := store.Open(filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	if err := st.PutNode(t.Context(), store.Node{ID: "grok", Name: "grok", Webhook: hook.URL, Secret: "s"}); err != nil {
		t.Fatalf("store.PutNode() error = %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	h := New(ctx, Config{Store: st, WebhookRetries: []time.Duration{0}, BaseURL: func() string { return "http://agent.example" }})
	entry := h.wake("grok", mailboxapi.Envelope{Kind: mailboxapi.KindPing})
	<-arrived

	cancel()
	h.Stop()

	h.mu.Lock()
	attempts := entry.Attempts
	h.mu.Unlock()
	if attempts != 1 {
		t.Fatalf("wake attempts after Stop() = %d, want 1", attempts)
	}
	if got := h.wake("grok", mailboxapi.Envelope{Kind: mailboxapi.KindPing}); got.Attempts != 0 {
		t.Fatalf("wake after Stop() attempts = %d, want 0", got.Attempts)
	}
}
