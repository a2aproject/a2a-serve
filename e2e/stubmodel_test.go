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

//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// stubModel is an OpenAI-compatible chat completions API that plays a model
// from a script. It records the requests, so that tests see what the model
// got.
type stubModel struct {
	t      *testing.T
	script modelScript
	srv    *httptest.Server
	// url is the base URL of the API, which containers reach.
	url string

	// auth is the Authorization header that requests need, or "" for any.
	auth string

	mu       sync.Mutex
	requests []chatRequest
	// auths are the Authorization headers of the requests.
	auths []string
}

// modelScript is how a stubModel answers a turn: with the tool calls, one
// per request, then with the answer. A non-zero status fails every request
// with body instead.
type modelScript struct {
	calls  []toolCall
	answer string
	status int
	body   string
}

type toolCall struct {
	name string
	args map[string]any
}

type chatRequest struct {
	Model    string        `json:"model"`
	Stream   bool          `json:"stream"`
	Messages []chatMessage `json:"messages"`
	Tools    []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
}

type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCallID string          `json:"tool_call_id"`
	ToolCalls  []struct {
		ID       string `json:"id"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tool_calls"`
}

// text returns the content of m, a string or a list of text parts.
func (m chatMessage) text() string {
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(m.Content, &parts); err != nil {
		return string(m.Content)
	}
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

// newStubModel starts a stubModel on the host's address on its default
// route. Containers reach it with rootful and rootless Docker alike; with
// rootless Docker, host.docker.internal leads to the network namespace of
// the daemon.
func newStubModel(t *testing.T, script modelScript) *stubModel {
	t.Helper()
	ip, err := defaultRouteIP()
	if err != nil {
		t.Fatalf("finding the host address for the stub model: %v", err)
	}
	l, err := net.Listen("tcp", net.JoinHostPort(ip, "0"))
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	m := &stubModel{t: t, script: script}
	m.srv = &httptest.Server{Listener: l, Config: &http.Server{Handler: m}}
	m.srv.Start()
	t.Cleanup(m.srv.Close)
	m.url = m.srv.URL + "/v1"
	return m
}

// defaultRouteIP returns the local address of the default route. Dialing
// UDP sends no packet.
func defaultRouteIP() (string, error) {
	c, err := net.Dial("udp", "192.0.2.1:9")
	if err != nil {
		return "", err
	}
	defer func() {
		if err := c.Close(); err != nil {
			fmt.Printf("e2e: closing a UDP socket: %v\n", err)
		}
	}()
	return c.LocalAddr().(*net.UDPAddr).IP.String(), nil
}

func (m *stubModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
		http.NotFound(w, r)
		return
	}
	m.mu.Lock()
	m.auths = append(m.auths, r.Header.Get("Authorization"))
	m.mu.Unlock()
	if m.auth != "" && r.Header.Get("Authorization") != m.auth {
		m.write(w, http.StatusUnauthorized, "application/json", `{"error":{"message":"wrong key","type":"invalid_request_error"}}`)
		return
	}
	var req chatRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20)).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	m.mu.Lock()
	m.requests = append(m.requests, req)
	m.mu.Unlock()
	if m.script.status != 0 {
		m.write(w, m.script.status, "application/json", m.script.body)
		return
	}
	text, call, step := m.reply(req)
	if req.Stream {
		m.write(w, http.StatusOK, "text/event-stream", streamBody(req.Model, text, call, step))
		return
	}
	m.write(w, http.StatusOK, "application/json", completionBody(req.Model, text, call, step))
}

// reply returns the next message of the script and its step: a tool call
// for each tool call the turn made so far, then the answer. Requests without
// tools, such as opencode's title requests, get a title.
func (m *stubModel) reply(req chatRequest) (string, *toolCall, int) {
	if len(req.Tools) == 0 {
		return "Scripted turn", nil, 0
	}
	step := 0
	for _, msg := range req.Messages {
		switch {
		case msg.Role == "user":
			step = 0
		case msg.Role == "assistant" && len(msg.ToolCalls) > 0:
			step++
		}
	}
	if step < len(m.script.calls) {
		return "", &m.script.calls[step], step
	}
	return m.script.answer, nil, step
}

func (m *stubModel) write(w http.ResponseWriter, status int, contentType, body string) {
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	if _, err := io.WriteString(w, body); err != nil {
		m.t.Logf("stub model: writing a response: %v", err)
	}
}

// toolResults returns the results of the tool calls of the last request
// with tools, by tool name.
func (m *stubModel) toolResults() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.requests) - 1; i >= 0; i-- {
		req := m.requests[i]
		if len(req.Tools) == 0 {
			continue
		}
		names := map[string]string{}
		results := map[string]string{}
		for _, msg := range req.Messages {
			for _, c := range msg.ToolCalls {
				names[c.ID] = c.Function.Name
			}
			if msg.Role == "tool" {
				results[names[msg.ToolCallID]] = msg.text()
			}
		}
		return results
	}
	return nil
}

// authorizations returns the Authorization headers of the requests so far.
func (m *stubModel) authorizations() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.auths...)
}

// requestCount returns the number of chat completion requests so far.
func (m *stubModel) requestCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.requests)
}

func callJSON(c *toolCall, step int) map[string]any {
	args, err := json.Marshal(c.args)
	if err != nil {
		panic(err)
	}
	return map[string]any{
		"index":    0,
		"id":       fmt.Sprintf("call_%d", step),
		"type":     "function",
		"function": map[string]any{"name": c.name, "arguments": string(args)},
	}
}

var stubUsage = map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}

func streamBody(model, text string, call *toolCall, step int) string {
	delta := map[string]any{"role": "assistant", "content": text}
	finish := "stop"
	if call != nil {
		delta = map[string]any{"role": "assistant", "tool_calls": []any{callJSON(call, step)}}
		finish = "tool_calls"
	}
	var b strings.Builder
	for _, chunk := range []map[string]any{
		{"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}}},
		{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}}, "usage": stubUsage},
	} {
		chunk["id"], chunk["object"], chunk["created"], chunk["model"] = "chatcmpl-stub", "chat.completion.chunk", 0, model
		data, err := json.Marshal(chunk)
		if err != nil {
			panic(err)
		}
		fmt.Fprintf(&b, "data: %s\n\n", data)
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func completionBody(model, text string, call *toolCall, step int) string {
	msg := map[string]any{"role": "assistant", "content": text}
	finish := "stop"
	if call != nil {
		msg = map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{callJSON(call, step)}}
		finish = "tool_calls"
	}
	data, err := json.Marshal(map[string]any{
		"id": "chatcmpl-stub", "object": "chat.completion", "created": 0, "model": model,
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}},
		"usage":   stubUsage,
	})
	if err != nil {
		panic(err)
	}
	return string(data)
}
