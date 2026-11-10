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
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/a2aproject/a2a-serve/internal/messenger"
	"github.com/a2aproject/a2a-serve/internal/messenger/telegram/telegramtest"
)

func TestMessenger(t *testing.T) {
	t.Parallel()
	api := telegramtest.NewServer(t, telegramtest.Options{})
	api.Push(`{"message":{"message_id":5,"date":0,"chat":{"id":42,"type":"private"},"from":{"id":42,"is_bot":false,"first_name":"Anna","username":"anna"},"text":"/start K7Q2-9XF4"}}`)
	api.Push(`{"callback_query":{"id":"cb1","from":{"id":42,"is_bot":false,"first_name":"Anna"},"data":"h:abcd1234:1","message":{"message_id":7,"date":1700000000,"chat":{"id":42,"type":"private"}}}}`)
	api.Push(`{"message":{"message_id":8,"date":0,"chat":{"id":42,"type":"private"},"from":{"id":42,"is_bot":false,"first_name":"Anna","username":"anna"},"text":"approve",` +
		`"reply_to_message":{"message_id":7,"date":0,"chat":{"id":42,"type":"private"},"from":{"id":99,"is_bot":true,"first_name":"Coder"}}}}`)
	c := New("123:abc", nil)
	c.ServerURL = api.URL

	if got := c.Describe(); got != "@my_coder_bot" {
		t.Fatalf("Describe() = %q, want @my_coder_bot", got)
	}
	if got := c.LinkHint("K7Q2-9XF4"); got != "https://t.me/my_coder_bot?start=K7Q2-9XF4" {
		t.Fatalf("LinkHint() = %q", got)
	}

	ctx := t.Context()
	in := make(chan messenger.Inbound, 4)
	go func() {
		if err := c.Run(ctx, in, func(error) {}); err != nil {
			t.Errorf("Run() error = %v", err)
		}
	}()
	var got []messenger.Inbound
	for len(got) < 3 {
		select {
		case m := <-in:
			got = append(got, m)
		case <-time.After(10 * time.Second):
			t.Fatalf("Run() delivered %d messages, want 3", len(got))
		}
	}
	want := []messenger.Inbound{
		{Messenger: "telegram", MessageID: "42:5", Identity: "telegram:42", Name: "@anna", Conv: "42", Direct: true, Text: "/start K7Q2-9XF4", Authentic: true},
		{Messenger: "telegram", MessageID: "cb:cb1", Identity: "telegram:42", Name: "Anna", Conv: "42", Direct: true, Authentic: true,
			Action: &messenger.Action{QuestionID: "abcd1234", Option: 1}, ReplyTo: messenger.Ref{Conv: "42", ID: "7"}},
		{Messenger: "telegram", MessageID: "42:8", Identity: "telegram:42", Name: "@anna", Conv: "42", Direct: true, Text: "approve", Authentic: true,
			ReplyTo: messenger.Ref{Conv: "42", ID: "7"}},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("Run() wrong messages (-want +got) diff = %s", diff)
	}

	ref, err := c.Send(ctx, messenger.Outbound{Conv: "42", Text: "Run **git push**?", Question: &messenger.Question{ID: "abcd1234", Options: []string{"Allow", "Deny"}}})
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	wantMsgs := []telegramtest.Message{{ID: 1, ChatID: 42, Text: "Run *git push*?", ParseMode: "MarkdownV2",
		Buttons: []telegramtest.Button{{Text: "Allow", Data: "h:abcd1234:0"}, {Text: "Deny", Data: "h:abcd1234:1"}}}}
	if diff := cmp.Diff(wantMsgs, api.Messages()); diff != "" {
		t.Fatalf("Send() wrong messages (-want +got) diff = %s", diff)
	}
	if ref != (messenger.Ref{Conv: "42", ID: "1"}) {
		t.Fatalf("Send() ref = %+v, want message 1", ref)
	}
	if err := c.Edit(ctx, ref, messenger.Outbound{Conv: "42", Text: "✓ Allow by Anna"}); err != nil {
		t.Fatalf("Edit() error = %v", err)
	}
	wantMsgs[0].Text, wantMsgs[0].Buttons, wantMsgs[0].Edits = "✓ Allow by Anna", nil, 1
	if diff := cmp.Diff(wantMsgs, api.Messages()); diff != "" {
		t.Fatalf("Edit() wrong messages (-want +got) diff = %s", diff)
	}
	if n := len(api.Calls("answerCallbackQuery")); n != 1 {
		t.Fatalf("answerCallbackQuery calls = %d, want 1", n)
	}
}

func TestRunKeepsUpdateOrder(t *testing.T) {
	t.Parallel()
	const n = 50
	var updates, want []string
	for i := range n {
		updates = append(updates, fmt.Sprintf(`{"message":{"message_id":%d,"date":0,"chat":{"id":42,"type":"private"},"from":{"id":42,"is_bot":false,"first_name":"Anna"},"text":"m%d"}}`, i+1, i))
		want = append(want, fmt.Sprintf("m%d", i))
	}
	api := telegramtest.NewServer(t, telegramtest.Options{})
	for _, u := range updates {
		api.Push(u)
	}
	c := New("123:abc", nil)
	c.ServerURL = api.URL

	ctx := t.Context()
	in := make(chan messenger.Inbound, n)
	go func() {
		if err := c.Run(ctx, in, func(error) {}); err != nil {
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

func TestRunLeavesUpdatesNotHandedOverForTheNextRun(t *testing.T) {
	t.Parallel()
	api := telegramtest.NewServer(t, telegramtest.Options{})
	for i := range 3 {
		api.Push(fmt.Sprintf(`{"message":{"message_id":%d,"date":0,"chat":{"id":42,"type":"private"},"from":{"id":42,"is_bot":false,"first_name":"Anna"},"text":"m%d"}}`, i+1, i))
	}
	run := func(ctx context.Context, in chan messenger.Inbound) <-chan struct{} {
		c := New("123:abc", nil)
		c.ServerURL = api.URL
		done := make(chan struct{})
		go func() {
			defer close(done)
			if err := c.Run(ctx, in, func(error) {}); err != nil {
				t.Errorf("Run() error = %v", err)
			}
		}()
		return done
	}
	receive := func(in chan messenger.Inbound) string {
		select {
		case m := <-in:
			return m.Text
		case <-time.After(10 * time.Second):
			t.Fatal("Run() delivered nothing")
			return ""
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	in := make(chan messenger.Inbound)
	done := run(ctx, in)
	if got := receive(in); got != "m0" {
		t.Fatalf("first Run() delivered %q, want m0", got)
	}
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	in = make(chan messenger.Inbound)
	run(t.Context(), in)

	var got []string
	for len(got) == 0 || got[len(got)-1] != "m2" {
		got = append(got, receive(in))
	}
	if diff := cmp.Diff([]string{"m1", "m2"}, got[len(got)-2:]); diff != "" {
		t.Fatalf("second Run() wrong messages (-want +got) diff = %s", diff)
	}
}

func TestRunReportsPollingState(t *testing.T) {
	t.Parallel()
	api := telegramtest.NewServer(t, telegramtest.Options{})
	api.Fail("getUpdates", telegramtest.Failure{Status: http.StatusUnauthorized, Body: `{"ok":false,"error_code":401,"description":"Unauthorized"}`})
	c := New("123:abc", nil)
	c.ServerURL = api.URL
	reports := make(chan error, 100)
	go func() {
		if err := c.Run(t.Context(), make(chan messenger.Inbound, 1), func(err error) { reports <- err }); err != nil {
			t.Errorf("Run() error = %v", err)
		}
	}()
	next := func() error {
		select {
		case err := <-reports:
			return err
		case <-time.After(10 * time.Second):
			t.Fatal("Run() reported nothing")
			return nil
		}
	}

	if err := next(); err == nil || !strings.Contains(err.Error(), "Unauthorized") || strings.Contains(err.Error(), "abc") {
		t.Fatalf("Run() reported %v, want the refused token without the token", err)
	}
	api.Recover("getUpdates")
	api.Push(`{"message":{"message_id":1,"date":0,"chat":{"id":42,"type":"private"},"from":{"id":42,"is_bot":false,"first_name":"Anna"},"text":"hi"}}`)
	for next() != nil {
	}
}

func TestGroupMessages(t *testing.T) {
	t.Parallel()
	group := func(id int, text string) string {
		return fmt.Sprintf(`{"message":{"message_id":%d,"date":0,"chat":{"id":-100,"type":"supergroup"},"from":{"id":42,"is_bot":false,"first_name":"Anna"},"text":%q}}`, id, text)
	}
	api := telegramtest.NewServer(t, telegramtest.Options{})
	for _, u := range []string{
		group(1, "/stop@otherbot"),
		group(2, "/stop@my_coder_bot"),
		group(3, "/stop"),
		group(4, "/new@my_coder_botx"),
		group(5, "hello"),
		group(6, "@my_coder_bot hello"),
		group(7, "/new@MY_CODER_BOT please"),
		`{"message":{"message_id":8,"date":0,"chat":{"id":-100,"type":"supergroup"},"from":{"id":42,"is_bot":false,"first_name":"Anna"},"text":"/stop@otherbot",` +
			`"reply_to_message":{"message_id":3,"date":0,"chat":{"id":-100,"type":"supergroup"},"from":{"id":99,"is_bot":true,"first_name":"Coder"}}}}`,
		`{"message":{"message_id":9,"date":0,"chat":{"id":42,"type":"private"},"from":{"id":42,"is_bot":false,"first_name":"Anna"},"text":"done"}}`,
	} {
		api.Push(u)
	}
	c := New("123:abc", nil)
	c.ServerURL = api.URL

	in := make(chan messenger.Inbound, 10)
	go func() {
		if err := c.Run(t.Context(), in, func(error) {}); err != nil {
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
		{name: "file larger than the limit", path: "big.bin", wantErr: "over 20 MiB"},
		{name: "stalled download", path: "slow.bin", timeout: 200 * time.Millisecond, wantErr: "deadline exceeded"},
	}
	api := telegramtest.NewServer(t, telegramtest.Options{})
	api.SetFile("small.txt", []byte("hello"))
	api.SetFile("big.bin", bytes.Repeat([]byte("x"), maxFile+1))
	api.SetFile("slow.bin", nil)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := New("123:abc", nil)
			c.ServerURL = api.URL
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
			if diff := cmp.Diff(messenger.File{Name: tc.path, MediaType: "text/plain", Data: tc.want}, got); diff != "" {
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
			api := telegramtest.NewServer(t, telegramtest.Options{})
			api.Fail("getMe", telegramtest.Failure{Status: http.StatusNotFound, Body: tc.response})
			c := New("123:abc", nil)
			c.ServerURL = api.URL
			err := c.Validate(t.Context())
			if err == nil || err.Error() != "invalid Telegram bot token" {
				t.Fatalf("Validate() error = %v, want invalid Telegram bot token", err)
			}
		})
	}
}

func TestSendAndEditRateLimit(t *testing.T) {
	t.Parallel()
	limited := telegramtest.Failure{Status: http.StatusTooManyRequests, Body: `{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 3","parameters":{"retry_after":3}}`}
	tests := []struct {
		name string
		call func(c *Messenger) error
	}{
		{name: "send", call: func(c *Messenger) error {
			_, err := c.Send(t.Context(), messenger.Outbound{Conv: "42", Text: "hi"})
			return err
		}},
		{name: "edit", call: func(c *Messenger) error {
			return c.Edit(t.Context(), messenger.Ref{Conv: "42", ID: "7"}, messenger.Outbound{Conv: "42", Text: "hi"})
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			api := telegramtest.NewServer(t, telegramtest.Options{})
			api.Fail("sendMessage", limited)
			api.Fail("editMessageText", limited)
			c := New("123:abc", nil)
			c.ServerURL = api.URL
			var limit *messenger.RateLimitError
			err := tc.call(c)
			if !errors.As(err, &limit) {
				t.Fatalf("Messenger error = %v, want a *messenger.RateLimitError", err)
			}
			if want := 3 * time.Second; limit.RetryAfter != want {
				t.Fatalf("RateLimitError.RetryAfter = %v, want %v", limit.RetryAfter, want)
			}
		})
	}
}

func TestSendAndEditFallBackToPlainTextWhenTooLong(t *testing.T) {
	t.Parallel()
	tooLong := telegramtest.Failure{Status: http.StatusBadRequest, Body: `{"ok":false,"error_code":400,"description":"Bad Request: message is too long"}`, OnlyWith: "MarkdownV2"}
	tests := []struct {
		name   string
		method string
		call   func(c *Messenger) error
	}{
		{name: "send", method: "sendMessage", call: func(c *Messenger) error {
			_, err := c.Send(t.Context(), messenger.Outbound{Conv: "42", Text: "a.b-c"})
			return err
		}},
		{name: "edit", method: "editMessageText", call: func(c *Messenger) error {
			ref, err := c.Send(t.Context(), messenger.Outbound{Conv: "42", Text: "a.b-c"})
			if err != nil {
				return err
			}
			return c.Edit(t.Context(), ref, messenger.Outbound{Conv: "42", Text: "a.b-c"})
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			api := telegramtest.NewServer(t, telegramtest.Options{})
			api.Fail(tc.method, tooLong)
			c := New("123:abc", nil)
			c.ServerURL = api.URL
			if err := tc.call(c); err != nil {
				t.Fatalf("Messenger error = %v, want nil", err)
			}
			calls := api.Calls(tc.method)
			if len(calls) != 2 {
				t.Fatalf("%s calls = %d, want 2", tc.method, len(calls))
			}
			if got := calls[1]; got.Get("parse_mode") != "" || got.Get("text") != "a.b-c" {
				t.Fatalf("%s retry = %v, want plain text a.b-c", tc.method, got)
			}
		})
	}
}

func TestSendAndEditShowLiteralTextWithoutMarkup(t *testing.T) {
	t.Parallel()
	const text = "Permission required: Run [rm -rf /](https://example.com/ok) *now* _please_."
	tests := []struct {
		name   string
		method string
		call   func(c *Messenger) error
	}{
		{name: "send", method: "sendMessage", call: func(c *Messenger) error {
			_, err := c.Send(t.Context(), messenger.Outbound{Conv: "42", Text: text, Literal: true})
			return err
		}},
		{name: "edit", method: "editMessageText", call: func(c *Messenger) error {
			ref, err := c.Send(t.Context(), messenger.Outbound{Conv: "42", Text: "Waiting"})
			if err != nil {
				return err
			}
			return c.Edit(t.Context(), ref, messenger.Outbound{Conv: "42", Text: text, Literal: true})
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			api := telegramtest.NewServer(t, telegramtest.Options{})
			c := New("123:abc", nil)
			c.ServerURL = api.URL

			if err := tc.call(c); err != nil {
				t.Fatalf("Messenger error = %v, want nil", err)
			}

			calls := api.Calls(tc.method)
			if len(calls) != 1 {
				t.Fatalf("%s calls = %d, want 1", tc.method, len(calls))
			}
			if got := calls[len(calls)-1]; got.Get("text") != text || got.Has("parse_mode") {
				t.Fatalf("%s = %v, want the text as written and no parse mode", tc.method, got)
			}
		})
	}
}

func TestDownloadErrorHidesBotToken(t *testing.T) {
	t.Parallel()
	api := telegramtest.NewServer(t, telegramtest.Options{})
	api.SetFile("slow.bin", nil)
	c := New("123:abc", nil)
	c.ServerURL = api.URL
	c.downloadTimeout = 100 * time.Millisecond
	if err := c.Validate(t.Context()); err != nil {
		t.Fatalf("Validate() error = %v, want nil", err)
	}

	_, err := c.download(t.Context(), c.bot, "slow.bin", "slow.bin", "text/plain")

	if err == nil || strings.Contains(err.Error(), "123:abc") {
		t.Fatalf("download() error = %v, want an error without the bot token", err)
	}
}

func TestHandleMessageReportsFailedAttachments(t *testing.T) {
	t.Parallel()
	api := telegramtest.NewServer(t, telegramtest.Options{})
	api.SetFile("big.bin", bytes.Repeat([]byte("x"), maxFile+1))
	api.Push(`{"message":{"message_id":5,"date":0,"chat":{"id":42,"type":"private"},"from":{"id":42,"is_bot":false,"first_name":"Anna"},"caption":"look","document":{"file_id":"big.bin","file_name":"big.bin"}}}`)
	c := New("123:abc", nil)
	c.ServerURL = api.URL
	in := make(chan messenger.Inbound, 1)
	go func() {
		if err := c.Run(t.Context(), in, func(error) {}); err != nil {
			t.Errorf("Run() error = %v", err)
		}
	}()
	var m messenger.Inbound
	select {
	case m = <-in:
	case <-time.After(10 * time.Second):
		t.Fatal("Run() delivered no message")
	}

	err := m.Prepare(t.Context(), &m)

	var failed *messenger.AttachmentError
	if !errors.As(err, &failed) || failed.Name != "big.bin" {
		t.Fatalf("Inbound.Prepare() error = %v, want a *messenger.AttachmentError for big.bin", err)
	}
}

func TestKeyboardRows(t *testing.T) {
	t.Parallel()
	got := keyboard(&messenger.Question{ID: "q1", Options: []string{"Allow once", "Always allow kubectl get *", "Deny"}})
	want := &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{
		{{Text: "Allow once", CallbackData: "h:q1:0"}, {Text: "Deny", CallbackData: "h:q1:2"}},
		{{Text: "Always allow kubectl get *", CallbackData: "h:q1:1"}},
	}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("keyboard() wrong result (-want +got) diff = %s", diff)
	}
}

func TestHandleMessageDownloadsVoiceNotes(t *testing.T) {
	t.Parallel()
	api := telegramtest.NewServer(t, telegramtest.Options{})
	api.SetFile("voice1", []byte("OggS voice"))
	api.SetFile("song1", []byte("ID3 song"))
	api.SayVoice(telegramtest.User{ID: 42, FirstName: "Anna"}, "voice1")
	api.Push(`{"message":{"message_id":9,"date":0,"chat":{"id":42,"type":"private"},"from":{"id":42,"is_bot":false,"first_name":"Anna"},"audio":{"file_id":"song1","file_unique_id":"song1","duration":5,"file_name":"memo.mp3","mime_type":"audio/mpeg"}}}`)
	c := New("123:abc", nil)
	c.ServerURL = api.URL
	in := make(chan messenger.Inbound, 2)
	go func() {
		if err := c.Run(t.Context(), in, func(error) {}); err != nil {
			t.Errorf("Run() error = %v", err)
		}
	}()
	var got [][]messenger.File
	for range 2 {
		var m messenger.Inbound
		select {
		case m = <-in:
		case <-time.After(10 * time.Second):
			t.Fatal("Run() delivered no message")
		}
		if err := m.Prepare(t.Context(), &m); err != nil {
			t.Fatalf("Inbound.Prepare() error = %v, want nil", err)
		}
		got = append(got, m.Files)
	}
	want := [][]messenger.File{
		{{Name: "voice.ogg", MediaType: "audio/ogg", Data: []byte("OggS voice")}},
		{{Name: "memo.mp3", MediaType: "audio/mpeg", Data: []byte("ID3 song")}},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("Inbound.Prepare() wrong files (-want +got) diff = %s", diff)
	}
}

func TestSendFiles(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		file      messenger.File
		failVoice bool
		voiceErr  string
		want      telegramtest.Message
	}{
		{
			name: "voice note",
			file: messenger.File{Name: "reply.ogg", MediaType: "audio/ogg", Data: []byte("OggS"), Voice: true},
			want: telegramtest.Message{ID: 1, ChatID: 42, Voice: "reply.ogg"},
		},
		{
			name: "mp3 voice note",
			file: messenger.File{Name: "reply.mp3", MediaType: "audio/mpeg", Data: []byte("ID3"), Voice: true},
			want: telegramtest.Message{ID: 1, ChatID: 42, Voice: "reply.mp3"},
		},
		{
			name: "audio file",
			file: messenger.File{Name: "song.mp3", MediaType: "audio/mpeg", Data: []byte("ID3")},
			want: telegramtest.Message{ID: 1, ChatID: 42, Document: "song.mp3"},
		},
		{
			name:      "voice notes refused",
			file:      messenger.File{Name: "reply.ogg", MediaType: "audio/ogg", Data: []byte("OggS"), Voice: true},
			failVoice: true,
			want:      telegramtest.Message{ID: 1, ChatID: 42, Document: "reply.ogg"},
		},
		{
			name:     "voice note failing otherwise",
			file:     messenger.File{Name: "reply.ogg", MediaType: "audio/ogg", Data: []byte("OggS"), Voice: true},
			voiceErr: "Bad Request: chat not found",
		},
		{
			name: "document",
			file: messenger.File{Name: "report.pdf", MediaType: "application/pdf", Data: []byte("%PDF")},
			want: telegramtest.Message{ID: 1, ChatID: 42, Document: "report.pdf"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			api := telegramtest.NewServer(t, telegramtest.Options{})
			if tc.failVoice {
				api.Fail("sendVoice", telegramtest.Failure{Status: http.StatusBadRequest, Body: `{"ok":false,"error_code":400,"description":"Bad Request: VOICE_MESSAGES_FORBIDDEN"}`})
			}
			if tc.voiceErr != "" {
				api.Fail("sendVoice", telegramtest.Failure{Status: http.StatusBadRequest, Body: `{"ok":false,"error_code":400,"description":"` + tc.voiceErr + `"}`})
			}
			c := New("123:abc", nil)
			c.ServerURL = api.URL
			_, err := c.Send(t.Context(), messenger.Outbound{Conv: "42", Files: []messenger.File{tc.file}})
			if tc.voiceErr != "" {
				if err == nil || len(api.Messages()) != 0 {
					t.Fatalf("Send() error = %v, messages %v, want the error and no file sent", err, api.Messages())
				}
				return
			}
			if err != nil {
				t.Fatalf("Send() error = %v, want nil", err)
			}
			if diff := cmp.Diff([]telegramtest.Message{tc.want}, api.Messages()); diff != "" {
				t.Fatalf("Send() wrong messages (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestRunSetsCommandMenu(t *testing.T) {
	t.Parallel()
	api := telegramtest.NewServer(t, telegramtest.Options{})
	c := New("123:abc", []messenger.Command{{Name: "new", Description: "Start a new conversation"}, {Name: "voice", Description: "Turn voice replies on or off for you", Choices: []string{"on", "off"}}})
	c.ServerURL = api.URL
	go func() {
		if err := c.Run(t.Context(), make(chan messenger.Inbound), func(error) {}); err != nil {
			t.Errorf("Run() error = %v", err)
		}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for len(api.Calls("setMyCommands")) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("Run() didn't set the command menu")
		}
		time.Sleep(5 * time.Millisecond)
	}
	got := api.Calls("setMyCommands")[0].Get("commands")
	want := `[{"command":"new","description":"Start a new conversation"},{"command":"voice","description":"Turn voice replies on or off for you"}]`
	if got != want {
		t.Fatalf("setMyCommands commands = %s, want %s", got, want)
	}
}
