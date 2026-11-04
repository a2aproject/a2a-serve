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

package telegram

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/a2aproject/a2a-serve/internal/channel"
)

// fakeBotAPI serves the Bot API methods the channel uses and records calls.
type fakeBotAPI struct {
	t       *testing.T
	mu      sync.Mutex
	updates []string
	calls   map[string][]string
	// files are served for download by path; a nil value never answers.
	files map[string][]byte
	// getMeError, if set, is the Bot API error answered to getMe.
	getMeError string
	// failures are the Bot API errors answered to a method.
	failures map[string]apiFailure
}

type apiFailure struct {
	status int
	body   string
	// onlyWith, if set, limits the failure to requests whose body contains it.
	onlyWith string
}

func (f *fakeBotAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if path, ok := strings.CutPrefix(r.URL.Path, "/file/bot123:abc/"); ok {
		f.serveFile(w, r, path)
		return
	}
	method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	body, err := io.ReadAll(r.Body)
	if err != nil {
		f.t.Errorf("reading %s: %v", method, err)
	}
	f.mu.Lock()
	f.calls[method] = append(f.calls[method], string(body))
	var result string
	if fail, ok := f.failures[method]; ok && strings.Contains(string(body), fail.onlyWith) {
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(fail.status)
		if _, err := io.WriteString(w, fail.body); err != nil {
			f.t.Errorf("writing %s: %v", method, err)
		}
		return
	}
	switch method {
	case "getMe":
		if f.getMeError != "" {
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			if _, err := io.WriteString(w, f.getMeError); err != nil {
				f.t.Errorf("writing %s: %v", method, err)
			}
			return
		}
		result = `{"id":99,"is_bot":true,"first_name":"Coder","username":"my_coder_bot"}`
	case "getFile":
		for path := range f.files {
			if strings.Contains(string(body), path) {
				result = `{"file_id":"` + path + `","file_unique_id":"u","file_path":"` + path + `"}`
			}
		}
	case "getUpdates":
		result = "[" + strings.Join(f.updates, ",") + "]"
		f.updates = nil
	case "sendMessage", "editMessageText":
		result = `{"message_id":7,"date":0,"chat":{"id":42,"type":"private"}}`
	default:
		result = "true"
	}
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if _, err := io.WriteString(w, `{"ok":true,"result":`+result+`}`); err != nil {
		f.t.Errorf("writing %s: %v", method, err)
	}
}

func (f *fakeBotAPI) serveFile(w http.ResponseWriter, r *http.Request, path string) {
	f.mu.Lock()
	data := f.files[path]
	f.mu.Unlock()
	if data == nil {
		<-r.Context().Done()
		return
	}
	if _, err := w.Write(data); err != nil {
		f.t.Errorf("writing %s: %v", path, err)
	}
}

func (f *fakeBotAPI) called(method string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls[method]...)
}

func TestChannel(t *testing.T) {
	t.Parallel()
	api := &fakeBotAPI{t: t, calls: map[string][]string{}, updates: []string{
		`{"update_id":1,"message":{"message_id":5,"date":0,"chat":{"id":42,"type":"private"},"from":{"id":42,"is_bot":false,"first_name":"Anna","username":"anna"},"text":"/start K7Q2-9XF4"}}`,
		`{"update_id":2,"callback_query":{"id":"cb1","from":{"id":42,"is_bot":false,"first_name":"Anna"},"data":"h:abcd1234:1","message":{"message_id":7,"date":1700000000,"chat":{"id":42,"type":"private"}}}}`,
	}}
	srv := httptest.NewServer(api)
	defer srv.Close()
	c := New("123:abc")
	c.ServerURL = srv.URL

	if got := c.Describe(); got != "@my_coder_bot" {
		t.Fatalf("Describe() = %q, want @my_coder_bot", got)
	}
	if got := c.LinkHint("K7Q2-9XF4"); got != "https://t.me/my_coder_bot?start=K7Q2-9XF4" {
		t.Fatalf("LinkHint() = %q", got)
	}

	ctx := t.Context()
	in := make(chan channel.Inbound, 4)
	go func() {
		if err := c.Run(ctx, in); err != nil {
			t.Errorf("Run() error = %v", err)
		}
	}()
	var got []channel.Inbound
	for len(got) < 2 {
		select {
		case m := <-in:
			got = append(got, m)
		case <-time.After(10 * time.Second):
			t.Fatalf("Run() delivered %d messages, want 2", len(got))
		}
	}
	want := []channel.Inbound{
		{Channel: "telegram", MessageID: "42:5", Identity: "telegram:42", Name: "@anna", Conv: "42", Direct: true, Text: "/start K7Q2-9XF4", Authentic: true},
		{Channel: "telegram", MessageID: "cb:cb1", Identity: "telegram:42", Name: "Anna", Conv: "42", Direct: true, Authentic: true,
			Action: &channel.Action{QuestionID: "abcd1234", Option: 1}, ReplyTo: channel.Ref{Conv: "42", ID: "7"}},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("Run() wrong messages (-want +got) diff = %s", diff)
	}

	ref, err := c.Send(ctx, channel.Outbound{Conv: "42", Text: "Run **git push**?", Question: &channel.Question{ID: "abcd1234", Options: []string{"Allow", "Deny"}}})
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if ref.ID != "7" {
		t.Fatalf("Send() ref = %+v, want message 7", ref)
	}
	sent := api.called("sendMessage")
	if len(sent) != 1 {
		t.Fatalf("sendMessage calls = %d, want 1", len(sent))
	}
	for _, want := range []string{`Run *git push*?`, "MarkdownV2", `h:abcd1234:0`, `h:abcd1234:1`} {
		if !strings.Contains(unescapeJSON(sent[0]), want) {
			t.Fatalf("sendMessage = %s, want it to contain %q", sent[0], want)
		}
	}
	if err := c.Edit(ctx, ref, channel.Outbound{Conv: "42", Text: "✓ Allow by Anna"}); err != nil {
		t.Fatalf("Edit() error = %v", err)
	}
	if len(api.called("answerCallbackQuery")) != 1 {
		t.Fatalf("answerCallbackQuery was not called for the button click")
	}
}

// unescapeJSON undoes JSON string escaping in multipart field values.
func unescapeJSON(s string) string {
	var out string
	if err := json.Unmarshal([]byte(`"`+strings.ReplaceAll(s, `"`, `\"`)+`"`), &out); err != nil {
		return s
	}
	return out
}

func TestRunKeepsUpdateOrder(t *testing.T) {
	t.Parallel()
	const n = 50
	var updates, want []string
	for i := range n {
		updates = append(updates, fmt.Sprintf(`{"update_id":%d,"message":{"message_id":%d,"date":0,"chat":{"id":42,"type":"private"},"from":{"id":42,"is_bot":false,"first_name":"Anna"},"text":"m%d"}}`, i+1, i+1, i))
		want = append(want, fmt.Sprintf("m%d", i))
	}
	srv := httptest.NewServer(&fakeBotAPI{t: t, calls: map[string][]string{}, updates: updates})
	defer srv.Close()
	c := New("123:abc")
	c.ServerURL = srv.URL

	ctx := t.Context()
	in := make(chan channel.Inbound, n)
	go func() {
		if err := c.Run(ctx, in); err != nil {
			t.Errorf("Run() error = %v", err)
		}
	}()
	var got []string
	for len(got) < n {
		select {
		case m := <-in:
			got = append(got, m.Text)
		case <-time.After(10 * time.Second):
			t.Fatalf("Run() delivered %d messages, want %d", len(got), n)
		}
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("Run() wrong message order (-want +got) diff = %s", diff)
	}
}

func TestGroupMessages(t *testing.T) {
	t.Parallel()
	group := func(id int, text string) string {
		return fmt.Sprintf(`{"update_id":%d,"message":{"message_id":%d,"date":0,"chat":{"id":-100,"type":"supergroup"},"from":{"id":42,"is_bot":false,"first_name":"Anna"},"text":%q}}`, id, id, text)
	}
	api := &fakeBotAPI{t: t, calls: map[string][]string{}, updates: []string{
		group(1, "/stop@otherbot"),
		group(2, "/stop@my_coder_bot"),
		group(3, "/stop"),
		group(4, "/new@my_coder_botx"),
		group(5, "hello"),
		group(6, "@my_coder_bot hello"),
		group(7, "/new@MY_CODER_BOT please"),
		`{"update_id":8,"message":{"message_id":8,"date":0,"chat":{"id":-100,"type":"supergroup"},"from":{"id":42,"is_bot":false,"first_name":"Anna"},"text":"/stop@otherbot",` +
			`"reply_to_message":{"message_id":3,"date":0,"chat":{"id":-100,"type":"supergroup"},"from":{"id":99,"is_bot":true,"first_name":"Coder"}}}}`,
		`{"update_id":9,"message":{"message_id":9,"date":0,"chat":{"id":42,"type":"private"},"from":{"id":42,"is_bot":false,"first_name":"Anna"},"text":"done"}}`,
	}}
	srv := httptest.NewServer(api)
	defer srv.Close()
	c := New("123:abc")
	c.ServerURL = srv.URL

	in := make(chan channel.Inbound, 10)
	go func() {
		if err := c.Run(t.Context(), in); err != nil {
			t.Errorf("Run() error = %v", err)
		}
	}()
	want := []string{"/stop", "/stop", "/new please", "done", "hello"}
	var got []string
	timeout := time.After(10 * time.Second)
	for len(got) < len(want) {
		select {
		case m := <-in:
			got = append(got, m.Text)
		case <-timeout:
			t.Fatalf("Run() delivered %q, want %q", got, want)
		}
	}
	select {
	case m := <-in:
		got = append(got, m.Text)
	case <-time.After(200 * time.Millisecond):
	}
	if diff := cmp.Diff(want, got, cmpopts.SortSlices(func(a, b string) bool { return a < b })); diff != "" {
		t.Fatalf("Run() wrong messages (-want +got) diff = %s", diff)
	}
}

func TestDownload(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		path    string
		timeout time.Duration
		want    []byte
		wantErr string
	}{
		{name: "small file", path: "small.txt", want: []byte("hello")},
		{name: "file larger than the limit", path: "big.bin", wantErr: "too large"},
		{name: "stalled download", path: "slow.bin", timeout: 200 * time.Millisecond, wantErr: "deadline exceeded"},
	}
	api := &fakeBotAPI{t: t, calls: map[string][]string{}, files: map[string][]byte{
		"small.txt": []byte("hello"),
		"big.bin":   bytes.Repeat([]byte("x"), maxFile+1),
		"slow.bin":  nil,
	}}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := New("123:abc")
			c.ServerURL = srv.URL
			if tc.timeout != 0 {
				c.downloadTimeout = tc.timeout
			}
			if err := c.Validate(t.Context()); err != nil {
				t.Fatalf("Validate() error = %v, want nil", err)
			}
			got, err := c.download(t.Context(), c.bot, tc.path, tc.path, "text/plain")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("download() error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("download() error = %v, want nil", err)
			}
			if diff := cmp.Diff(channel.File{Name: tc.path, MediaType: "text/plain", Data: tc.want}, got); diff != "" {
				t.Fatalf("download() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestValidateInvalidToken(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		response string
	}{
		{name: "not found", response: `{"ok":false,"error_code":404,"description":"Not Found"}`},
		{name: "unauthorized", response: `{"ok":false,"error_code":401,"description":"Unauthorized"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(&fakeBotAPI{t: t, calls: map[string][]string{}, getMeError: tc.response})
			defer srv.Close()
			c := New("123:abc")
			c.ServerURL = srv.URL
			err := c.Validate(t.Context())
			if err == nil || err.Error() != "invalid Telegram bot token" {
				t.Fatalf("Validate() error = %v, want invalid Telegram bot token", err)
			}
		})
	}
}

func TestSendAndEditRateLimit(t *testing.T) {
	t.Parallel()
	limited := apiFailure{status: http.StatusTooManyRequests, body: `{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 3","parameters":{"retry_after":3}}`}
	tests := []struct {
		name string
		call func(c *Channel) error
	}{
		{name: "send", call: func(c *Channel) error {
			_, err := c.Send(t.Context(), channel.Outbound{Conv: "42", Text: "hi"})
			return err
		}},
		{name: "edit", call: func(c *Channel) error {
			return c.Edit(t.Context(), channel.Ref{Conv: "42", ID: "7"}, channel.Outbound{Conv: "42", Text: "hi"})
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			api := &fakeBotAPI{t: t, calls: map[string][]string{}, failures: map[string]apiFailure{"sendMessage": limited, "editMessageText": limited}}
			srv := httptest.NewServer(api)
			defer srv.Close()
			c := New("123:abc")
			c.ServerURL = srv.URL
			var limit *channel.RateLimitError
			err := tc.call(c)
			if !errors.As(err, &limit) {
				t.Fatalf("Channel error = %v, want a *channel.RateLimitError", err)
			}
			if want := 3 * time.Second; limit.RetryAfter != want {
				t.Fatalf("RateLimitError.RetryAfter = %v, want %v", limit.RetryAfter, want)
			}
		})
	}
}

func TestSendAndEditFallBackToPlainTextWhenTooLong(t *testing.T) {
	t.Parallel()
	tooLong := apiFailure{status: http.StatusBadRequest, body: `{"ok":false,"error_code":400,"description":"Bad Request: message is too long"}`, onlyWith: "MarkdownV2"}
	tests := []struct {
		name   string
		method string
		call   func(c *Channel) error
	}{
		{name: "send", method: "sendMessage", call: func(c *Channel) error {
			_, err := c.Send(t.Context(), channel.Outbound{Conv: "42", Text: "a.b-c"})
			return err
		}},
		{name: "edit", method: "editMessageText", call: func(c *Channel) error {
			return c.Edit(t.Context(), channel.Ref{Conv: "42", ID: "7"}, channel.Outbound{Conv: "42", Text: "a.b-c"})
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			api := &fakeBotAPI{t: t, calls: map[string][]string{}, failures: map[string]apiFailure{tc.method: tooLong}}
			srv := httptest.NewServer(api)
			defer srv.Close()
			c := New("123:abc")
			c.ServerURL = srv.URL
			if err := tc.call(c); err != nil {
				t.Fatalf("Channel error = %v, want nil", err)
			}
			calls := api.called(tc.method)
			if len(calls) != 2 {
				t.Fatalf("%s calls = %d, want 2", tc.method, len(calls))
			}
			if last := unescapeJSON(calls[1]); strings.Contains(last, "MarkdownV2") || !strings.Contains(last, "a.b-c") {
				t.Fatalf("%s retry = %s, want plain text a.b-c", tc.method, calls[1])
			}
		})
	}
}
