package adksidecar

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/plugin"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"
)

const (
	HeaderOrigin  = "A2A-Sidecar-Origin"
	OriginSidecar = "a2a-serve-sidecar"
	HeaderContext = "A2A-Sidecar-Context"
	HeaderTask    = "A2A-Sidecar-Task"
	HeaderToken   = "A2A-Sidecar-Token"

	MethodBeforeTool = "hooks/beforeTool"
	MethodAfterTool  = "hooks/afterTool"
	HooksCapability  = "a2a-sidecar/hooks"

	// MaxModelResponse bounds a model answer buffered for after_model callbacks.
	MaxModelResponse = 16 << 20
	maxMCPBody       = 1 << 20
)

var hopByHopHeaders = map[string]bool{
	"connection":          true,
	"keep-alive":          true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"te":                  true,
	"trailers":            true,
	"transfer-encoding":   true,
	"upgrade":             true,
	"host":                true,
	"content-length":      true,
}

// BeforeCall describes a tool call reported to hooks/beforeTool.
type BeforeCall struct {
	ID      string `json:"id"`
	Kind    string `json:"kind,omitempty"`
	Title   string `json:"title,omitempty"`
	Command string `json:"command,omitempty"`
	MCP     string `json:"mcp,omitempty"`
}

// BeforeToolParams are the JSON-RPC params of hooks/beforeTool.
type BeforeToolParams struct {
	mcp.ParamsBase
	Context string     `json:"context,omitempty"`
	Task    string     `json:"task,omitempty"`
	Call    BeforeCall `json:"call"`
}

// BeforeToolResult is the JSON-RPC result of hooks/beforeTool.
type BeforeToolResult struct {
	mcp.ResultBase
	Decision string `json:"decision"`
	Message  string `json:"message,omitempty"`
}

// AfterCall describes a completed or failed tool call reported to hooks/afterTool.
type AfterCall struct {
	ID        string `json:"id,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Title     string `json:"title,omitempty"`
	Status    string `json:"status,omitempty"`
	Output    string `json:"output,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

// AfterToolParams are the JSON-RPC params of hooks/afterTool.
type AfterToolParams struct {
	mcp.ParamsBase
	Context string    `json:"context,omitempty"`
	Task    string    `json:"task,omitempty"`
	Call    AfterCall `json:"call"`
}

// AfterToolResult is the empty result returned by the registered hooks/afterTool
// custom method handler.
type AfterToolResult struct {
	mcp.ResultBase
}

// MiddlewareEvent records an intercepted model egress request.
type MiddlewareEvent struct {
	Method            string
	Path              string
	ContextID         string
	TaskID            string
	Status            int
	BlockedByCallback bool
}

// HookEvent records a hooks/beforeTool or hooks/afterTool invocation.
type HookEvent struct {
	Hook      string
	ContextID string
	TaskID    string
	Call      map[string]any
	Decision  string
	Message   string
}

type activeInvocation struct {
	contextID string
	invCtx    agent.InvocationContext
	agent     *SandboxAgent
}

type sidecarKey struct {
	agent   string
	command string
	model   string
}

type readyLine struct {
	Ready bool   `json:"ready"`
	A2A   string `json:"a2a"`
	Token string `json:"token"`
	Error string `json:"error"`
}

type managedSidecar struct {
	key        sidecarKey
	cmd        *exec.Cmd
	ready      readyLine
	waitDone   chan struct{}
	stderrDone chan struct{}
	mu         sync.Mutex
	exited     bool
	stderrBuf  []string
}

func (m *managedSidecar) a2aURL() string {
	return strings.TrimRight(m.ready.A2A, "/")
}

func (m *managedSidecar) token() string {
	return m.ready.Token
}

func (m *managedSidecar) alive() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return !m.exited
}

// BackendConfig configures an A2AServeBackend.
type BackendConfig struct {
	Sandbox           string
	Egress            string
	EgressAllow       []string
	Workspace         *SessionWorkspace
	Contacts          map[string]string
	Binary            string
	GeminiAPIKey      string
	GeminiUpstream    string
	AnthropicAPIKey   string
	AnthropicUpstream string
	Plugins           []*plugin.Plugin
}

// A2AServeBackend starts and shares a2a-serve-sidecar processes and serves the
// ADK control endpoints (/mcp, /gemini/..., /anthropic/..., /workspace/...).
type A2AServeBackend struct {
	sandbox           string
	egress            string
	egressAllow       []string
	workspace         *SessionWorkspace
	hostToken         string
	contacts          map[string]string
	binary            string
	geminiAPIKey      string
	geminiUpstream    string
	anthropicAPIKey   string
	anthropicUpstream string
	plugins           []*plugin.Plugin

	httpClient *http.Client
	gemini     GeminiAdapter
	anthropic  AnthropicAdapter
	mcpServer  *mcp.Server
	mcpHandler http.Handler
	handler    http.Handler

	// startMu serializes sidecar subprocess launches without holding mu while
	// the new sidecar connects back to /mcp.
	startMu sync.Mutex

	mu               sync.Mutex
	tools            map[string]tool.Tool
	invocations      map[string]*activeInvocation
	sidecars         map[sidecarKey]*managedSidecar
	middlewareEvents []MiddlewareEvent
	hookEvents       []HookEvent

	server   *http.Server
	listener net.Listener
	baseURL  string
}

// NewBackend constructs an A2AServeBackend with its MCP server, custom hook
// methods, workspace router, and model egress middleware wired.
func NewBackend(cfg BackendConfig) (*A2AServeBackend, error) {
	if cfg.Sandbox == "" {
		cfg.Sandbox = "docker"
	}
	if cfg.Egress == "" {
		cfg.Egress = "allowlist"
	}
	if cfg.Workspace == nil {
		cfg.Workspace = NewSessionWorkspace("./workspace", 0)
	}
	if cfg.Binary == "" {
		cfg.Binary = defaultSidecarBinary()
	}
	if cfg.GeminiAPIKey == "" {
		cfg.GeminiAPIKey = os.Getenv("GEMINI_API_KEY")
	}
	if cfg.GeminiUpstream == "" {
		cfg.GeminiUpstream = "https://generativelanguage.googleapis.com"
	}
	if cfg.AnthropicAPIKey == "" {
		cfg.AnthropicAPIKey = os.Getenv("ANTHROPIC_API_KEY")
	}
	if cfg.AnthropicUpstream == "" {
		cfg.AnthropicUpstream = "https://api.anthropic.com"
	}

	token, err := randomToken()
	if err != nil {
		return nil, fmt.Errorf("generating host token: %w", err)
	}

	b := &A2AServeBackend{
		sandbox:           cfg.Sandbox,
		egress:            cfg.Egress,
		egressAllow:       slices.Clone(cfg.EgressAllow),
		workspace:         cfg.Workspace,
		hostToken:         token,
		contacts:          maps.Clone(cfg.Contacts),
		binary:            cfg.Binary,
		geminiAPIKey:      cfg.GeminiAPIKey,
		geminiUpstream:    strings.TrimRight(cfg.GeminiUpstream, "/"),
		anthropicAPIKey:   cfg.AnthropicAPIKey,
		anthropicUpstream: strings.TrimRight(cfg.AnthropicUpstream, "/"),
		plugins:           slices.Clone(cfg.Plugins),
		httpClient:        &http.Client{Timeout: 10 * time.Minute},
		tools:             make(map[string]tool.Tool),
		invocations:       make(map[string]*activeInvocation),
		sidecars:          make(map[sidecarKey]*managedSidecar),
	}

	b.mcpServer = mcp.NewServer(
		&mcp.Implementation{Name: "adk-a2a-serve-backend", Version: "0.1.0"},
		&mcp.ServerOptions{
			Capabilities: &mcp.ServerCapabilities{
				Tools: &mcp.ToolCapabilities{},
				Experimental: map[string]any{
					HooksCapability: map[string]any{
						"beforeTool": true,
						"afterTool":  true,
					},
				},
			},
		},
	)

	if err := mcp.AddReceivingCustomMethod(b.mcpServer, MethodBeforeTool, b.handleBeforeTool); err != nil {
		return nil, fmt.Errorf("registering %s: %w", MethodBeforeTool, err)
	}
	if err := mcp.AddReceivingCustomMethod(b.mcpServer, MethodAfterTool, b.handleAfterTool); err != nil {
		return nil, fmt.Errorf("registering %s: %w", MethodAfterTool, err)
	}

	b.mcpHandler = mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return b.mcpServer },
		&mcp.StreamableHTTPOptions{JSONResponse: true},
	)
	b.handler = b.buildHandler()
	return b, nil
}

// HostToken returns the run-scoped token required in A2A-Sidecar-Token.
func (b *A2AServeBackend) HostToken() string { return b.hostToken }

// Workspace returns the backend's SessionWorkspace.
func (b *A2AServeBackend) Workspace() *SessionWorkspace { return b.workspace }

// BaseURL returns the control server's loopback URL after Start.
func (b *A2AServeBackend) BaseURL() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.baseURL
}

// ToolsURL returns the MCP tools endpoint URL (/mcp).
func (b *A2AServeBackend) ToolsURL() string { return b.BaseURL() + "/mcp" }

// WorkspaceURL returns the HTTP workspace endpoint URL (/workspace).
func (b *A2AServeBackend) WorkspaceURL() string { return b.BaseURL() + "/workspace" }

// GeminiMiddlewareURL returns the Gemini egress middleware URL (/gemini).
func (b *A2AServeBackend) GeminiMiddlewareURL() string { return b.BaseURL() + "/gemini" }

// AnthropicMiddlewareURL returns the Anthropic egress middleware URL (/anthropic).
func (b *A2AServeBackend) AnthropicMiddlewareURL() string { return b.BaseURL() + "/anthropic" }

// MiddlewareEvents returns a snapshot of recorded model middleware events.
func (b *A2AServeBackend) MiddlewareEvents() []MiddlewareEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.middlewareEvents)
}

// HookEvents returns a snapshot of recorded hook events.
func (b *A2AServeBackend) HookEvents() []HookEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.hookEvents)
}

// RegisterTools registers ADK tools on the MCP server using mcp.AddTool.
func (b *A2AServeBackend) RegisterTools(tools ...tool.Tool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, t := range tools {
		if t == nil {
			continue
		}
		b.tools[t.Name()] = t
		b.addMCPToolLocked(t)
	}
}

func (b *A2AServeBackend) addMCPToolLocked(t tool.Tool) {
	name := t.Name()
	mcpTool := &mcp.Tool{
		Name:        name,
		Description: t.Description(),
		InputSchema: toolInputSchema(t),
	}
	if declTool, ok := t.(declarableTool); ok {
		if decl := declTool.Declaration(); decl != nil && decl.Description != "" {
			mcpTool.Description = decl.Description
		}
	}
	mcp.AddTool[map[string]any, any](b.mcpServer, mcpTool, func(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
		return b.callADKTool(ctx, req, name, args)
	})
}

func toolInputSchema(t tool.Tool) any {
	if declTool, ok := t.(declarableTool); ok {
		if decl := declTool.Declaration(); decl != nil && decl.ParametersJsonSchema != nil {
			if s, ok := decl.ParametersJsonSchema.(*jsonschema.Schema); ok {
				cp := *s
				if cp.Type == "" {
					cp.Type = "object"
				}
				return &cp
			}
			return decl.ParametersJsonSchema
		}
	}
	return &jsonschema.Schema{Type: "object", Properties: map[string]*jsonschema.Schema{}}
}

// RegisterInvocation binds an active ADK invocation and SandboxAgent to contextID.
func (b *A2AServeBackend) RegisterInvocation(contextID string, invCtx agent.InvocationContext, sa *SandboxAgent) {
	if sa != nil {
		b.RegisterTools(sa.Tools()...)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.invocations[contextID] = &activeInvocation{
		contextID: contextID,
		invCtx:    invCtx,
		agent:     sa,
	}
}

// UnregisterInvocation removes the active invocation for contextID.
func (b *A2AServeBackend) UnregisterInvocation(contextID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.invocations, contextID)
}

func (b *A2AServeBackend) lookupInvocation(contextID string) *activeInvocation {
	if contextID == "" {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.invocations[contextID]
}

// Start binds and serves the shared loopback HTTP control server.
func (b *A2AServeBackend) Start(addr string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.server != nil {
		return nil
	}
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", addr, err)
	}
	srv := &http.Server{
		Handler:           b.handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	b.listener = ln
	b.server = srv
	b.baseURL = "http://" + ln.Addr().String()
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("control server exited", "error", err)
		}
	}()
	return nil
}

// Close stops all managed sidecar processes and the shared control server.
func (b *A2AServeBackend) Close() error {
	b.mu.Lock()
	sidecars := make([]*managedSidecar, 0, len(b.sidecars))
	for _, m := range b.sidecars {
		sidecars = append(sidecars, m)
	}
	clear(b.sidecars)
	srv := b.server
	b.server = nil
	b.listener = nil
	b.mu.Unlock()

	var errs []error
	for _, m := range sidecars {
		if m.cmd.Process != nil && m.alive() {
			if err := m.cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
				slog.Debug("sending SIGTERM to sidecar", "error", err)
			}
			select {
			case <-m.waitDone:
			case <-time.After(15 * time.Second):
				if err := m.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
					errs = append(errs, err)
				}
				<-m.waitDone
			}
		}
		select {
		case <-m.stderrDone:
		case <-time.After(2 * time.Second):
		}
	}

	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	b.httpClient.CloseIdleConnections()
	return errors.Join(errs...)
}

// BuildSidecarCommand returns the CLI arguments for launching a2a-serve-sidecar.
func (b *A2AServeBackend) BuildSidecarCommand(agentProfile, command, modelID string) []string {
	if agentProfile == "" {
		agentProfile = DefaultAgentProfile
	}
	cmd := []string{b.binary, "--agent", agentProfile}
	if command != "" {
		cmd = append(cmd, "--command", command)
	} else if modelID != "" {
		cmd = append(cmd, "--model", modelID)
	}
	cmd = append(cmd,
		"--sandbox", b.sandbox,
		"--workspace", b.WorkspaceURL(),
		"--tools", b.ToolsURL(),
	)
	contactNames := make([]string, 0, len(b.contacts))
	for name := range b.contacts {
		contactNames = append(contactNames, name)
	}
	slices.Sort(contactNames)
	for _, name := range contactNames {
		cmd = append(cmd, "--contact", name+"="+b.contacts[name])
	}
	if b.sandbox != "local" {
		cmd = append(cmd, "--egress", b.egress)
		for _, rule := range b.egressAllow {
			cmd = append(cmd, "--egress-allow", rule)
		}
		cmd = append(cmd,
			"--egress-middleware", "generativelanguage.googleapis.com="+b.GeminiMiddlewareURL(),
			"--egress-middleware", "api.anthropic.com="+b.AnthropicMiddlewareURL(),
		)
	}
	return cmd
}

func (b *A2AServeBackend) ensureSidecar(sa *SandboxAgent) (*managedSidecar, error) {
	if err := b.Start(""); err != nil {
		return nil, err
	}
	b.RegisterTools(sa.Tools()...)

	agentProfile := sa.AgentProfile()
	sidecarModel := sa.SidecarModelID()
	key := sidecarKey{agent: agentProfile, command: sa.Command(), model: sidecarModel}
	b.mu.Lock()
	if existing, ok := b.sidecars[key]; ok && existing.alive() {
		b.mu.Unlock()
		return existing, nil
	}
	b.mu.Unlock()

	b.startMu.Lock()
	defer b.startMu.Unlock()

	b.mu.Lock()
	if existing, ok := b.sidecars[key]; ok && existing.alive() {
		b.mu.Unlock()
		return existing, nil
	}
	b.mu.Unlock()

	args := b.BuildSidecarCommand(agentProfile, sa.Command(), sidecarModel)
	slog.Info("starting a2a-serve-sidecar", "cmd", strings.Join(args, " "))
	cmd := exec.Command(args[0], args[1:]...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting a2a-serve-sidecar: %w", err)
	}

	if _, err := io.WriteString(stdin, b.hostToken+"\n"); err != nil {
		slog.Debug("writing host token to sidecar stdin", "error", err)
	}
	if err := stdin.Close(); err != nil {
		slog.Debug("closing sidecar stdin", "error", err)
	}

	managed := &managedSidecar{
		key:        key,
		cmd:        cmd,
		waitDone:   make(chan struct{}),
		stderrDone: make(chan struct{}),
	}

	go func() {
		defer close(managed.stderrDone)
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			managed.mu.Lock()
			managed.stderrBuf = append(managed.stderrBuf, line)
			managed.mu.Unlock()
			slog.Info("[sidecar] " + line)
		}
		if err := sc.Err(); err != nil {
			slog.Debug("reading sidecar stderr", "error", err)
		}
	}()

	go func() {
		defer close(managed.waitDone)
		if err := cmd.Wait(); err != nil {
			slog.Debug("sidecar process exited", "error", err)
		}
		managed.mu.Lock()
		managed.exited = true
		managed.mu.Unlock()
	}()

	reader := bufio.NewReader(stdout)
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		<-managed.stderrDone
		managed.mu.Lock()
		errText := strings.Join(managed.stderrBuf, "\n")
		managed.mu.Unlock()
		return nil, fmt.Errorf("a2a-serve-sidecar exited before printing ready line: %w\nstderr:\n%s", err, errText)
	}
	if err := json.Unmarshal([]byte(line), &managed.ready); err != nil {
		b.stopSidecar(managed)
		return nil, fmt.Errorf("parsing sidecar ready line %q: %w", strings.TrimSpace(line), err)
	}
	if !managed.ready.Ready {
		b.stopSidecar(managed)
		managed.mu.Lock()
		errText := strings.Join(managed.stderrBuf, "\n")
		managed.mu.Unlock()
		return nil, fmt.Errorf("a2a-serve-sidecar failed to start: %s\nstderr:\n%s", managed.ready.Error, errText)
	}

	go func() {
		if _, err := io.Copy(io.Discard, reader); err != nil {
			slog.Debug("draining sidecar stdout", "error", err)
		}
	}()

	b.mu.Lock()
	b.sidecars[key] = managed
	b.mu.Unlock()
	return managed, nil
}

func (b *A2AServeBackend) stopSidecar(m *managedSidecar) {
	if m.cmd.Process != nil && m.alive() {
		if err := m.cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
			slog.Debug("stopping failed sidecar", "error", err)
		}
		select {
		case <-m.waitDone:
		case <-time.After(5 * time.Second):
			if err := m.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				slog.Debug("killing failed sidecar", "error", err)
			}
			<-m.waitDone
		}
	}
}

func (b *A2AServeBackend) buildHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", b.serveMCP)
	mux.Handle("/workspace/", http.StripPrefix("/workspace", b.workspace.Handler()))
	mux.HandleFunc("/gemini/", b.serveGemini)
	mux.HandleFunc("/anthropic/", b.serveAnthropic)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get(HeaderToken)
		if subtle.ConstantTimeCompare([]byte(got), []byte(b.hostToken)) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "missing or wrong A2A-Sidecar-Token", http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (b *A2AServeBackend) serveMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		b.mcpHandler.ServeHTTP(w, r)
		return
	}

	rawBody, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxMCPBody))
	if err != nil {
		http.Error(w, "MCP request body too large or unreadable", http.StatusRequestEntityTooLarge)
		return
	}

	var envelope map[string]json.RawMessage
	var method string
	if json.Unmarshal(rawBody, &envelope) == nil && envelope != nil {
		if rawMethod, ok := envelope["method"]; ok {
			if err := json.Unmarshal(rawMethod, &method); err != nil {
				method = ""
			}
		}
	}

	if strings.HasPrefix(method, "hooks/") && r.Header.Get(HeaderOrigin) != OriginSidecar {
		http.Error(w, "hooks are restricted to a2a-serve-sidecar", http.StatusForbidden)
		return
	}

	// go-sdk v1.8's AddReceivingCustomMethod requires a JSON-RPC id, while
	// hooks/afterTool is sent as a notification without an id. Inject a synthetic
	// id so the request flows through session validation and the registered
	// custom method handler, then translate a 200 OK back to 202 Accepted.
	_, hasID := envelope["id"]
	if method == MethodAfterTool && !hasID && envelope != nil {
		envelope["id"] = json.RawMessage(`"__after_tool_notification__"`)
		rewritten, err := json.Marshal(envelope)
		if err != nil {
			http.Error(w, "re-encoding hooks/afterTool notification", http.StatusInternalServerError)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(rewritten))
		r.ContentLength = int64(len(rewritten))
		rec := newStatusBuffer()
		b.mcpHandler.ServeHTTP(rec, r)
		if rec.status == http.StatusOK {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		rec.writeTo(w)
		return
	}

	r.Body = io.NopCloser(bytes.NewReader(rawBody))
	r.ContentLength = int64(len(rawBody))
	if hasID && method != "" {
		rec := newStatusBuffer()
		b.mcpHandler.ServeHTTP(rec, r)
		if rec.status == http.StatusBadRequest && strings.Contains(rec.body.String(), "JSON RPC not handled") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			if err := json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      envelope["id"],
				"error": map[string]any{
					"code":    jsonrpc.CodeMethodNotFound,
					"message": strings.TrimSpace(rec.body.String()),
				},
			}); err != nil {
				slog.Debug("writing JSON-RPC method not found error", "error", err)
			}
			return
		}
		rec.writeTo(w)
		return
	}
	b.mcpHandler.ServeHTTP(w, r)
}

func (b *A2AServeBackend) callADKTool(_ context.Context, req *mcp.CallToolRequest, toolName string, args map[string]any) (*mcp.CallToolResult, any, error) {
	contextID := ""
	if req != nil && req.Extra != nil && req.Extra.Header != nil {
		contextID = req.Extra.Header.Get(HeaderContext)
	}
	inv := b.lookupInvocation(contextID)
	if inv == nil {
		return nil, nil, fmt.Errorf("no active ADK invocation for context %q", contextID)
	}

	t := b.findTool(toolName, inv.agent)
	if t == nil {
		return nil, nil, fmt.Errorf("unknown tool: %s", toolName)
	}
	runnable, ok := t.(runnableTool)
	if !ok {
		return nil, nil, fmt.Errorf("tool %s is not runnable", toolName)
	}

	if args == nil {
		args = map[string]any{}
	}
	// Approval for RequireConfirmation tools already happened in hooks/beforeTool.
	toolCtx := agent.NewToolContext(inv.invCtx, "", nil, &toolconfirmation.ToolConfirmation{Confirmed: true})
	res, err := runnable.Run(toolCtx, args)
	if err != nil {
		slog.Error("ADK tool failed", "tool", toolName, "error", err)
		return nil, nil, fmt.Errorf("tool error: %w", err)
	}

	text, err := formatToolResult(res)
	if err != nil {
		return nil, nil, fmt.Errorf("encoding tool result: %w", err)
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
	}, nil, nil
}

func formatToolResult(res map[string]any) (string, error) {
	if len(res) == 1 {
		if s, ok := res["result"].(string); ok {
			return s, nil
		}
	}
	data, err := json.Marshal(res)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (b *A2AServeBackend) findTool(name string, sa *SandboxAgent) tool.Tool {
	if sa != nil {
		for _, t := range sa.Tools() {
			if t.Name() == name {
				return t
			}
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.tools[name]
}

type sandboxToolProxy struct {
	name        string
	description string
}

func (p sandboxToolProxy) Name() string        { return p.name }
func (p sandboxToolProxy) Description() string { return p.description }
func (p sandboxToolProxy) IsLongRunning() bool { return false }

func (b *A2AServeBackend) resolveToolForHook(mcpTool, title, kind string, sa *SandboxAgent) tool.Tool {
	var candidates []tool.Tool
	if sa != nil {
		candidates = append(candidates, sa.Tools()...)
	}
	b.mu.Lock()
	for _, t := range b.tools {
		candidates = append(candidates, t)
	}
	b.mu.Unlock()

	mcpShort := strings.TrimPrefix(mcpTool, "host.")
	for _, t := range candidates {
		if mcpShort != "" && t.Name() == mcpShort {
			return t
		}
		if t.Name() != "" && strings.Contains(title, t.Name()) {
			return t
		}
	}
	proxyName := firstNonEmpty(mcpTool, kind, title, "sandbox_tool")
	return sandboxToolProxy{name: proxyName, description: title}
}

func (b *A2AServeBackend) handleBeforeTool(_ context.Context, _ *mcp.ServerSession, params *BeforeToolParams) (*BeforeToolResult, error) {
	if params == nil || params.Call.ID == "" {
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "hooks/beforeTool needs a call with an id"}
	}
	call := params.Call
	inv := b.lookupInvocation(params.Context)
	var sa *SandboxAgent
	if inv != nil {
		sa = inv.agent
	}
	resolved := b.resolveToolForHook(call.MCP, call.Title, call.Kind, sa)
	toolArgs := map[string]any{
		"id":      call.ID,
		"kind":    call.Kind,
		"title":   call.Title,
		"command": call.Command,
		"mcp":     call.MCP,
	}

	decision := "allow"
	message := ""

	if inv != nil {
		toolCtx := agent.NewToolContext(inv.invCtx, call.ID, nil, nil)
		override, err := b.runBeforeToolCallbacks(inv, toolCtx, resolved, toolArgs)
		if err != nil {
			decision = "deny"
			message = err.Error()
		} else if override != nil {
			decision = "deny"
			if msg, ok := override["error"]; ok {
				message = fmt.Sprint(msg)
			} else {
				message = fmt.Sprint(override)
			}
		}
	}

	if decision == "allow" {
		if ct, ok := resolved.(ConfirmableTool); ok && ct.RequiresConfirmation() {
			decision = "ask"
			message = fmt.Sprintf("explicit user approval required for %s", firstNonEmpty(call.MCP, resolved.Name(), call.Title))
		}
	}

	b.recordHook(HookEvent{
		Hook:      "beforeTool",
		ContextID: params.Context,
		TaskID:    params.Task,
		Call:      structToMap(call),
		Decision:  decision,
		Message:   message,
	})
	slog.Info("hooks/beforeTool", "context", params.Context, "task", params.Task, "tool", resolved.Name(), "decision", decision)
	return &BeforeToolResult{Decision: decision, Message: message}, nil
}

func (b *A2AServeBackend) handleAfterTool(_ context.Context, _ *mcp.ServerSession, params *AfterToolParams) (*AfterToolResult, error) {
	if params == nil {
		return &AfterToolResult{}, nil
	}
	call := params.Call
	inv := b.lookupInvocation(params.Context)
	if inv != nil {
		resolved := b.resolveToolForHook("", call.Title, call.Kind, inv.agent)
		toolCtx := agent.NewToolContext(inv.invCtx, call.ID, nil, nil)
		toolArgs := map[string]any{
			"id":    call.ID,
			"kind":  call.Kind,
			"title": call.Title,
		}
		resultMap := map[string]any{
			"status":    call.Status,
			"output":    call.Output,
			"truncated": call.Truncated,
		}
		if err := b.runAfterToolCallbacks(inv, toolCtx, resolved, toolArgs, resultMap); err != nil {
			slog.Warn("after_tool callback returned error", "tool", resolved.Name(), "error", err)
		}
	}

	b.recordHook(HookEvent{
		Hook:      "afterTool",
		ContextID: params.Context,
		TaskID:    params.Task,
		Call:      structToMap(call),
	})
	slog.Info("hooks/afterTool", "context", params.Context, "task", params.Task, "kind", call.Kind, "status", call.Status)
	return &AfterToolResult{}, nil
}

func (b *A2AServeBackend) recordHook(ev HookEvent) {
	b.mu.Lock()
	b.hookEvents = append(b.hookEvents, ev)
	b.mu.Unlock()
}

func (b *A2AServeBackend) effectivePlugins(inv *activeInvocation) []*plugin.Plugin {
	if inv != nil && inv.agent != nil && len(inv.agent.Plugins()) > 0 {
		out := make([]*plugin.Plugin, 0, len(b.plugins)+len(inv.agent.Plugins()))
		out = append(out, b.plugins...)
		out = append(out, inv.agent.Plugins()...)
		return out
	}
	return b.plugins
}

func (b *A2AServeBackend) runBeforeToolCallbacks(inv *activeInvocation, toolCtx agent.Context, t tool.Tool, args map[string]any) (map[string]any, error) {
	for _, p := range b.effectivePlugins(inv) {
		if cb := p.BeforeToolCallback(); cb != nil {
			res, err := cb(toolCtx, t, args)
			if res != nil || err != nil {
				return res, err
			}
		}
	}
	if inv.agent != nil {
		for _, cb := range inv.agent.BeforeToolCallbacks() {
			res, err := cb(toolCtx, t, args)
			if res != nil || err != nil {
				return res, err
			}
		}
	}
	return nil, nil
}

func (b *A2AServeBackend) runAfterToolCallbacks(inv *activeInvocation, toolCtx agent.Context, t tool.Tool, args, result map[string]any) error {
	for _, p := range b.effectivePlugins(inv) {
		if cb := p.AfterToolCallback(); cb != nil {
			if _, err := cb(toolCtx, t, args, result, nil); err != nil {
				return err
			}
		}
	}
	if inv.agent != nil {
		for _, cb := range inv.agent.AfterToolCallbacks() {
			if _, err := cb(toolCtx, t, args, result, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

func (b *A2AServeBackend) runBeforeModelCallbacks(inv *activeInvocation, cbCtx agent.Context, req *model.LLMRequest) (*model.LLMResponse, error) {
	for _, p := range b.effectivePlugins(inv) {
		if cb := p.BeforeModelCallback(); cb != nil {
			resp, err := cb(cbCtx, req)
			if resp != nil || err != nil {
				return resp, err
			}
		}
	}
	if inv.agent != nil {
		for _, cb := range inv.agent.BeforeModelCallbacks() {
			resp, err := cb(cbCtx, req)
			if resp != nil || err != nil {
				return resp, err
			}
		}
	}
	return nil, nil
}

func (b *A2AServeBackend) runAfterModelCallbacks(inv *activeInvocation, cbCtx agent.Context, resp *model.LLMResponse) (*model.LLMResponse, error) {
	for _, p := range b.effectivePlugins(inv) {
		if cb := p.AfterModelCallback(); cb != nil {
			replaced, err := cb(cbCtx, resp, nil)
			if replaced != nil || err != nil {
				return replaced, err
			}
		}
	}
	if inv.agent != nil {
		for _, cb := range inv.agent.AfterModelCallbacks() {
			replaced, err := cb(cbCtx, resp, nil)
			if replaced != nil || err != nil {
				return replaced, err
			}
		}
	}
	return nil, nil
}

func (b *A2AServeBackend) recordMiddleware(method, path, contextID, taskID string, status int, blocked bool) {
	b.mu.Lock()
	b.middlewareEvents = append(b.middlewareEvents, MiddlewareEvent{
		Method:            method,
		Path:              "/" + strings.TrimPrefix(path, "/"),
		ContextID:         contextID,
		TaskID:            taskID,
		Status:            status,
		BlockedByCallback: blocked,
	})
	b.mu.Unlock()
	slog.Info("egress-middleware", "method", method, "path", "/"+path, "context", contextID, "task", taskID, "status", status, "blocked", blocked)
}

type modelRoute struct {
	name           string
	prefix         string
	adapter        ModelAdapter
	upstream       string
	apiKey         string
	apiKeyHeader   string
	stripQueryKey  string
	rewriteRequest func(sa *SandboxAgent, path string, body []byte) (string, []byte, error)
}

func (b *A2AServeBackend) serveGemini(w http.ResponseWriter, r *http.Request) {
	b.serveModelMiddleware(w, r, modelRoute{
		name:          "Gemini",
		prefix:        "/gemini/",
		adapter:       b.gemini,
		upstream:      b.geminiUpstream,
		apiKey:        b.geminiAPIKey,
		apiKeyHeader:  "x-goog-api-key",
		stripQueryKey: "key",
		rewriteRequest: func(sa *SandboxAgent, path string, body []byte) (string, []byte, error) {
			if sa == nil {
				return path, body, nil
			}
			if configured := sa.GeminiModelName(); configured != "" {
				if m := geminiModelPathRE.FindStringSubmatch(path); m != nil {
					path = m[1] + configured + m[3]
				}
			}
			return path, body, nil
		},
	})
}

func (b *A2AServeBackend) serveAnthropic(w http.ResponseWriter, r *http.Request) {
	b.serveModelMiddleware(w, r, modelRoute{
		name:         "Anthropic",
		prefix:       "/anthropic/",
		adapter:      b.anthropic,
		upstream:     b.anthropicUpstream,
		apiKey:       b.anthropicAPIKey,
		apiKeyHeader: "x-api-key",
		rewriteRequest: func(sa *SandboxAgent, path string, body []byte) (string, []byte, error) {
			if sa == nil || len(bytes.TrimSpace(body)) == 0 {
				return path, body, nil
			}
			configured := sa.AnthropicModelName()
			if configured == "" {
				return path, body, nil
			}
			raw, err := parseJSONObject(body)
			if err != nil {
				return path, body, nil
			}
			var current string
			if v, ok := raw["model"]; ok && json.Unmarshal(v, &current) == nil && current == configured {
				return path, body, nil
			}
			modelBytes, err := json.Marshal(configured)
			if err != nil {
				return "", nil, err
			}
			raw["model"] = modelBytes
			out, err := json.Marshal(raw)
			if err != nil {
				return "", nil, err
			}
			return path, out, nil
		},
	})
}

func (b *A2AServeBackend) serveModelMiddleware(w http.ResponseWriter, r *http.Request, route modelRoute) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	contextID := r.Header.Get(HeaderContext)
	taskID := r.Header.Get(HeaderTask)
	inv := b.lookupInvocation(contextID)
	if inv == nil {
		http.Error(w, fmt.Sprintf("no active ADK invocation for context %q", contextID), http.StatusForbidden)
		return
	}

	effectivePath := strings.TrimPrefix(r.URL.Path, route.prefix)
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxModelResponse))
	if err != nil {
		http.Error(w, fmt.Sprintf("reading %s request body: %v", route.name, err), http.StatusRequestEntityTooLarge)
		return
	}
	if route.rewriteRequest != nil {
		effectivePath, body, err = route.rewriteRequest(inv.agent, effectivePath, body)
		if err != nil {
			http.Error(w, fmt.Sprintf("rewriting %s request: %v", route.name, err), http.StatusInternalServerError)
			return
		}
	}

	mode := route.adapter.StreamMode(effectivePath, r.URL.Query(), body)
	cbCtx := agent.NewCallbackContext(inv.invCtx, nil)

	var llmReq *model.LLMRequest
	var snap requestSnapshot
	if r.Method == http.MethodPost {
		llmReq, err = route.adapter.ParseRequest(effectivePath, body)
		if err != nil {
			slog.Warn("cannot read model call for callbacks", "provider", route.name, "error", err)
			http.Error(w, fmt.Sprintf("the backend cannot read this %s call: %v", route.name, err), http.StatusBadRequest)
			return
		}
		snap, err = route.adapter.Snapshot(llmReq)
		if err != nil {
			http.Error(w, fmt.Sprintf("snapshotting %s request: %v", route.name, err), http.StatusInternalServerError)
			return
		}
		override, cbErr := b.runBeforeModelCallbacks(inv, cbCtx, llmReq)
		if cbErr != nil {
			http.Error(w, fmt.Sprintf("before_model callback error: %v", cbErr), http.StatusForbidden)
			return
		}
		if override != nil {
			b.recordMiddleware(r.Method, effectivePath, contextID, taskID, http.StatusOK, true)
			rendered, err := route.adapter.RenderResponse(override, mode)
			if err != nil {
				http.Error(w, fmt.Sprintf("rendering override response: %v", err), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", rendered.ContentType)
			w.WriteHeader(http.StatusOK)
			if _, err := w.Write(rendered.Body); err != nil {
				slog.Debug("writing blocked model response", "error", err)
			}
			return
		}
		effectivePath, body, err = route.adapter.ApplyRequest(effectivePath, body, snap, llmReq)
		if err != nil {
			http.Error(w, fmt.Sprintf("applying model callback edits: %v", err), http.StatusInternalServerError)
			return
		}
	}

	if route.apiKey == "" {
		if llmReq != nil && inv.agent != nil && inv.agent.Model() != nil {
			b.serveViaADKModel(w, r, inv, cbCtx, route, effectivePath, contextID, taskID, mode, llmReq)
			return
		}
		http.Error(w, fmt.Sprintf("host middleware has no %s API key", route.name), http.StatusInternalServerError)
		return
	}

	targetURL := route.upstream + "/" + effectivePath
	q := url.Values{}
	for k, vals := range r.URL.Query() {
		if route.stripQueryKey != "" && k == route.stripQueryKey {
			continue
		}
		q[k] = vals
	}
	if encoded := q.Encode(); encoded != "" {
		targetURL += "?" + encoded
	}

	upReq, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, bytes.NewReader(body))
	if err != nil {
		http.Error(w, fmt.Sprintf("building upstream request: %v", err), http.StatusInternalServerError)
		return
	}
	dropKeyHeader := strings.ToLower(route.apiKeyHeader)
	for k, vals := range r.Header {
		low := strings.ToLower(k)
		if hopByHopHeaders[low] || low == dropKeyHeader || low == "authorization" || strings.HasPrefix(low, "a2a-sidecar-") {
			continue
		}
		for _, v := range vals {
			upReq.Header.Add(k, v)
		}
	}
	upReq.Header.Set(route.apiKeyHeader, route.apiKey)

	upResp, err := b.httpClient.Do(upReq)
	if err != nil {
		http.Error(w, fmt.Sprintf("calling upstream %s: %v", route.name, err), http.StatusBadGateway)
		return
	}
	defer func() {
		if err := upResp.Body.Close(); err != nil {
			slog.Debug("closing upstream model body", "error", err)
		}
	}()

	b.recordMiddleware(r.Method, effectivePath, contextID, taskID, upResp.StatusCode, false)

	if llmReq == nil || upResp.StatusCode != http.StatusOK {
		copyResponseHeaders(w.Header(), upResp.Header)
		w.WriteHeader(upResp.StatusCode)
		if _, err := io.Copy(w, upResp.Body); err != nil {
			slog.Debug("streaming upstream model response", "error", err)
		}
		return
	}

	collected, err := io.ReadAll(io.LimitReader(upResp.Body, MaxModelResponse+1))
	if err != nil {
		http.Error(w, fmt.Sprintf("reading %s answer: %v", route.name, err), http.StatusBadGateway)
		return
	}
	if len(collected) > MaxModelResponse {
		http.Error(w, fmt.Sprintf("the %s answer is larger than %d bytes", route.name, MaxModelResponse), http.StatusBadGateway)
		return
	}

	llmResp, err := route.adapter.ParseResponse(collected, mode)
	if err != nil {
		slog.Warn("cannot read model answer for callbacks", "provider", route.name, "error", err)
		http.Error(w, fmt.Sprintf("the backend cannot read the %s answer: %v", route.name, err), http.StatusBadGateway)
		return
	}
	replaced, cbErr := b.runAfterModelCallbacks(inv, cbCtx, llmResp)
	if cbErr != nil {
		http.Error(w, fmt.Sprintf("after_model callback error: %v", cbErr), http.StatusBadGateway)
		return
	}
	if replaced != nil {
		rendered, err := route.adapter.RenderResponse(replaced, mode)
		if err != nil {
			http.Error(w, fmt.Sprintf("rendering replacement response: %v", err), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", rendered.ContentType)
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write(rendered.Body); err != nil {
			slog.Debug("writing replaced model response", "error", err)
		}
		return
	}

	copyResponseHeaders(w.Header(), upResp.Header)
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(collected); err != nil {
		slog.Debug("writing model response", "error", err)
	}
}

func (b *A2AServeBackend) serveViaADKModel(
	w http.ResponseWriter,
	r *http.Request,
	inv *activeInvocation,
	cbCtx agent.Context,
	route modelRoute,
	effectivePath, contextID, taskID, mode string,
	llmReq *model.LLMRequest,
) {
	llmResp, err := invokeADKModel(r.Context(), inv.agent.Model(), llmReq)
	if err != nil {
		b.recordMiddleware(r.Method, effectivePath, contextID, taskID, http.StatusBadGateway, false)
		http.Error(w, fmt.Sprintf("calling ADK model.LLM: %v", err), http.StatusBadGateway)
		return
	}
	b.recordMiddleware(r.Method, effectivePath, contextID, taskID, http.StatusOK, false)

	replaced, cbErr := b.runAfterModelCallbacks(inv, cbCtx, llmResp)
	if cbErr != nil {
		http.Error(w, fmt.Sprintf("after_model callback error: %v", cbErr), http.StatusBadGateway)
		return
	}
	if replaced != nil {
		llmResp = replaced
	}
	rendered, err := route.adapter.RenderResponse(llmResp, mode)
	if err != nil {
		http.Error(w, fmt.Sprintf("rendering ADK model response: %v", err), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", rendered.ContentType)
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(rendered.Body); err != nil {
		slog.Debug("writing ADK model response", "error", err)
	}
}

func invokeADKModel(ctx context.Context, llm model.LLM, req *model.LLMRequest) (*model.LLMResponse, error) {
	var parts []*genai.Part
	var finishReason genai.FinishReason
	var usage *genai.GenerateContentResponseUsageMetadata
	var modelVersion string
	var gotAny bool
	for resp, err := range llm.GenerateContent(ctx, req, false) {
		if err != nil {
			return nil, err
		}
		if resp == nil {
			continue
		}
		gotAny = true
		if resp.Content != nil {
			parts = append(parts, resp.Content.Parts...)
		}
		if resp.FinishReason != "" {
			finishReason = resp.FinishReason
		}
		if resp.UsageMetadata != nil {
			usage = resp.UsageMetadata
		}
		if resp.ModelVersion != "" {
			modelVersion = resp.ModelVersion
		}
	}
	if !gotAny {
		return nil, errors.New("ADK model.LLM returned no response")
	}
	return &model.LLMResponse{
		Content:       &genai.Content{Role: genai.RoleModel, Parts: mergeTextParts(parts)},
		FinishReason:  finishReason,
		UsageMetadata: usage,
		ModelVersion:  modelVersion,
	}, nil
}

func copyResponseHeaders(dst, src http.Header) {
	for k, vals := range src {
		low := strings.ToLower(k)
		if hopByHopHeaders[low] || low == "content-encoding" {
			continue
		}
		dst[k] = slices.Clone(vals)
	}
}

type statusBuffer struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newStatusBuffer() *statusBuffer {
	return &statusBuffer{
		header: make(http.Header),
		status: http.StatusOK,
	}
}

func (s *statusBuffer) Header() http.Header         { return s.header }
func (s *statusBuffer) WriteHeader(code int)        { s.status = code }
func (s *statusBuffer) Write(p []byte) (int, error) { return s.body.Write(p) }

func (s *statusBuffer) writeTo(w http.ResponseWriter) {
	for k, vals := range s.header {
		w.Header()[k] = slices.Clone(vals)
	}
	w.WriteHeader(s.status)
	if _, err := w.Write(s.body.Bytes()); err != nil {
		slog.Debug("forwarding buffered MCP response", "error", err)
	}
}

func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func defaultSidecarBinary() string {
	if envBin := os.Getenv("SIDECAR_BIN"); envBin != "" {
		return envBin
	}
	localBin := filepath.Join("bin", "a2a-serve-sidecar")
	if info, err := os.Stat(localBin); err == nil && !info.IsDir() {
		return localBin
	}
	if found, err := exec.LookPath("a2a-serve-sidecar"); err == nil {
		return found
	}
	return "a2a-serve-sidecar"
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func structToMap(v any) map[string]any {
	data, err := json.Marshal(v)
	if err != nil {
		return map[string]any{}
	}
	out := make(map[string]any)
	if err := json.Unmarshal(data, &out); err != nil {
		return map[string]any{}
	}
	return out
}
