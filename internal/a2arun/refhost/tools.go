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

package refhost

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"slices"
	"sync"

	"github.com/a2aproject/a2a-serve/internal/a2arun/hosttools"
)

// ToolName is the name of the host's tool, which returns its text
// argument after "echo: ".
const ToolName = "echo"

const (
	maxBody        = 1 << 20
	maxSessions    = 1000
	maxToolRecords = 10000
)

// ToolsConfig configures a [Tools].
type ToolsConfig struct {
	// Hooks are the hooks the host advertises.
	Hooks hosttools.HookSupport
	// Decide answers hooks/beforeTool; nil allows every call.
	Decide func(hosttools.BeforeToolParams) hosttools.BeforeToolResult
	// Stream answers requests with an event stream, which first carries a
	// notification, in place of a JSON body.
	Stream bool
	// OnRecord sees each message the host gets.
	OnRecord func(Record)
}

// Record is a message the tools endpoint got.
type Record struct {
	Method string
	Params json.RawMessage
}

// Tools is the reference host's tools endpoint: an MCP server over
// streamable HTTP with one tool, echo, that answers the hooks of a2a-run.
// It records every message it gets. It is an [http.Handler].
type Tools struct {
	cfg ToolsConfig

	mu       sync.Mutex
	sessions []string
	records  []Record
}

// NewTools returns the tools endpoint of cfg.
func NewTools(cfg ToolsConfig) *Tools { return &Tools{cfg: cfg} }

// Records returns the messages of method that the endpoint got, oldest first.
func (h *Tools) Records(method string) []Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []Record
	for _, r := range h.records {
		if r.Method == method {
			out = append(out, r)
		}
	}
	return out
}

// ForgetSessions ends every session, as a host that restarted would.
func (h *Tools) ForgetSessions() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sessions = nil
}

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ServeHTTP serves the MCP endpoint.
func (h *Tools) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
	case http.MethodDelete:
		if !h.endSession(r.Header.Get("Mcp-Session-Id")) {
			http.Error(w, "unknown session", http.StatusNotFound)
		}
		return
	default:
		w.Header().Set("Allow", "POST, DELETE")
		http.Error(w, "this endpoint serves no event stream", http.StatusMethodNotAllowed)
		return
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		http.Error(w, "want application/json", http.StatusUnsupportedMediaType)
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		http.Error(w, "the body is too large or broken", http.StatusRequestEntityTooLarge)
		return
	}
	var m message
	if err := json.Unmarshal(data, &m); err != nil || m.JSONRPC != "2.0" || m.Method == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"jsonrpc": "2.0", "id": nil, "error": rpcError{-32700, "want a JSON-RPC request or notification"}})
		return
	}
	if m.Method != "initialize" {
		switch id := r.Header.Get("Mcp-Session-Id"); {
		case id == "":
			http.Error(w, "missing Mcp-Session-Id", http.StatusBadRequest)
			return
		case !h.knows(id):
			http.Error(w, "unknown session", http.StatusNotFound)
			return
		}
	}
	h.record(m)
	if len(m.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	result, rerr := h.handle(m)
	if m.Method == "initialize" && rerr == nil {
		w.Header().Set("Mcp-Session-Id", h.newSession())
	}
	resp := map[string]any{"jsonrpc": "2.0", "id": m.ID}
	if rerr != nil {
		resp["error"] = rerr
	} else {
		resp["result"] = result
	}
	if !h.cfg.Stream {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	note, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "notifications/message", "params": map[string]any{"level": "info", "data": "working"}})
	if err != nil {
		slog.Error("encoding a notification", "error", err)
		return
	}
	body, err := json.Marshal(resp)
	if err != nil {
		slog.Error("encoding an answer", "error", err)
		return
	}
	if _, err := fmt.Fprintf(w, "event: message\ndata: %s\n\nevent: message\ndata: %s\n\n", note, body); err != nil {
		slog.Debug("writing an event stream", "error", err)
	}
}

func (h *Tools) handle(m message) (any, *rpcError) {
	switch m.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if err := json.Unmarshal(m.Params, &p); err != nil || p.ProtocolVersion == "" {
			return nil, &rpcError{-32602, "initialize needs a protocolVersion"}
		}
		caps := map[string]any{"tools": map[string]any{}}
		if h.cfg.Hooks.BeforeTool || h.cfg.Hooks.AfterTool {
			caps["experimental"] = map[string]any{hosttools.HooksCapability: h.cfg.Hooks}
		}
		return map[string]any{"protocolVersion": hosttools.ProtocolVersion, "capabilities": caps, "serverInfo": map[string]any{"name": "refhost", "version": "1.0.0"}}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": []any{map[string]any{
			"name":        ToolName,
			"description": "Returns the text after \"echo: \".",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"text": map[string]any{"type": "string"}},
				"required":   []string{"text"},
			},
		}}}, nil
	case "tools/call":
		var p struct {
			Name      string `json:"name"`
			Arguments struct {
				Text string `json:"text"`
			} `json:"arguments"`
		}
		if err := json.Unmarshal(m.Params, &p); err != nil || p.Name != ToolName {
			return nil, &rpcError{-32602, "unknown tool"}
		}
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": "echo: " + p.Arguments.Text}}}, nil
	case hosttools.MethodBeforeTool:
		if !h.cfg.Hooks.BeforeTool {
			break
		}
		var p hosttools.BeforeToolParams
		if err := json.Unmarshal(m.Params, &p); err != nil || p.Call.ID == "" {
			return nil, &rpcError{-32602, "hooks/beforeTool needs a call with an id"}
		}
		if h.cfg.Decide == nil {
			return hosttools.BeforeToolResult{Decision: hosttools.DecisionAllow}, nil
		}
		return h.cfg.Decide(p), nil
	}
	return nil, &rpcError{-32601, "method not found: " + m.Method}
}

func (h *Tools) record(m message) {
	rec := Record{Method: m.Method, Params: m.Params}
	h.mu.Lock()
	if len(h.records) >= maxToolRecords {
		h.records = slices.Delete(h.records, 0, 1)
	}
	h.records = append(h.records, rec)
	h.mu.Unlock()
	if h.cfg.OnRecord != nil {
		h.cfg.OnRecord(rec)
	}
}

func (h *Tools) newSession() string {
	id := rand.Text()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.sessions) >= maxSessions {
		h.sessions = slices.Delete(h.sessions, 0, 1)
	}
	h.sessions = append(h.sessions, id)
	return id
}

func (h *Tools) knows(id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Contains(h.sessions, id)
}

func (h *Tools) endSession(id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	i := slices.Index(h.sessions, id)
	if i < 0 {
		return false
	}
	h.sessions = slices.Delete(h.sessions, i, i+1)
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Debug("writing an answer", "error", err)
	}
}
