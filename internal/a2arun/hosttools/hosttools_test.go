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

package hosttools_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/core/a2aserve"
	"github.com/a2aproject/a2a-serve/internal/a2arun/hosttools"
	"github.com/a2aproject/a2a-serve/internal/a2arun/refhost"
	"github.com/a2aproject/a2a-serve/internal/testutil"
)

var allHooks = hosttools.HookSupport{BeforeTool: true, AfterTool: true}

// serve serves h over HTTP for the test and returns its endpoint.
func serve(t *testing.T, h http.Handler) hosttools.Endpoint {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	ep, err := hosttools.Parse(srv.URL + "/mcp?token=secret-token")
	if err != nil {
		t.Fatalf("hosttools.Parse() error = %v", err)
	}
	return ep
}

// dial starts a session with ep and ends it at the end of the test.
func dial(t *testing.T, ep hosttools.Endpoint) *hosttools.Client {
	t.Helper()
	c, err := hosttools.Dial(t.Context(), ep, "test")
	if err != nil {
		t.Fatalf("hosttools.Dial() error = %v", err)
	}
	t.Cleanup(func() {
		if err := c.Close(context.Background()); err != nil {
			t.Errorf("Client.Close() error = %v", err)
		}
	})
	return c
}

// hooks returns the hooks of a session with ep, closed at the end of the
// test, with timeouts.
func hooks(t *testing.T, ep hosttools.Endpoint, timeouts hosttools.HookTimeouts) *hosttools.Hooks {
	t.Helper()
	h := hosttools.NewHooks(dial(t, ep), timeouts, slog.New(slog.DiscardHandler))
	t.Cleanup(func() { h.Close(context.Background()) })
	return h
}

func TestParse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in       string
		wantURL  string
		wantName string
		wantErr  bool
	}{
		{in: "http://127.0.0.1:9000", wantURL: "http://127.0.0.1:9000/", wantName: "http://127.0.0.1:9000/"},
		{in: "https://tools.example/mcp?key=secret", wantURL: "https://tools.example/mcp?key=secret", wantName: "https://tools.example/mcp"},
		{in: "unix:///run/host/tools.sock", wantURL: "http://tools/", wantName: "unix:///run/host/tools.sock"},
		{in: "unix://tools.sock", wantErr: true},
		{in: "ftp://tools.example", wantErr: true},
		{in: "http://user:pass@tools.example", wantErr: true},
		{in: "tools.example:80", wantErr: true},
		{in: "http:///mcp", wantErr: true},
	}
	for _, tc := range tests {
		ep, err := hosttools.Parse(tc.in)
		if (err != nil) != tc.wantErr {
			t.Fatalf("hosttools.Parse(%q) error = %v, want error %v", tc.in, err, tc.wantErr)
		}
		if err != nil {
			continue
		}
		if ep.URL() != tc.wantURL || ep.String() != tc.wantName {
			t.Fatalf("hosttools.Parse(%q) = URL %q, String %q, want %q and %q", tc.in, ep.URL(), ep.String(), tc.wantURL, tc.wantName)
		}
	}
}

func TestDialReadsTheHooks(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cfg  refhost.ToolsConfig
		want hosttools.HookSupport
	}{
		{name: "both", cfg: refhost.ToolsConfig{Hooks: allHooks}, want: allHooks},
		{name: "after tool only", cfg: refhost.ToolsConfig{Hooks: hosttools.HookSupport{AfterTool: true}}, want: hosttools.HookSupport{AfterTool: true}},
		{name: "none", cfg: refhost.ToolsConfig{}, want: hosttools.HookSupport{}},
		{name: "event stream", cfg: refhost.ToolsConfig{Hooks: allHooks, Stream: true}, want: allHooks},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			host := refhost.NewTools(tc.cfg)
			c := dial(t, serve(t, host))
			if diff := cmp.Diff(tc.want, c.Hooks()); diff != "" {
				t.Fatalf("Client.Hooks() wrong result (-want +got) diff = %s", diff)
			}
			if got := len(host.Records("notifications/initialized")); got != 1 {
				t.Fatalf("host got %d notifications/initialized, want 1", got)
			}
		})
	}
}

func TestDialOverAUnixSocket(t *testing.T) {
	t.Parallel()
	sock := filepath.Join(testutil.ShortTempDir(t), "tools.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: refhost.NewTools(refhost.ToolsConfig{Hooks: allHooks})}
	go func() {
		if err := srv.Serve(ln); err != http.ErrServerClosed {
			t.Errorf("Server.Serve() error = %v", err)
		}
	}()
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Error(err)
		}
	})
	ep, err := hosttools.Parse("unix://" + sock)
	if err != nil {
		t.Fatal(err)
	}
	var res struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := dial(t, ep).Call(t.Context(), "tools/list", nil, &res); err != nil || len(res.Tools) != 1 || res.Tools[0].Name != refhost.ToolName {
		t.Fatalf("Client.Call(tools/list) = %+v, %v, want the tool %s", res, err, refhost.ToolName)
	}
}

func TestDecide(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		answer hosttools.BeforeToolResult
		want   a2aserve.ToolDecision
		wantOK bool
	}{
		{name: "allow", answer: hosttools.BeforeToolResult{Decision: "allow"}, want: a2aserve.ToolDecision{Allow: true, Rule: "hooks/beforeTool"}, wantOK: true},
		{name: "deny", answer: hosttools.BeforeToolResult{Decision: "deny", Message: "use go test instead"}, want: a2aserve.ToolDecision{Rule: "hooks/beforeTool"}, wantOK: true},
		{name: "ask", answer: hosttools.BeforeToolResult{Decision: "ask"}},
		{name: "unknown decision denies", answer: hosttools.BeforeToolResult{Decision: "maybe"}, want: a2aserve.ToolDecision{Rule: "hooks/beforeTool failed"}, wantOK: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			host := refhost.NewTools(refhost.ToolsConfig{Hooks: allHooks, Decide: func(hosttools.BeforeToolParams) hosttools.BeforeToolResult { return tc.answer }})
			h := hooks(t, serve(t, host), hosttools.DefaultHookTimeouts)
			got, ok, err := h.Decide(t.Context(), a2aserve.ToolCall{ID: "t1", Kind: "execute", Title: "go test", Command: "go test ./...", ContextID: "c1", TaskID: "k1"})
			if err != nil || ok != tc.wantOK {
				t.Fatalf("Hooks.Decide() = %v, %v, want ok %v and no error", ok, err, tc.wantOK)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("Hooks.Decide() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestDecideSendsTheCall(t *testing.T) {
	t.Parallel()
	host := refhost.NewTools(refhost.ToolsConfig{Hooks: allHooks})
	h := hooks(t, serve(t, host), hosttools.DefaultHookTimeouts)
	calls := []a2aserve.ToolCall{
		{ID: "t1", Kind: "execute", Title: "go test", Command: "go test ./...", ContextID: "c1", TaskID: "k1"},
		{ID: "t2", Kind: "other", Title: "host_echo", Tool: "host.echo", MCP: true, ContextID: "c1", TaskID: "k1"},
	}
	for _, c := range calls {
		if _, _, err := h.Decide(t.Context(), c); err != nil {
			t.Fatalf("Hooks.Decide() error = %v", err)
		}
	}
	var got []map[string]any
	for _, r := range host.Records(hosttools.MethodBeforeTool) {
		var p map[string]any
		if err := json.Unmarshal(r.Params, &p); err != nil {
			t.Fatal(err)
		}
		got = append(got, p)
	}
	want := []map[string]any{
		{"context": "c1", "task": "k1", "call": map[string]any{"id": "t1", "kind": "execute", "title": "go test", "command": "go test ./...", "mcp": nil}},
		{"context": "c1", "task": "k1", "call": map[string]any{"id": "t2", "kind": "other", "title": "host_echo", "command": nil, "mcp": "host.echo"}},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("hooks/beforeTool params wrong result (-want +got) diff = %s", diff)
	}
}

func TestDecideWithoutTheHookLeavesTheCall(t *testing.T) {
	t.Parallel()
	host := refhost.NewTools(refhost.ToolsConfig{Hooks: hosttools.HookSupport{AfterTool: true}})
	h := hooks(t, serve(t, host), hosttools.DefaultHookTimeouts)
	if _, ok, err := h.Decide(t.Context(), a2aserve.ToolCall{ID: "t1", Kind: "execute"}); ok || err != nil {
		t.Fatalf("Hooks.Decide() = %v, %v, want false and no error", ok, err)
	}
	if got := host.Records(hosttools.MethodBeforeTool); len(got) != 0 {
		t.Fatalf("host got %d hooks/beforeTool, want none", len(got))
	}
}

// failing serves the MCP of refhost, but answers hooks/beforeTool with
// answer.
func failing(answer http.HandlerFunc) http.Handler {
	host := refhost.NewTools(refhost.ToolsConfig{Hooks: allHooks})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if strings.Contains(string(body), hosttools.MethodBeforeTool) {
			answer(w, r)
			return
		}
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		host.ServeHTTP(w, r)
	})
}

func TestDecideDeniesWhenTheHookFails(t *testing.T) {
	t.Parallel()
	jsonAnswer := func(body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if _, err := io.WriteString(w, body); err != nil {
				return
			}
		}
	}
	tests := []struct {
		name   string
		answer http.HandlerFunc
	}{
		{name: "timeout", answer: func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }},
		{name: "server error", answer: func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "boom", http.StatusInternalServerError) }},
		{name: "JSON-RPC error", answer: jsonAnswer(`{"jsonrpc":"2.0","id":2,"error":{"code":-32601,"message":"method not found"}}`)},
		{name: "not JSON", answer: jsonAnswer(`<html>`)},
		{name: "another ID", answer: jsonAnswer(`{"jsonrpc":"2.0","id":99,"result":{"decision":"allow"}}`)},
		{name: "no decision", answer: jsonAnswer(`{"jsonrpc":"2.0","id":2,"result":{}}`)},
		{name: "decision of another type", answer: jsonAnswer(`{"jsonrpc":"2.0","id":2,"result":{"decision":true}}`)},
		{name: "too large", answer: jsonAnswer(`{"jsonrpc":"2.0","id":2,"result":{"decision":"allow","message":"` + strings.Repeat("x", hosttools.MaxMessage) + `"}}`)},
		{name: "event stream without the answer", answer: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			if _, err := io.WriteString(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/message\"}\n\n"); err != nil {
				return
			}
		}},
		{name: "plain text", answer: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			if _, err := io.WriteString(w, "allow"); err != nil {
				return
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := hooks(t, serve(t, failing(tc.answer)), hosttools.HookTimeouts{BeforeTool: 200 * time.Millisecond, AfterTool: time.Second})
			got, ok, err := h.Decide(t.Context(), a2aserve.ToolCall{ID: "t1", Kind: "execute", Command: "go test"})
			want := a2aserve.ToolDecision{Rule: "hooks/beforeTool failed"}
			if err != nil || !ok || got != want {
				t.Fatalf("Hooks.Decide() = %+v, %v, %v, want %+v, true and no error", got, ok, err, want)
			}
		})
	}
}

func TestErrorsLeaveOutTheQuery(t *testing.T) {
	t.Parallel()
	ep := serve(t, failing(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "boom", http.StatusInternalServerError) }))
	err := dial(t, ep).Call(t.Context(), hosttools.MethodBeforeTool, map[string]any{}, nil)
	if err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("Client.Call() error = %v, want an error without the query of the URL", err)
	}
}

func TestCallStartsANewSessionWhenTheHostForgetsIt(t *testing.T) {
	t.Parallel()
	host := refhost.NewTools(refhost.ToolsConfig{Hooks: allHooks})
	h := hooks(t, serve(t, host), hosttools.DefaultHookTimeouts)
	host.ForgetSessions()
	got, ok, err := h.Decide(t.Context(), a2aserve.ToolCall{ID: "t1", Kind: "execute", Command: "ls"})
	if err != nil || !ok || !got.Allow {
		t.Fatalf("Hooks.Decide() after the host forgot the session = %+v, %v, %v, want allowed", got, ok, err)
	}
	if n := len(host.Records("initialize")); n != 2 {
		t.Fatalf("host got %d initialize, want 2", n)
	}
}

func TestToolDoneSendsAfterToolInOrder(t *testing.T) {
	t.Parallel()
	host := refhost.NewTools(refhost.ToolsConfig{Hooks: allHooks, Stream: true})
	h := hosttools.NewHooks(dial(t, serve(t, host)), hosttools.DefaultHookTimeouts, slog.New(slog.DiscardHandler))
	for _, id := range []string{"t1", "t2", "t3"} {
		h.ToolDone(t.Context(), a2aserve.ToolResult{ID: id, Kind: "execute", Title: "go test", Status: "completed", Output: "ok " + id, ContextID: "c1", TaskID: "k1"})
	}
	h.Close(t.Context())
	h.ToolDone(t.Context(), a2aserve.ToolResult{ID: "late"})
	var got []hosttools.AfterToolParams
	for _, r := range host.Records(hosttools.MethodAfterTool) {
		var p hosttools.AfterToolParams
		if err := json.Unmarshal(r.Params, &p); err != nil {
			t.Fatal(err)
		}
		got = append(got, p)
	}
	var want []hosttools.AfterToolParams
	for _, id := range []string{"t1", "t2", "t3"} {
		want = append(want, hosttools.AfterToolParams{Context: "c1", Task: "k1", Call: hosttools.AfterCall{ID: id, Kind: "execute", Title: "go test", Status: "completed", Output: "ok " + id}})
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("hooks/afterTool params wrong result (-want +got) diff = %s", diff)
	}
}

func TestToolDoneWithoutTheHookSendsNothing(t *testing.T) {
	t.Parallel()
	host := refhost.NewTools(refhost.ToolsConfig{Hooks: hosttools.HookSupport{BeforeTool: true}})
	h := hosttools.NewHooks(dial(t, serve(t, host)), hosttools.DefaultHookTimeouts, slog.New(slog.DiscardHandler))
	h.ToolDone(t.Context(), a2aserve.ToolResult{ID: "t1", Status: "completed"})
	h.Close(t.Context())
	if got := host.Records(hosttools.MethodAfterTool); len(got) != 0 {
		t.Fatalf("host got %d hooks/afterTool, want none", len(got))
	}
}

func TestDialFailsWithoutAnMCPServer(t *testing.T) {
	t.Parallel()
	ep := serve(t, http.NotFoundHandler())
	if _, err := hosttools.Dial(t.Context(), ep, "test"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("hosttools.Dial() error = %v, want a 404", err)
	}
}

func TestProxySendsRequestsToTheEndpoint(t *testing.T) {
	t.Parallel()
	got := make(chan string, 1)
	ep := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Host + " " + r.URL.RequestURI()
		w.WriteHeader(http.StatusNoContent)
	}))
	if ep.Path() != "/mcp" {
		t.Fatalf("Endpoint.Path() = %q, want /mcp", ep.Path())
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://tools.a2a-run.internal/mcp?token=agent", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := ep.Proxy().RoundTrip(req)
	if err != nil {
		t.Fatalf("Proxy().RoundTrip() error = %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(ep.URL())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := <-got, u.Host+" /mcp?token=secret-token"; got != want {
		t.Fatalf("Proxy().RoundTrip() reached the endpoint with %q, want %q", got, want)
	}
}

func TestProxyRefusesWhatIsNotTheEndpoint(t *testing.T) {
	t.Parallel()
	tests := []struct {
		method, path string
		wantStatus   int
		wantReached  bool
	}{
		{method: http.MethodPost, path: "/mcp", wantStatus: http.StatusNoContent, wantReached: true},
		{method: http.MethodGet, path: "/mcp", wantStatus: http.StatusNoContent, wantReached: true},
		{method: http.MethodDelete, path: "/mcp", wantStatus: http.StatusNoContent, wantReached: true},
		{method: http.MethodGet, path: "/admin", wantStatus: http.StatusForbidden},
		{method: http.MethodPost, path: "/mcp/x", wantStatus: http.StatusForbidden},
		{method: http.MethodPost, path: "/MCP", wantStatus: http.StatusForbidden},
		{method: http.MethodPost, path: "/a/../mcp", wantStatus: http.StatusForbidden},
		{method: http.MethodPut, path: "/mcp", wantStatus: http.StatusMethodNotAllowed},
		{method: http.MethodOptions, path: "/mcp", wantStatus: http.StatusMethodNotAllowed},
	}
	for _, tc := range tests {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			t.Parallel()
			reached := make(chan struct{}, 1)
			ep := serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				reached <- struct{}{}
				w.WriteHeader(http.StatusNoContent)
			}))
			req, err := http.NewRequestWithContext(t.Context(), tc.method, "http://tools.a2a-run.internal"+tc.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := ep.Proxy().RoundTrip(req)
			if err != nil {
				t.Fatalf("Proxy().RoundTrip() error = %v", err)
			}
			if err := resp.Body.Close(); err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != tc.wantStatus || (len(reached) == 1) != tc.wantReached {
				t.Fatalf("Proxy().RoundTrip(%s %s) = %d, reached the endpoint %v, want %d and %v", tc.method, tc.path, resp.StatusCode, len(reached) == 1, tc.wantStatus, tc.wantReached)
			}
		})
	}
}

func TestProxyMarksTheSandboxOrigin(t *testing.T) {
	t.Parallel()
	got := make(chan http.Header, 1)
	ep := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Clone()
		w.WriteHeader(http.StatusNoContent)
	}))
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://tools.a2a-run.internal/mcp", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(hosttools.HeaderOrigin, hosttools.OriginA2ARun)
	req.Header["A2a_Run_Origin"] = []string{hosttools.OriginA2ARun}
	req.Header.Set("A2A-Run-Context", "another")
	req.Header.Set("X-Other", "kept")
	resp, err := ep.Proxy().RoundTrip(req)
	if err != nil {
		t.Fatalf("Proxy().RoundTrip() error = %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	h := <-got
	var a2aRun []string
	for name, v := range h {
		if strings.HasPrefix(strings.ToLower(strings.ReplaceAll(name, "_", "-")), "a2a-run-") {
			a2aRun = append(a2aRun, name+"="+strings.Join(v, ","))
		}
	}
	if diff := cmp.Diff([]string{http.CanonicalHeaderKey(hosttools.HeaderOrigin) + "=" + hosttools.OriginSandbox}, a2aRun); diff != "" || h.Get("X-Other") != "kept" {
		t.Fatalf("headers that reached the endpoint wrong result (-want +got) diff = %s, X-Other %q", diff, h.Get("X-Other"))
	}
}

func TestClientSendsTheA2ARunOrigin(t *testing.T) {
	t.Parallel()
	host := refhost.NewTools(refhost.ToolsConfig{Hooks: allHooks})
	var mu sync.Mutex
	var origins []string
	ep := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		origins = append(origins, r.Method+" "+r.Header.Get(hosttools.HeaderOrigin))
		mu.Unlock()
		host.ServeHTTP(w, r)
	}))
	c, err := hosttools.Dial(t.Context(), ep, "test")
	if err != nil {
		t.Fatalf("hosttools.Dial() error = %v", err)
	}
	if err := c.Close(t.Context()); err != nil {
		t.Fatalf("Client.Close() error = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if diff := cmp.Diff([]string{"POST a2a-run", "POST a2a-run", "DELETE a2a-run"}, origins); diff != "" {
		t.Fatalf("origins of the client's requests wrong result (-want +got) diff = %s", diff)
	}
}

func TestToolDoneStartsANewSessionWhenTheHostForgetsIt(t *testing.T) {
	t.Parallel()
	host := refhost.NewTools(refhost.ToolsConfig{Hooks: allHooks})
	h := hosttools.NewHooks(dial(t, serve(t, host)), hosttools.DefaultHookTimeouts, slog.New(slog.DiscardHandler))
	host.ForgetSessions()
	h.ToolDone(t.Context(), a2aserve.ToolResult{ID: "t1", Status: "completed"})
	h.ToolDone(t.Context(), a2aserve.ToolResult{ID: "t2", Status: "completed"})
	h.Close(t.Context())
	if got := len(host.Records(hosttools.MethodAfterTool)); got != 2 {
		t.Fatalf("host got %d hooks/afterTool after it forgot the session, want 2", got)
	}
	if got := len(host.Records("initialize")); got != 2 {
		t.Fatalf("host got %d initialize, want 2", got)
	}
}

func TestConcurrentCallsShareOneNewSession(t *testing.T) {
	t.Parallel()
	host := refhost.NewTools(refhost.ToolsConfig{Hooks: allHooks})
	var mu sync.Mutex
	deletes := 0
	ep := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			mu.Lock()
			deletes++
			mu.Unlock()
		}
		host.ServeHTTP(w, r)
	}))
	h := hooks(t, ep, hosttools.DefaultHookTimeouts)
	host.ForgetSessions()
	const n = 8
	var wg sync.WaitGroup
	errs := make(chan string, n)
	for i := range n {
		wg.Go(func() {
			got, ok, err := h.Decide(context.Background(), a2aserve.ToolCall{ID: fmt.Sprint("t", i), Kind: "execute", Command: "ls"})
			if err != nil || !ok || !got.Allow {
				errs <- fmt.Sprintf("Hooks.Decide() = %+v, %v, %v, want allowed", got, ok, err)
			}
		})
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
	if got := len(host.Records("initialize")); got != 2 {
		t.Fatalf("host got %d initialize for %d calls after it forgot the session, want 2", got, n)
	}
	mu.Lock()
	defer mu.Unlock()
	if deletes != 1 {
		t.Fatalf("client ended %d sessions, want the replaced one ended once", deletes)
	}
}
