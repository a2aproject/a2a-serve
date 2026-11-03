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
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/go-cmp/cmp"

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
	if err := json.Unmarshal([]byte(resultText(task)), &got); err != nil {
		t.Fatalf("json.Unmarshal(%q) error = %v", resultText(task), err)
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
