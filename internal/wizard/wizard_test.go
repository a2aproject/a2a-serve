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

package wizard

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/project"
)

func TestRun(t *testing.T) {
	t.Parallel()
	env := map[string]string{"OPENROUTER_API_KEY": "x"}
	getenv := func(k string) string { return env[k] }
	command := func(s agentdir.Settings) ([]string, error) {
		lines := []string{"a2a-serve start " + s.Harness + " --name " + s.Name}
		if s.Telegram != "" {
			lines = append(lines, "a2a-serve channel add telegram --token env:TELEGRAM_BOT_TOKEN")
		}
		return lines, nil
	}
	tests := []struct {
		name       string
		input      string
		dockerErr  error
		secret     string
		want       Result
		wantOut    []string
		wantNotOut string
	}{
		{
			name:  "defaults",
			input: "\n\n\n\n\n\n\n\n\n",
			want: Result{Mode: ModeForeground, Settings: agentdir.Settings{
				Harness: "opencode", Name: "app", Secrets: []string{"OPENROUTER_API_KEY"}, Private: "none", Public: "none",
			}},
			wantOut: []string{"Sandbox (docker, local) [docker]", "  a2a-serve start opencode --name app\n"},
		},
		{
			name:      "choices with retries without Docker",
			input:     "claude\ncoder\n\nMISSING\nOPENROUTER_API_KEY\nwifi\ntailscale\nngrok\ntailscale\nservice\ny\n",
			dockerErr: errors.New("docker is not running"),
			secret:    "123:telegram-token",
			want: Result{Mode: ModeService, Settings: agentdir.Settings{
				Harness: "claude", Name: "coder", Sandbox: agentdir.SandboxLocal, Secrets: []string{"OPENROUTER_API_KEY"}, Private: "tailscale", Public: "tailscale",
				Telegram: "123:telegram-token",
			}},
			wantOut:    []string{"Docker is not available", "Sandbox (docker, local) [local]", "Telegram bot token", "  a2a-serve start claude --name coder --service\n  a2a-serve channel add telegram --token env:TELEGRAM_BOT_TOKEN\n"},
			wantNotOut: "123:telegram-token",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out strings.Builder
			opts := Options{DefaultName: "app", Harnesses: []string{"claude", "opencode"}, Getenv: getenv, DockerErr: tc.dockerErr, Command: command}
			if tc.secret != "" {
				opts.ReadSecret = func(prompt string) (string, error) {
					out.WriteString(prompt + ": ")
					return tc.secret, nil
				}
			}

			got, err := Run(strings.NewReader(tc.input), &out, opts)
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}

			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("Run() wrong result (-want +got) diff = %s", diff)
			}
			for _, want := range tc.wantOut {
				if !strings.Contains(out.String(), want) {
					t.Fatalf("Run() output = %q, want %q", out.String(), want)
				}
			}
			if tc.wantNotOut != "" && strings.Contains(out.String(), tc.wantNotOut) {
				t.Fatalf("Run() output = %q, want no %q", out.String(), tc.wantNotOut)
			}
		})
	}
}

func TestRunNotSaved(t *testing.T) {
	t.Parallel()
	errInvalid := errors.New("invalid settings")
	tests := []struct {
		name    string
		input   string
		command func(agentdir.Settings) ([]string, error)
		wantErr error
	}{
		{
			name:    "declined",
			input:   "\n\n\n\n\n\n\n\nn\n",
			command: func(agentdir.Settings) ([]string, error) { return []string{"a2a-serve start opencode"}, nil },
			wantErr: ErrCanceled,
		},
		{
			name:    "invalid settings",
			input:   "\n\n\n\n\n\n\n\n",
			command: func(agentdir.Settings) ([]string, error) { return nil, errInvalid },
			wantErr: errInvalid,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts := Options{DefaultName: "app", Harnesses: []string{"opencode"}, Getenv: func(string) string { return "" }, Command: tc.command}

			_, err := Run(strings.NewReader(tc.input), &strings.Builder{}, opts)

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Run() error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestRunAsksForMCPVariables(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	mcp := `{"mcpServers": {
		"github": {"command": "npx", "args": ["-y", "server-github"], "env": {"TOKEN": "${GITHUB_TOKEN}"}},
		"docs": {"type": "http", "url": "https://docs.example.com/mcp", "headers": {"Authorization": "Bearer ${DOCS_TOKEN}", "X-Region": "${REGION:-eu}"}},
		"skipped": {"command": "x", "env": {"K": "${SKIPPED_KEY}"}}
	}}`
	if err := os.WriteFile(filepath.Join(dir, project.MCPFile), []byte(mcp), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".agents", "skills", "deploy"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".agents", "skills", "deploy", "SKILL.md"), []byte("---\nname: deploy\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"OPENROUTER_API_KEY": "x", "GITHUB_TOKEN": "gh"}
	secrets := map[string]string{"DOCS_TOKEN": "docs-secret", "SKIPPED_KEY": ""}
	var out strings.Builder
	opts := Options{
		DefaultName: "app", Harnesses: []string{"opencode"}, Getenv: func(k string) string { return env[k] }, Workspace: dir,
		ReadSecret: func(prompt string) (string, error) {
			out.WriteString(prompt + ": ")
			for name, v := range secrets {
				if strings.Contains(prompt, name) {
					return v, nil
				}
			}
			return "", nil
		},
	}

	// harness, name, sandbox, API keys, pass GITHUB_TOKEN, private, public, mode
	got, err := Run(strings.NewReader("\n\n\n\n\n\n\n\n"), &out, opts)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	want := agentdir.Settings{
		Harness: "opencode", Name: "app", Private: "none", Public: "none",
		Secrets:      []string{"OPENROUTER_API_KEY", "DOCS_TOKEN", "GITHUB_TOKEN"},
		SecretValues: agentdir.Secrets{"DOCS_TOKEN": "docs-secret"},
	}
	if diff := cmp.Diff(want, got.Settings); diff != "" {
		t.Fatalf("Run() wrong settings (-want +got) diff = %s", diff)
	}
	for _, s := range []string{"skills work with every agent: deploy", "docs: http https://docs.example.com/mcp", "github: npx -y server-github", "Pass GITHUB_TOKEN from this shell for github"} {
		if !strings.Contains(out.String(), s) {
			t.Fatalf("Run() printed %q, want it to contain %q", out.String(), s)
		}
	}
	if strings.Contains(out.String(), "docs-secret") || strings.Contains(out.String(), "REGION") {
		t.Fatalf("Run() printed %q, want neither the secret nor variables with defaults", out.String())
	}
}
