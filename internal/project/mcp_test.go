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

package project

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("os.MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
}

func TestLoadMCP(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		file    string
		want    []MCPServer
		wantErr bool
	}{
		{name: "no file"},
		{
			name: "stdio and http servers",
			file: `{"mcpServers": {
				"github": {"command": "npx", "args": ["-y", "@modelcontextprotocol/server-github"], "env": {"GITHUB_TOKEN": "${GITHUB_TOKEN}"}},
				"api": {"type": "http", "url": "https://api.example.com/mcp", "headers": {"Authorization": "Bearer ${API_KEY}"}},
				"events": {"type": "sse", "url": "https://events.example.com/sse"},
				"stream": {"type": "streamable-http", "url": "https://stream.example.com/mcp"},
				"untyped": {"url": "https://untyped.example.com/mcp"}
			}}`,
			want: []MCPServer{
				{Name: "api", Type: MCPHTTP, URL: "https://api.example.com/mcp", Headers: map[string]string{"Authorization": "Bearer ${API_KEY}"}},
				{Name: "events", Type: MCPSSE, URL: "https://events.example.com/sse"},
				{Name: "github", Type: MCPStdio, Command: "npx", Args: []string{"-y", "@modelcontextprotocol/server-github"}, Env: map[string]string{"GITHUB_TOKEN": "${GITHUB_TOKEN}"}},
				{Name: "stream", Type: MCPHTTP, URL: "https://stream.example.com/mcp"},
				{Name: "untyped", Type: MCPHTTP, URL: "https://untyped.example.com/mcp"},
			},
		},
		{name: "invalid JSON", file: `{`, wantErr: true},
		{name: "stdio without command", file: `{"mcpServers": {"x": {"type": "stdio"}}}`, wantErr: true},
		{name: "http without url", file: `{"mcpServers": {"x": {"type": "http"}}}`, wantErr: true},
		{name: "unknown type", file: `{"mcpServers": {"x": {"type": "ws", "url": "wss://x"}}}`, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if tc.file != "" {
				writeFile(t, filepath.Join(dir, MCPFile), tc.file)
			}
			got, err := LoadMCP(dir)
			if (err != nil) != tc.wantErr {
				t.Fatalf("LoadMCP() error = %v, want error %v", err, tc.wantErr)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("LoadMCP() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestLoadMCPSymlinks(t *testing.T) {
	t.Parallel()
	const servers = `{"mcpServers": {"x": {"command": "x"}}}`
	tests := []struct {
		name    string
		target  func(t *testing.T, dir string) string
		want    []MCPServer
		wantErr bool
	}{
		{
			name: "inside the workspace",
			target: func(t *testing.T, dir string) string {
				writeFile(t, filepath.Join(dir, "config", "mcp.json"), servers)
				return filepath.Join("config", "mcp.json")
			},
			want: []MCPServer{{Name: "x", Type: MCPStdio, Command: "x"}},
		},
		{
			name: "out of the workspace",
			target: func(t *testing.T, _ string) string {
				outside := filepath.Join(t.TempDir(), "claude.json")
				writeFile(t, outside, servers)
				return outside
			},
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if err := os.Symlink(tc.target(t, dir), filepath.Join(dir, MCPFile)); err != nil {
				t.Fatalf("os.Symlink() error = %v", err)
			}
			got, err := LoadMCP(dir)
			if (err != nil) != tc.wantErr {
				t.Fatalf("LoadMCP() error = %v, want error %v", err, tc.wantErr)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("LoadMCP() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestMCPServerResolve(t *testing.T) {
	t.Parallel()
	vars := map[string]string{"TOKEN": "t0k", "HOST": "api.example.com"}
	lookup := func(name string) (string, bool) {
		v, ok := vars[name]
		return v, ok
	}
	tests := []struct {
		name        string
		server      MCPServer
		want        MCPServer
		wantMissing []string
	}{
		{
			name:        "stdio",
			server:      MCPServer{Type: MCPStdio, Command: "${BIN:-npx}", Args: []string{"--token=${TOKEN}"}, Env: map[string]string{"A": "${TOKEN}", "B": "${NOPE}"}},
			want:        MCPServer{Type: MCPStdio, Command: "npx", Args: []string{"--token=t0k"}, Env: map[string]string{"A": "t0k", "B": ""}},
			wantMissing: []string{"NOPE"},
		},
		{
			name:        "http",
			server:      MCPServer{Type: MCPHTTP, URL: "https://${HOST}/mcp", Headers: map[string]string{"Authorization": "Bearer ${TOKEN}", "X-Key": "${KEY}${OTHER}"}},
			want:        MCPServer{Type: MCPHTTP, URL: "https://api.example.com/mcp", Headers: map[string]string{"Authorization": "Bearer t0k", "X-Key": ""}},
			wantMissing: []string{"KEY", "OTHER"},
		},
		{
			name:   "no references",
			server: MCPServer{Type: MCPStdio, Command: "uvx", Args: []string{"mcp-server-git"}},
			want:   MCPServer{Type: MCPStdio, Command: "uvx", Args: []string{"mcp-server-git"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, missing := tc.server.Resolve(lookup)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("MCPServer.Resolve() wrong result (-want +got) diff = %s", diff)
			}
			if diff := cmp.Diff(tc.wantMissing, missing); diff != "" {
				t.Fatalf("MCPServer.Resolve() wrong missing variables (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestMCPServerVars(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		server MCPServer
		want   []string
	}{
		{
			name:   "stdio",
			server: MCPServer{Type: MCPStdio, Command: "${BIN:-npx}", Args: []string{"--token=${TOKEN}"}, Env: map[string]string{"A": "${TOKEN}", "B": "${URL}"}},
			want:   []string{"BIN", "TOKEN", "URL"},
		},
		{
			name:   "http",
			server: MCPServer{Type: MCPHTTP, URL: "https://${HOST}/mcp", Headers: map[string]string{"X-Key": "${KEY}${OTHER:-x}"}},
			want:   []string{"HOST", "KEY", "OTHER"},
		},
		{
			name:   "no references",
			server: MCPServer{Type: MCPStdio, Command: "uvx", Args: []string{"mcp-server-git"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(tc.want, tc.server.Vars()); diff != "" {
				t.Fatalf("MCPServer.Vars() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestMCPServerACP(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		server MCPServer
		want   string
	}{
		{
			name:   "stdio",
			server: MCPServer{Name: "git", Type: MCPStdio, Command: "uvx", Args: []string{"mcp-server-git"}, Env: map[string]string{"B": "2", "A": "1"}},
			want:   `{"name":"git","command":"uvx","args":["mcp-server-git"],"env":[{"name":"A","value":"1"},{"name":"B","value":"2"}]}`,
		},
		{
			name:   "stdio without args",
			server: MCPServer{Name: "x", Type: MCPStdio, Command: "x"},
			want:   `{"name":"x","command":"x","args":[],"env":[]}`,
		},
		{
			name:   "http",
			server: MCPServer{Name: "api", Type: MCPHTTP, URL: "https://api.example.com/mcp", Headers: map[string]string{"Authorization": "Bearer t"}},
			want:   `{"type":"http","name":"api","url":"https://api.example.com/mcp","headers":[{"name":"Authorization","value":"Bearer t"}]}`,
		},
		{
			name:   "sse",
			server: MCPServer{Name: "ev", Type: MCPSSE, URL: "https://ev.example.com/sse"},
			want:   `{"type":"sse","name":"ev","url":"https://ev.example.com/sse","headers":[]}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := tc.server.ACP()
			if err != nil {
				t.Fatalf("MCPServer.ACP() error = %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("MCPServer.ACP() = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestSkills(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".claude/skills/review/SKILL.md"), "---\nname: review\n---\n")
	writeFile(t, filepath.Join(root, ".agents/skills/deploy/SKILL.md"), "---\nname: deploy\n---\n")
	writeFile(t, filepath.Join(root, ".agents/skills/build/SKILL.md"), "---\nname: build\n---\n")
	writeFile(t, filepath.Join(root, ".agents/skills/notes.md"), "not a skill")
	writeFile(t, filepath.Join(root, ".opencode/skills/empty/README.md"), "no SKILL.md")

	got, err := Skills(root)
	if err != nil {
		t.Fatalf("Skills() error = %v", err)
	}
	want := []Skill{
		{Name: "review", Dir: ".claude/skills"},
		{Name: "build", Dir: ".agents/skills"},
		{Name: "deploy", Dir: ".agents/skills"},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("Skills() wrong result (-want +got) diff = %s", diff)
	}
}

func TestSkillsSymlinks(t *testing.T) {
	t.Parallel()
	root, outside := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(root, ".agents/skills/review/SKILL.md"), "---\nname: review\n---\n")
	writeFile(t, filepath.Join(outside, "secret/SKILL.md"), "---\nname: secret\n---\n")
	writeFile(t, filepath.Join(outside, "skills/stolen/SKILL.md"), "---\nname: stolen\n---\n")
	symlink := func(target, link string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
			t.Fatalf("os.MkdirAll() error = %v", err)
		}
		if err := os.Symlink(target, link); err != nil {
			t.Fatalf("os.Symlink() error = %v", err)
		}
	}
	symlink(filepath.Join("..", "..", ".agents", "skills", "review"), filepath.Join(root, ".claude/skills/review"))
	symlink(filepath.Join(outside, "secret"), filepath.Join(root, ".claude/skills/secret"))
	symlink(filepath.Join(outside, "skills"), filepath.Join(root, ".opencode/skills"))

	got, err := Skills(root)
	if err != nil {
		t.Fatalf("Skills() error = %v", err)
	}
	want := []Skill{
		{Name: "review", Dir: ".claude/skills"},
		{Name: "review", Dir: ".agents/skills"},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("Skills() wrong result (-want +got) diff = %s", diff)
	}
}

func TestLoadMCPErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		setup   func(t *testing.T, dir string)
		wantErr string
	}{
		{
			name: "link out of the workspace",
			setup: func(t *testing.T, dir string) {
				outside := filepath.Join(t.TempDir(), "mcp.json")
				writeFile(t, outside, `{}`)
				if err := os.Symlink(outside, filepath.Join(dir, MCPFile)); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: "%s links out of the workspace",
		},
		{
			name: "link to a directory in the workspace",
			setup: func(t *testing.T, dir string) {
				if err := os.Mkdir(filepath.Join(dir, "config"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("config", filepath.Join(dir, MCPFile)); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: "reading %s: ",
		},
		{name: "empty file", setup: func(t *testing.T, dir string) { writeFile(t, filepath.Join(dir, MCPFile), "") }, wantErr: "%s isn't valid JSON (unexpected end of JSON input)"},
		{name: "invalid JSON", setup: func(t *testing.T, dir string) { writeFile(t, filepath.Join(dir, MCPFile), "{") }, wantErr: "%s isn't valid JSON ("},
		{name: "wrong shape", setup: func(t *testing.T, dir string) { writeFile(t, filepath.Join(dir, MCPFile), `{"mcpServers": []}`) }, wantErr: "parsing %s: "},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			tc.setup(t, dir)
			_, err := LoadMCP(dir)
			if want := strings.ReplaceAll(tc.wantErr, "%s", filepath.Join(dir, MCPFile)); err == nil || !strings.HasPrefix(err.Error(), want) {
				t.Fatalf("LoadMCP() error = %v, want it to start with %q", err, want)
			}
		})
	}
}

func TestProjectReadsDoNotBlockOnFIFO(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		fifo string
		read func(dir string) error
	}{
		{
			name: "MCP file",
			fifo: MCPFile,
			read: func(dir string) error {
				_, err := LoadMCP(dir)
				return err
			},
		},
		{
			name: "skill folder",
			fifo: filepath.Join(".claude", "skills"),
			read: func(dir string) error {
				_, err := Skills(dir)
				return err
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, tc.fifo)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(path, 0o600); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- tc.read(dir) }()
			select {
			case err := <-done:
				if err == nil {
					t.Fatalf("reading %s error = nil, want an error", tc.fifo)
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("reading %s blocked", tc.fifo)
			}
		})
	}
}
