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

package doctor

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/credstore"
	"github.com/a2aproject/a2a-serve/internal/harness"
	"github.com/a2aproject/a2a-serve/internal/network"
	"github.com/a2aproject/a2a-serve/internal/project"
)

// fakeMachine is a machine on which every tool works and is configured.
type fakeMachine struct {
	home    string
	env     map[string]string
	paths   map[string]string
	outputs map[string]string
	fails   map[string]bool
	self    string
	status  *controlapi.Status
	busy    map[int]bool
	unit    string
	channel error
}

func newFakeMachine(t *testing.T) *fakeMachine {
	t.Helper()
	m := &fakeMachine{
		home: t.TempDir(),
		env:  map[string]string{"PATH": "/usr/bin"},
		paths: map[string]string{
			"a2a":       "/usr/bin/a2a",
			"a2a-serve": "/usr/bin/a2a-serve",
			"docker":    "/usr/bin/docker",
		},
		outputs: map[string]string{
			"/usr/bin/a2a version -o json":            `{"version":"v0.4.0"}`,
			"docker info --format {{.ServerVersion}}": "27.0.1\n",
		},
		fails: map[string]bool{},
		self:  "/usr/bin/a2a-serve",
		busy:  map[int]bool{},
	}
	m.writeUserConfig(t, "plugins-enabled: true\ncredential-helper:\n  - command: [a2a-serve, credential]\n")
	return m
}

func (m *fakeMachine) writeUserConfig(t *testing.T, content string) {
	t.Helper()
	path := UserConfigPath(m.home)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("os.MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
}

func (m *fakeMachine) Env() Env {
	return Env{
		Home:        m.home,
		Credentials: credstore.At(m.home),
		LookupEnv: func(name string) (string, bool) {
			v, ok := m.env[name]
			return v, ok
		},
		LookPath: func(name string) (string, error) {
			if p, ok := m.paths[name]; ok {
				return p, nil
			}
			return "", exec.ErrNotFound
		},
		Executable: func() (string, error) { return m.self, nil },
		Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			key := strings.Join(append([]string{name}, args...), " ")
			if m.fails[key] {
				return nil, errors.New("exit status 1: " + key + " failed")
			}
			if out, ok := m.outputs[key]; ok {
				return []byte(out), nil
			}
			if strings.HasPrefix(key, "docker image inspect") || strings.HasPrefix(key, "docker manifest inspect") {
				return nil, errors.New("exit status 1: no such image")
			}
			return nil, nil
		},
		HTTP: http.DefaultClient,
		Status: func(context.Context, agentdir.Dir) (*controlapi.Status, error) {
			if m.status == nil {
				return nil, controlapi.ErrNotRunning
			}
			return m.status, nil
		},
		PortFree:    func(port int) bool { return !m.busy[port] },
		ServiceUnit: func(agentdir.Dir) (string, error) { return m.unit, nil },
		ValidateChannel: func(context.Context, *agentdir.Config, agentdir.Secrets, string) error {
			return m.channel
		},
	}
}

type check struct {
	ID     string
	Status Status
}

func checks(results []Result) []check {
	out := make([]check, len(results))
	for i, r := range results {
		out[i] = check{r.ID, r.Status}
	}
	return out
}

var healthyCLI = []check{{"a2a", StatusOK}, {"plugins", StatusOK}, {"a2a-serve", StatusOK}, {"credential-helper", StatusOK}}

func TestCLIChecks(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(*testing.T, *fakeMachine)
		want  []check
	}{
		{name: "healthy", setup: func(*testing.T, *fakeMachine) {}, want: healthyCLI},
		{
			name:  "a2a missing",
			setup: func(_ *testing.T, m *fakeMachine) { delete(m.paths, "a2a") },
			want:  []check{{"a2a", StatusFail}, {"plugins", StatusOK}, {"a2a-serve", StatusOK}, {"credential-helper", StatusOK}},
		},
		{
			name:  "a2a too old",
			setup: func(_ *testing.T, m *fakeMachine) { m.outputs["/usr/bin/a2a version -o json"] = `{"version":"0.3.1"}` },
			want:  []check{{"a2a", StatusFail}, {"plugins", StatusOK}, {"a2a-serve", StatusOK}, {"credential-helper", StatusOK}},
		},
		{
			name:  "a2a development build",
			setup: func(_ *testing.T, m *fakeMachine) { m.outputs["/usr/bin/a2a version -o json"] = `{"version":"dev"}` },
			want:  healthyCLI,
		},
		{
			name:  "no config",
			setup: func(t *testing.T, m *fakeMachine) { m.writeUserConfig(t, "") },
			want:  []check{{"a2a", StatusOK}, {"plugins", StatusFail}, {"a2a-serve", StatusOK}, {"credential-helper", StatusFail}},
		},
		{
			name: "config under environment variable names",
			setup: func(t *testing.T, m *fakeMachine) {
				m.writeUserConfig(t, "A2ACLI_PLUGINS_ENABLED: \"true\"\nA2ACLI_CREDENTIAL_HELPER:\n  - {scope: example.com, command: [other]}\n  - command: [/usr/bin/a2a-serve, credential]\n")
			},
			want: healthyCLI,
		},
		{
			name: "environment overrides",
			setup: func(t *testing.T, m *fakeMachine) {
				m.env["A2ACLI_PLUGINS_ENABLED"] = "false"
				m.env["A2ACLI_CREDENTIAL_HELPER"] = "other-helper"
			},
			want: []check{{"a2a", StatusOK}, {"plugins", StatusFail}, {"a2a-serve", StatusOK}, {"credential-helper", StatusFail}},
		},
		{
			name:  "environment helper with arguments",
			setup: func(_ *testing.T, m *fakeMachine) { m.env["A2ACLI_CREDENTIAL_HELPER"] = "a2a-serve credential" },
			want:  []check{{"a2a", StatusOK}, {"plugins", StatusOK}, {"a2a-serve", StatusOK}, {"credential-helper", StatusFail}},
		},
		{
			name: "other credential helper",
			setup: func(t *testing.T, m *fakeMachine) {
				m.writeUserConfig(t, "plugins-enabled: true\ncredential-helper: other-helper\n")
			},
			want: []check{{"a2a", StatusOK}, {"plugins", StatusOK}, {"a2a-serve", StatusOK}, {"credential-helper", StatusFail}},
		},
		{
			name: "old credential helper form",
			setup: func(t *testing.T, m *fakeMachine) {
				m.writeUserConfig(t, "plugins-enabled: true\ncredential-helper: a2a-serve credential\n")
			},
			want: []check{{"a2a", StatusOK}, {"plugins", StatusOK}, {"a2a-serve", StatusOK}, {"credential-helper", StatusFail}},
		},
		{
			name: "old credential helper list",
			setup: func(t *testing.T, m *fakeMachine) {
				m.writeUserConfig(t, "plugins-enabled: true\ncredential-helper: [example.com=other, a2a-serve credential]\n")
			},
			want: []check{{"a2a", StatusOK}, {"plugins", StatusOK}, {"a2a-serve", StatusOK}, {"credential-helper", StatusFail}},
		},
		{
			name: "invalid credential helper",
			setup: func(t *testing.T, m *fakeMachine) {
				m.writeUserConfig(t, "plugins-enabled: true\ncredential-helper:\n  - cmd: [a2a-serve, credential]\n")
			},
			want: []check{{"a2a", StatusOK}, {"plugins", StatusOK}, {"a2a-serve", StatusOK}, {"credential-helper", StatusFail}},
		},
		{
			name:  "invalid config",
			setup: func(t *testing.T, m *fakeMachine) { m.writeUserConfig(t, "plugins-enabled: [\n") },
			want:  []check{{"a2a", StatusOK}, {"a2a-config", StatusFail}, {"a2a-serve", StatusOK}},
		},
		{
			name:  "a2a-serve missing",
			setup: func(_ *testing.T, m *fakeMachine) { delete(m.paths, "a2a-serve") },
			want:  []check{{"a2a", StatusOK}, {"plugins", StatusOK}, {"a2a-serve", StatusFail}, {"credential-helper", StatusOK}},
		},
		{
			name: "a2a-serve shadowed by a stale copy",
			setup: func(_ *testing.T, m *fakeMachine) {
				m.self = "/home/u/go/bin/a2a-serve"
				m.env["PATH"] = "/usr/bin:/home/u/go/bin"
			},
			want: []check{{"a2a", StatusOK}, {"plugins", StatusOK}, {"a2a-serve", StatusFail}, {"credential-helper", StatusOK}},
		},
		{
			name:  "a2a-serve start from elsewhere",
			setup: func(_ *testing.T, m *fakeMachine) { m.self = "/tmp/build/a2a-serve" },
			want:  []check{{"a2a", StatusOK}, {"plugins", StatusOK}, {"a2a-serve", StatusWarn}, {"credential-helper", StatusOK}},
		},
		{
			name: "credentials readable by others",
			setup: func(t *testing.T, m *fakeMachine) {
				path := credstore.At(m.home).Path
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatalf("os.MkdirAll() error = %v", err)
				}
				if err := os.WriteFile(path, []byte("[]"), 0o644); err != nil {
					t.Fatalf("os.WriteFile() error = %v", err)
				}
			},
			want: append(healthyCLI[:4:4], check{"credentials", StatusFail}),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := newFakeMachine(t)
			tc.setup(t, m)
			got := CLIChecks(t.Context(), m.Env())
			if diff := cmp.Diff(tc.want, checks(got)); diff != "" {
				t.Fatalf("CLIChecks() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestFixes(t *testing.T) {
	t.Parallel()
	m := newFakeMachine(t)
	m.writeUserConfig(t, "# my settings\ntimeout: 30s\nplugins-enabled: false\n")
	credPath := credstore.At(m.home).Path
	if err := os.MkdirAll(filepath.Dir(credPath), 0o700); err != nil {
		t.Fatalf("os.MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(credPath, []byte("[]"), 0o644); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	d := newAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode", Sandbox: agentdir.SandboxLocal})
	m.paths["opencode"] = "/usr/bin/opencode"
	if err := os.Chmod(d.StateDir(), 0o755); err != nil {
		t.Fatalf("os.Chmod() error = %v", err)
	}
	opts := Options{Dir: &d}

	for range 2 {
		for _, r := range Run(t.Context(), m.Env(), opts) {
			if !r.Fixable() {
				continue
			}
			if _, err := r.Apply(); err != nil {
				t.Fatalf("Result{ID: %q}.Apply() error = %v", r.ID, err)
			}
		}
	}

	if got := Run(t.Context(), m.Env(), opts); Failed(got) {
		t.Fatalf("Run() after fixes = %+v, want no failures", got)
	}
	data, err := os.ReadFile(UserConfigPath(m.home))
	if err != nil {
		t.Fatalf("os.ReadFile() error = %v", err)
	}
	want := "# my settings\ntimeout: 30s\nplugins-enabled: true\ncredential-helper:\n  - command: [a2a-serve, credential]\n"
	if diff := cmp.Diff(want, string(data)); diff != "" {
		t.Fatalf("config.yaml after fixes wrong result (-want +got) diff = %s", diff)
	}
}

func TestFixCredentialHelper(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		config string
		want   string
	}{
		{
			name:   "not configured",
			config: "timeout: 30s\n",
			want:   "timeout: 30s\ncredential-helper:\n  - command: [a2a-serve, credential]\n",
		},
		{
			name:   "old form",
			config: "credential-helper: a2a-serve credential # caller tokens\ntimeout: 30s\n",
			want:   "credential-helper:\n  - command: [a2a-serve, credential]\ntimeout: 30s\n",
		},
		{
			name:   "old list under the environment variable name",
			config: "A2ACLI_CREDENTIAL_HELPER:\n  - \"*.corp.example=corp-helper --realm 'Corp Users'\"\n  - /usr/bin/a2a-serve credential\n",
			want:   "A2ACLI_CREDENTIAL_HELPER:\n  - scope: '*.corp.example'\n    command: [corp-helper, --realm, Corp Users]\n  - command: [/usr/bin/a2a-serve, credential]\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := newFakeMachine(t)
			m.writeUserConfig(t, tc.config)
			helper := func() Result {
				for _, r := range CLIChecks(t.Context(), m.Env()) {
					if r.ID == "credential-helper" {
						return r
					}
				}
				t.Fatalf("CLIChecks() has no credential-helper result")
				return Result{}
			}
			r := helper()
			if r.Status != StatusFail || !r.Fixable() {
				t.Fatalf("CLIChecks() credential-helper = %+v, want a fixable failure", r)
			}
			if _, err := r.Apply(); err != nil {
				t.Fatalf("Result{ID: %q}.Apply() error = %v", r.ID, err)
			}
			if r := helper(); r.Status != StatusOK {
				t.Fatalf("CLIChecks() credential-helper after the fix = %+v, want OK", r)
			}
			data, err := os.ReadFile(UserConfigPath(m.home))
			if err != nil {
				t.Fatalf("os.ReadFile() error = %v", err)
			}
			if diff := cmp.Diff(tc.want, string(data)); diff != "" {
				t.Fatalf("config.yaml after the fix wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestSetUserConfigKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		existing    *string
		want        string
		wantChanged bool
	}{
		{name: "no file", want: "credential-helper: a2a-serve credential\n", wantChanged: true},
		{name: "empty file", existing: new(""), want: "credential-helper: a2a-serve credential\n", wantChanged: true},
		{name: "empty mapping", existing: new("{}\n"), want: "credential-helper: a2a-serve credential\n", wantChanged: true},
		{
			name:        "other keys",
			existing:    new("timeout: 30s # default\nagent: http://x\n"),
			want:        "timeout: 30s # default\nagent: http://x\ncredential-helper: a2a-serve credential\n",
			wantChanged: true,
		},
		{
			name:        "replaced value",
			existing:    new("credential-helper: old\ntimeout: 30s\n"),
			want:        "credential-helper: a2a-serve credential\ntimeout: 30s\n",
			wantChanged: true,
		},
		{name: "same value", existing: new("credential-helper: a2a-serve credential\n"), want: "credential-helper: a2a-serve credential\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "a2a-cli", "config.yaml")
			if tc.existing != nil {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatalf("os.MkdirAll() error = %v", err)
				}
				if err := os.WriteFile(path, []byte(*tc.existing), 0o644); err != nil {
					t.Fatalf("os.WriteFile() error = %v", err)
				}
			}
			changed, err := SetUserConfigKey(path, "credential-helper", scalar("!!str", "a2a-serve credential"))
			if err != nil {
				t.Fatalf("SetUserConfigKey() error = %v", err)
			}
			if changed != tc.wantChanged {
				t.Fatalf("SetUserConfigKey() = %v, want %v", changed, tc.wantChanged)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("os.ReadFile() error = %v", err)
			}
			if diff := cmp.Diff(tc.want, string(data)); diff != "" {
				t.Fatalf("SetUserConfigKey() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func newAgent(t *testing.T, cfg *agentdir.Config) agentdir.Dir {
	t.Helper()
	d, err := agentdir.Open(t.TempDir())
	if err != nil {
		t.Fatalf("agentdir.Open() error = %v", err)
	}
	if err := d.SaveConfig(cfg); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}
	return d
}

// relayStatus is how doctor sees a relay offline: fine when the build can
// join one.
var relayStatus = map[bool]Status{true: StatusOK, false: StatusFail}[network.AgentMeshBuilt]

func TestAgentChecks(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		cfg    agentdir.Config
		online bool
		setup  func(*testing.T, *fakeMachine, agentdir.Dir)
		want   []check
	}{
		{
			name: "stopped docker agent",
			cfg:  agentdir.Config{Name: "coder", Harness: "opencode"},
			want: []check{{"docker", StatusOK}, {"harness-image", StatusWarn}, {"config", StatusOK}, {"running", StatusWarn}},
		},
		{
			name: "docker unreachable",
			cfg:  agentdir.Config{Name: "coder", Harness: "opencode"},
			setup: func(_ *testing.T, m *fakeMachine, _ agentdir.Dir) {
				m.fails["docker info --format {{.ServerVersion}}"] = true
			},
			want: []check{{"docker", StatusFail}, {"config", StatusOK}, {"running", StatusWarn}},
		},
		{
			name: "docker not installed",
			cfg:  agentdir.Config{Name: "coder", Harness: "opencode"},
			setup: func(_ *testing.T, m *fakeMachine, _ agentdir.Dir) {
				delete(m.paths, "docker")
			},
			want: []check{{"docker", StatusFail}, {"config", StatusOK}, {"running", StatusWarn}},
		},
		{
			name: "image present",
			cfg:  agentdir.Config{Name: "coder", Harness: "opencode", Image: "example/img"},
			setup: func(_ *testing.T, m *fakeMachine, _ agentdir.Dir) {
				m.outputs["docker image inspect --format {{.Id}} example/img"] = "sha256:1"
			},
			want: []check{{"docker", StatusOK}, {"harness-image", StatusOK}, {"config", StatusOK}, {"running", StatusWarn}},
		},
		{
			name:   "image pullable",
			cfg:    agentdir.Config{Name: "coder", Harness: "opencode", Image: "example/img"},
			online: true,
			setup: func(_ *testing.T, m *fakeMachine, _ agentdir.Dir) {
				m.outputs["docker manifest inspect example/img"] = "{}"
			},
			want: []check{{"docker", StatusOK}, {"harness-image", StatusOK}, {"config", StatusOK}, {"running", StatusWarn}},
		},
		{
			name:   "image not pullable",
			cfg:    agentdir.Config{Name: "coder", Harness: "opencode", Image: "example/img"},
			online: true,
			want:   []check{{"docker", StatusOK}, {"harness-image", StatusFail}, {"config", StatusOK}, {"running", StatusWarn}},
		},
		{
			name:   "profile image built locally",
			cfg:    agentdir.Config{Name: "coder", Harness: "opencode"},
			online: true,
			want:   []check{{"docker", StatusOK}, {"harness-image", StatusWarn}, {"config", StatusOK}, {"running", StatusWarn}},
		},
		{
			name: "profile image built before",
			cfg:  agentdir.Config{Name: "coder", Harness: "opencode"},
			setup: func(_ *testing.T, m *fakeMachine, _ agentdir.Dir) {
				m.outputs["docker image inspect --format {{.Id}} "+harness.LocalImage("opencode")] = "sha256:1"
			},
			want: []check{{"docker", StatusOK}, {"harness-image", StatusOK}, {"config", StatusOK}, {"running", StatusWarn}},
		},
		{
			name: "local sandbox without the harness command",
			cfg:  agentdir.Config{Name: "coder", Harness: "opencode", Sandbox: agentdir.SandboxLocal},
			want: []check{{"harness-command", StatusFail}, {"config", StatusOK}, {"running", StatusWarn}},
		},
		{
			name: "local sandbox",
			cfg:  agentdir.Config{Name: "coder", Harness: "acp", Command: []string{"fakeacp", "--x"}, Sandbox: agentdir.SandboxLocal},
			setup: func(_ *testing.T, m *fakeMachine, _ agentdir.Dir) {
				m.paths["fakeacp"] = "/usr/bin/fakeacp"
			},
			want: []check{{"harness-command", StatusOK}, {"config", StatusOK}, {"running", StatusWarn}},
		},
		{
			name: "invalid config",
			cfg:  agentdir.Config{Name: "coder", Harness: "opencode", Sandbox: agentdir.SandboxLocal, Network: agentdir.NetworkConfig{Private: "tailscal", Public: "ngrk"}},
			setup: func(_ *testing.T, m *fakeMachine, _ agentdir.Dir) {
				m.paths["opencode"] = "/usr/bin/opencode"
			},
			want: []check{{"harness-command", StatusOK}, {"config", StatusFail}, {"config", StatusFail}, {"running", StatusWarn}},
		},
		{
			name: "missing secrets and mounts",
			cfg: agentdir.Config{
				Name: "coder", Harness: "opencode", Sandbox: agentdir.SandboxLocal,
				Secrets: []string{"API_KEY", "OTHER_KEY"}, Mounts: []string{".", "/nonexistent-a2a:/docs"},
				Channels: agentdir.ChannelsConfig{Telegram: &agentdir.TelegramConfig{Token: "secret:TELEGRAM_TOKEN"}},
			},
			setup: func(t *testing.T, m *fakeMachine, d agentdir.Dir) {
				m.paths["opencode"] = "/usr/bin/opencode"
				if err := d.SaveSecrets(agentdir.Secrets{"OTHER_KEY": "x"}); err != nil {
					t.Fatalf("SaveSecrets() error = %v", err)
				}
			},
			want: []check{{"harness-command", StatusOK}, {"config", StatusOK}, {"secrets", StatusFail}, {"secrets", StatusFail}, {"mounts", StatusFail}, {"running", StatusWarn}},
		},
		{
			name: "project MCP servers",
			cfg:  agentdir.Config{Name: "coder", Harness: "opencode", Sandbox: agentdir.SandboxLocal, Secrets: []string{"GH_TOKEN"}},
			setup: func(t *testing.T, m *fakeMachine, d agentdir.Dir) {
				m.paths["opencode"] = "/usr/bin/opencode"
				if err := d.SaveSecrets(agentdir.Secrets{"GH_TOKEN": "x"}); err != nil {
					t.Fatalf("SaveSecrets() error = %v", err)
				}
				mcp := `{"mcpServers": {"github": {"command": "gh-mcp", "env": {"T": "${GH_TOKEN}"}}}}`
				if err := os.WriteFile(filepath.Join(d.Root, project.MCPFile), []byte(mcp), 0o644); err != nil {
					t.Fatalf("os.WriteFile() error = %v", err)
				}
			},
			want: []check{{"harness-command", StatusOK}, {"config", StatusOK}, {"secrets", StatusOK}, {"mcp", StatusOK}, {"running", StatusWarn}},
		},
		{
			name: "project MCP servers without values",
			cfg:  agentdir.Config{Name: "coder", Harness: "opencode", Sandbox: agentdir.SandboxLocal},
			setup: func(t *testing.T, m *fakeMachine, d agentdir.Dir) {
				m.paths["opencode"] = "/usr/bin/opencode"
				mcp := `{"mcpServers": {"github": {"command": "gh-mcp", "env": {"T": "${GH_TOKEN}"}}, "api": {"url": "https://x/${KEY}"}}}`
				if err := os.WriteFile(filepath.Join(d.Root, project.MCPFile), []byte(mcp), 0o644); err != nil {
					t.Fatalf("os.WriteFile() error = %v", err)
				}
			},
			want: []check{{"harness-command", StatusOK}, {"config", StatusOK}, {"mcp", StatusWarn}, {"mcp", StatusWarn}, {"running", StatusWarn}},
		},
		{
			name: "invalid project MCP file",
			cfg:  agentdir.Config{Name: "coder", Harness: "opencode", Sandbox: agentdir.SandboxLocal},
			setup: func(t *testing.T, m *fakeMachine, d agentdir.Dir) {
				m.paths["opencode"] = "/usr/bin/opencode"
				if err := os.WriteFile(filepath.Join(d.Root, project.MCPFile), []byte("{"), 0o644); err != nil {
					t.Fatalf("os.WriteFile() error = %v", err)
				}
			},
			want: []check{{"harness-command", StatusOK}, {"config", StatusOK}, {"mcp", StatusFail}, {"running", StatusWarn}},
		},
		{
			name: "relay without Agent Mesh in the build",
			cfg:  agentdir.Config{Name: "coder", Harness: "opencode", Sandbox: agentdir.SandboxLocal, Network: agentdir.NetworkConfig{Private: "https://relay.example.com"}},
			setup: func(_ *testing.T, m *fakeMachine, _ agentdir.Dir) {
				m.paths["opencode"] = "/usr/bin/opencode"
			},
			want: []check{{"harness-command", StatusOK}, {"config", StatusOK}, {"relay", relayStatus}, {"running", StatusWarn}},
		},
		{
			name: "backups to GCS",
			cfg: agentdir.Config{Name: "coder", Harness: "opencode", Sandbox: agentdir.SandboxLocal,
				Backup: agentdir.BackupConfig{To: "gs://bucket/coder", Key: "secret:BACKUP_KEY"}},
			setup: func(t *testing.T, m *fakeMachine, d agentdir.Dir) {
				m.paths["opencode"] = "/usr/bin/opencode"
				m.paths["gcloud"] = "/usr/bin/gcloud"
				if err := d.SaveSecrets(agentdir.Secrets{"BACKUP_KEY": "k"}); err != nil {
					t.Fatalf("SaveSecrets() error = %v", err)
				}
			},
			want: []check{{"harness-command", StatusOK}, {"config", StatusOK}, {"secrets", StatusOK}, {"backup", StatusOK}, {"running", StatusWarn}},
		},
		{
			name: "backups to S3 without the aws CLI or the key",
			cfg: agentdir.Config{Name: "coder", Harness: "opencode", Sandbox: agentdir.SandboxLocal,
				Backup: agentdir.BackupConfig{To: "s3://bucket/coder", Key: "secret:BACKUP_KEY"}},
			setup: func(_ *testing.T, m *fakeMachine, _ agentdir.Dir) {
				m.paths["opencode"] = "/usr/bin/opencode"
			},
			want: []check{{"harness-command", StatusOK}, {"config", StatusOK}, {"secrets", StatusFail}, {"backup", StatusFail}, {"running", StatusWarn}},
		},
		{
			name: "state readable by others",
			cfg:  agentdir.Config{Name: "coder", Harness: "opencode", Sandbox: agentdir.SandboxLocal, Secrets: []string{"API_KEY"}},
			setup: func(t *testing.T, m *fakeMachine, d agentdir.Dir) {
				m.paths["opencode"] = "/usr/bin/opencode"
				if err := d.SaveSecrets(agentdir.Secrets{"API_KEY": "x"}); err != nil {
					t.Fatalf("SaveSecrets() error = %v", err)
				}
				if err := os.Chmod(d.SecretsPath(), 0o644); err != nil {
					t.Fatalf("os.Chmod() error = %v", err)
				}
			},
			want: []check{{"harness-command", StatusOK}, {"config", StatusOK}, {"secrets", StatusOK}, {"state-permissions", StatusFail}},
		},
		{
			name: "port taken",
			cfg:  agentdir.Config{Name: "coder", Harness: "opencode", Sandbox: agentdir.SandboxLocal, Port: 46757},
			setup: func(_ *testing.T, m *fakeMachine, _ agentdir.Dir) {
				m.paths["opencode"] = "/usr/bin/opencode"
				m.busy[46757] = true
			},
			want: []check{{"harness-command", StatusOK}, {"config", StatusOK}, {"running", StatusWarn}, {"port", StatusFail}},
		},
		{
			name: "stale service",
			cfg:  agentdir.Config{Name: "coder", Harness: "opencode", Sandbox: agentdir.SandboxLocal},
			setup: func(t *testing.T, m *fakeMachine, d agentdir.Dir) {
				m.paths["opencode"] = "/usr/bin/opencode"
				m.unit = writeFile(t, "ExecStart=/old/a2a-serve --dir "+d.Root+"\n")
			},
			want: []check{{"harness-command", StatusOK}, {"config", StatusOK}, {"running", StatusWarn}, {"service", StatusFail}},
		},
		{
			name: "current service",
			cfg:  agentdir.Config{Name: "coder", Harness: "opencode", Sandbox: agentdir.SandboxLocal},
			setup: func(t *testing.T, m *fakeMachine, d agentdir.Dir) {
				m.paths["opencode"] = "/usr/bin/opencode"
				m.unit = writeFile(t, "ExecStart=/usr/bin/a2a-serve start --dir "+d.Root+"\n")
			},
			want: []check{{"harness-command", StatusOK}, {"config", StatusOK}, {"running", StatusWarn}, {"service", StatusOK}},
		},
		{
			name: "service without start",
			cfg:  agentdir.Config{Name: "coder", Harness: "opencode", Sandbox: agentdir.SandboxLocal},
			setup: func(t *testing.T, m *fakeMachine, d agentdir.Dir) {
				m.paths["opencode"] = "/usr/bin/opencode"
				m.unit = writeFile(t, "ExecStart=/usr/bin/a2a-serve --dir "+d.Root+"\n")
			},
			want: []check{{"harness-command", StatusOK}, {"config", StatusOK}, {"running", StatusWarn}, {"service", StatusFail}},
		},
		{
			name: "network",
			cfg: agentdir.Config{
				Name: "coder", Harness: "opencode", Sandbox: agentdir.SandboxLocal,
				Network: agentdir.NetworkConfig{Private: agentdir.NetworkTailscale, Public: agentdir.NetworkNgrok},
			},
			setup: func(_ *testing.T, m *fakeMachine, _ agentdir.Dir) {
				m.paths["opencode"] = "/usr/bin/opencode"
			},
			want: []check{{"harness-command", StatusOK}, {"config", StatusOK}, {"running", StatusWarn}, {"tailscale", StatusWarn}, {"ngrok", StatusFail}},
		},
		{
			name: "network credentials stored",
			cfg: agentdir.Config{
				Name: "coder", Harness: "opencode", Sandbox: agentdir.SandboxLocal,
				Network: agentdir.NetworkConfig{Public: agentdir.NetworkTailscale},
			},
			setup: func(t *testing.T, m *fakeMachine, d agentdir.Dir) {
				m.paths["opencode"] = "/usr/bin/opencode"
				if err := os.MkdirAll(d.TSNetDir(), 0o700); err != nil {
					t.Fatalf("os.MkdirAll() error = %v", err)
				}
				if err := os.WriteFile(filepath.Join(d.TSNetDir(), "tailscaled.state"), []byte("{}"), 0o600); err != nil {
					t.Fatalf("os.WriteFile() error = %v", err)
				}
			},
			want: []check{{"harness-command", StatusOK}, {"config", StatusOK}, {"running", StatusWarn}, {"tailscale", StatusOK}},
		},
		{
			name:   "rejected channel credentials",
			online: true,
			cfg: agentdir.Config{
				Name: "coder", Harness: "opencode", Sandbox: agentdir.SandboxLocal,
				Channels: agentdir.ChannelsConfig{
					Telegram: &agentdir.TelegramConfig{Token: "secret:TELEGRAM_TOKEN"},
					Email:    &agentdir.EmailConfig{Address: "a@x", IMAP: "imap:993", SMTP: "smtp:465", Password: "pw", TrustedAuthServ: "x"},
				},
			},
			setup: func(t *testing.T, m *fakeMachine, d agentdir.Dir) {
				m.paths["opencode"] = "/usr/bin/opencode"
				m.channel = errors.New("invalid Telegram bot token")
				if err := d.SaveSecrets(agentdir.Secrets{"TELEGRAM_TOKEN": "x"}); err != nil {
					t.Fatalf("SaveSecrets() error = %v", err)
				}
			},
			want: []check{{"harness-command", StatusOK}, {"config", StatusOK}, {"secrets", StatusOK}, {"running", StatusWarn}, {"telegram", StatusFail}, {"email", StatusFail}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := newFakeMachine(t)
			d := newAgent(t, &tc.cfg)
			if tc.setup != nil {
				tc.setup(t, m, d)
			}
			got := Run(t.Context(), m.Env(), Options{Dir: &d, Online: tc.online})
			if diff := cmp.Diff(append(healthyCLI[:4:4], tc.want...), checks(got)); diff != "" {
				t.Fatalf("Run() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "unit")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	return path
}

func TestRunningAgent(t *testing.T) {
	t.Parallel()
	const token = "owner-token"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	t.Cleanup(srv.Close)
	tests := []struct {
		name  string
		token string
		local string
		want  []check
	}{
		{name: "token accepted", token: token, local: srv.URL, want: []check{{"running", StatusOK}, {"owner-token", StatusOK}}},
		{name: "token rejected", token: "stale", local: srv.URL, want: []check{{"running", StatusOK}, {"owner-token", StatusFail}}},
		{name: "no token", local: srv.URL, want: []check{{"running", StatusOK}, {"owner-token", StatusFail}}},
		{name: "local URL down", token: token, local: "http://127.0.0.1:1", want: []check{{"running", StatusOK}, {"local-url", StatusFail}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := newFakeMachine(t)
			m.paths["opencode"] = "/usr/bin/opencode"
			m.status = &controlapi.Status{Local: tc.local, PID: 42}
			env := m.Env()
			env.HTTP = &http.Client{Timeout: 5 * time.Second}
			if tc.token != "" {
				if err := env.Credentials.Put(credstore.Credential{URL: tc.local, Token: tc.token}); err != nil {
					t.Fatalf("Credentials.Put() error = %v", err)
				}
			}
			d := newAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode", Sandbox: agentdir.SandboxLocal})
			got := Run(t.Context(), env, Options{Dir: &d})
			want := append(healthyCLI[:4:4], check{"harness-command", StatusOK}, check{"config", StatusOK})
			if diff := cmp.Diff(append(want, tc.want...), checks(got)); diff != "" {
				t.Fatalf("Run() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestRender(t *testing.T) {
	t.Parallel()
	results := []Result{
		{ID: "a2a", Status: StatusOK, Message: "a2a v0.4.0 at /usr/bin/a2a", Group: GroupCLI},
		{ID: "credential-helper", Status: StatusFail, Message: "credential helper not configured", Detail: "calls get 401", Fix: fixCommand, Group: GroupCLI},
		{ID: "config", Status: StatusOK, Message: "config valid", Group: "Agent coder (/src/app)"},
		{ID: "running", Status: StatusWarn, Message: "not running", Fix: "a2a-serve start --detach", Group: "Agent coder (/src/app)"},
	}
	var buf bytes.Buffer
	if err := Render(&buf, results); err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	want := `a2a CLI
  ✓ a2a v0.4.0 at /usr/bin/a2a
  ✗ credential helper not configured
      calls get 401
      fix: a2a-serve doctor --fix
Agent coder (/src/app)
  ✓ config valid
  ! not running: a2a-serve start --detach
1 failed, 1 warning
`
	if diff := cmp.Diff(want, buf.String()); diff != "" {
		t.Fatalf("Render() wrong result (-want +got) diff = %s", diff)
	}
}

func TestCheckSecretsFixes(t *testing.T) {
	t.Parallel()
	cfg := &agentdir.Config{Name: "coder", Harness: "opencode", Backup: agentdir.BackupConfig{To: "/tmp/b", Key: "secret:BACKUP_KEY"}}
	for _, kind := range []string{agentdir.ChannelTelegram, agentdir.ChannelDiscord} {
		if err := cfg.SetChannel(kind, "secret:"+strings.ToUpper(kind)+"_TOKEN", nil); err != nil {
			t.Fatalf("SetChannel(%s) error = %v", kind, err)
		}
	}
	var got []string
	for _, r := range checkSecrets(cfg, agentdir.Secrets{}) {
		got = append(got, r.Message+" | "+r.Fix)
	}
	want := []string{
		"secret TELEGRAM_TOKEN (telegram token) not stored | a2a-serve channel add telegram --token -",
		"secret DISCORD_TOKEN (discord token) not stored | a2a-serve channel add discord --token -",
		"secret BACKUP_KEY (--backup-key) not stored | a2a-serve config set --backup-key -",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("checkSecrets() wrong result (-want +got) diff = %s", diff)
	}
}
