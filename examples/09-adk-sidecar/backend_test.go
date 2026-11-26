package adksidecar_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/plugin"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"

	adksidecar "github.com/a2aproject/a2a-serve/examples/09-adk-sidecar"
)

type recordedUpstreamRequest struct {
	Path    string
	Headers map[string]string
	Body    string
}

type mockGeminiUpstream struct {
	server *httptest.Server

	mu       sync.Mutex
	received []recordedUpstreamRequest
}

func newMockGeminiUpstream(t *testing.T) *mockGeminiUpstream {
	t.Helper()
	m := &mockGeminiUpstream{}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		headers := make(map[string]string, len(r.Header))
		for k, v := range r.Header {
			headers[strings.ToLower(k)] = strings.Join(v, ", ")
		}
		m.mu.Lock()
		m.received = append(m.received, recordedUpstreamRequest{
			Path:    r.URL.RequestURI(),
			Headers: headers,
			Body:    string(body),
		})
		m.mu.Unlock()

		if strings.Contains(r.URL.Path, "streamGenerateContent") {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			chunk := `{"candidates":[{"content":{"role":"model","parts":[{"text":"hello from mock","futurePartField":1}]}}],"futureField":{"x":1}}`
			if _, err := io.WriteString(w, "data: "+chunk+"\n\n"); err != nil {
				t.Errorf("writing mock SSE chunk: %v", err)
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if _, err := io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"hello from mock json"}]},"finishReason":"STOP"}]}`); err != nil {
			t.Errorf("writing mock JSON response: %v", err)
		}
	}))
	t.Cleanup(m.server.Close)
	return m
}

func (m *mockGeminiUpstream) Requests() []recordedUpstreamRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.received)
}

type testInvocationContext struct {
	agent.InvocationContext
	ctx  context.Context
	sess session.Session
	ag   agent.Agent
	id   string
}

func newTestInvocationContext(t *testing.T, contextID string, ag agent.Agent) agent.InvocationContext {
	t.Helper()
	ctx := context.Background()
	svc := session.InMemoryService()
	created, err := svc.Create(ctx, &session.CreateRequest{
		AppName:   "dev",
		UserID:    "u1",
		SessionID: contextID,
	})
	if err != nil {
		t.Fatalf("sessionService.Create() error = %v, want nil", err)
	}
	return &testInvocationContext{
		ctx:  ctx,
		sess: created.Session,
		ag:   ag,
		id:   "inv-" + contextID,
	}
}

func (c *testInvocationContext) Deadline() (time.Time, bool)               { return c.ctx.Deadline() }
func (c *testInvocationContext) Done() <-chan struct{}                     { return c.ctx.Done() }
func (c *testInvocationContext) Err() error                                { return c.ctx.Err() }
func (c *testInvocationContext) Value(key any) any                         { return c.ctx.Value(key) }
func (c *testInvocationContext) Agent() agent.Agent                        { return c.ag }
func (c *testInvocationContext) Artifacts() agent.Artifacts                { return nil }
func (c *testInvocationContext) Memory() agent.Memory                      { return nil }
func (c *testInvocationContext) Session() session.Session                  { return c.sess }
func (c *testInvocationContext) InvocationID() string                      { return c.id }
func (c *testInvocationContext) Branch() string                            { return "" }
func (c *testInvocationContext) IsolationScope() string                    { return "" }
func (c *testInvocationContext) UserContent() *genai.Content               { return nil }
func (c *testInvocationContext) RunConfig() *agent.RunConfig               { return &agent.RunConfig{} }
func (c *testInvocationContext) EndInvocation()                            {}
func (c *testInvocationContext) Ended() bool                               { return false }
func (c *testInvocationContext) ResumedInput(string) (any, bool)           { return nil, false }
func (c *testInvocationContext) WithContext(ctx context.Context) agent.InvocationContext {
	cp := *c
	cp.ctx = ctx
	return &cp
}
func (c *testInvocationContext) WithICDelta(*agent.InvocationContextDelta) agent.InvocationContext {
	return c
}

func startTestBackend(t *testing.T, upstreamURL string, plugins ...*plugin.Plugin) *adksidecar.A2AServeBackend {
	t.Helper()
	b, err := adksidecar.NewBackend(adksidecar.BackendConfig{
		GeminiAPIKey:   "real-gemini-secret",
		GeminiUpstream: upstreamURL,
		Workspace:      adksidecar.NewSessionWorkspace("workspace", 0),
		Plugins:        plugins,
	})
	if err != nil {
		t.Fatalf("NewBackend() error = %v, want nil", err)
	}
	if err := b.Start("127.0.0.1:0"); err != nil {
		t.Fatalf("Backend.Start() error = %v, want nil", err)
	}
	t.Cleanup(func() {
		if err := b.Close(); err != nil {
			t.Errorf("Backend.Close() error = %v", err)
		}
	})
	return b
}

func doHTTP(t *testing.T, method, url string, headers map[string]string, body []byte) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("http.NewRequest(%s %s) error = %v", method, url, err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("http.Do(%s %s) error = %v", method, url, err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("resp.Body.Close() error = %v", err)
		}
	}()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("io.ReadAll() error = %v", err)
	}
	return resp.StatusCode, resp.Header, data
}

func postJSON(t *testing.T, url string, headers map[string]string, payload any) (int, http.Header, []byte) {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	return doHTTP(t, http.MethodPost, url, headers, body)
}

func decodeJSONMap(t *testing.T, data []byte) map[string]any {
	t.Helper()
	out := make(map[string]any)
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("json.Unmarshal(%q) error = %v", string(data), err)
	}
	return out
}

func TestControlEndpointsNeedHostToken(t *testing.T) {
	t.Parallel()
	upstream := newMockGeminiUpstream(t)
	backend := startTestBackend(t, upstream.server.URL)

	urls := []string{
		backend.ToolsURL(),
		backend.GeminiMiddlewareURL() + "/v1beta/models/m:generateContent",
		backend.WorkspaceURL() + "/stat?path=.",
	}
	badHeaders := []map[string]string{
		{},
		{adksidecar.HeaderToken: "wrong"},
	}
	for _, u := range urls {
		for _, hdrs := range badHeaders {
			status, _, _ := doHTTP(t, http.MethodGet, u, hdrs, nil)
			if status != http.StatusUnauthorized {
				t.Fatalf("GET %s with %v status = %d, want %d", u, hdrs, status, http.StatusUnauthorized)
			}
		}
	}

	status, _, body := doHTTP(t, http.MethodGet, backend.WorkspaceURL()+"/stat?path=.", map[string]string{
		adksidecar.HeaderToken: backend.HostToken(),
	}, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /workspace/stat status = %d, want %d (body=%s)", status, http.StatusOK, string(body))
	}
}

func TestMCPToolsAndHooks(t *testing.T) {
	t.Parallel()
	upstream := newMockGeminiUpstream(t)
	guardrail, err := adksidecar.NewGuardrailPlugin(adksidecar.GuardrailConfig{})
	if err != nil {
		t.Fatalf("NewGuardrailPlugin() error = %v", err)
	}
	backend := startTestBackend(t, upstream.server.URL, guardrail.Plugin())
	store := adksidecar.NewDeploymentStore()
	tools, err := adksidecar.DefaultADKTools(store)
	if err != nil {
		t.Fatalf("DefaultADKTools() error = %v", err)
	}
	coder, err := adksidecar.NewSandboxAgent(adksidecar.SandboxAgentConfig{
		Name:    "coder",
		Backend: backend,
		Tools:   tools,
	})
	if err != nil {
		t.Fatalf("NewSandboxAgent() error = %v", err)
	}
	backend.RegisterInvocation("ctx-1", newTestInvocationContext(t, "ctx-1", coder), coder)
	t.Cleanup(func() { backend.UnregisterInvocation("ctx-1") })

	baseHeaders := func(extra map[string]string) map[string]string {
		h := map[string]string{
			"Accept":               "application/json, text/event-stream",
			"Content-Type":         "application/json",
			adksidecar.HeaderToken: backend.HostToken(),
		}
		for k, v := range extra {
			h[k] = v
		}
		return h
	}

	status, hdrs, body := postJSON(t, backend.ToolsURL(), baseHeaders(map[string]string{
		adksidecar.HeaderOrigin: adksidecar.OriginSidecar,
	}), map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "test", "version": "1.0"},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("initialize status = %d, want %d (body=%s)", status, http.StatusOK, string(body))
	}
	sid := hdrs.Get("Mcp-Session-Id")
	if sid == "" {
		t.Fatalf("initialize missing Mcp-Session-Id header")
	}
	initJSON := decodeJSONMap(t, body)
	caps := initJSON["result"].(map[string]any)["capabilities"].(map[string]any)
	exp := caps["experimental"].(map[string]any)
	gotHooks := exp[adksidecar.HooksCapability]
	wantHooks := map[string]any{"beforeTool": true, "afterTool": true}
	if diff := cmp.Diff(wantHooks, gotHooks); diff != "" {
		t.Fatalf("initialize experimental hooks wrong result (-want +got) diff = %s", diff)
	}

	sessionHeaders := func(extra map[string]string) map[string]string {
		h := baseHeaders(extra)
		h["Mcp-Session-Id"] = sid
		h["Mcp-Protocol-Version"] = "2025-06-18"
		return h
	}

	status, _, body = postJSON(t, backend.ToolsURL(), sessionHeaders(map[string]string{
		adksidecar.HeaderOrigin: adksidecar.OriginSidecar,
	}), map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	})
	if status != http.StatusAccepted {
		t.Fatalf("notifications/initialized status = %d, want %d (body=%s)", status, http.StatusAccepted, string(body))
	}

	status, _, body = postJSON(t, backend.ToolsURL(), sessionHeaders(map[string]string{
		adksidecar.HeaderOrigin: "sandbox",
	}), map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/list",
		"params":  map[string]any{},
	})
	if status != http.StatusOK {
		t.Fatalf("tools/list status = %d, want %d (body=%s)", status, http.StatusOK, string(body))
	}
	listed := decodeJSONMap(t, body)["result"].(map[string]any)["tools"].([]any)
	gotNames := make([]string, 0, len(listed))
	for _, item := range listed {
		gotNames = append(gotNames, item.(map[string]any)["name"].(string))
	}
	slices.Sort(gotNames)
	wantNames := []string{"deploy_service", "echo", "get_deployment_policy"}
	if diff := cmp.Diff(wantNames, gotNames); diff != "" {
		t.Fatalf("tools/list wrong result (-want +got) diff = %s", diff)
	}

	status, _, body = postJSON(t, backend.ToolsURL(), sessionHeaders(map[string]string{
		adksidecar.HeaderOrigin:  "sandbox",
		adksidecar.HeaderContext: "ctx-unknown",
	}), map[string]any{
		"jsonrpc": "2.0",
		"id":      30,
		"method":  "tools/call",
		"params":  map[string]any{"name": "get_deployment_policy", "arguments": map[string]any{"service": "payments"}},
	})
	if status != http.StatusOK {
		t.Fatalf("tools/call unknown context status = %d, want %d (body=%s)", status, http.StatusOK, string(body))
	}
	if isErr, _ := decodeJSONMap(t, body)["result"].(map[string]any)["isError"].(bool); !isErr {
		t.Fatalf("tools/call unknown context isError = %v, want true (body=%s)", isErr, string(body))
	}

	status, _, body = postJSON(t, backend.ToolsURL(), sessionHeaders(map[string]string{
		adksidecar.HeaderOrigin:  "sandbox",
		adksidecar.HeaderContext: "ctx-1",
	}), map[string]any{
		"jsonrpc": "2.0",
		"id":      3,
		"method":  "tools/call",
		"params":  map[string]any{"name": "get_deployment_policy", "arguments": map[string]any{"service": "payments"}},
	})
	if status != http.StatusOK {
		t.Fatalf("tools/call get_deployment_policy status = %d, want %d (body=%s)", status, http.StatusOK, string(body))
	}
	contentText := decodeJSONMap(t, body)["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	policyOut := decodeJSONMap(t, []byte(contentText))
	if reqCanary, _ := policyOut["require_canary"].(bool); !reqCanary {
		t.Fatalf("get_deployment_policy require_canary = %v, want true", reqCanary)
	}

	status, _, _ = postJSON(t, backend.ToolsURL(), sessionHeaders(map[string]string{
		adksidecar.HeaderOrigin: "sandbox",
	}), map[string]any{
		"jsonrpc": "2.0",
		"id":      4,
		"method":  "hooks/beforeTool",
		"params":  map[string]any{"context": "c", "task": "t", "call": map[string]any{"id": "1", "kind": "execute", "command": "ls"}},
	})
	if status != http.StatusForbidden {
		t.Fatalf("spoofed hooks/beforeTool status = %d, want %d", status, http.StatusForbidden)
	}

	beforeCases := []struct {
		name         string
		id           int
		call         map[string]any
		wantDecision string
	}{
		{
			name:         "allow safe shell command",
			id:           5,
			call:         map[string]any{"id": "1", "kind": "execute", "command": "ls"},
			wantDecision: "allow",
		},
		{
			name:         "ask for require_confirmation tool",
			id:           51,
			call:         map[string]any{"id": "3", "kind": "other", "title": "host_deploy_service", "mcp": "host.deploy_service"},
			wantDecision: "ask",
		},
		{
			name:         "deny dangerous rm command",
			id:           6,
			call:         map[string]any{"id": "2", "kind": "execute", "command": "rm -rf /tmp/x"},
			wantDecision: "deny",
		},
	}
	for _, tc := range beforeCases {
		status, _, body := postJSON(t, backend.ToolsURL(), sessionHeaders(map[string]string{
			adksidecar.HeaderOrigin: adksidecar.OriginSidecar,
		}), map[string]any{
			"jsonrpc": "2.0",
			"id":      tc.id,
			"method":  "hooks/beforeTool",
			"params":  map[string]any{"context": "ctx-1", "task": "task-1", "call": tc.call},
		})
		if status != http.StatusOK {
			t.Fatalf("%s: status = %d, want %d (body=%s)", tc.name, status, http.StatusOK, string(body))
		}
		gotDecision := decodeJSONMap(t, body)["result"].(map[string]any)["decision"].(string)
		if gotDecision != tc.wantDecision {
			t.Fatalf("%s: decision = %q, want %q", tc.name, gotDecision, tc.wantDecision)
		}
	}

	status, _, body = postJSON(t, backend.ToolsURL(), sessionHeaders(map[string]string{
		adksidecar.HeaderOrigin: adksidecar.OriginSidecar,
	}), map[string]any{
		"jsonrpc": "2.0",
		"method":  "hooks/afterTool",
		"params":  map[string]any{"context": "ctx-1", "task": "task-1", "call": map[string]any{"id": "1", "status": "completed"}},
	})
	if status != http.StatusAccepted {
		t.Fatalf("hooks/afterTool notification status = %d, want %d (body=%s)", status, http.StatusAccepted, string(body))
	}

	badSessionHeaders := sessionHeaders(map[string]string{
		adksidecar.HeaderOrigin: adksidecar.OriginSidecar,
	})
	badSessionHeaders["Mcp-Session-Id"] = "nonexistent-session"
	status, _, _ = postJSON(t, backend.ToolsURL(), badSessionHeaders, map[string]any{
		"jsonrpc": "2.0",
		"method":  "hooks/afterTool",
		"params":  map[string]any{"context": "ctx-1", "task": "task-1", "call": map[string]any{"id": "1", "status": "completed"}},
	})
	if status != http.StatusNotFound {
		t.Fatalf("hooks/afterTool unknown session status = %d, want %d", status, http.StatusNotFound)
	}

	if got := len(backend.HookEvents()); got != 4 {
		t.Fatalf("len(Backend.HookEvents()) = %d, want 4", got)
	}
	if got := len(guardrail.ToolResults()); got != 1 {
		t.Fatalf("len(GuardrailPlugin.ToolResults()) = %d, want 1", got)
	}
}

func TestEgressMiddlewareModelRoutingAndGuardrailCallbacks(t *testing.T) {
	t.Parallel()
	upstream := newMockGeminiUpstream(t)
	guardrail, err := adksidecar.NewGuardrailPlugin(adksidecar.GuardrailConfig{})
	if err != nil {
		t.Fatalf("NewGuardrailPlugin() error = %v", err)
	}
	backend := startTestBackend(t, upstream.server.URL, guardrail.Plugin())
	coder, err := adksidecar.NewSandboxAgent(adksidecar.SandboxAgentConfig{
		Name:      "coder",
		Backend:   backend,
		ModelName: "gemini-3.5-flash-lite",
	})
	if err != nil {
		t.Fatalf("NewSandboxAgent() error = %v", err)
	}
	backend.RegisterInvocation("ctx-99", newTestInvocationContext(t, "ctx-99", coder), coder)
	t.Cleanup(func() { backend.UnregisterInvocation("ctx-99") })

	status, _, body := postJSON(t, backend.GeminiMiddlewareURL()+"/v1beta/models/other-model:generateContent", map[string]string{
		adksidecar.HeaderToken:   backend.HostToken(),
		adksidecar.HeaderContext: "ctx-unknown",
		"Content-Type":           "application/json",
	}, map[string]any{
		"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hello"}}}},
	})
	if status != http.StatusForbidden {
		t.Fatalf("unknown context model call status = %d, want %d (body=%s)", status, http.StatusForbidden, string(body))
	}

	status, _, body = postJSON(t, backend.GeminiMiddlewareURL()+"/v1beta/models/other-model:streamGenerateContent?alt=sse&key=AIza-placeholder", map[string]string{
		adksidecar.HeaderToken:   backend.HostToken(),
		"x-goog-api-key":         "AIza-placeholder",
		adksidecar.HeaderContext: "ctx-99",
		adksidecar.HeaderTask:    "task-42",
		"Content-Type":           "application/json",
	}, map[string]any{
		"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hello"}}}},
	})
	if status != http.StatusOK {
		t.Fatalf("normal model call status = %d, want %d (body=%s)", status, http.StatusOK, string(body))
	}
	if !strings.Contains(string(body), "hello from mock") {
		t.Fatalf("normal model call body = %q, want substring %q", string(body), "hello from mock")
	}

	status, _, body = postJSON(t, backend.GeminiMiddlewareURL()+"/v1beta/models/other-model:streamGenerateContent?alt=sse", map[string]string{
		adksidecar.HeaderToken:   backend.HostToken(),
		adksidecar.HeaderContext: "ctx-99",
		adksidecar.HeaderTask:    "task-43",
		"Content-Type":           "application/json",
	}, map[string]any{
		"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "please BLOCK_MODEL_CALL now"}}}},
	})
	if status != http.StatusOK {
		t.Fatalf("blocked model call status = %d, want %d (body=%s)", status, http.StatusOK, string(body))
	}
	if !strings.Contains(string(body), "Blocked by GuardrailPlugin") {
		t.Fatalf("blocked model call body = %q, want substring %q", string(body), "Blocked by GuardrailPlugin")
	}

	reqs := upstream.Requests()
	if len(reqs) != 1 {
		t.Fatalf("len(upstream.Requests()) = %d, want 1", len(reqs))
	}
	got := reqs[0]
	if got.Path != "/v1beta/models/gemini-3.5-flash-lite:streamGenerateContent?alt=sse" {
		t.Fatalf("upstream path = %q, want %q", got.Path, "/v1beta/models/gemini-3.5-flash-lite:streamGenerateContent?alt=sse")
	}
	if got.Headers["x-goog-api-key"] != "real-gemini-secret" {
		t.Fatalf("upstream x-goog-api-key = %q, want %q", got.Headers["x-goog-api-key"], "real-gemini-secret")
	}
	for _, forbidden := range []string{"a2a-sidecar-context", "a2a-sidecar-task", "a2a-sidecar-token"} {
		if _, exists := got.Headers[forbidden]; exists {
			t.Fatalf("upstream headers leaked %q", forbidden)
		}
	}

	if diff := cmp.Diff([]string{"gemini-3.5-flash-lite", "gemini-3.5-flash-lite"}, guardrail.ModelRequests()); diff != "" {
		t.Fatalf("GuardrailPlugin.ModelRequests() wrong result (-want +got) diff = %s", diff)
	}
	if diff := cmp.Diff([]string{"hello from mock"}, guardrail.ModelResponses()); diff != "" {
		t.Fatalf("GuardrailPlugin.ModelResponses() wrong result (-want +got) diff = %s", diff)
	}
}

func TestBeforeModelChangesReachGemini(t *testing.T) {
	t.Parallel()
	upstream := newMockGeminiUpstream(t)

	var seenFnName string
	editPlugin, err := plugin.New(plugin.Config{
		Name: "scripted-edit",
		BeforeModelCallback: func(_ agent.Context, req *model.LLMRequest) (*model.LLMResponse, error) {
			if len(req.Contents) > 0 && len(req.Contents[0].Parts) > 0 && req.Contents[0].Parts[0].FunctionCall != nil {
				seenFnName = req.Contents[0].Parts[0].FunctionCall.Name
			}
			last := req.Contents[len(req.Contents)-1]
			last.Parts[0].Text += " (checked)"
			if req.Config == nil {
				req.Config = &genai.GenerateContentConfig{}
			}
			req.Config.SystemInstruction = &genai.Content{Parts: []*genai.Part{{Text: "be brief"}}}
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("plugin.New() error = %v", err)
	}

	backend := startTestBackend(t, upstream.server.URL, editPlugin)
	coder, err := adksidecar.NewSandboxAgent(adksidecar.SandboxAgentConfig{
		Name:    "coder",
		Backend: backend,
	})
	if err != nil {
		t.Fatalf("NewSandboxAgent() error = %v", err)
	}
	backend.RegisterInvocation("ctx-m", newTestInvocationContext(t, "ctx-m", coder), coder)
	t.Cleanup(func() { backend.UnregisterInvocation("ctx-m") })

	reqBody := map[string]any{
		"contents": []any{
			map[string]any{"role": "model", "parts": []any{map[string]any{"functionCall": map[string]any{"name": "ls", "args": map[string]any{"dir": "."}}}}},
			map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hello"}}},
		},
		"generationConfig": map[string]any{"temperature": 0.3},
	}
	status, _, respBody := postJSON(t, backend.GeminiMiddlewareURL()+"/v1beta/models/m1:generateContent", map[string]string{
		adksidecar.HeaderToken:   backend.HostToken(),
		adksidecar.HeaderContext: "ctx-m",
		"Content-Type":           "application/json",
	}, reqBody)
	if status != http.StatusOK {
		t.Fatalf("POST generateContent status = %d, want %d (body=%s)", status, http.StatusOK, string(respBody))
	}

	reqs := upstream.Requests()
	if len(reqs) != 1 {
		t.Fatalf("len(upstream.Requests()) = %d, want 1", len(reqs))
	}
	sent := decodeJSONMap(t, []byte(reqs[0].Body))
	contents := sent["contents"].([]any)
	gotText := contents[1].(map[string]any)["parts"].([]any)[0].(map[string]any)["text"].(string)
	if gotText != "hello (checked)" {
		t.Fatalf("sent text = %q, want %q", gotText, "hello (checked)")
	}
	gotFnCall := contents[0].(map[string]any)["parts"].([]any)[0].(map[string]any)["functionCall"]
	wantFnCall := map[string]any{"name": "ls", "args": map[string]any{"dir": "."}}
	if diff := cmp.Diff(wantFnCall, gotFnCall); diff != "" {
		t.Fatalf("sent functionCall wrong result (-want +got) diff = %s", diff)
	}
	sysText := sent["systemInstruction"].(map[string]any)["parts"].([]any)[0].(map[string]any)["text"].(string)
	if sysText != "be brief" {
		t.Fatalf("sent systemInstruction = %q, want %q", sysText, "be brief")
	}
	if seenFnName != "ls" {
		t.Fatalf("callback seen functionCall.Name = %q, want %q", seenFnName, "ls")
	}
}

func TestUnchangedGeminiRequestForwardedAsSent(t *testing.T) {
	t.Parallel()
	upstream := newMockGeminiUpstream(t)
	noopPlugin, err := plugin.New(plugin.Config{
		Name: "noop",
		BeforeModelCallback: func(_ agent.Context, _ *model.LLMRequest) (*model.LLMResponse, error) {
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("plugin.New() error = %v", err)
	}
	backend := startTestBackend(t, upstream.server.URL, noopPlugin)
	coder, err := adksidecar.NewSandboxAgent(adksidecar.SandboxAgentConfig{Name: "coder", Backend: backend})
	if err != nil {
		t.Fatalf("NewSandboxAgent() error = %v", err)
	}
	backend.RegisterInvocation("ctx-m", newTestInvocationContext(t, "ctx-m", coder), coder)
	t.Cleanup(func() { backend.UnregisterInvocation("ctx-m") })

	wantBody := map[string]any{
		"contents":     []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hello"}}}},
		"unknownField": float64(1),
	}
	status, _, respBody := postJSON(t, backend.GeminiMiddlewareURL()+"/v1beta/models/m1:generateContent", map[string]string{
		adksidecar.HeaderToken:   backend.HostToken(),
		adksidecar.HeaderContext: "ctx-m",
		"Content-Type":           "application/json",
	}, wantBody)
	if status != http.StatusOK {
		t.Fatalf("POST generateContent status = %d, want %d (body=%s)", status, http.StatusOK, string(respBody))
	}
	gotBody := decodeJSONMap(t, []byte(upstream.Requests()[0].Body))
	if diff := cmp.Diff(wantBody, gotBody); diff != "" {
		t.Fatalf("forwarded body wrong result (-want +got) diff = %s", diff)
	}
}

func TestAfterModelReplacesGeminiAnswer(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		path     string
		wantSeen string
		check    func(t *testing.T, body []byte)
	}{
		{
			name:     "SSE stream",
			path:     "v1beta/models/m1:streamGenerateContent?alt=sse",
			wantSeen: "hello from mock",
			check: func(t *testing.T, body []byte) {
				t.Helper()
				if !strings.Contains(string(body), `"text":"replaced"`) {
					t.Fatalf("SSE body = %q, want replaced text", string(body))
				}
			},
		},
		{
			name:     "JSON response",
			path:     "v1beta/models/m1:generateContent",
			wantSeen: "hello from mock json",
			check: func(t *testing.T, body []byte) {
				t.Helper()
				parsed := decodeJSONMap(t, body)
				parts := parsed["candidates"].([]any)[0].(map[string]any)["content"].(map[string]any)["parts"]
				want := []any{map[string]any{"text": "replaced"}}
				if diff := cmp.Diff(want, parts); diff != "" {
					t.Fatalf("JSON parts wrong result (-want +got) diff = %s", diff)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			upstream := newMockGeminiUpstream(t)
			var seenText string
			replacePlugin, err := plugin.New(plugin.Config{
				Name: "replace",
				AfterModelCallback: func(_ agent.Context, resp *model.LLMResponse, _ error) (*model.LLMResponse, error) {
					if resp != nil && resp.Content != nil {
						for _, p := range resp.Content.Parts {
							if p != nil {
								seenText += p.Text
							}
						}
					}
					return &model.LLMResponse{
						Content: &genai.Content{
							Role:  genai.RoleModel,
							Parts: []*genai.Part{{Text: "replaced"}},
						},
					}, nil
				},
			})
			if err != nil {
				t.Fatalf("plugin.New() error = %v", err)
			}
			backend := startTestBackend(t, upstream.server.URL, replacePlugin)
			coder, err := adksidecar.NewSandboxAgent(adksidecar.SandboxAgentConfig{Name: "coder", Backend: backend})
			if err != nil {
				t.Fatalf("NewSandboxAgent() error = %v", err)
			}
			backend.RegisterInvocation("ctx-m", newTestInvocationContext(t, "ctx-m", coder), coder)
			t.Cleanup(func() { backend.UnregisterInvocation("ctx-m") })

			status, _, body := postJSON(t, backend.GeminiMiddlewareURL()+"/"+tc.path, map[string]string{
				adksidecar.HeaderToken:   backend.HostToken(),
				adksidecar.HeaderContext: "ctx-m",
				"Content-Type":           "application/json",
			}, map[string]any{
				"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hi"}}}},
			})
			if status != http.StatusOK {
				t.Fatalf("POST %s status = %d, want %d (body=%s)", tc.path, status, http.StatusOK, string(body))
			}
			tc.check(t, body)
			if seenText != tc.wantSeen {
				t.Fatalf("after_model saw %q, want %q", seenText, tc.wantSeen)
			}
		})
	}
}

func TestSessionWorkspaceIsolationAndLRU(t *testing.T) {
	t.Parallel()
	upstream := newMockGeminiUpstream(t)
	backend := startTestBackend(t, upstream.server.URL)
	base := backend.WorkspaceURL()
	tok := backend.HostToken()

	status, _, body := doHTTP(t, http.MethodPut, base+"/file?path=notes.txt&mode=0644", map[string]string{
		adksidecar.HeaderToken:   tok,
		adksidecar.HeaderContext: "session-a",
	}, []byte("from a\n"))
	if status != http.StatusOK {
		t.Fatalf("PUT notes.txt status = %d, want %d (body=%s)", status, http.StatusOK, string(body))
	}

	status, _, body = doHTTP(t, http.MethodGet, base+"/file?path=notes.txt", map[string]string{
		adksidecar.HeaderToken:   tok,
		adksidecar.HeaderContext: "session-a",
	}, nil)
	if status != http.StatusOK || string(body) != "from a\n" {
		t.Fatalf("GET session-a notes.txt = (%d, %q), want (200, %q)", status, string(body), "from a\n")
	}

	status, _, _ = doHTTP(t, http.MethodGet, base+"/file?path=notes.txt", map[string]string{
		adksidecar.HeaderToken:   tok,
		adksidecar.HeaderContext: "session-b",
	}, nil)
	if status != http.StatusNotFound {
		t.Fatalf("GET session-b notes.txt status = %d, want %d", status, http.StatusNotFound)
	}

	status, _, _ = doHTTP(t, http.MethodGet, base+"/file?path=sample.py", map[string]string{
		adksidecar.HeaderToken:   tok,
		adksidecar.HeaderContext: "session-b",
	}, nil)
	if status != http.StatusOK {
		t.Fatalf("GET session-b sample.py status = %d, want %d", status, http.StatusOK)
	}

	status, _, _ = doHTTP(t, http.MethodGet, base+"/file?path=notes.txt", map[string]string{
		adksidecar.HeaderToken: tok,
	}, nil)
	if status != http.StatusNotFound {
		t.Fatalf("GET no-session notes.txt status = %d, want %d", status, http.StatusNotFound)
	}

	status, _, body = doHTTP(t, http.MethodPut, base+"/file?path=x.txt&mode=0644", map[string]string{
		adksidecar.HeaderToken: tok,
	}, []byte("x"))
	if status != http.StatusForbidden {
		t.Fatalf("PUT no-session status = %d, want %d (body=%s)", status, http.StatusForbidden, string(body))
	}
	if code := decodeJSONMap(t, body)["code"]; code != "permission" {
		t.Fatalf("PUT no-session error code = %v, want %q", code, "permission")
	}

	ws := adksidecar.NewSessionWorkspace("", 2)
	a := ws.TreeFor("a")
	b := ws.TreeFor("b")
	ws.TreeFor("a")
	ws.TreeFor("c")
	if ws.TreeFor("a") != a {
		t.Fatalf("SessionWorkspace.TreeFor(a) was evicted, want retained")
	}
	if ws.TreeFor("b") == b {
		t.Fatalf("SessionWorkspace.TreeFor(b) was retained, want evicted")
	}
}

func TestLiveSidecarAppWithBackendPluginsAndWorkspace(t *testing.T) {
	t.Parallel()
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("filepath.Abs(../..) error = %v", err)
	}
	tmpDir := t.TempDir()
	sidecarBin := filepath.Join(tmpDir, "a2a-serve-sidecar")
	fakeacpBin := filepath.Join(tmpDir, "fakeacp")
	seedDir := filepath.Join(tmpDir, "seed")
	if err := os.MkdirAll(seedDir, 0o755); err != nil {
		t.Fatalf("os.MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(seedDir, "hello.txt"), []byte("seeded workspace file\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	buildBin := func(out, pkg string) {
		cmd := exec.Command("go", "build", "-o", out, pkg)
		cmd.Dir = repoRoot
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go build %s error = %v\n%s", pkg, err, string(output))
		}
	}
	buildBin(sidecarBin, "./cmd/a2a-serve-sidecar")
	buildBin(fakeacpBin, "./internal/agentsrv/testdata/fakeacp")

	type guardedArgs struct {
		Text string `json:"text" jsonschema:"Text to echo."`
	}
	guardedEcho, err := adksidecar.NewFunctionTool(functiontool.Config{
		Name:                "guarded_echo",
		Description:         "Echo text after explicit user confirmation, reading live ADK session state.",
		RequireConfirmation: true,
	}, func(ctx agent.Context, args guardedArgs) (string, error) {
		userLabel := "unknown"
		if val, err := ctx.State().Get("user_label"); err == nil {
			if s, ok := val.(string); ok && s != "" {
				userLabel = s
			}
		}
		return "adk guarded echo (" + userLabel + "): " + args.Text, nil
	})
	if err != nil {
		t.Fatalf("NewFunctionTool(guarded_echo) error = %v", err)
	}

	guardrail, err := adksidecar.NewGuardrailPlugin(adksidecar.GuardrailConfig{})
	if err != nil {
		t.Fatalf("NewGuardrailPlugin() error = %v", err)
	}

	backend, err := adksidecar.NewBackend(adksidecar.BackendConfig{
		Binary:    sidecarBin,
		Sandbox:   "local",
		Workspace: adksidecar.NewSessionWorkspace(seedDir, 0),
		Plugins:   []*plugin.Plugin{guardrail.Plugin()},
	})
	if err != nil {
		t.Fatalf("NewBackend() error = %v", err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Errorf("Backend.Close() error = %v", err)
		}
	})

	coder, err := adksidecar.NewSandboxAgent(adksidecar.SandboxAgentConfig{
		Name:        "coder",
		Description: "Sandboxed test agent",
		Backend:     backend,
		Command:     fakeacpBin,
		Tools:       []tool.Tool{guardedEcho},
	})
	if err != nil {
		t.Fatalf("NewSandboxAgent() error = %v", err)
	}

	ctx := context.Background()
	sessSvc := session.InMemoryService()
	artSvc := artifact.InMemoryService()
	r, err := runner.New(runner.Config{
		AppName:         "dev",
		Agent:           coder,
		SessionService:  sessSvc,
		ArtifactService: artSvc,
		PluginConfig:    runner.PluginConfig{Plugins: []*plugin.Plugin{guardrail.Plugin()}},
	})
	if err != nil {
		t.Fatalf("runner.New() error = %v", err)
	}

	created, err := sessSvc.Create(ctx, &session.CreateRequest{
		AppName: "dev",
		UserID:  "test-user",
		State:   map[string]any{"user_label": "alice"},
	})
	if err != nil {
		t.Fatalf("sessionService.Create() error = %v", err)
	}

	send := func(text string) string {
		var texts []string
		for ev, err := range r.Run(ctx, "test-user", created.Session.ID(), genai.NewContentFromText(text, genai.RoleUser), agent.RunConfig{}) {
			if err != nil {
				t.Fatalf("runner.Run(%q) error = %v", text, err)
			}
			if ev == nil || ev.Content == nil {
				continue
			}
			for _, p := range ev.Content.Parts {
				if p == nil {
					continue
				}
				if p.Text != "" {
					texts = append(texts, p.Text)
				}
			}
		}
		return strings.Join(texts, "\n")
	}

	askPrompt := send("mcpcall host guarded_echo hi")
	if !strings.Contains(askPrompt, "Permission required") {
		t.Fatalf("askPrompt = %q, want substring %q", askPrompt, "Permission required")
	}

	mcpOutput := send("once")
	if !strings.Contains(mcpOutput, "mcp result: adk guarded echo (alice): hi") {
		t.Fatalf("mcpOutput = %q, want substring %q", mcpOutput, "mcp result: adk guarded echo (alice): hi")
	}

	artList, err := artSvc.List(ctx, &artifact.ListRequest{
		AppName:   "dev",
		UserID:    "test-user",
		SessionID: created.Session.ID(),
	})
	if err != nil {
		t.Fatalf("artifactService.List() error = %v", err)
	}
	if !slices.Contains(artList.FileNames, "workspace:hello.txt") {
		t.Fatalf("artifactService.List() = %v, want %q", artList.FileNames, "workspace:hello.txt")
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var sawBefore, sawAfter bool
		for _, ev := range backend.HookEvents() {
			if ev.Hook == "beforeTool" {
				sawBefore = true
			}
			if ev.Hook == "afterTool" {
				sawAfter = true
			}
		}
		if sawBefore && sawAfter && len(guardrail.ToolResults()) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	var sawBefore, sawAfter bool
	for _, ev := range backend.HookEvents() {
		if ev.Hook == "beforeTool" {
			sawBefore = true
		}
		if ev.Hook == "afterTool" {
			sawAfter = true
		}
	}
	if !sawBefore || !sawAfter {
		t.Fatalf("HookEvents() beforeTool=%v afterTool=%v, want both true", sawBefore, sawAfter)
	}
	if len(guardrail.ToolChecks()) < 1 {
		t.Fatalf("len(GuardrailPlugin.ToolChecks()) = %d, want >= 1", len(guardrail.ToolChecks()))
	}
	if len(guardrail.ToolResults()) < 1 {
		t.Fatalf("len(GuardrailPlugin.ToolResults()) = %d, want >= 1", len(guardrail.ToolResults()))
	}
}

func TestSidecarToolsConformance(t *testing.T) {
	t.Parallel()
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("filepath.Abs(../..) error = %v", err)
	}
	upstream := newMockGeminiUpstream(t)
	backend := startTestBackend(t, upstream.server.URL)
	tools, err := adksidecar.DefaultADKTools(adksidecar.NewDeploymentStore())
	if err != nil {
		t.Fatalf("DefaultADKTools() error = %v", err)
	}
	backend.RegisterTools(tools...)

	cmd := exec.Command("go", "test", "-v", "./sidecar/conformance",
		"-tools", backend.ToolsURL(),
		"-token", backend.HostToken(),
	)
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("conformance suite failed: %v\n%s", err, string(out))
	}
}

type fakeADKModel struct {
	name     string
	reply    string
	mu       sync.Mutex
	requests []*model.LLMRequest
}

func (f *fakeADKModel) Name() string { return f.name }

func (f *fakeADKModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{
			Content: &genai.Content{
				Role:  genai.RoleModel,
				Parts: []*genai.Part{{Text: f.reply}},
			},
			FinishReason: genai.FinishReasonStop,
			ModelVersion: f.name,
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount:     10,
				CandidatesTokenCount: 5,
				TotalTokenCount:      15,
			},
		}, nil)
	}
}

func TestSandboxAgentProfileAndModelFormatting(t *testing.T) {
	t.Parallel()
	upstream := newMockGeminiUpstream(t)
	backend := startTestBackend(t, upstream.server.URL)

	cases := []struct {
		name           string
		cfg            adksidecar.SandboxAgentConfig
		wantAgent      string
		wantGemini     string
		wantAnthropic  string
		wantSidecarMod string
	}{
		{
			name:           "default opencode with ADK model.LLM",
			cfg:            adksidecar.SandboxAgentConfig{Name: "c", Backend: backend, Model: &fakeADKModel{name: "models/gemini-3.5-flash-lite"}},
			wantAgent:      "opencode",
			wantGemini:     "gemini-3.5-flash-lite",
			wantAnthropic:  "",
			wantSidecarMod: "google/gemini-3.5-flash-lite",
		},
		{
			name:           "opencode with claude model",
			cfg:            adksidecar.SandboxAgentConfig{Name: "c", Backend: backend, Agent: "opencode", ModelName: "claude-sonnet-4-5"},
			wantAgent:      "opencode",
			wantGemini:     "",
			wantAnthropic:  "claude-sonnet-4-5",
			wantSidecarMod: "anthropic/claude-sonnet-4-5",
		},
		{
			name:           "claude profile with sonnet alias",
			cfg:            adksidecar.SandboxAgentConfig{Name: "c", Backend: backend, Agent: "claude", ModelName: "sonnet"},
			wantAgent:      "claude",
			wantGemini:     "",
			wantAnthropic:  "sonnet",
			wantSidecarMod: "sonnet",
		},
		{
			name:           "claude profile strips anthropic prefix",
			cfg:            adksidecar.SandboxAgentConfig{Name: "c", Backend: backend, Agent: "claude", Model: &fakeADKModel{name: "anthropic/claude-opus-4"}},
			wantAgent:      "claude",
			wantGemini:     "",
			wantAnthropic:  "claude-opus-4",
			wantSidecarMod: "claude-opus-4",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sa, err := adksidecar.NewSandboxAgent(tc.cfg)
			if err != nil {
				t.Fatalf("NewSandboxAgent() error = %v, want nil", err)
			}
			if got := sa.AgentProfile(); got != tc.wantAgent {
				t.Fatalf("SandboxAgent.AgentProfile() = %q, want %q", got, tc.wantAgent)
			}
			if got := sa.GeminiModelName(); got != tc.wantGemini {
				t.Fatalf("SandboxAgent.GeminiModelName() = %q, want %q", got, tc.wantGemini)
			}
			if got := sa.AnthropicModelName(); got != tc.wantAnthropic {
				t.Fatalf("SandboxAgent.AnthropicModelName() = %q, want %q", got, tc.wantAnthropic)
			}
			if got := sa.SidecarModelID(); got != tc.wantSidecarMod {
				t.Fatalf("SandboxAgent.SidecarModelID() = %q, want %q", got, tc.wantSidecarMod)
			}
		})
	}

	cmd := backend.BuildSidecarCommand("claude", "", "sonnet")
	joined := strings.Join(cmd, " ")
	for _, want := range []string{
		"--agent claude",
		"--model sonnet",
		"--egress-middleware generativelanguage.googleapis.com=" + backend.GeminiMiddlewareURL(),
		"--egress-middleware api.anthropic.com=" + backend.AnthropicMiddlewareURL(),
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("BuildSidecarCommand() = %q, want substring %q", joined, want)
		}
	}
}

type mockAnthropicUpstream struct {
	server *httptest.Server

	mu       sync.Mutex
	received []recordedUpstreamRequest
}

func newMockAnthropicUpstream(t *testing.T) *mockAnthropicUpstream {
	t.Helper()
	m := &mockAnthropicUpstream{}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		headers := make(map[string]string, len(r.Header))
		for k, v := range r.Header {
			headers[strings.ToLower(k)] = strings.Join(v, ", ")
		}
		m.mu.Lock()
		m.received = append(m.received, recordedUpstreamRequest{
			Path:    r.URL.RequestURI(),
			Headers: headers,
			Body:    string(body),
		})
		m.mu.Unlock()

		var probe struct {
			Stream bool `json:"stream"`
		}
		if json.Unmarshal(body, &probe) == nil && probe.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			sse := strings.Join([]string{
				`event: message_start`,
				`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[],"usage":{"input_tokens":12,"output_tokens":0}}}`,
				``,
				`event: content_block_start`,
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				``,
				`event: content_block_delta`,
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello from claude mock"}}`,
				``,
				`event: content_block_stop`,
				`data: {"type":"content_block_stop","index":0}`,
				``,
				`event: message_delta`,
				`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}`,
				``,
				`event: message_stop`,
				`data: {"type":"message_stop"}`,
				``,
				``,
			}, "\n")
			if _, err := io.WriteString(w, sse); err != nil {
				t.Errorf("writing mock Anthropic SSE: %v", err)
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if _, err := io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[{"type":"text","text":"hello from claude json"}],"stop_reason":"end_turn","usage":{"input_tokens":12,"output_tokens":4}}`); err != nil {
			t.Errorf("writing mock Anthropic JSON: %v", err)
		}
	}))
	t.Cleanup(m.server.Close)
	return m
}

func (m *mockAnthropicUpstream) Requests() []recordedUpstreamRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.received)
}

func TestAnthropicEgressMiddlewareCallbacks(t *testing.T) {
	t.Parallel()
	upstream := newMockAnthropicUpstream(t)
	guardrail, err := adksidecar.NewGuardrailPlugin(adksidecar.GuardrailConfig{})
	if err != nil {
		t.Fatalf("NewGuardrailPlugin() error = %v", err)
	}
	backend, err := adksidecar.NewBackend(adksidecar.BackendConfig{
		AnthropicAPIKey:   "real-anthropic-secret",
		AnthropicUpstream: upstream.server.URL,
		Workspace:         adksidecar.NewSessionWorkspace("workspace", 0),
		Plugins:           []*plugin.Plugin{guardrail.Plugin()},
	})
	if err != nil {
		t.Fatalf("NewBackend() error = %v", err)
	}
	if err := backend.Start("127.0.0.1:0"); err != nil {
		t.Fatalf("Backend.Start() error = %v", err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Errorf("Backend.Close() error = %v", err)
		}
	})

	coder, err := adksidecar.NewSandboxAgent(adksidecar.SandboxAgentConfig{
		Name:    "coder",
		Backend: backend,
		Agent:   "claude",
		Model:   &fakeADKModel{name: "claude-sonnet-4-5"},
	})
	if err != nil {
		t.Fatalf("NewSandboxAgent() error = %v", err)
	}
	backend.RegisterInvocation("ctx-claude", newTestInvocationContext(t, "ctx-claude", coder), coder)
	t.Cleanup(func() { backend.UnregisterInvocation("ctx-claude") })

	status, _, body := postJSON(t, backend.AnthropicMiddlewareURL()+"/v1/messages", map[string]string{
		adksidecar.HeaderToken:   backend.HostToken(),
		"x-api-key":              "sk-ant-placeholder",
		adksidecar.HeaderContext: "ctx-claude",
		adksidecar.HeaderTask:    "task-1",
		"Content-Type":           "application/json",
	}, map[string]any{
		"model":      "claude-haiku-other",
		"max_tokens": 128,
		"stream":     true,
		"messages":   []any{map[string]any{"role": "user", "content": "hello"}},
	})
	if status != http.StatusOK {
		t.Fatalf("POST /anthropic/v1/messages status = %d, want %d (body=%s)", status, http.StatusOK, string(body))
	}
	if !strings.Contains(string(body), "hello from claude mock") {
		t.Fatalf("Anthropic SSE body = %q, want substring %q", string(body), "hello from claude mock")
	}

	status, _, blockedBody := postJSON(t, backend.AnthropicMiddlewareURL()+"/v1/messages", map[string]string{
		adksidecar.HeaderToken:   backend.HostToken(),
		adksidecar.HeaderContext: "ctx-claude",
		"Content-Type":           "application/json",
	}, map[string]any{
		"model":      "claude-haiku-other",
		"max_tokens": 128,
		"stream":     true,
		"messages":   []any{map[string]any{"role": "user", "content": "please BLOCK_MODEL_CALL"}},
	})
	if status != http.StatusOK {
		t.Fatalf("blocked Anthropic call status = %d, want %d (body=%s)", status, http.StatusOK, string(blockedBody))
	}
	if !strings.Contains(string(blockedBody), "Blocked by GuardrailPlugin") {
		t.Fatalf("blocked Anthropic SSE body = %q, want substring %q", string(blockedBody), "Blocked by GuardrailPlugin")
	}

	reqs := upstream.Requests()
	if len(reqs) != 1 {
		t.Fatalf("len(upstream.Requests()) = %d, want 1", len(reqs))
	}
	if reqs[0].Headers["x-api-key"] != "real-anthropic-secret" {
		t.Fatalf("upstream x-api-key = %q, want %q", reqs[0].Headers["x-api-key"], "real-anthropic-secret")
	}
	sent := decodeJSONMap(t, []byte(reqs[0].Body))
	if sent["model"] != "claude-sonnet-4-5" {
		t.Fatalf("upstream model = %v, want %q", sent["model"], "claude-sonnet-4-5")
	}
	if diff := cmp.Diff([]string{"claude-sonnet-4-5", "claude-sonnet-4-5"}, guardrail.ModelRequests()); diff != "" {
		t.Fatalf("GuardrailPlugin.ModelRequests() wrong result (-want +got) diff = %s", diff)
	}
	if diff := cmp.Diff([]string{"hello from claude mock"}, guardrail.ModelResponses()); diff != "" {
		t.Fatalf("GuardrailPlugin.ModelResponses() wrong result (-want +got) diff = %s", diff)
	}
}

func TestADKModelLLMFallbackRouting(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "")
	guardrail, err := adksidecar.NewGuardrailPlugin(adksidecar.GuardrailConfig{})
	if err != nil {
		t.Fatalf("NewGuardrailPlugin() error = %v", err)
	}
	backend, err := adksidecar.NewBackend(adksidecar.BackendConfig{
		Workspace: adksidecar.NewSessionWorkspace("workspace", 0),
		Plugins:   []*plugin.Plugin{guardrail.Plugin()},
	})
	if err != nil {
		t.Fatalf("NewBackend() error = %v", err)
	}
	if err := backend.Start("127.0.0.1:0"); err != nil {
		t.Fatalf("Backend.Start() error = %v", err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Errorf("Backend.Close() error = %v", err)
		}
	})

	llm := &fakeADKModel{name: "gemini-3.5-flash-lite", reply: "direct from ADK model.LLM"}
	coder, err := adksidecar.NewSandboxAgent(adksidecar.SandboxAgentConfig{
		Name:    "coder",
		Backend: backend,
		Model:   llm,
	})
	if err != nil {
		t.Fatalf("NewSandboxAgent() error = %v", err)
	}
	backend.RegisterInvocation("ctx-llm", newTestInvocationContext(t, "ctx-llm", coder), coder)
	t.Cleanup(func() { backend.UnregisterInvocation("ctx-llm") })

	status, _, body := postJSON(t, backend.GeminiMiddlewareURL()+"/v1beta/models/other:generateContent", map[string]string{
		adksidecar.HeaderToken:   backend.HostToken(),
		adksidecar.HeaderContext: "ctx-llm",
		"Content-Type":           "application/json",
	}, map[string]any{
		"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hi"}}}},
	})
	if status != http.StatusOK {
		t.Fatalf("POST /gemini via ADK model.LLM status = %d, want %d (body=%s)", status, http.StatusOK, string(body))
	}
	if !strings.Contains(string(body), "direct from ADK model.LLM") {
		t.Fatalf("response body = %q, want substring %q", string(body), "direct from ADK model.LLM")
	}
	if diff := cmp.Diff([]string{"direct from ADK model.LLM"}, guardrail.ModelResponses()); diff != "" {
		t.Fatalf("GuardrailPlugin.ModelResponses() wrong result (-want +got) diff = %s", diff)
	}
}
