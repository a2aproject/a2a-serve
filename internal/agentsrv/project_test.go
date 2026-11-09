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

package agentsrv

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/a2atext"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/project"
)

const testMCPFile = `{"mcpServers": {
	"github": {"command": "npx", "args": ["-y", "server-github"], "env": {"GITHUB_TOKEN": "${GH_TOKEN}"}},
	"api": {"type": "http", "url": "https://api.example.com/mcp", "headers": {"Authorization": "Bearer ${API_KEY}"}},
	"bot": {"command": "bot-mcp", "env": {"TOKEN": "${CODER_BOT_TOKEN}"}}
}}`

func startProjectServer(t *testing.T) *testServer {
	t.Helper()
	d, err := os.MkdirTemp("", "a2a")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(d); err != nil {
			t.Errorf("removing %s: %v", d, err)
		}
	})
	if err := os.WriteFile(filepath.Join(d, project.MCPFile), []byte(testMCPFile), 0o644); err != nil {
		t.Fatal(err)
	}
	skill := filepath.Join(d, ".agents", "skills", "deploy")
	if err := os.MkdirAll(skill, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte("---\nname: deploy\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return startServer(t, serverOpts{
		dir:       d,
		secrets:   agentdir.Secrets{"GH_TOKEN": "gh-secret", "CODER_BOT_TOKEN": "bot-secret"},
		configure: func(c *agentdir.Config) { c.Secrets = []string{"GH_TOKEN"} },
	})
}

func TestSessionGetsProjectMCPServers(t *testing.T) {
	t.Parallel()
	ts := startProjectServer(t)
	c := ts.client(ts.pair("me", auth.RoleOwner))

	task := send(t, c, userText("mcp", "", ""))
	wantResult(t, task, a2a.TaskStateCompleted, "")
	var got []map[string]any
	if err := json.Unmarshal([]byte(a2atext.Result(task)), &got); err != nil {
		t.Fatalf("json.Unmarshal(%q) error = %v", a2atext.Result(task), err)
	}
	want := []map[string]any{{
		"name": "github", "command": "npx", "args": []any{"-y", "server-github"},
		"env": []any{map[string]any{"name": "GITHUB_TOKEN", "value": "gh-secret"}},
	}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("session MCP servers wrong result (-want +got) diff = %s", diff)
	}
}

func TestStatusListsProject(t *testing.T) {
	t.Parallel()
	ts := startProjectServer(t)

	st, err := controlapi.NewClient(ts.dir).Status(context.Background())
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	wantServers := []controlapi.MCPServerStatus{
		{Name: "api", Type: project.MCPHTTP, Summary: "http https://api.example.com/mcp", Missing: []string{"API_KEY"}},
		{Name: "bot", Type: project.MCPStdio, Summary: "bot-mcp", Missing: []string{"CODER_BOT_TOKEN"}},
		{Name: "github", Type: project.MCPStdio, Summary: "npx -y server-github"},
	}
	if diff := cmp.Diff(wantServers, st.MCPServers); diff != "" {
		t.Fatalf("Status() wrong MCP servers (-want +got) diff = %s", diff)
	}
	if diff := cmp.Diff([]project.Skill{{Name: "deploy", Dir: ".agents/skills"}}, st.Skills); diff != "" {
		t.Fatalf("Status() wrong skills (-want +got) diff = %s", diff)
	}
}

func TestDisabledMCPServersApplyLive(t *testing.T) {
	t.Parallel()
	ts := startProjectServer(t)
	cfg, err := ts.dir.ReadConfig()
	if err != nil {
		t.Fatalf("ReadConfig() error = %v", err)
	}
	cfg.MCP.Disabled = []string{"github", "api"}
	if err := ts.dir.SaveConfig(cfg); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}
	control := controlapi.NewClient(ts.dir)
	if _, err := control.ReloadConfig(t.Context()); err != nil {
		t.Fatalf("ReloadConfig() error = %v", err)
	}
	c := ts.client(ts.pair("me", auth.RoleOwner))

	task := send(t, c, userText("mcp", "", ""))

	wantResult(t, task, a2a.TaskStateCompleted, "")
	var got []map[string]any
	if err := json.Unmarshal([]byte(a2atext.Result(task)), &got); err != nil {
		t.Fatalf("json.Unmarshal(%q) error = %v", a2atext.Result(task), err)
	}
	if len(got) != 0 {
		t.Fatalf("session MCP servers = %v, want none", got)
	}
	st, err := control.Status(t.Context())
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	wantServers := []controlapi.MCPServerStatus{
		{Name: "api", Type: project.MCPHTTP, Summary: "http https://api.example.com/mcp", Disabled: true},
		{Name: "bot", Type: project.MCPStdio, Summary: "bot-mcp", Missing: []string{"CODER_BOT_TOKEN"}},
		{Name: "github", Type: project.MCPStdio, Summary: "npx -y server-github", Disabled: true},
	}
	if diff := cmp.Diff(wantServers, st.MCPServers); diff != "" {
		t.Fatalf("Status() wrong MCP servers (-want +got) diff = %s", diff)
	}
}

func TestConversationContinuesAfterWorkspaceMoved(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	token := ts.pair("me", auth.RoleOwner)
	first := send(t, ts.client(token), userText("remember red", "", ""))
	wantResult(t, first, a2a.TaskStateCompleted, "OK.")
	dir := ts.dir.Root
	ts.stop()
	moved := shortTempDir(t)
	copyAgent(t, dir, moved)

	ts = startServer(t, serverOpts{dir: moved})
	c := ts.client(token)
	task := send(t, c, userText("earlier", "", first.ContextID))

	wantResult(t, task, a2a.TaskStateCompleted, "User: remember red\n\nAgent: OK.")
	if got := a2atext.Message(task.Status.Message); !strings.Contains(got, "new session") {
		t.Fatalf("task status message = %q, want a note about the new session", got)
	}
	for _, m := range task.History {
		if text := a2atext.Message(m); strings.Contains(text, earlierPrefix) {
			t.Fatalf("task history has %q, want the earlier messages kept out of it", text)
		}
	}
	next := send(t, c, userText("earlier", "", first.ContextID))
	wantResult(t, next, a2a.TaskStateCompleted, "Earlier: ")
	if got := a2atext.Result(next); got != "Earlier: " {
		t.Fatalf("task result in the new session = %q, want no earlier conversation", got)
	}
}

func TestProjectStatusWithRefusedMCPFile(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		setup   func(t *testing.T, path string)
		wantErr string
	}{
		{
			name: "empty file",
			setup: func(t *testing.T, path string) {
				if err := os.WriteFile(path, nil, 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: "%s isn't valid JSON (unexpected end of JSON input); the agent gets no MCP servers from it",
		},
		{
			name: "link out of the workspace",
			setup: func(t *testing.T, path string) {
				outside := filepath.Join(t.TempDir(), "mcp.json")
				if err := os.WriteFile(outside, []byte(testMCPFile), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, path); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: "%s links out of the workspace; the agent gets no MCP servers from it",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			path := filepath.Join(root, project.MCPFile)
			tc.setup(t, path)
			skill := filepath.Join(root, ".agents", "skills", "deploy")
			if err := os.MkdirAll(skill, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte("---\nname: deploy\n---\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			servers, skills, err := ProjectStatus(agentdir.Dir{Root: root}, &agentdir.Config{}, nil)

			if want := strings.ReplaceAll(tc.wantErr, "%s", path); err == nil || err.Error() != want {
				t.Fatalf("ProjectStatus() error = %v, want %q", err, want)
			}
			if len(servers) != 0 {
				t.Fatalf("ProjectStatus() servers = %v, want none", servers)
			}
			if diff := cmp.Diff([]project.Skill{{Name: "deploy", Dir: ".agents/skills"}}, skills); diff != "" {
				t.Fatalf("ProjectStatus() wrong skills (-want +got) diff = %s", diff)
			}
		})
	}
}
