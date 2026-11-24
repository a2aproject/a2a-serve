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

package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/a2aproject/a2a-serve/internal/sidecar/hosttools"
)

// toolsTimeout bounds each step of the tools tests that has no hook
// timeout.
const toolsTimeout = 30 * time.Second

func testTools(t *testing.T, ep hosttools.Endpoint, token string) {
	if token != "" {
		t.Run("refuses missing or wrong token", func(t *testing.T) {
			for _, tc := range []struct {
				name string
				tok  string
			}{
				{name: "missing", tok: ""},
				{name: "wrong", tok: token + "-wrong"},
			} {
				bad, err := ep.WithToken(tc.tok)
				if err != nil {
					t.Fatal(err)
				}
				c, err := hosttools.Dial(toolsCtx(t), bad, "conformance")
				if err == nil {
					if cerr := c.Close(context.Background()); cerr != nil {
						t.Errorf("closing unexpected session: %v", cerr)
					}
					t.Fatalf("initialize with %s token succeeded, want 401", tc.name)
				}
			}
		})
		var err error
		if ep, err = ep.WithToken(token); err != nil {
			t.Fatalf("Endpoint.WithToken() error = %v", err)
		}
	}
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
	t.Run("hooks refuse the sandbox's requests", func(t *testing.T) {
		if !hooks.BeforeTool && !hooks.AfterTool {
			t.Skip("the endpoint doesn't advertise hooks")
		}
		testSandboxOrigin(t, ep, hooks)
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
	err := c.Call(toolsCtx(t), "a2a-sidecar/conformance-unknown", map[string]any{}, nil)
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

// testSandboxOrigin sends the hooks as the agent would, through the proxy
// of a2a-serve-sidecar, which marks them as requests of the sandbox.
func testSandboxOrigin(t *testing.T, ep hosttools.Endpoint, hooks hosttools.HookSupport) {
	c, err := hosttools.DialThrough(toolsCtx(t), ep, "conformance", ep.Proxy())
	if err != nil {
		t.Fatalf("initialize from the sandbox: %v", err)
	}
	t.Cleanup(func() {
		if err := c.Close(context.Background()); err != nil {
			t.Errorf("ending the session: %v", err)
		}
	})
	if hooks.BeforeTool {
		command := "rm -rf /"
		var res hosttools.BeforeToolResult
		p := hosttools.BeforeToolParams{Context: "conformance", Task: "task-1", Call: hosttools.BeforeCall{ID: "forged", Kind: "execute", Title: command, Command: &command}}
		if err := c.Call(toolsCtx(t), hosttools.MethodBeforeTool, p, &res); err == nil {
			t.Fatalf("hooks/beforeTool with %s: %s decided %q, want it refused", hosttools.HeaderOrigin, hosttools.OriginSandbox, res.Decision)
		}
	}
	if hooks.AfterTool {
		p := hosttools.AfterToolParams{Context: "conformance", Task: "task-1", Call: hosttools.AfterCall{ID: "forged", Status: "completed", Output: "forged"}}
		if err := c.Notify(toolsCtx(t), hosttools.MethodAfterTool, p); err == nil {
			t.Fatalf("hooks/afterTool with %s: %s was accepted, want it refused", hosttools.HeaderOrigin, hosttools.OriginSandbox)
		}
	}
}
