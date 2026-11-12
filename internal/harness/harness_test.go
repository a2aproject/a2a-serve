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

package harness

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
)

func TestOpencodeContinuesAfterDeniedTool(t *testing.T) {
	t.Parallel()
	p, err := Lookup("opencode", nil)
	if err != nil {
		t.Fatalf("Lookup() error = %v", err)
	}
	var content string
	for _, kv := range p.Command(Workspace{Dir: "/w", Home: "/h"}).Env {
		if v, ok := strings.CutPrefix(kv, "OPENCODE_CONFIG_CONTENT="); ok {
			content = v
		}
	}
	var cfg struct {
		Experimental struct {
			ContinueLoopOnDeny bool `json:"continue_loop_on_deny"`
		} `json:"experimental"`
	}
	if err := json.Unmarshal([]byte(content), &cfg); err != nil {
		t.Fatalf("json.Unmarshal(OPENCODE_CONFIG_CONTENT %q) error = %v", content, err)
	}
	if !cfg.Experimental.ContinueLoopOnDeny {
		t.Fatalf("OPENCODE_CONFIG_CONTENT = %q, want experimental.continue_loop_on_deny true", content)
	}
}

func TestLookup(t *testing.T) {
	t.Parallel()
	ws := Workspace{Dir: "/w", Home: "/h"}
	tests := []struct {
		name    string
		command []string
		want    Command
		image   string
		wantErr bool
	}{
		{name: "opencode", want: Command{Path: "opencode", Args: []string{"acp"}, Dir: "/w", Env: opencodeEnv},
			image: ImageRepo + "opencode:" + ImageTag},
		{name: "claude", want: Command{Path: "claude-agent-acp", Dir: "/w"}, image: ImageRepo + "claude:" + ImageTag},
		{name: "opencode", command: []string{"opencode", "acp", "--print-logs"}, want: Command{Path: "opencode", Args: []string{"acp", "--print-logs"}, Dir: "/w", Env: opencodeEnv},
			image: ImageRepo + "opencode:" + ImageTag},
		{name: "gemini", command: []string{"gemini", "--experimental-acp"}, want: Command{Path: "gemini", Args: []string{"--experimental-acp"}, Dir: "/w"}},
		{name: "unknown", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name+strings.Join(tc.command, "_"), func(t *testing.T) {
			t.Parallel()
			p, err := Lookup(tc.name, tc.command)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Lookup() error = %v, want error %v", err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if diff := cmp.Diff(tc.want, p.Command(ws)); diff != "" {
				t.Fatalf("Command() wrong result (-want +got) diff = %s", diff)
			}
			if p.Image() != tc.image {
				t.Fatalf("Image() = %q, want %q", p.Image(), tc.image)
			}
		})
	}
}

func TestSessionSettings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command []string
		model   string
		want    map[string]any
	}{
		{name: "opencode", model: "openrouter/a/b", want: map[string]any{"model": "openrouter/a/b"}},
		{name: "opencode", command: []string{"opencode", "acp", "--print-logs"}, model: "openrouter/a/b", want: map[string]any{"model": "openrouter/a/b"}},
		{name: "opencode"},
		{name: "claude", model: "sonnet"},
		{name: "gemini", command: []string{"gemini", "--experimental-acp"}, model: "gemini-pro"},
	}
	for _, tc := range tests {
		t.Run(tc.name+strings.Join(tc.command, "_")+tc.model, func(t *testing.T) {
			t.Parallel()
			p, err := Lookup(tc.name, tc.command)
			if err != nil {
				t.Fatalf("Lookup() error = %v", err)
			}
			if diff := cmp.Diff(tc.want, p.SessionSettings(tc.model)); diff != "" {
				t.Fatalf("SessionSettings() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestWarnings(t *testing.T) {
	t.Parallel()
	openrouter := []string{"OPENROUTER_API_KEY"}
	tests := []struct {
		name string
		cfg  agentdir.Config
		want int
	}{
		{name: "opencode with OpenRouter and no model", cfg: agentdir.Config{Harness: "opencode", Secrets: openrouter}, want: 1},
		{name: "opencode with OpenRouter and a model", cfg: agentdir.Config{Harness: "opencode", Secrets: openrouter, Model: "openrouter/a/b"}},
		{name: "opencode with another key", cfg: agentdir.Config{Harness: "opencode", Secrets: []string{"ANTHROPIC_API_KEY"}}},
		{name: "claude with OpenRouter", cfg: agentdir.Config{Harness: "claude", Secrets: openrouter}},
		{name: "opencode model without its key", cfg: agentdir.Config{Harness: "opencode", Model: "google/gemini-3-flash-preview"}, want: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Warnings(&tc.cfg); len(got) != tc.want {
				t.Fatalf("Warnings() = %q, want %d warnings", got, tc.want)
			}
		})
	}
}

func TestMissingKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cfg  agentdir.Config
		want string
	}{
		{
			name: "google model without a key",
			cfg:  agentdir.Config{Harness: "opencode", Model: "google/gemini-3-flash-preview", Secrets: []string{"GITHUB_TOKEN"}},
			want: "no key for provider google: set one with `a2a-serve config set --secret GOOGLE_GENERATIVE_AI_API_KEY`",
		},
		{
			name: "openrouter model without a key",
			cfg:  agentdir.Config{Harness: "opencode", Model: "openrouter/~google/gemini-flash-latest"},
			want: "no key for provider openrouter: set one with `a2a-serve config set --secret OPENROUTER_API_KEY`",
		},
		{name: "google model with another google key", cfg: agentdir.Config{Harness: "opencode", Model: "google/gemini-3-flash-preview", Secrets: []string{"GEMINI_API_KEY"}}},
		{name: "key in env", cfg: agentdir.Config{Harness: "opencode", Model: "anthropic/claude-sonnet-4-5", Env: []string{"ANTHROPIC_API_KEY=sk"}}},
		{name: "unknown provider", cfg: agentdir.Config{Harness: "opencode", Model: "acme/model"}},
		{name: "no model", cfg: agentdir.Config{Harness: "opencode"}},
		{name: "claude", cfg: agentdir.Config{Harness: "claude", Model: "anthropic/claude-sonnet-4-5"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := MissingKey(&tc.cfg); got != tc.want {
				t.Fatalf("MissingKey() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBootstrap(t *testing.T) {
	t.Parallel()
	tests := []struct {
		harness  string
		file     string
		want     string
		wantKept string
	}{
		{harness: "opencode", file: ".config/opencode/opencode.json", want: `"*": "ask"`, wantKept: `{"theme": "dark"}`},
		{
			harness: "claude", file: ".claude/settings.json", want: `"defaultMode": "default"`,
			wantKept: "{\n  \"enableAllProjectMcpServers\": false,\n  \"theme\": \"dark\"\n}\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.harness, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			p, err := Lookup(tc.harness, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := p.Bootstrap(Workspace{Dir: t.TempDir(), Home: home}); err != nil {
				t.Fatalf("Bootstrap() error = %v", err)
			}
			data, err := os.ReadFile(filepath.Join(home, tc.file))
			if err != nil {
				t.Fatalf("reading %s error = %v", tc.file, err)
			}
			if !strings.Contains(string(data), tc.want) {
				t.Fatalf("Bootstrap() wrote %s, want it to contain %s", data, tc.want)
			}

			if err := os.WriteFile(filepath.Join(home, tc.file), []byte(`{"theme": "dark"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := p.Bootstrap(Workspace{Home: home}); err != nil {
				t.Fatalf("Bootstrap() error = %v", err)
			}
			data, err = os.ReadFile(filepath.Join(home, tc.file))
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.wantKept, string(data)); diff != "" {
				t.Fatalf("Bootstrap() wrong user config (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestBootstrapRefusesSymlinksOutOfHome(t *testing.T) {
	t.Parallel()
	tests := []struct {
		harness string
		link    string
	}{
		{harness: "opencode", link: ".config"},
		{harness: "claude", link: ".claude"},
	}
	for _, tc := range tests {
		t.Run(tc.harness, func(t *testing.T) {
			t.Parallel()
			home, outside := t.TempDir(), t.TempDir()
			if err := os.Symlink(outside, filepath.Join(home, tc.link)); err != nil {
				t.Fatal(err)
			}
			p, err := Lookup(tc.harness, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := p.Bootstrap(Workspace{Dir: t.TempDir(), Home: home}); err == nil {
				t.Fatalf("Bootstrap() through linked %s error = nil, want an error", tc.link)
			}
			entries, err := os.ReadDir(outside)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("Bootstrap() through linked %s wrote %v outside the home, want nothing", tc.link, entries)
			}
		})
	}
}

func TestBootstrapSkipsFilesItCannotRead(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(t *testing.T, path string)
	}{
		{
			name: "FIFO",
			setup: func(t *testing.T, path string) {
				if err := syscall.Mkfifo(path, 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "over 1 MiB",
			setup: func(t *testing.T, path string) {
				data := append([]byte(`{"x": "`), bytes.Repeat([]byte("x"), 1<<20)...)
				if err := os.WriteFile(path, append(data, `"}`...), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			settings := filepath.Join(home, ".claude", "settings.json")
			if err := os.MkdirAll(filepath.Dir(settings), 0o700); err != nil {
				t.Fatal(err)
			}
			tc.setup(t, settings)
			before, err := os.Stat(settings)
			if err != nil {
				t.Fatal(err)
			}
			p, err := Lookup("claude", nil)
			if err != nil {
				t.Fatal(err)
			}

			done := make(chan error, 1)
			go func() { done <- p.Bootstrap(Workspace{Dir: t.TempDir(), Home: home}) }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("Bootstrap() error = %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("Bootstrap() blocked")
			}

			after, err := os.Stat(settings)
			if err != nil {
				t.Fatal(err)
			}
			if after.Mode() != before.Mode() || after.Size() != before.Size() {
				t.Fatalf("Bootstrap() changed %s to %v of %d bytes, want it skipped", settings, after.Mode(), after.Size())
			}
		})
	}
}

func TestDockerfiles(t *testing.T) {
	t.Parallel()
	for _, name := range ProfileNames() {
		if _, ok := Dockerfile(name); !ok {
			t.Errorf("Dockerfile(%q) missing", name)
		}
	}
}

func TestProfileNamesAreNotReserved(t *testing.T) {
	t.Parallel()
	for _, name := range ProfileNames() {
		if slices.Contains(agentdir.ReservedHarnesses, name) {
			t.Fatalf("ProfileNames() = %v, want none of the reserved %v", ProfileNames(), agentdir.ReservedHarnesses)
		}
	}
}

func TestOpencodeAsksBeforeToolsThatChangeThings(t *testing.T) {
	t.Parallel()
	var got map[string]any
	if err := json.Unmarshal([]byte(opencodePermission), &got); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	want := map[string]any{
		"*": "ask", "glob": "allow", "grep": "allow", "list": "allow", "lsp": "allow", "question": "allow",
		"read":  map[string]any{"*": "allow", "*.env": "ask", "*.env.*": "ask", "*.env.example": "allow"},
		"skill": "allow", "task": "allow", "todowrite": "allow", "websearch": "allow",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("opencode permission wrong result (-want +got) diff = %s", diff)
	}
	if !strings.HasPrefix(opencodePermission, `{"*":"ask",`) {
		t.Fatalf("opencode permission = %s, want \"*\" first, since opencode applies the last rule that matches", opencodePermission)
	}
}
