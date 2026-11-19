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

package a2aserve_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/core/a2aserve"
	"github.com/a2aproject/a2a-serve/internal/testutil"
)

func TestSessionsGetTheMCPServersOfTheEnvironment(t *testing.T) {
	t.Parallel()
	ws := t.TempDir()
	mcp := `{"mcpServers":{"host":{"command":"fake-host"},"other":{"type":"http","url":"http://other.example/mcp"}}}`
	if err := os.WriteFile(filepath.Join(ws, ".mcp.json"), []byte(mcp), 0o600); err != nil {
		t.Fatal(err)
	}
	env := a2aserve.Environment{
		Workspace:  a2aserve.SharedDir(ws),
		Home:       filepath.Join(t.TempDir(), "home"),
		MCPServers: []a2aserve.MCPServer{{Name: "host", URL: "http://tools.example/mcp", Headers: map[string]string{"X-Key": "k"}}},
	}
	task := sendTask(t, client(t, newAgent(t, a2aserve.Config{Main: env})), "c1", "mcp")
	if task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("Client.SendMessage(mcp) state = %v, want %v", task.Status.State, a2a.TaskStateCompleted)
	}
	var got []map[string]any
	if err := json.Unmarshal([]byte(testutil.AllArtifactText(task)), &got); err != nil {
		t.Fatalf("json.Unmarshal(%q) error = %v", testutil.AllArtifactText(task), err)
	}
	want := []map[string]any{
		{"type": "http", "name": "host", "url": "http://tools.example/mcp", "headers": []any{map[string]any{"name": "X-Key", "value": "k"}}},
		{"type": "http", "name": "other", "url": "http://other.example/mcp", "headers": []any{}},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("MCP servers of the session wrong result (-want +got) diff = %s", diff)
	}
}

func TestSessionFailsWithAnInvalidMCPServer(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		servers []a2aserve.MCPServer
		want    string
	}{
		{name: "name with a dot", servers: []a2aserve.MCPServer{{Name: "a.b", URL: "http://a/"}}, want: `invalid MCP server name "a.b"`},
		{name: "not http", servers: []a2aserve.MCPServer{{Name: "a", URL: "file:///tmp/a"}}, want: "want an http or https URL"},
		{name: "twice", servers: []a2aserve.MCPServer{{Name: "a", URL: "http://a/"}, {Name: "a", URL: "http://b/"}}, want: "two MCP servers named a"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := environment(t, "")
			env.MCPServers = tc.servers
			task := sendTask(t, client(t, newAgent(t, a2aserve.Config{Main: env})), "c1", "mcp")
			if task.Status.State != a2a.TaskStateFailed || task.Status.Message == nil || !strings.Contains(task.Status.Message.Parts[0].Text(), tc.want) {
				t.Fatalf("Client.SendMessage(mcp) = %v, %v, want failed with %q", task.Status.State, task.Status.Message, tc.want)
			}
		})
	}
}

func TestToolDoneGetsTheEndedToolCalls(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var got []a2aserve.ToolResult
	done := func(_ context.Context, r a2aserve.ToolResult) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, r)
	}
	task := sendTask(t, client(t, newAgent(t, a2aserve.Config{Hooks: a2aserve.Hooks{ToolDone: done}})), "c1", "tool")
	if task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("Client.SendMessage(tool) state = %v, want %v", task.Status.State, a2a.TaskStateCompleted)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []a2aserve.ToolResult{{ID: "t1", Kind: "read", Title: "list files", Status: "completed", ContextID: "c1", TaskID: string(task.ID)}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("ToolDone got wrong results (-want +got) diff = %s", diff)
	}
}
