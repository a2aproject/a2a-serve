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
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-serve/internal/a2atext"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/approvals"
	"github.com/a2aproject/a2a-serve/internal/harness"
	"github.com/google/go-cmp/cmp"
)

const rejected = "The user rejected permission to use this specific tool call."

// addEchoMCP gives the project in dir an MCP server "echo" with a tool
// "echo".
func addEchoMCP(t *testing.T, dir string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(binDir, "mcpecho"))
	if err != nil {
		t.Fatalf("os.ReadFile() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mcpecho"), data, 0o700); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	writeJSON(t, filepath.Join(dir, ".mcp.json"), map[string]any{
		"mcpServers": map[string]any{"echo": map[string]any{"command": filepath.Join(dir, "mcpecho")}},
	})
}

func TestHarnessOpencodeToolApprovals(t *testing.T) {
	t.Parallel()
	requireHarness(t, "opencode")
	tests := []struct {
		name    string
		flags   []string
		prepare func(t *testing.T, dir string)
		calls   func(dir string) []toolCall
		answers []string
		// wantAsked are the tool kinds of the questions that answers answer.
		wantAsked []string
		// wantResults are parts of the tool results the model got.
		wantResults map[string]string
	}{
		{
			name:        "tool denied by a rule",
			flags:       []string{"--deny-tools", "execute"},
			calls:       func(string) []toolCall { return []toolCall{bash("echo stub-ran")} },
			wantResults: map[string]string{"bash": rejected},
		},
		{
			name:  "tool denied by a rule with opencode.json in the workspace allowing it",
			flags: []string{"--deny-tools", "execute"},
			prepare: func(t *testing.T, dir string) {
				writeJSON(t, filepath.Join(dir, "opencode.json"), map[string]any{"permission": map[string]any{"bash": "allow", "edit": "allow"}})
			},
			calls:       func(string) []toolCall { return []toolCall{bash("echo stub-ran")} },
			wantResults: map[string]string{"bash": rejected},
		},
		{
			name:        "tool rejected by the user",
			calls:       func(string) []toolCall { return []toolCall{bash("echo stub-ran")} },
			answers:     []string{"reject"},
			wantAsked:   []string{"execute"},
			wantResults: map[string]string{"bash": rejected},
		},
		{
			name:  "tool kinds with allowed reads and denied fetches",
			flags: []string{"--allow-tools", "read", "--deny-tools", "fetch"},
			prepare: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "notes.txt"), "notes from the workspace")
			},
			calls: func(dir string) []toolCall {
				return []toolCall{
					{name: "read", args: map[string]any{"filePath": filepath.Join(dir, "notes.txt")}},
					{name: "write", args: map[string]any{"filePath": filepath.Join(dir, "out.txt"), "content": "written"}},
					bash("echo stub-ran"),
					{name: "webfetch", args: map[string]any{"url": "https://example.com"}},
				}
			},
			answers:   []string{"once", "once"},
			wantAsked: []string{"edit", "execute"},
			wantResults: map[string]string{
				"read":     "notes from the workspace",
				"write":    "Wrote file successfully",
				"bash":     "stub-ran",
				"webfetch": rejected,
			},
		},
		{
			// opencode asks for its external_directory permission, which
			// has no tool kind, before a read outside the workspace.
			name:  "read outside the workspace asks as other despite allowed reads",
			flags: []string{"--allow-tools", "read"},
			calls: func(string) []toolCall {
				return []toolCall{{name: "read", args: map[string]any{"filePath": "/etc/hostname"}}}
			},
			answers:     []string{"reject"},
			wantAsked:   []string{"other"},
			wantResults: map[string]string{"read": rejected},
		},
		{
			name:    "MCP tool of the project asks",
			prepare: addEchoMCP,
			calls: func(string) []toolCall {
				return []toolCall{{name: "echo_echo", args: map[string]any{"text": "ping"}}}
			},
			answers:     []string{"once"},
			wantAsked:   []string{"other"},
			wantResults: map[string]string{"echo_echo": "echo: ping"},
		},
		{
			name:    "MCP tool allowed by a rule runs without asking",
			flags:   []string{"--allow-tools", "tool:echo.echo"},
			prepare: addEchoMCP,
			calls: func(string) []toolCall {
				return []toolCall{{name: "echo_echo", args: map[string]any{"text": "ping"}}}
			},
			wantResults: map[string]string{"echo_echo": "echo: ping"},
		},
		{
			name:  "settings from the environment in an agent home bootstrapped before them",
			flags: []string{"--deny-tools", "execute"},
			prepare: func(t *testing.T, dir string) {
				writeJSON(t, filepath.Join(dir, ".a2a", "state", "home", ".config", "opencode", "opencode.json"), map[string]any{
					"$schema":    "https://opencode.ai/config.json",
					"autoupdate": false,
				})
			},
			calls: func(dir string) []toolCall {
				return []toolCall{
					bash("echo stub-ran"),
					{name: "write", args: map[string]any{"filePath": filepath.Join(dir, "out.txt"), "content": "written"}},
				}
			},
			answers:     []string{"reject"},
			wantAsked:   []string{"edit"},
			wantResults: map[string]string{"bash": rejected, "write": rejected},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			owner := newUser(t)
			coder := owner.newAgent("coder")
			if tc.prepare != nil {
				tc.prepare(t, coder.dir)
			}
			model := newStubModel(t, modelScript{calls: tc.calls(coder.dir), answer: "Done."})
			coder.startOpencode(model, tc.flags...)

			task := owner.mustSend(coder.url, "Use the tools.")
			var asked []string
			for task.Status.State == a2a.TaskStateInputRequired && len(asked) < len(tc.answers) {
				q := approvals.FromStatus(task.Status)
				if q == nil {
					t.Fatalf("a2a send state = %v without a question", task.Status.State)
				}
				asked = append(asked, q.Kind)
				task = owner.mustSend(coder.url, "--task-id", string(task.ID), tc.answers[len(asked)-1])
			}

			if diff := cmp.Diff(tc.wantAsked, asked); diff != "" {
				t.Fatalf("question kinds wrong result (-want +got) diff = %s", diff)
			}
			got := resultOf(task)
			want := taskResult{State: a2a.TaskStateCompleted, Text: "Done."}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Fatalf("a2a send wrong result (-want +got) diff = %s", diff)
			}
			results := model.toolResults()
			gotResults := map[string]string{}
			for tool, part := range tc.wantResults {
				gotResults[tool] = results[tool]
				if strings.Contains(results[tool], part) {
					gotResults[tool] = part
				}
			}
			if diff := cmp.Diff(tc.wantResults, gotResults); diff != "" {
				t.Fatalf("tool results of the model wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestHarnessOpencodeModelUsageLimitFailsTask(t *testing.T) {
	t.Parallel()
	requireHarness(t, "opencode")
	owner := newUser(t)
	model := newStubModel(t, modelScript{
		status: http.StatusTooManyRequests,
		body:   `{"error":{"message":"You exceeded your current usage limit.","type":"rate_limit_exceeded","code":"rate_limit_exceeded"}}`,
	})
	coder := owner.newAgent("coder")
	coder.startOpencode(model)

	start := time.Now()
	task := owner.mustSend(coder.url, "--async", "hello")
	var got *a2a.Task
	eventuallyWithin(t, 3*time.Minute, "waiting for the task to fail", func() (bool, string) {
		got = owner.mustGetTask(coder.url, string(task.ID))
		return got.Status.State.Terminal(), fmt.Sprintf("%v after %d model requests", got.Status.State, model.requestCount())
	})
	t.Logf("failed after %s and %d model requests: %q", time.Since(start), model.requestCount(), a2atext.Message(got.Status.Message))
	if got.Status.State != a2a.TaskStateFailed || !strings.Contains(a2atext.Message(got.Status.Message), "usage limit") {
		t.Fatalf("a2a task get = %v %q, want failed with the provider's message", got.Status.State, a2atext.Message(got.Status.Message))
	}
}

func bash(command string) toolCall {
	return toolCall{name: "bash", args: map[string]any{"command": command}}
}

// requireHarness marks a test of the real image of an agent, and skips it
// unless E2E_HARNESS names the agent.
func requireHarness(t *testing.T, name string) {
	t.Helper()
	dependsOnSandbox(t)
	if harnessName != name {
		t.Skipf("needs E2E_HARNESS=%s", name)
	}
	if sandbox != "docker" {
		t.Skip("needs E2E_SANDBOX=docker")
	}
}

// startOpencode starts an opencode agent in its real image, with model as
// its model in the config of the agent home.
func (a *agent) startOpencode(model *stubModel, flags ...string) {
	a.u.t.Helper()
	a.addStubProvider(model.url, "stub")
	u, err := url.Parse(model.url)
	if err != nil {
		a.u.t.Fatalf("url.Parse(%q) error = %v", model.url, err)
	}
	// The stub model listens on the host's LAN address, a private one.
	a.start("opencode", append([]string{"--sandbox", "docker", "--image", harnessImage, "--model", "stub/scripted", "--egress-allow", u.Host}, flags...)...)
}

// addStubProvider adds the stub provider with the model at baseURL to the
// opencode config of the agent home, since the sandbox hides the one of the
// workspace. A home without a config first gets the one the server writes.
func (a *agent) addStubProvider(baseURL, apiKey string) {
	t := a.u.t
	t.Helper()
	home := filepath.Join(a.dir, ".a2a", "state", "home")
	path := filepath.Join(home, ".config", "opencode", "opencode.json")
	profile, err := harness.Lookup("opencode", nil)
	if err != nil {
		t.Fatalf("harness.Lookup() error = %v", err)
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("os.MkdirAll() error = %v", err)
	}
	if err := profile.Bootstrap(harness.Workspace{Dir: a.dir, Home: home}); err != nil {
		t.Fatalf("Bootstrap() error = %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("os.ReadFile() error = %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("json.Unmarshal(%s) error = %v", path, err)
	}
	cfg["provider"] = map[string]any{"stub": map[string]any{
		"npm":     "@ai-sdk/openai-compatible",
		"options": map[string]any{"baseURL": baseURL, "apiKey": apiKey},
		"models":  map[string]any{"scripted": map[string]any{}},
	}}
	writeJSON(t, path, cfg)
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("json.MarshalIndent() error = %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("os.MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
}
