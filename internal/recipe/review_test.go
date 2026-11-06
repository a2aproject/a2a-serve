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

package recipe

import (
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
)

func loadTestRecipe(t *testing.T, config string, extra map[string]string) *Recipe {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, dir, ".a2a/config.yaml", config)
	writeFile(t, dir, ".a2a/recipe.yaml", testManifest)
	writeFile(t, dir, ".mcp.json", testMCP)
	for name, content := range extra {
		writeFile(t, dir, name, content)
	}
	r, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	return r
}

func TestReview(t *testing.T) {
	t.Parallel()
	r := loadTestRecipe(t, testConfig+"port: 8123\nnetwork:\n  private: tailscale\n", map[string]string{
		"AGENTS.md":                      "instructions",
		".agents/skills/triage/SKILL.md": "skill",
		".claude/skills/triage/SKILL.md": "skill",
		".opencode/agent/reviewer.md":    "agent",
		".claude/settings.json":          "{}",
	})
	cfg := r.NewConfig("o11y")
	cfg.Env = []string{"GRAFANA_URL=https://g.example.com"}
	cfg.Approvals.Deny = []string{"execute"}
	cfg.Limits.Memory = "8g"
	cfg.MCP.Disabled = []string{"grafana"}

	got := Review(r, cfg, "./o11y")

	want := []Line{
		{Label: "agent", Items: []Item{{Text: "opencode"}}},
		{Label: "sandbox", Items: []Item{{Text: "docker"}}},
		{Label: "workspace", Items: []Item{{Text: "./o11y, read-write"}}},
		{Label: "mounts", Items: []Item{{Text: filepath.Join("~", ".kube", "a2a-o11y") + " → /kube, read-only, for kubernetes"}}},
		{Label: "environment", Items: []Item{{Text: "secrets OPENROUTER_API_KEY"}, {Text: "env GRAFANA_URL=https://g.example.com"}}},
		{Label: "mcp", Items: []Item{
			{Text: "grafana, off: uvx mcp-grafana", Detail: "metrics and logs"},
			{Text: "kubernetes, optional: npx kubernetes-mcp-server", Detail: "cluster state"},
		}},
		{Label: "skills", Items: []Item{{Text: "triage"}}},
		{Label: "instructions", Items: []Item{{Text: "AGENTS.md"}}},
		{Label: "agent config", Items: []Item{{Text: ".claude/settings.json, .opencode/: they can change what the agent runs and approves", Warn: true}}},
		{Label: "approvals", Items: []Item{{Text: "approved without asking: read"}, {Text: "denied: execute"}}},
		{Label: "limits", Items: []Item{{Text: "memory 8g"}}},
		{Label: "not taken", Items: []Item{{Text: "port, private: the owner sets these"}}},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("Review() wrong result (-want +got) diff = %s", diff)
	}
}

func TestReviewWarnings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		edit  func(cfg *agentdir.Config)
		label string
		want  Item
	}{
		{
			name:  "local sandbox",
			edit:  func(cfg *agentdir.Config) { cfg.Sandbox = agentdir.SandboxLocal },
			label: "sandbox",
			want:  Item{Text: "local: no isolation, with your permissions", Warn: true},
		},
		{
			name: "command on the host",
			edit: func(cfg *agentdir.Config) {
				cfg.Sandbox = agentdir.SandboxLocal
				cfg.Command = []string{"my-agent", "--acp"}
			},
			label: "agent",
			want:  Item{Text: "opencode: my-agent --acp", Warn: true},
		},
		{
			name:  "command in the sandbox",
			edit:  func(cfg *agentdir.Config) { cfg.Command = []string{"my-agent"} },
			label: "agent",
			want:  Item{Text: "opencode: my-agent"},
		},
		{
			name:  "read-write mount",
			edit:  func(cfg *agentdir.Config) { cfg.Mounts = []string{"/srv/data:/data"} },
			label: "mounts",
			want:  Item{Text: "/srv/data → /data, read-write", Warn: true},
		},
		{
			name:  "approvals that change things",
			edit:  func(cfg *agentdir.Config) { cfg.Approvals.Allow = []string{"read", "e*"} },
			label: "approvals",
			want:  Item{Text: "approved without asking: read, e*", Warn: true},
		},
		{
			name:  "approvals that don't change anything",
			edit:  func(cfg *agentdir.Config) { cfg.Approvals.Allow = []string{"read", "search", "think"} },
			label: "approvals",
			want:  Item{Text: "approved without asking: read, search, think"},
		},
		{
			name:  "approvals of MCP tools",
			edit:  func(cfg *agentdir.Config) { cfg.Approvals.Allow = []string{"read", "tool:grafana.*"} },
			label: "approvals",
			want:  Item{Text: "approved without asking: read, tool:grafana.*", Warn: true},
		},
		{
			name:  "approvals of commands",
			edit:  func(cfg *agentdir.Config) { cfg.Approvals.Allow = []string{"command:kubectl get *"} },
			label: "approvals",
			want:  Item{Text: "approved without asking: command:kubectl get *", Warn: true},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := loadTestRecipe(t, testConfig, nil)
			cfg := r.NewConfig("o11y")
			tc.edit(cfg)

			for _, line := range Review(r, cfg, "./o11y") {
				if line.Label != tc.label {
					continue
				}
				if diff := cmp.Diff(tc.want, line.Items[0]); diff != "" {
					t.Fatalf("Review() wrong %s line (-want +got) diff = %s", tc.label, diff)
				}
				return
			}
			t.Fatalf("Review() has no %s line", tc.label)
		})
	}
}

func TestRecipePlan(t *testing.T) {
	t.Parallel()
	r := loadTestRecipe(t, testConfig, nil)
	cfg := r.NewConfig("o11y")
	cfg.Mounts = []string{filepath.Join(t.TempDir(), "missing") + ":/kube:ro"}

	if diff := cmp.Diff([]Var{{Name: "OPENROUTER_API_KEY", Description: "model key", Secret: true}}, r.SecretVars()); diff != "" {
		t.Fatalf("Recipe.SecretVars() wrong result (-want +got) diff = %s", diff)
	}
	grafana := r.MCP[0]
	wantVars := []Var{{Name: "GRAFANA_TOKEN", Description: "a Viewer token", Secret: true}, {Name: "GRAFANA_URL", Description: "base URL of your Grafana"}}
	if diff := cmp.Diff(wantVars, r.ServerVars(grafana, func(string) bool { return false })); diff != "" {
		t.Fatalf("Recipe.ServerVars() wrong result (-want +got) diff = %s", diff)
	}
	if got := r.ServerVars(grafana, func(name string) bool { return name == "GRAFANA_URL" }); len(got) != 1 || got[0].Name != "GRAFANA_TOKEN" {
		t.Fatalf("Recipe.ServerVars() with GRAFANA_URL provided = %v, want GRAFANA_TOKEN", got)
	}
	if got := r.MissingMounts(cfg, "kubernetes"); len(got) != 1 {
		t.Fatalf("Recipe.MissingMounts(kubernetes) = %v, want the missing source", got)
	}
	if got := r.MissingMounts(cfg, "grafana"); len(got) != 0 {
		t.Fatalf("Recipe.MissingMounts(grafana) = %v, want none", got)
	}
	cfg.Mounts = append(cfg.Mounts, "/srv/data:/data")
	r.DropMounts(cfg, []string{"kubernetes"})
	restored := cfg.Clone()
	r.RestoreMounts(restored, r.Config, []string{"kubernetes"})
	if diff := cmp.Diff([]string{"/srv/data:/data", "~/.kube/a2a-o11y:/kube:ro"}, restored.Mounts); diff != "" {
		t.Fatalf("Recipe.RestoreMounts() wrong mounts (-want +got) diff = %s", diff)
	}
	if diff := cmp.Diff([]string{"/srv/data:/data"}, cfg.Mounts); diff != "" {
		t.Fatalf("Recipe.DropMounts() wrong mounts (-want +got) diff = %s", diff)
	}
}
