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

package cli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/credstore"
	"github.com/a2aproject/a2a-serve/internal/recipe"
	"github.com/a2aproject/a2a-serve/internal/wizard"
)

const testRecipeMCP = `{"mcpServers": {
  "grafana": {"command": "uvx", "args": ["mcp-grafana"], "env": {"GRAFANA_URL": "${GRAFANA_URL}", "GRAFANA_TOKEN": "${GRAFANA_TOKEN}"}},
  "kubernetes": {"command": "npx", "args": ["kubernetes-mcp-server"], "env": {"KUBECONFIG": "/kube/config", "KUBE_TOKEN": "${KUBE_TOKEN}"}}
}}`

// newTestRecipe writes a recipe with a required and an optional MCP
// server, the optional one with a tied mount, and returns its directory.
func newTestRecipe(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "o11y")
	kube := filepath.Join(t.TempDir(), "kube")
	if err := os.Mkdir(kube, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		".a2a/config.yaml": "name: o11y\nagent: opencode\nport: 8123\nsecrets: [OPENROUTER_API_KEY]\nmounts: [\"" + kube + ":/kube:ro\"]\n" +
			"approvals:\n  allow: [read, other]\nnetwork:\n  private: tailscale\n",
		".a2a/recipe.yaml": "mcp:\n  grafana:\n    description: metrics\n  kubernetes:\n    description: cluster state\n    optional: true\n    mounts: [/kube]\n" +
			"vars:\n  GRAFANA_URL:\n    description: base URL\n    secret: false\n",
		".mcp.json": testRecipeMCP,
		"AGENTS.md": "instructions",
	}
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func recipeConfig(terminal bool, stdin *os.File, env map[string]string) *globalConfig {
	return &globalConfig{
		credentials: &credstore.Store{Path: filepath.Join(os.TempDir(), "unused-credentials.json")},
		terminal:    &terminal,
		stdin:       stdin,
		getenv:      func(name string) string { return env[name] },
	}
}

func readAgent(t *testing.T, root string) (*agentdir.Config, agentdir.Secrets) {
	t.Helper()
	d, err := agentdir.Open(root)
	if err != nil {
		t.Fatalf("agentdir.Open() error = %v", err)
	}
	cfg, err := d.ReadConfig()
	if err != nil {
		t.Fatalf("ReadConfig() error = %v", err)
	}
	secrets, err := d.LoadSecrets()
	if err != nil {
		t.Fatalf("LoadSecrets() error = %v", err)
	}
	return cfg, secrets
}

func TestRecipeInteractive(t *testing.T) {
	t.Parallel()
	src := newTestRecipe(t)
	target := filepath.Join(t.TempDir(), "agents", "obs")
	answers := strings.Join([]string{"y", "sk-1", "tok", "https://grafana.example.com", "n"}, "\n") + "\n"

	out, err := runCMDWith(t, recipeConfig(true, pipeWith(t, answers), nil), "recipe", src, "--dir", target, "--memory", "8g")
	if err != nil {
		t.Fatalf("a2a-serve recipe error = %v\n%s", err, out)
	}

	cfg, secrets := readAgent(t, target)
	want := &agentdir.Config{
		Name: "o11y", Harness: "opencode",
		Secrets:   []string{"OPENROUTER_API_KEY", "GRAFANA_TOKEN"},
		Env:       []string{"GRAFANA_URL=https://grafana.example.com"},
		Approvals: agentdir.ApprovalsConfig{Allow: []string{"read", "other"}},
		Limits:    agentdir.LimitsConfig{Memory: "8g"},
		MCP:       agentdir.MCPConfig{Disabled: []string{"kubernetes"}},
	}
	if diff := cmp.Diff(want, cfg); diff != "" {
		t.Fatalf("a2a-serve recipe wrong config (-want +got) diff = %s", diff)
	}
	if diff := cmp.Diff(agentdir.Secrets{"OPENROUTER_API_KEY": "sk-1", "GRAFANA_TOKEN": "tok"}, secrets); diff != "" {
		t.Fatalf("a2a-serve recipe wrong secrets (-want +got) diff = %s", diff)
	}
	for _, name := range []string{"AGENTS.md", ".mcp.json"} {
		if _, err := os.Stat(filepath.Join(target, name)); err != nil {
			t.Fatalf("a2a-serve recipe didn't copy %s: %v", name, err)
		}
	}
	d, err := agentdir.Open(target)
	if err != nil {
		t.Fatalf("agentdir.Open() error = %v", err)
	}
	rec, err := recipe.ReadRecord(d)
	if err != nil {
		t.Fatalf("recipe.ReadRecord() error = %v", err)
	}
	if rec.Dir != src || !strings.Contains(rec.Config, "port: 8123") {
		t.Fatalf("recipe.lock = %+v, want the source %s and the recipe's config", rec, src)
	}
	for _, want := range []string{"o11y from " + src + " will run:", "not taken     port, private", "✓ created o11y in", "with kubernetes off", "a2a-serve start"} {
		if !strings.Contains(out, want) {
			t.Fatalf("a2a-serve recipe output = %s, want %q", out, want)
		}
	}
}

func TestRecipeCanceled(t *testing.T) {
	t.Parallel()
	src := newTestRecipe(t)
	parent := t.TempDir()
	target := filepath.Join(parent, "obs")

	_, err := runCMDWith(t, recipeConfig(true, pipeWith(t, "n\n"), nil), "recipe", src, "--dir", target)
	if !errors.Is(err, wizard.ErrCanceled) {
		t.Fatalf("a2a-serve recipe answered no error = %v, want %v", err, wizard.ErrCanceled)
	}
	assertEmptyDir(t, parent)
}

func TestRecipeWithoutTerminal(t *testing.T) {
	t.Parallel()
	env := map[string]string{"OPENROUTER_API_KEY": "sk-env", "GRAFANA_URL": "https://g.example.com"}
	tests := []struct {
		name        string
		args        []string
		wantErr     string
		wantCreated bool
		wantSecrets agentdir.Secrets
		wantOff     []string
		noKube      bool
	}{
		{
			name:    "needs --yes or --dry-run",
			wantErr: "without a terminal, pass --yes or --dry-run",
		},
		{
			name:    "missing secret",
			args:    []string{"--yes"},
			wantErr: "pass --secret GRAFANA_TOKEN, or turn off the MCP servers that need them with --disable-mcp",
		},
		{
			name:        "values from flags and the environment",
			args:        []string{"--yes", "--secret", "GRAFANA_TOKEN=tok"},
			wantCreated: true,
			wantSecrets: agentdir.Secrets{"OPENROUTER_API_KEY": "sk-env", "GRAFANA_TOKEN": "tok"},
			wantOff:     []string{"kubernetes"},
		},
		{
			name:        "optional server with its values",
			args:        []string{"--yes", "--secret", "GRAFANA_TOKEN=tok", "--secret", "KUBE_TOKEN=k"},
			wantCreated: true,
			wantSecrets: agentdir.Secrets{"OPENROUTER_API_KEY": "sk-env", "GRAFANA_TOKEN": "tok", "KUBE_TOKEN": "k"},
		},
		{
			name:        "required server turned off",
			args:        []string{"--yes", "--disable-mcp", "grafana"},
			wantCreated: true,
			wantSecrets: agentdir.Secrets{"OPENROUTER_API_KEY": "sk-env"},
			wantOff:     []string{"grafana", "kubernetes"},
		},
		{
			name:        "optional server without its mount source",
			args:        []string{"--yes", "--secret", "GRAFANA_TOKEN=tok", "--secret", "KUBE_TOKEN=k"},
			noKube:      true,
			wantCreated: true,
			wantSecrets: agentdir.Secrets{"OPENROUTER_API_KEY": "sk-env", "GRAFANA_TOKEN": "tok", "KUBE_TOKEN": "k"},
			wantOff:     []string{"kubernetes"},
		},
		{
			name: "dry run",
			args: []string{"--dry-run"},
		},
		{
			name:    "invalid flag on top",
			args:    []string{"--yes", "--sandbox", "nope"},
			wantErr: "sandbox",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			src := newTestRecipe(t)
			if tc.noKube {
				removeKube(t, src)
			}
			parent := t.TempDir()
			target := filepath.Join(parent, "obs")

			out, err := runCMDWith(t, recipeConfig(false, nil, env), append([]string{"recipe", src, "--dir", target}, tc.args...)...)

			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !isUsage(err) {
					t.Fatalf("a2a-serve recipe error = %v, want a usage error with %q", err, tc.wantErr)
				}
				assertEmptyDir(t, parent)
				return
			}
			if err != nil {
				t.Fatalf("a2a-serve recipe error = %v\n%s", err, out)
			}
			if !tc.wantCreated {
				if !strings.Contains(out, "will run:") || !strings.Contains(out, "(dry run: nothing saved)") {
					t.Fatalf("a2a-serve recipe --dry-run output = %s, want the review", out)
				}
				assertEmptyDir(t, parent)
				return
			}
			cfg, secrets := readAgent(t, target)
			if diff := cmp.Diff(tc.wantSecrets, secrets); diff != "" {
				t.Fatalf("a2a-serve recipe wrong secrets (-want +got) diff = %s", diff)
			}
			if diff := cmp.Diff(tc.wantOff, cfg.MCP.Disabled); diff != "" {
				t.Fatalf("a2a-serve recipe wrong servers off (-want +got) diff = %s", diff)
			}
			if kubeOn := !cfg.MCPDisabled("kubernetes"); kubeOn != (len(cfg.Mounts) == 1) {
				t.Fatalf("a2a-serve recipe mounts = %v with kubernetes on %v, want the tied mount to follow the server", cfg.Mounts, kubeOn)
			}
		})
	}
}

func TestRecipeWithoutTerminalReadsLocalSourceFirst(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "missing")
	tests := []struct {
		name    string
		src     string
		wantErr string
	}{
		{name: "missing directory", src: missing, wantErr: "recipe " + missing + ": "},
		{name: "git source", src: "https://127.0.0.1:1/acme/agents.git", wantErr: "without a terminal, pass --yes or --dry-run"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := runCMDWith(t, recipeConfig(false, nil, nil), "recipe", tc.src, "--dir", filepath.Join(t.TempDir(), "obs"))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("a2a-serve recipe error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestRecipeRefusesAgentBehindOneWithoutConfig(t *testing.T) {
	t.Parallel()
	outer, err := agentdir.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := outer.SaveConfig(&agentdir.Config{Name: "outer", Harness: "opencode"}); err != nil {
		t.Fatal(err)
	}
	stale := agentdir.Dir{Root: filepath.Join(outer.Root, "old")}
	if err := stale.Init(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale.LockPath(), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = runCMDWith(t, recipeConfig(false, nil, nil), "recipe", newTestRecipe(t), "--dir", filepath.Join(stale.Root, "new"), "--dry-run")

	if want := "inside the agent in " + outer.Root; err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("a2a-serve recipe error = %v, want %q", err, want)
	}
}

func TestRecipeRefuses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		setup   func(t *testing.T, src, target string)
		wantErr string
	}{
		{
			name: "directory with files",
			setup: func(t *testing.T, _, target string) {
				if err := os.MkdirAll(target, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(target, "notes.md"), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: "isn't empty",
		},
		{
			name: "inside another agent",
			setup: func(t *testing.T, _, target string) {
				d, err := agentdir.Open(filepath.Dir(target))
				if err != nil {
					t.Fatal(err)
				}
				if err := d.SaveConfig(&agentdir.Config{Name: "outer", Harness: "opencode"}); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: "could read the new agent's secrets",
		},
		{
			name: "unknown setting",
			setup: func(t *testing.T, src, _ string) {
				if err := os.WriteFile(filepath.Join(src, ".a2a", "config.yaml"), []byte("agent: opencode\nbadge: {}\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: "doesn't know: badge",
		},
		{
			name: "missing mount source of a required server",
			setup: func(t *testing.T, src, _ string) {
				if err := os.WriteFile(filepath.Join(src, ".a2a", "recipe.yaml"), nil, 0o644); err != nil {
					t.Fatal(err)
				}
				removeKube(t, src)
			},
			wantErr: "the source does not exist",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			src := newTestRecipe(t)
			target := filepath.Join(t.TempDir(), "obs")
			tc.setup(t, src, target)

			_, err := runCMDWith(t, recipeConfig(false, nil, nil), "recipe", src, "--dir", target, "--dry-run")
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("a2a-serve recipe error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestRecipeFromGit(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	src := newTestRecipe(t)
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = src
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	target := filepath.Join(t.TempDir(), "obs")
	env := map[string]string{"OPENROUTER_API_KEY": "sk", "GRAFANA_URL": "u", "GRAFANA_TOKEN": "t"}

	out, err := runCMDWith(t, recipeConfig(false, nil, env), "recipe", "file://"+src+"#main", "--dir", target, "--yes")
	if err != nil {
		t.Fatalf("a2a-serve recipe error = %v\n%s", err, out)
	}
	d, err := agentdir.Open(target)
	if err != nil {
		t.Fatalf("agentdir.Open() error = %v", err)
	}
	rec, err := recipe.ReadRecord(d)
	if err != nil {
		t.Fatalf("recipe.ReadRecord() error = %v", err)
	}
	if rec.Repo != "file://"+src || rec.Ref != "main" || len(rec.Commit) != 40 {
		t.Fatalf("recipe.lock = %+v, want the repository, ref and commit", rec)
	}
	if _, err := os.Stat(filepath.Join(target, ".git")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stat(.git) error = %v, want no .git in the agent", err)
	}
}

// removeKube removes the source of the mount the test recipe ties to
// kubernetes.
func removeKube(t *testing.T, src string) {
	t.Helper()
	d, err := agentdir.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := d.ReadConfig()
	if err != nil {
		t.Fatal(err)
	}
	m, err := agentdir.ParseMount(cfg.Mounts[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(m.Src); err != nil {
		t.Fatal(err)
	}
}

func assertEmptyDir(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) > 0 {
		t.Fatalf("%s holds %v, want nothing left behind", dir, entries)
	}
}
