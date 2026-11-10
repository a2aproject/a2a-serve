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
	"github.com/a2aproject/a2a-serve/internal/recipe"
)

var recipeEnv = map[string]string{"OPENROUTER_API_KEY": "sk", "GRAFANA_URL": "https://g.example.com", "GRAFANA_TOKEN": "tok"}

// newRecipeAgent creates an agent from a new test recipe without asking,
// and returns the recipe's and the agent's directories.
func newRecipeAgent(t *testing.T, extra map[string]string) (src, target string) {
	t.Helper()
	src = newTestRecipe(t)
	for name, content := range extra {
		writeTestFile(t, src, name, content)
	}
	target = filepath.Join(t.TempDir(), "obs")
	if out, err := runCMDWith(t, recipeConfig(false, nil, recipeEnv), "recipe", src, "--dir", target, "--yes"); err != nil {
		t.Fatalf("a2a-serve recipe error = %v\n%s", err, out)
	}
	return src, target
}

func writeTestFile(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readTestFile(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return string(data)
}

func editConfig(t *testing.T, root string, edit func(*agentdir.Config)) {
	t.Helper()
	d, err := agentdir.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateConfig(func(c *agentdir.Config) error {
		edit(c)
		return nil
	}); err != nil {
		t.Fatalf("UpdateConfig() error = %v", err)
	}
}

func TestRecipeUpdate(t *testing.T) {
	t.Parallel()
	src, target := newRecipeAgent(t, map[string]string{"ENVIRONMENT.md": "template v1", ".agents/skills/old/SKILL.md": "old"})
	writeTestFile(t, src, "AGENTS.md", "instructions v2")
	writeTestFile(t, src, "ENVIRONMENT.md", "template v2")
	writeTestFile(t, src, ".agents/skills/new/SKILL.md", "new")
	if err := os.RemoveAll(filepath.Join(src, ".agents", "skills", "old")); err != nil {
		t.Fatal(err)
	}
	editConfig(t, src, func(c *agentdir.Config) {
		c.Description = "from the recipe"
		c.Approvals.Deny = []string{"execute"}
		c.Limits.Memory = "8g"
	})
	writeTestFile(t, target, "ENVIRONMENT.md", "our services")
	writeTestFile(t, target, "NOTES.md", "the agent's notes")
	editConfig(t, target, func(c *agentdir.Config) {
		c.Limits.Memory = "2g"
		c.Network.Private = "tailscale"
	})

	out, err := runCMDWith(t, recipeConfig(false, nil, nil), "recipe", "--dir", target, "--yes")
	if err != nil {
		t.Fatalf("a2a-serve recipe update error = %v\n%s", err, out)
	}

	for name, want := range map[string]string{
		"AGENTS.md":                   "instructions v2",
		"ENVIRONMENT.md":              "our services",
		"ENVIRONMENT.md.recipe":       "template v2",
		".agents/skills/new/SKILL.md": "new",
		"NOTES.md":                    "the agent's notes",
	} {
		if got := readTestFile(t, target, name); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
	if _, err := os.Stat(filepath.Join(target, ".agents", "skills", "old")); !os.IsNotExist(err) {
		t.Fatalf("Stat(old skill) error = %v, want it removed", err)
	}
	cfg, secrets := readAgent(t, target)
	want := &agentdir.Config{
		Name: "o11y", Harness: "opencode", Description: "from the recipe",
		Secrets:   []string{"OPENROUTER_API_KEY", "GRAFANA_TOKEN"},
		Env:       []string{"GRAFANA_URL=https://g.example.com"},
		Network:   agentdir.NetworkConfig{Private: "tailscale"},
		Approvals: agentdir.ApprovalsConfig{Allow: []string{"read", "other"}, Deny: []string{"execute"}},
		Limits:    agentdir.LimitsConfig{Memory: "2g"},
		MCP:       agentdir.MCPConfig{Disabled: []string{"kubernetes"}},
	}
	if diff := cmp.Diff(want, cfg); diff != "" {
		t.Fatalf("a2a-serve recipe update wrong config (-want +got) diff = %s", diff)
	}
	if diff := cmp.Diff(agentdir.Secrets{"OPENROUTER_API_KEY": "sk", "GRAFANA_TOKEN": "tok"}, secrets); diff != "" {
		t.Fatalf("a2a-serve recipe update wrong secrets (-want +got) diff = %s", diff)
	}
	for _, line := range []string{
		"files         + .agents/skills/new/SKILL.md, - .agents/skills/old/SKILL.md, ~ AGENTS.md",
		"kept          ENVIRONMENT.md, changed here and in the recipe; the recipe's version is in ENVIRONMENT.md.recipe",
		"description   (default) → from the recipe",
		"deny-tools    + execute",
		"kept          memory, changed here and in the recipe",
		"✓ saved",
	} {
		if !strings.Contains(out, line) {
			t.Fatalf("a2a-serve recipe update output = %s, want %q", out, line)
		}
	}

	out, err = runCMDWith(t, recipeConfig(false, nil, nil), "recipe", "--dir", target, "--yes")
	if err != nil {
		t.Fatalf("a2a-serve recipe again error = %v\n%s", err, out)
	}
	if !strings.Contains(out, "✓ o11y is up to date with "+src) || !strings.Contains(out, "unresolved    ENVIRONMENT.md") {
		t.Fatalf("a2a-serve recipe again output = %s, want up to date with the unresolved copy", out)
	}
	writeTestFile(t, target, "ENVIRONMENT.md", "template v2")
	if _, err := runCMDWith(t, recipeConfig(false, nil, nil), "recipe", "--dir", target, "--yes"); err != nil {
		t.Fatalf("a2a-serve recipe after resolving error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "ENVIRONMENT.md.recipe")); err == nil {
		t.Fatalf("ENVIRONMENT.md.recipe still exists after the conflict was resolved")
	}
}

func TestRecipeUpdateDryRun(t *testing.T) {
	t.Parallel()
	src, target := newRecipeAgent(t, nil)
	writeTestFile(t, src, "AGENTS.md", "instructions v2")
	before, err := os.ReadFile(filepath.Join(target, ".a2a", "recipe.lock"))
	if err != nil {
		t.Fatal(err)
	}

	out, err := runCMDWith(t, recipeConfig(false, nil, nil), "recipe", "--dir", target, "--dry-run")
	if err != nil {
		t.Fatalf("a2a-serve recipe --dry-run error = %v\n%s", err, out)
	}

	if !strings.Contains(out, "~ AGENTS.md") || !strings.Contains(out, "(dry run: nothing saved)") {
		t.Fatalf("a2a-serve recipe --dry-run output = %s, want the update", out)
	}
	if got := readTestFile(t, target, "AGENTS.md"); got != "instructions" {
		t.Fatalf("AGENTS.md = %q after a dry run, want it unchanged", got)
	}
	after, err := os.ReadFile(filepath.Join(target, ".a2a", "recipe.lock"))
	if err != nil || string(after) != string(before) {
		t.Fatalf("recipe.lock changed after a dry run: %v", err)
	}
}

func TestRecipeUpdateServers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		mcp     string
		env     map[string]string
		args    []string
		wantErr string
		wantOff []string
	}{
		{
			name:    "new required server without its value",
			mcp:     `"logs": {"command": "logs-mcp", "env": {"LOGS_TOKEN": "${LOGS_TOKEN}"}}`,
			wantErr: "pass --secret LOGS_TOKEN, or turn off the MCP servers that need them with --disable-mcp",
		},
		{
			name:    "new required server with its value",
			mcp:     `"logs": {"command": "logs-mcp", "env": {"LOGS_TOKEN": "${LOGS_TOKEN}"}}`,
			env:     map[string]string{"LOGS_TOKEN": "lt"},
			wantOff: []string{"kubernetes"},
		},
		{
			name:    "new required server turned off",
			mcp:     `"logs": {"command": "logs-mcp", "env": {"LOGS_TOKEN": "${LOGS_TOKEN}"}}`,
			args:    []string{"--disable-mcp", "logs"},
			wantOff: []string{"kubernetes", "logs"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			src, target := newRecipeAgent(t, nil)
			writeTestFile(t, src, ".mcp.json", strings.Replace(testRecipeMCP, "{\"mcpServers\": {", "{\"mcpServers\": {"+tc.mcp+",", 1))

			out, err := runCMDWith(t, recipeConfig(false, nil, tc.env), append([]string{"recipe", "--dir", target, "--yes"}, tc.args...)...)

			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("a2a-serve recipe update error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("a2a-serve recipe update error = %v\n%s", err, out)
			}
			cfg, _ := readAgent(t, target)
			if diff := cmp.Diff(tc.wantOff, cfg.MCP.Disabled); diff != "" {
				t.Fatalf("a2a-serve recipe update wrong servers off (-want +got) diff = %s", diff)
			}
			if !strings.Contains(out, "mcp           + logs: logs-mcp") {
				t.Fatalf("a2a-serve recipe update output = %s, want the new server", out)
			}
		})
	}
}

func TestRecipeUpdateRefuses(t *testing.T) {
	t.Parallel()
	plain := newTestAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode"})
	if _, err := runCMDWith(t, recipeConfig(false, nil, nil), "recipe", "--dir", plain.Root, "--yes"); err == nil || !strings.Contains(err.Error(), "wasn't created from a recipe") {
		t.Fatalf("a2a-serve recipe in an agent without a record error = %v, want it refused", err)
	}
	if _, err := runCMDWith(t, recipeConfig(false, nil, nil), "recipe", newTestRecipe(t), "--dir", plain.Root, "--yes"); err == nil || !strings.Contains(err.Error(), "wasn't created from a recipe") {
		t.Fatalf("a2a-serve recipe <source> into an agent without a record error = %v, want it refused", err)
	}
}

func TestRecipeUpdateDryRunRunsNoOptionLikeRefInRecord(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	_, target := newRecipeAgent(t, nil)
	marker := filepath.Join(t.TempDir(), "ran")
	writeTestFile(t, target, ".a2a/recipe.lock", "repo: .\nref: \"--upload-pack=touch "+marker+"\"\n")

	_, err := runCMDWith(t, recipeConfig(false, nil, nil), "recipe", "--dir", target, "--dry-run")

	if err == nil || !strings.Contains(err.Error(), "invalid recipe source") {
		t.Fatalf("a2a-serve recipe --dry-run error = %v, want the record refused", err)
	}
	if _, err := os.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Lstat(%s) error = %v, want the ref's command not run", marker, err)
	}
}

func TestRecipeUpdateChangesSource(t *testing.T) {
	t.Parallel()
	_, target := newRecipeAgent(t, nil)
	other := newTestRecipe(t)
	writeTestFile(t, other, "AGENTS.md", "other instructions")

	out, err := runCMDWith(t, recipeConfig(false, nil, nil), "recipe", other, "--dir", target, "--yes")
	if err != nil {
		t.Fatalf("a2a-serve recipe with another source error = %v\n%s", err, out)
	}
	if !strings.Contains(out, "! the source changes from") {
		t.Fatalf("a2a-serve recipe output = %s, want the change of source", out)
	}
	d, err := agentdir.Open(target)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := recipe.ReadRecord(d)
	if err != nil || rec.Dir != other {
		t.Fatalf("recipe.ReadRecord() = %+v, %v, want the new source %s", rec, err, other)
	}
}

func TestRecipeUpdateFromGit(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	src := newTestRecipe(t)
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = src
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	git("add", "-A")
	git("commit", "-q", "-m", "v1")
	target := filepath.Join(t.TempDir(), "obs")
	if out, err := runCMDWith(t, recipeConfig(false, nil, recipeEnv), "recipe", "file://"+src+"#main", "--dir", target, "--yes"); err != nil {
		t.Fatalf("a2a-serve recipe error = %v\n%s", err, out)
	}
	writeTestFile(t, src, "AGENTS.md", "instructions v2")
	git("commit", "-q", "-am", "v2")

	out, err := runCMDWith(t, recipeConfig(false, nil, nil), "recipe", "--dir", target, "--yes")
	if err != nil {
		t.Fatalf("a2a-serve recipe update error = %v\n%s", err, out)
	}
	if got := readTestFile(t, target, "AGENTS.md"); got != "instructions v2" {
		t.Fatalf("AGENTS.md = %q, want the branch's newest commit", got)
	}
	if !strings.Contains(out, "o11y: main ") || !strings.Contains(out, "the recipe is at main (") {
		t.Fatalf("a2a-serve recipe update output = %s, want the commits of main", out)
	}
}
