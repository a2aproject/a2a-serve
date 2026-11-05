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

// Package telegramtest is a fake Telegram Bot API for tests. It keeps the
// messages the bot sends and edits, and plays users who write to the bot and
// press its buttons.
//
// With an upstream, the methods that send or change messages go to the real
// Bot API, so that Telegram checks the formatting and the user sees the
// messages, while the fake keeps playing the user: a bot can't receive
// messages from another bot.
package telegramtest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// User is a Telegram account.
type User struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	Username  string `json:"username,omitempty"`
}

// Bot is the account getMe returns without an upstream.
var Bot = User{ID: 99, IsBot: true, FirstName: "Coder", Username: "my_coder_bot"}

// Button is an inline keyboard button.
type Button struct {
	Text string
	Data string
}

// Message is a message the bot sent, as it reads after its last edit.
type Message struct {
	ID        int
	ChatID    int64
	Text      string
	ParseMode string
	Buttons   []Button
	Document  string
	Edits     int
}

// Plain returns the text without MarkdownV2 escapes and markers.
func (m Message) Plain() string {
	if m.ParseMode != "MarkdownV2" {
		return m.Text
	}
	var b strings.Builder
	escaped := false
	for _, r := range m.Text {
		switch {
		case escaped:
			b.WriteRune(r)
			escaped = false
		case r == '\\':
			escaped = true
		case strings.ContainsRune("*_~`|", r):
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Failure is a Bot API error answered to a method.
type Failure struct {
	Status int
	Body   string
	// OnlyWith, if set, limits the failure to calls with a parameter that
	// contains it.
	OnlyWith string
}

// Options configures a [Server].
type Options struct {
	// Upstream is the URL of a real Bot API, such as https://api.telegram.org.
	// Calls other than getUpdates, answerCallbackQuery and getFile go there.
	Upstream string
}

// Server is a fake Bot API.
type Server struct {
	// URL is the Bot API URL to give the bot.
	URL string

	t        testing.TB
	srv      *httptest.Server
	upstream string
	closed   chan struct{}

	mu             sync.Mutex
	changed        chan struct{}
	updates        []map[string]any
	nextUpdate     int64
	nextMessage    int
	nextCallback   int
	messages       []*Message
	calls          map[string][]url.Values
	failures       map[string]Failure
	files          map[string][]byte
	upstreamErrors []string
}

// NewServer starts a fake Bot API, which stops when the test ends.
func NewServer(t testing.TB, opts Options) *Server {
	s := &Server{
		t:           t,
		upstream:    strings.TrimSuffix(opts.Upstream, "/"),
		closed:      make(chan struct{}),
		changed:     make(chan struct{}),
		nextUpdate:  1,
		nextMessage: 1,
		calls:       map[string][]url.Values{},
		failures:    map[string]Failure{},
		files:       map[string][]byte{},
	}
	s.srv = httptest.NewServer(s)
	s.URL = s.srv.URL
	t.Cleanup(s.close)
	return s
}

// close ends the long polls first, since httptest waits for open requests.
func (s *Server) close() {
	close(s.closed)
	s.srv.Close()
}

// Push queues an update; its update_id is set to the next one.
func (s *Server) Push(update string) {
	var u map[string]any
	if err := json.Unmarshal([]byte(update), &u); err != nil {
		s.t.Fatalf("telegramtest: invalid update %s: %v", update, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	u["update_id"] = s.nextUpdate
	s.nextUpdate++
	s.updates = append(s.updates, u)
	s.notifyLocked()
}

// Say queues a message from u to the bot in their private chat.
func (s *Server) Say(u User, text string) {
	s.mu.Lock()
	id := s.nextMessage
	s.nextMessage++
	s.mu.Unlock()
	s.push(map[string]any{"message": map[string]any{
		"message_id": id, "date": time.Now().Unix(), "chat": privateChat(u.ID), "from": u, "text": text,
	}})
}

// Press queues u pressing the button of m labeled label.
func (s *Server) Press(u User, m Message, label string) error {
	i := slices.IndexFunc(m.Buttons, func(b Button) bool { return b.Text == label })
	if i < 0 {
		return fmt.Errorf("message %d has no button %q, it has %v", m.ID, label, m.Buttons)
	}
	s.mu.Lock()
	id := s.nextCallback
	s.nextCallback++
	s.mu.Unlock()
	s.push(map[string]any{"callback_query": map[string]any{
		"id": "cb" + strconv.Itoa(id), "from": u, "data": m.Buttons[i].Data, "chat_instance": "1",
		"message": map[string]any{"message_id": m.ID, "date": time.Now().Unix(), "chat": privateChat(m.ChatID)},
	}})
	return nil
}

func privateChat(id int64) map[string]any { return map[string]any{"id": id, "type": "private"} }

func (s *Server) push(u map[string]any) {
	data, err := json.Marshal(u)
	if err != nil {
		s.t.Fatalf("telegramtest: encoding update: %v", err)
	}
	s.Push(string(data))
}

// Messages returns the messages the bot sent, oldest first.
func (s *Server) Messages() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Message, 0, len(s.messages))
	for _, m := range s.messages {
		c := *m
		c.Buttons = slices.Clone(m.Buttons)
		out = append(out, c)
	}
	return out
}

// WaitMessage waits until a message the bot sent satisfies match, and
// returns the first that does.
func (s *Server) WaitMessage(ctx context.Context, match func(Message) bool) (Message, error) {
	for {
		s.mu.Lock()
		changed := s.changed
		s.mu.Unlock()
		msgs := s.Messages()
		if i := slices.IndexFunc(msgs, match); i >= 0 {
			return msgs[i], nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			var b strings.Builder
			for _, m := range msgs {
				fmt.Fprintf(&b, "\n  %d: %q %v", m.ID, m.Plain(), m.Buttons)
			}
			return Message{}, fmt.Errorf("no matching message: %w; the bot sent:%s", ctx.Err(), b.String())
		}
	}
}

// Calls returns the parameters of the calls of method.
func (s *Server) Calls(method string) []url.Values {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls[method])
}

// Fail makes method answer with f.
func (s *Server) Fail(method string, f Failure) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures[method] = f
}

// SetFile serves data for download at path. A nil data never answers.
func (s *Server) SetFile(path string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files[path] = data
}

// UpstreamErrors returns the errors the upstream answered.
func (s *Server) UpstreamErrors() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.upstreamErrors)
}

func (s *Server) notifyLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

// ServeHTTP implements [http.Handler].
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/file/bot") {
		s.serveFile(w, r)
		return
	}
	method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	params, doc, err := parseParams(r.Header.Get("Content-Type"), body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.calls[method] = append(s.calls[method], params)
	fail, failing := s.failures[method]
	s.mu.Unlock()
	if failing && (fail.OnlyWith == "" || hasValue(params, fail.OnlyWith)) {
		s.writeRaw(w, fail.Status, fail.Body)
		return
	}
	switch method {
	case "getUpdates":
		s.getUpdates(w, r, params)
		return
	case "answerCallbackQuery":
		s.writeResult(w, true)
		return
	case "getFile":
		s.getFile(w, params)
		return
	}
	if s.upstream != "" {
		s.forward(w, r, method, params, doc, body)
		return
	}
	switch method {
	case "getMe":
		s.writeResult(w, Bot)
	case "sendMessage", "sendDocument":
		s.writeResult(w, s.send(params, doc, 0))
	case "editMessageText":
		s.edit(w, params)
	default:
		s.writeResult(w, true)
	}
}

func hasValue(params url.Values, sub string) bool {
	for _, vs := range params {
		for _, v := range vs {
			if strings.Contains(v, sub) {
				return true
			}
		}
	}
	return false
}

// parseParams returns the form fields of a call, and the name of the file
// it uploads as document.
func parseParams(contentType string, body []byte) (url.Values, string, error) {
	params := url.Values{}
	if len(body) == 0 {
		return params, "", nil
	}
	mediaType, ctParams, err := mime.ParseMediaType(contentType)
	if err != nil {
		return nil, "", err
	}
	if mediaType != "multipart/form-data" {
		q, err := url.ParseQuery(string(body))
		return q, "", err
	}
	form, err := multipart.NewReader(bytes.NewReader(body), ctParams["boundary"]).ReadForm(32 << 20)
	if err != nil {
		return nil, "", err
	}
	maps.Copy(params, form.Value)
	doc := ""
	if fs := form.File["document"]; len(fs) > 0 {
		doc = fs[0].Filename
	}
	return params, doc, form.RemoveAll()
}

func (s *Server) getUpdates(w http.ResponseWriter, r *http.Request, params url.Values) {
	offset, _ := strconv.ParseInt(params.Get("offset"), 10, 64)
	timeout, _ := strconv.Atoi(params.Get("timeout"))
	deadline := time.After(time.Duration(timeout) * time.Second)
	for {
		s.mu.Lock()
		s.updates = slices.DeleteFunc(s.updates, func(u map[string]any) bool { return u["update_id"].(int64) < offset })
		pending := slices.Clone(s.updates)
		changed := s.changed
		s.mu.Unlock()
		if len(pending) > 0 {
			s.writeResult(w, pending)
			return
		}
		select {
		case <-changed:
		case <-deadline:
			s.writeResult(w, []any{})
			return
		case <-s.closed:
			s.writeResult(w, []any{})
			return
		case <-r.Context().Done():
			return
		}
	}
}

func (s *Server) getFile(w http.ResponseWriter, params url.Values) {
	id := params.Get("file_id")
	s.mu.Lock()
	_, ok := s.files[id]
	s.mu.Unlock()
	if !ok {
		s.writeRaw(w, http.StatusBadRequest, `{"ok":false,"error_code":400,"description":"Bad Request: invalid file_id"}`)
		return
	}
	s.writeResult(w, map[string]any{"file_id": id, "file_unique_id": id, "file_path": id})
}

func (s *Server) serveFile(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/file/bot")
	_, path, _ := strings.Cut(rest, "/")
	s.mu.Lock()
	data, ok := s.files[path]
	s.mu.Unlock()
	switch {
	case !ok:
		http.NotFound(w, r)
	case data == nil:
		select {
		case <-r.Context().Done():
		case <-s.closed:
		}
	default:
		if _, err := w.Write(data); err != nil {
			s.t.Errorf("telegramtest: writing %s: %v", path, err)
		}
	}
}

// send records a message the bot sent; id is its ID, or 0 for the next.
func (s *Server) send(params url.Values, doc string, id int) map[string]any {
	chat, _ := strconv.ParseInt(params.Get("chat_id"), 10, 64)
	m := &Message{ID: id, ChatID: chat, Text: params.Get("text"), ParseMode: params.Get("parse_mode"), Buttons: buttons(params), Document: doc}
	s.mu.Lock()
	defer s.mu.Unlock()
	if m.ID == 0 {
		m.ID = s.nextMessage
		s.nextMessage++
	}
	s.messages = append(s.messages, m)
	s.notifyLocked()
	return map[string]any{"message_id": m.ID, "date": time.Now().Unix(), "chat": privateChat(chat), "text": m.Text}
}

func (s *Server) edit(w http.ResponseWriter, params url.Values) {
	chat, _ := strconv.ParseInt(params.Get("chat_id"), 10, 64)
	id, _ := strconv.Atoi(params.Get("message_id"))
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.messages, func(m *Message) bool { return m.ChatID == chat && m.ID == id })
	if i < 0 {
		s.writeRaw(w, http.StatusBadRequest, `{"ok":false,"error_code":400,"description":"Bad Request: message to edit not found"}`)
		return
	}
	m := s.messages[i]
	text, mode, btns := params.Get("text"), params.Get("parse_mode"), buttons(params)
	if m.Text == text && m.ParseMode == mode && slices.Equal(m.Buttons, btns) {
		s.writeRaw(w, http.StatusBadRequest, `{"ok":false,"error_code":400,"description":"Bad Request: message is not modified"}`)
		return
	}
	m.Text, m.ParseMode, m.Buttons = text, mode, btns
	m.Edits++
	s.notifyLocked()
	s.writeResult(w, map[string]any{"message_id": m.ID, "date": time.Now().Unix(), "chat": privateChat(chat), "text": text})
}

func buttons(params url.Values) []Button {
	var markup struct {
		InlineKeyboard [][]struct {
			Text         string `json:"text"`
			CallbackData string `json:"callback_data"`
		} `json:"inline_keyboard"`
	}
	if err := json.Unmarshal([]byte(params.Get("reply_markup")), &markup); err != nil {
		return nil
	}
	var out []Button
	for _, row := range markup.InlineKeyboard {
		for _, b := range row {
			out = append(out, Button{Text: b.Text, Data: b.CallbackData})
		}
	}
	return out
}

// forward sends a call to the upstream and records the messages it sent or
// edited there.
func (s *Server) forward(w http.ResponseWriter, r *http.Request, method string, params url.Values, doc string, body []byte) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, s.upstream+r.URL.Path, bytes.NewReader(body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			s.t.Logf("telegramtest: closing upstream response: %v", err)
		}
	}()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	var result struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
		Result      struct {
			MessageID int `json:"message_id"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		http.Error(w, fmt.Sprintf("upstream %s: %v", method, err), http.StatusBadGateway)
		return
	}
	switch {
	case !result.OK:
		s.mu.Lock()
		s.upstreamErrors = append(s.upstreamErrors, method+": "+result.Description)
		s.mu.Unlock()
	case method == "sendMessage" || method == "sendDocument":
		s.send(params, doc, result.Result.MessageID)
	case method == "editMessageText":
		s.edit(httptest.NewRecorder(), params)
	}
	s.writeRaw(w, resp.StatusCode, string(data))
}

func (s *Server) writeResult(w http.ResponseWriter, result any) {
	data, err := json.Marshal(map[string]any{"ok": true, "result": result})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.writeRaw(w, http.StatusOK, string(data))
}

func (s *Server) writeRaw(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := io.WriteString(w, body); err != nil {
		s.t.Logf("telegramtest: writing response: %v", err)
	}
}
