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
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/project"
)

const (
	testConfig = `name: o11y
agent: opencode
mounts: ["~/.kube/a2a-o11y:/kube:ro"]
secrets: [OPENROUTER_API_KEY]
approvals:
  allow: [read]
`
	testMCP = `{"mcpServers": {
  "grafana": {"command": "uvx", "args": ["mcp-grafana"], "env": {"GRAFANA_URL": "${GRAFANA_URL}", "GRAFANA_TOKEN": "${GRAFANA_TOKEN}"}},
  "kubernetes": {"command": "npx", "args": ["kubernetes-mcp-server"], "env": {"KUBECONFIG": "/kube/config"}}
}}`
	testManifest = `mcp:
  grafana:
    description: metrics and logs
  kubernetes:
    description: cluster state
    optional: true
    mounts: [/kube/]
vars:
  GRAFANA_URL:
    description: base URL of your Grafana
    secret: false
  GRAFANA_TOKEN:
    description: a Viewer token
  OPENROUTER_API_KEY:
    description: model key
`
)

func TestLoad(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, ".a2a/config.yaml", testConfig)
	writeFile(t, dir, ".a2a/recipe.yaml", testManifest)
	writeFile(t, dir, ".mcp.json", testMCP)
	writeFile(t, dir, "AGENTS.md", "instructions")
	writeFile(t, dir, ".agents/skills/triage/SKILL.md", "skill")

	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	f := false
	want := &Recipe{
		Config: &agentdir.Config{
			Name: "o11y", Harness: "opencode",
			Mounts:    []string{"~/.kube/a2a-o11y:/kube:ro"},
			Secrets:   []string{"OPENROUTER_API_KEY"},
			Approvals: agentdir.ApprovalsConfig{Allow: []string{"read"}},
		},
		ConfigData: []byte(testConfig),
		Manifest: &Manifest{
			MCP: map[string]MCPInfo{
				"grafana":    {Description: "metrics and logs"},
				"kubernetes": {Description: "cluster state", Optional: true, Mounts: []string{"/kube"}},
			},
			Vars: map[string]VarInfo{
				"GRAFANA_URL":        {Description: "base URL of your Grafana", Secret: &f},
				"GRAFANA_TOKEN":      {Description: "a Viewer token"},
				"OPENROUTER_API_KEY": {Description: "model key"},
			},
		},
		MCP: []project.MCPServer{
			{Name: "grafana", Type: project.MCPStdio, Command: "uvx", Args: []string{"mcp-grafana"}, Env: map[string]string{"GRAFANA_URL": "${GRAFANA_URL}", "GRAFANA_TOKEN": "${GRAFANA_TOKEN}"}},
			{Name: "kubernetes", Type: project.MCPStdio, Command: "npx", Args: []string{"kubernetes-mcp-server"}, Env: map[string]string{"KUBECONFIG": "/kube/config"}},
		},
		Skills: []project.Skill{{Name: "triage", Dir: ".agents/skills"}},
		Files: Files{
			".mcp.json":                      {Hash: sha(testMCP), Size: int64(len(testMCP))},
			"AGENTS.md":                      {Hash: sha("instructions"), Size: 12},
			".agents/skills/triage/SKILL.md": {Hash: sha("skill"), Size: 5},
		},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("Load() wrong result (-want +got) diff = %s", diff)
	}
	if got.Manifest.MountServer("/kube") != "kubernetes" || got.Manifest.MountServer("/data") != "" {
		t.Fatalf("Manifest.MountServer() = %q, %q, want kubernetes and none", got.Manifest.MountServer("/kube"), got.Manifest.MountServer("/data"))
	}
	if got.Manifest.Vars["GRAFANA_URL"].IsSecret() || !got.Manifest.Vars["GRAFANA_TOKEN"].IsSecret() {
		t.Fatalf("VarInfo.IsSecret() wrong for GRAFANA_URL or GRAFANA_TOKEN")
	}
}

func TestLoadWithoutManifest(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, ".a2a/config.yaml", testConfig)
	writeFile(t, dir, ".mcp.json", testMCP)

	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if diff := cmp.Diff(&Manifest{}, got.Manifest); diff != "" {
		t.Fatalf("Load() wrong manifest (-want +got) diff = %s", diff)
	}
	if got.Manifest.Optional("kubernetes") {
		t.Fatalf("Manifest.Optional(kubernetes) = true, want false")
	}
}

func TestLoadErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		config   string
		manifest string
		setup    func(t *testing.T, dir string)
		wantErr  string
	}{
		{
			name:    "no config",
			wantErr: "has no .a2a/config.yaml; a recipe is a directory with one",
		},
		{
			name:    "unknown settings",
			config:  "agent: opencode\nbadge: {}\nnetwork:\n  allowlist: [x]\n",
			wantErr: "the recipe uses settings this a2a-serve doesn't know: badge, network.allowlist; upgrade a2a-serve",
		},
		{
			name:    "config symlinked out of the recipe",
			setup:   func(t *testing.T, dir string) { symlink(t, dir, ".a2a/config.yaml", "/etc/hostname") },
			wantErr: "config.yaml",
		},
		{
			name:     "unknown manifest key",
			config:   testConfig,
			manifest: "mcp:\n  grafana:\n    required: true\n",
			wantErr:  "field required not found",
		},
		{
			name:     "unknown server",
			config:   testConfig,
			manifest: "mcp:\n  github:\n    optional: true\n",
			wantErr:  `mcp.github: .mcp.json has no server "github"`,
		},
		{
			name:     "unknown mount",
			config:   testConfig,
			manifest: "mcp:\n  kubernetes:\n    mounts: [/data]\n",
			wantErr:  "mcp.kubernetes.mounts: config.yaml has no mount at /data",
		},
		{
			name:     "relative mount",
			config:   testConfig,
			manifest: "mcp:\n  kubernetes:\n    mounts: [kube]\n",
			wantErr:  `mcp.kubernetes.mounts: "kube" is not an absolute path`,
		},
		{
			name:     "mount tied to two servers",
			config:   testConfig,
			manifest: "mcp:\n  grafana:\n    mounts: [/kube]\n  kubernetes:\n    mounts: [/kube]\n",
			wantErr:  "the mount at /kube is tied to grafana too",
		},
		{
			name:     "unknown variable",
			config:   testConfig,
			manifest: "vars:\n  NOPE:\n    description: x\n",
			wantErr:  "vars.NOPE: neither .mcp.json nor the secrets of config.yaml use NOPE",
		},
		{
			name:     "config secret marked not secret",
			config:   testConfig,
			manifest: "vars:\n  OPENROUTER_API_KEY:\n    secret: false\n",
			wantErr:  "vars.OPENROUTER_API_KEY: OPENROUTER_API_KEY is a secret of config.yaml",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writeFile(t, dir, ".mcp.json", testMCP)
			if tc.config != "" {
				writeFile(t, dir, ".a2a/config.yaml", tc.config)
			}
			if tc.manifest != "" {
				writeFile(t, dir, ".a2a/recipe.yaml", tc.manifest)
			}
			if tc.setup != nil {
				tc.setup(t, dir)
			}
			_, err := Load(dir)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Load() error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}
