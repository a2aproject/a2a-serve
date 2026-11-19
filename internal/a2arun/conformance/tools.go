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

// Package conformance tests that the endpoints of a host, in any language,
// speak the protocols a2a-run expects. Run it with
//
//	go test ./internal/a2arun/conformance -tools <url>
//
// where url is the tools endpoint: http://, https:// or unix:///path.
// Without -tools it tests the reference host.
package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/a2aproject/a2a-serve/internal/a2arun/hosttools"
)

// toolsTimeout bounds each step of the tools tests that has no hook
// timeout.
const toolsTimeout = 30 * time.Second

func testTools(t *testing.T, ep hosttools.Endpoint) {
	c := dialTools(t, ep)
	t.Run("lists tools", func(t *testing.T) { testListTools(t, c) })
	t.Run("refuses unknown methods", func(t *testing.T) { testUnknownMethod(t, c) })
	hooks := c.Hooks()
	t.Logf("hooks: beforeTool %v, afterTool %v", hooks.BeforeTool, hooks.AfterTool)
	t.Run("beforeTool decides", func(t *testing.T) {
		if !hooks.BeforeTool {
			t.Skip("the endpoint doesn't advertise beforeTool")
		}
		testBeforeTool(t, c)
	})
	t.Run("beforeTool refuses invalid params", func(t *testing.T) {
		if !hooks.BeforeTool {
			t.Skip("the endpoint doesn't advertise beforeTool")
		}
		testBeforeToolInvalid(t, c)
	})
	t.Run("afterTool accepts notifications", func(t *testing.T) {
		if !hooks.AfterTool {
			t.Skip("the endpoint doesn't advertise afterTool")
		}
		testAfterTool(t, c)
	})
	t.Run("ends sessions", func(t *testing.T) {
		if err := dialTools(t, ep).Close(toolsCtx(t)); err != nil {
			t.Fatalf("ending the session: %v", err)
		}
	})
}

func toolsCtx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(t.Context(), toolsTimeout)
	t.Cleanup(cancel)
	return c
}

func dialTools(t *testing.T, ep hosttools.Endpoint) *hosttools.Client {
	t.Helper()
	c, err := hosttools.Dial(toolsCtx(t), ep, "conformance")
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	t.Cleanup(func() {
		if err := c.Close(context.Background()); err != nil {
			t.Errorf("ending the session: %v", err)
		}
	})
	return c
}

func testListTools(t *testing.T, c *hosttools.Client) {
	var res struct {
		Tools []struct {
			Name        string          `json:"name"`
			InputSchema json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := c.Call(toolsCtx(t), "tools/list", map[string]any{}, &res); err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	for _, tool := range res.Tools {
		var schema struct {
			Type string `json:"type"`
		}
		if tool.Name == "" || json.Unmarshal(tool.InputSchema, &schema) != nil || schema.Type != "object" {
			t.Fatalf("tools/list has a tool %q without a name or an object inputSchema", tool.Name)
		}
	}
}

func testUnknownMethod(t *testing.T, c *hosttools.Client) {
	err := c.Call(toolsCtx(t), "a2a-run/conformance-unknown", map[string]any{}, nil)
	rerr, ok := errors.AsType[*hosttools.RPCError](err)
	if !ok || rerr.Code != -32601 {
		t.Fatalf("an unknown method got %v, want the JSON-RPC error -32601", err)
	}
}

func testBeforeTool(t *testing.T, c *hosttools.Client) {
	command, tool := "go test ./...", "host.echo"
	calls := []hosttools.BeforeToolParams{
		{Context: "conformance", Task: "task-1", Call: hosttools.BeforeCall{ID: "call-1", Kind: "execute", Title: "go test ./...", Command: &command}},
		{Context: "conformance", Task: "task-1", Call: hosttools.BeforeCall{ID: "call-2", Kind: "other", Title: "host_echo", MCP: &tool}},
		{Context: "conformance", Task: "task-1", Call: hosttools.BeforeCall{ID: "call-3", Kind: "edit", Title: "Edit main.go"}},
	}
	for _, p := range calls {
		ctx, cancel := context.WithTimeout(t.Context(), hosttools.DefaultHookTimeouts.BeforeTool)
		var res hosttools.BeforeToolResult
		err := c.Call(ctx, hosttools.MethodBeforeTool, p, &res)
		cancel()
		if err != nil {
			t.Fatalf("hooks/beforeTool for %s within %s: %v", p.Call.Title, hosttools.DefaultHookTimeouts.BeforeTool, err)
		}
		if !slices.Contains([]string{hosttools.DecisionAllow, hosttools.DecisionDeny, hosttools.DecisionAsk}, res.Decision) {
			t.Fatalf("hooks/beforeTool for %s decided %q, want allow, deny or ask", p.Call.Title, res.Decision)
		}
	}
}

func testBeforeToolInvalid(t *testing.T, c *hosttools.Client) {
	var res hosttools.BeforeToolResult
	err := c.Call(toolsCtx(t), hosttools.MethodBeforeTool, map[string]any{"context": "conformance"}, &res)
	if _, ok := errors.AsType[*hosttools.RPCError](err); !ok {
		t.Fatalf("hooks/beforeTool without a call got %v and %+v, want a JSON-RPC error", err, res)
	}
}

func testAfterTool(t *testing.T, c *hosttools.Client) {
	p := hosttools.AfterToolParams{Context: "conformance", Task: "task-1", Call: hosttools.AfterCall{ID: "call-1", Kind: "execute", Title: "go test ./...", Status: "completed", Output: "ok"}}
	ctx, cancel := context.WithTimeout(t.Context(), hosttools.DefaultHookTimeouts.AfterTool)
	defer cancel()
	if err := c.Notify(ctx, hosttools.MethodAfterTool, p); err != nil {
		t.Fatalf("hooks/afterTool within %s: %v", hosttools.DefaultHookTimeouts.AfterTool, err)
	}
}
