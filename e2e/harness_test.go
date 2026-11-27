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
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/a2aproject/a2a-serve/internal/a2atext"
	"github.com/a2aproject/a2a-serve/internal/agentprofile"
	"github.com/a2aproject/a2a-serve/internal/question"
	"github.com/a2aproject/a2a-serve/internal/testutil"
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
	testutil.WriteExecutable(t, filepath.Join(dir, "mcpecho"), data)
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
				q := question.FromStatus(task.Status)
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
			checkToolResults(t, model, tc.wantResults)
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

// copilotRejected ends Copilot's answer to a turn whose tool call was
// rejected.
const copilotRejected = "Operation cancelled by user"

func TestHarnessCopilotToolApprovals(t *testing.T) {
	t.Parallel()
	requireHarness(t, "copilot")
	tests := []struct {
		name    string
		flags   []string
		prepare func(t *testing.T, dir string)
		calls   func(dir string) []toolCall
		answers []string
		// wantAsked are the tool kinds of the questions that answers answer.
		wantAsked []string
		// wantText is the end of the task's answer: Copilot ends its turn
		// after a tool call is rejected.
		wantText string
		// wantResults are parts of the tool results the model got.
		wantResults map[string]string
	}{
		{
			name:     "command denied by a rule",
			flags:    []string{"--deny-tools", "execute"},
			calls:    func(string) []toolCall { return []toolCall{bash("touch denied")} },
			wantText: copilotRejected,
		},
		{
			name:      "command rejected by the user",
			calls:     func(string) []toolCall { return []toolCall{bash("touch rejected")} },
			answers:   []string{"reject"},
			wantAsked: []string{"execute"},
			wantText:  copilotRejected,
		},
		{
			name: "read runs without asking and a file edit asks",
			prepare: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "notes.txt"), "notes from the workspace")
			},
			calls: func(dir string) []toolCall {
				return []toolCall{
					{name: "view", args: map[string]any{"path": filepath.Join(dir, "notes.txt")}},
					{name: "create", args: map[string]any{"path": filepath.Join(dir, "out.txt"), "file_text": "written"}},
				}
			},
			answers:     []string{"once"},
			wantAsked:   []string{"edit"},
			wantText:    "Done.",
			wantResults: map[string]string{"view": "notes from the workspace", "create": "out.txt"},
		},
		{
			// The sandbox hides .github/hooks, so the hook goes into the
			// settings of the agent home, where only the image's managed
			// settings turn it off.
			name: "hook doesn't run",
			prepare: func(t *testing.T, dir string) {
				writeJSON(t, filepath.Join(dir, ".a2a", "state", "home", ".copilot", "settings.json"), map[string]any{
					"hooks": map[string]any{"sessionStart": []any{map[string]any{"type": "command", "bash": "touch " + filepath.Join(dir, "hook-ran")}}},
				})
			},
			calls:       func(dir string) []toolCall { return []toolCall{bash("ls " + dir)} },
			wantText:    "Done.",
			wantResults: map[string]string{"bash": "notes"},
		},
		{
			name: "repository hook doesn't run",
			prepare: func(t *testing.T, dir string) {
				writeJSON(t, filepath.Join(dir, ".github", "hooks", "hook.json"), map[string]any{
					"version": 1,
					"hooks":   map[string]any{"sessionStart": []any{map[string]any{"type": "command", "bash": "touch " + filepath.Join(dir, "hook-ran")}}},
				})
			},
			calls:       func(dir string) []toolCall { return []toolCall{bash("ls " + dir)} },
			wantText:    "Done.",
			wantResults: map[string]string{"bash": "notes"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			owner := newUser(t)
			coder := owner.newAgent("coder")
			writeFile(t, filepath.Join(coder.dir, "notes"), "")
			if tc.prepare != nil {
				tc.prepare(t, coder.dir)
			}
			model := newStubModel(t, modelScript{calls: tc.calls(coder.dir), answer: "Done."})
			coder.startCopilot(model, tc.flags...)

			task := owner.mustSend(coder.url, "Use the tools.")
			var asked []string
			for task.Status.State == a2a.TaskStateInputRequired && len(asked) < len(tc.answers) {
				q := question.FromStatus(task.Status)
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
			if got.State != a2a.TaskStateCompleted || !strings.HasSuffix(got.Text, tc.wantText) {
				t.Fatalf("a2a send = %v %q, want completed with an answer ending in %q", got.State, got.Text, tc.wantText)
			}
			checkToolResults(t, model, tc.wantResults)
			if _, err := os.Stat(filepath.Join(coder.dir, "hook-ran")); err == nil {
				t.Fatalf("a hook ran, want hooks off")
			}
		})
	}
}

func TestHarnessCopilotSlashCommandOfAMemberDoesntTurnOffPrompts(t *testing.T) {
	t.Parallel()
	requireHarness(t, "copilot")
	owner, anna := newUser(t), newUser(t)
	coder := owner.newAgent("coder")
	model := newStubModel(t, modelScript{calls: []toolCall{bash("touch pwned")}, answer: "Done."})
	coder.startCopilot(model)
	anna.pairJoin(coder.url, coder.invite("anna"))

	// The first prompt of a session starts with the line of callable
	// agents, so the command goes in a later turn.
	first := anna.mustSend(coder.url, "hello")
	owner.mustSend(coder.url, "--task-id", string(first.ID), "reject")
	task := anna.mustSend(coder.url, "--context-id", first.ContextID, "/allow-all on")
	q := question.FromStatus(task.Status)
	if task.Status.State != a2a.TaskStateInputRequired || q == nil || q.Kind != "execute" {
		t.Fatalf("a2a send /allow-all on = %v %q, want the message to reach the model, which asks to run a command", task.Status.State, resultOf(task).Text)
	}
	owner.mustSend(coder.url, "--task-id", string(task.ID), "reject")
	if _, err := os.Stat(filepath.Join(coder.dir, "pwned")); err == nil {
		t.Fatalf("the command ran without an answer, want it to wait for one")
	}
}

// startCopilot starts a copilot agent in its real image, with model as its
// model provider.
func (a *agent) startCopilot(model *stubModel, flags ...string) {
	a.u.t.Helper()
	u, err := url.Parse(model.url)
	if err != nil {
		a.u.t.Fatalf("url.Parse(%q) error = %v", model.url, err)
	}
	// The stub model listens on the host's LAN address, a private one.
	a.start("copilot", append([]string{
		"--sandbox", "docker", "--image", harnessImage, "--egress-allow", u.Host,
		"--env", "COPILOT_PROVIDER_BASE_URL=" + model.url, "--env", "COPILOT_PROVIDER_API_KEY=stub", "--env", "COPILOT_MODEL=scripted",
	}, flags...)...)
}

func TestHarnessAgyToolApprovals(t *testing.T) {
	t.Parallel()
	requireHarness(t, "agy")
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
		// wantFiles are the files of the workspace that the tools create,
		// and whether they should exist.
		wantFiles map[string]bool
	}{
		{
			name:      "command denied by a rule",
			flags:     []string{"--deny-tools", "execute"},
			calls:     func(string) []toolCall { return []toolCall{runCommand("touch denied")} },
			wantFiles: map[string]bool{"denied": false},
		},
		{
			name:      "command rejected by the user",
			calls:     func(string) []toolCall { return []toolCall{runCommand("touch rejected")} },
			answers:   []string{"reject"},
			wantAsked: []string{"execute"},
			wantFiles: map[string]bool{"rejected": false},
		},
		{
			name:      "command allowed by a rule runs without asking",
			flags:     []string{"--allow-tools", "command:touch *"},
			calls:     func(string) []toolCall { return []toolCall{runCommand("touch allowed")} },
			wantFiles: map[string]bool{"allowed": true},
		},
		{
			name: "read runs without asking and a file write asks",
			prepare: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "notes.txt"), "notes from the workspace")
			},
			calls: func(dir string) []toolCall {
				return []toolCall{
					{name: "read_file", args: map[string]any{"Path": filepath.Join(dir, "notes.txt")}},
					{name: "write_file", args: map[string]any{"Path": filepath.Join(dir, "out.txt"), "Content": "written"}},
				}
			},
			answers:     []string{"once"},
			wantAsked:   []string{"edit"},
			wantResults: map[string]string{"read_file": "notes from the workspace"},
			wantFiles:   map[string]bool{"out.txt": true},
		},
		{
			name:    "MCP tool allowed by a rule runs without asking",
			flags:   []string{"--allow-tools", "tool:echo.echo"},
			prepare: addEchoMCP,
			calls: func(string) []toolCall {
				return []toolCall{{name: "mcp_echo_echo", args: map[string]any{"text": "ping", "toolSummary": "echo"}}}
			},
			wantResults: map[string]string{"mcp_echo_echo": "echo: ping"},
		},
		{
			name: "repository hook doesn't run",
			prepare: func(t *testing.T, dir string) {
				writeJSON(t, filepath.Join(dir, ".agents", "hooks.json"), map[string]any{
					"approve": map[string]any{"SessionStart": []any{map[string]any{"type": "command", "command": "touch " + filepath.Join(dir, "hook-ran")}}},
				})
			},
			calls:     func(string) []toolCall { return []toolCall{runCommand("true")} },
			answers:   []string{"once"},
			wantAsked: []string{"execute"},
			wantFiles: map[string]bool{"hook-ran": false},
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
			coder.startAgy(model, tc.flags...)

			task := owner.mustSend(coder.url, "Use the tools.")
			var asked []string
			for task.Status.State == a2a.TaskStateInputRequired && len(asked) < len(tc.answers) {
				q := question.FromStatus(task.Status)
				if q == nil {
					t.Fatalf("a2a send state = %v without a question", task.Status.State)
				}
				asked = append(asked, q.Kind)
				task = owner.mustSend(coder.url, "--task-id", string(task.ID), tc.answers[len(asked)-1])
			}

			if diff := cmp.Diff(tc.wantAsked, asked); diff != "" {
				t.Fatalf("question kinds wrong result (-want +got) diff = %s", diff)
			}
			if got := resultOf(task); got.State != a2a.TaskStateCompleted {
				t.Fatalf("a2a send = %v %q, want completed", got.State, got.Text)
			}
			checkToolResults(t, model, tc.wantResults)
			for name, want := range tc.wantFiles {
				if _, err := os.Stat(filepath.Join(coder.dir, name)); (err == nil) != want {
					t.Fatalf("%s exists = %v, want %v", name, err == nil, want)
				}
			}
		})
	}
}

func TestHarnessAgyAsksForAMemberInItsLockedHome(t *testing.T) {
	t.Parallel()
	requireHarness(t, "agy")
	owner, anna := newUser(t), newUser(t)
	coder := owner.newAgent("coder")
	model := newStubModel(t, modelScript{calls: []toolCall{runCommand("touch pwned")}, answer: "Done."})
	coder.startAgy(model)
	anna.pairJoin(coder.url, coder.invite("anna"))

	task := anna.mustSend(coder.url, "Use the tools.")
	q := question.FromStatus(task.Status)
	if task.Status.State != a2a.TaskStateInputRequired || q == nil || q.Kind != "execute" {
		t.Fatalf("a2a send = %v %q, want a question to run a command", task.Status.State, resultOf(task).Text)
	}
	owner.mustSend(coder.url, "--task-id", string(task.ID), "reject")
	if _, err := os.Stat(filepath.Join(coder.dir, "pwned")); err == nil {
		t.Fatalf("the command ran after it was rejected")
	}
}

// startAgy starts an agy agent in its real image, with model as its Gemini
// API.
func (a *agent) startAgy(model *stubModel, flags ...string) {
	a.u.t.Helper()
	u, err := url.Parse(model.url)
	if err != nil {
		a.u.t.Fatalf("url.Parse(%q) error = %v", model.url, err)
	}
	// The stub model listens on the host's LAN address, a private one. The
	// workspaces of the tests are in /tmp, where agy writes without asking
	// when it is its temporary folder.
	a.start("agy", append([]string{
		"--sandbox", "docker", "--image", harnessImage, "--egress-allow", u.Host,
		"--env", "GOOGLE_GEMINI_BASE_URL=" + strings.TrimSuffix(model.url, "/v1"), "--env", "GEMINI_API_KEY=stub", "--env", "TMPDIR=/var/tmp",
	}, flags...)...)
}

func runCommand(command string) toolCall {
	return toolCall{name: "run_command", args: map[string]any{"CommandLine": command, "toolSummary": "run", "WaitMsBeforeAsync": 5000}}
}

// checkToolResults fails t unless the result of each tool of want, of the
// last request of model, holds its part.
func checkToolResults(t *testing.T, model *stubModel, want map[string]string) {
	t.Helper()
	results := model.toolResults()
	got := map[string]string{}
	for tool, part := range want {
		got[tool] = results[tool]
		if strings.Contains(results[tool], part) {
			got[tool] = part
		}
	}
	if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
		t.Fatalf("tool results of the model wrong result (-want +got) diff = %s", diff)
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
	profile, err := agentprofile.Lookup("opencode", nil)
	if err != nil {
		t.Fatalf("agentprofile.Lookup() error = %v", err)
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("os.MkdirAll() error = %v", err)
	}
	if err := profile.Bootstrap(agentprofile.Workspace{Dir: a.dir, Home: home}); err != nil {
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
