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
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/project"
	"github.com/a2aproject/a2a-serve/internal/sandboxnet"
)

func TestPrepareLocal(t *testing.T) {
	t.Parallel()
	ws, home := t.TempDir(), t.TempDir()
	profile, err := Lookup("opencode", []string{"my-acp", "--flag"})
	if err != nil {
		t.Fatalf("Lookup() error = %v", err)
	}
	p, err := Prepare(context.Background(), Spec{
		Profile: profile,
		Sandbox: SandboxLocal,
		Mounts:  []agentdir.Mount{{Src: ws, Dst: ws}},
		Home:    home,
		EnvDir:  t.TempDir(),
		Env:     []string{"API_KEY=k"},
		Socket:  "/run/agent/a2a.sock",
		CLI:     "/opt/a2a/bin/a2a",
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if !slices.Equal(p.Args, []string{"my-acp", "--flag"}) {
		t.Fatalf("Prepare() args = %v, want the custom command", p.Args)
	}
	for _, want := range []string{"API_KEY=k", EnvUnixSocket + "=/run/agent/a2a.sock", "PATH=/opt/a2a/bin" + string(os.PathListSeparator)} {
		if !slices.ContainsFunc(p.Env, func(kv string) bool { return strings.HasPrefix(kv, want) }) {
			t.Fatalf("Prepare() env is missing %q", want)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "opencode", "opencode.json")); err != nil {
		t.Fatalf("Prepare() did not bootstrap the harness home: %v", err)
	}
	if err := p.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
}

func TestPrepareLocalChecksCommand(t *testing.T) {
	t.Parallel()
	ws := t.TempDir()
	agent := filepath.Join(ws, "bin", "agent")
	if err := os.MkdirAll(filepath.Dir(agent), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agent, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	const notInstalled = " is not installed; install it, or run a2a-serve config set --sandbox docker, or run a2a-serve doctor"
	tests := []struct {
		name    string
		command string
		wantErr string
	}{
		{name: "absolute path", command: agent},
		{name: "path in the workspace", command: "./bin/agent"},
		{name: "missing", command: "a2a-serve-missing-agent", wantErr: "a2a-serve-missing-agent" + notInstalled},
		{name: "missing path", command: "./bin/missing", wantErr: "./bin/missing" + notInstalled},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			profile, err := Lookup("opencode", []string{tc.command})
			if err != nil {
				t.Fatalf("Lookup() error = %v", err)
			}
			p, err := Prepare(context.Background(), Spec{Profile: profile, Sandbox: SandboxLocal, Mounts: []agentdir.Mount{{Src: ws, Dst: ws}}, Home: t.TempDir()})
			if err != nil {
				t.Fatalf("Prepare() error = %v", err)
			}
			err = p.Check()
			if tc.wantErr == "" && err != nil {
				t.Fatalf("Prepared.Check() error = %v, want nil", err)
			}
			if tc.wantErr != "" && (err == nil || err.Error() != tc.wantErr) {
				t.Fatalf("Prepared.Check() error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestPrepareModel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command []string
		model   string
		want    []string
	}{
		{name: "opencode", model: "openrouter/a/b", want: []string{
			`OPENCODE_CONFIG_CONTENT={"experimental":{"continue_loop_on_deny":true},"model":"openrouter/a/b"}`,
		}},
		{name: "opencode", want: []string{
			`OPENCODE_CONFIG_CONTENT={"experimental":{"continue_loop_on_deny":true}}`,
		}},
		{name: "claude", model: "sonnet", want: []string{"ANTHROPIC_MODEL=sonnet"}},
		{name: "claude", command: []string{"my-claude"}, model: "opus", want: []string{"ANTHROPIC_MODEL=opus"}},
		{name: "claude"},
		{name: "gemini", command: []string{"gemini"}, model: "m"},
	}
	for _, tc := range tests {
		t.Run(tc.name+strings.Join(tc.command, "_")+tc.model, func(t *testing.T) {
			t.Parallel()
			ws := t.TempDir()
			profile, err := Lookup(tc.name, tc.command)
			if err != nil {
				t.Fatalf("Lookup() error = %v", err)
			}
			p, err := Prepare(context.Background(), Spec{
				Profile: profile,
				Sandbox: SandboxLocal,
				Mounts:  []agentdir.Mount{{Src: ws, Dst: ws}},
				Home:    t.TempDir(),
				EnvDir:  t.TempDir(),
				Model:   tc.model,
			})
			if err != nil {
				t.Fatalf("Prepare() error = %v", err)
			}
			var got []string
			for _, kv := range p.Env {
				if strings.HasPrefix(kv, "ANTHROPIC_MODEL=") || strings.Contains(kv, "_CONFIG_CONTENT=") {
					got = append(got, kv)
				}
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("Prepare() model env wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

// runningDocker runs rootful, has every image and removes any container.
const runningDocker = `case "$1" in
info) echo "[name=seccomp,profile=builtin]" ;;
image|rm) ;;
*) echo "fake docker: $1 failed" >&2; exit 1 ;;
esac
`

func TestPrepareDocker(t *testing.T) {
	fakeDocker(t, runningDocker)
	ws, home, sockDir := t.TempDir(), t.TempDir(), t.TempDir()
	p, err := Prepare(context.Background(), Spec{
		Profile: claude{},
		Mounts:  []agentdir.Mount{{Src: ws, Dst: ws}},
		Home:    home,
		EnvDir:  t.TempDir(),
		Name:    "a2a-test",
		Socket:  filepath.Join(sockDir, "a2a.sock"),
		CLI:     "/opt/a2a/bin/a2a",
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	t.Cleanup(func() {
		if err := p.Stop(context.Background()); err != nil {
			t.Errorf("Stop() error = %v", err)
		}
	})
	var mounts []string
	var envFile string
	for i := 0; i+1 < len(p.Args); i++ {
		switch p.Args[i] {
		case "-v":
			mounts = append(mounts, p.Args[i+1])
		case "--env-file":
			envFile = p.Args[i+1]
		}
	}
	wantMounts := []string{ws + ":" + ws, sockDir + ":" + SandboxDir + ":ro", home + ":" + ContainerHome}
	if diff := cmp.Diff(wantMounts, mounts); diff != "" {
		t.Fatalf("Prepare() mounts wrong result (-want +got) diff = %s", diff)
	}
	env, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("os.ReadFile() error = %v", err)
	}
	if want := EnvUnixSocket + "=" + SandboxDir + "/a2a.sock"; !slices.Contains(strings.Split(string(env), "\n"), want) {
		t.Fatalf("Prepare() env = %q, want it to contain %q", env, want)
	}
}

// noRelayImageDocker has the agent's image but not the egress container's,
// which it can't pull.
const noRelayImageDocker = `case "$1 $2" in
"info "*) echo "[name=seccomp,profile=builtin]" ;;
"image inspect") case "$*" in *example.com/egress*) exit 1 ;; esac; echo "amd64 null" ;;
"rm "*) ;;
*) echo "fake docker: $1 failed" >&2; exit 1 ;;
esac
`

func TestPrepareDockerRelayGetsMissingImage(t *testing.T) {
	log := fakeDocker(t, noRelayImageDocker)
	t.Setenv(EnvForceRelay, "1")
	t.Setenv(EnvRelayImage, "example.com/egress:test")
	ws, home, sockDir := t.TempDir(), t.TempDir(), t.TempDir()
	p, err := Prepare(context.Background(), Spec{
		Profile: claude{},
		Mounts:  []agentdir.Mount{{Src: ws, Dst: ws}},
		Home:    home,
		EnvDir:  t.TempDir(),
		Name:    "a2a-test",
		Socket:  filepath.Join(sockDir, "a2a.sock"),
		CLI:     "/opt/a2a/bin/a2a",
		OpenEgress: func(HostGatewayFunc, bool) (EgressEndpoint, error) {
			return EgressEndpoint{}, errors.New("unexpected boundary")
		},
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	t.Cleanup(func() {
		if err := p.Stop(context.Background()); err != nil {
			t.Errorf("Stop() error = %v", err)
		}
	})
	calls, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("os.ReadFile() error = %v", err)
	}
	if want := "pull example.com/egress:test"; !strings.Contains(string(calls), want) {
		t.Fatalf("Prepare() docker calls = %q, want them to contain %q", calls, want)
	}
	if want := "the egress container's image isn't available"; !strings.Contains(p.EgressProblem, want) {
		t.Fatalf("Prepare().EgressProblem = %q, want it to contain %q", p.EgressProblem, want)
	}
}

func TestPrepareCLITimeout(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		env  []string
		want string
	}{
		{name: "default", want: EnvTimeout + "=5m0s"},
		{name: "configured", env: []string{EnvTimeout + "=1h"}, want: EnvTimeout + "=1h"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ws := t.TempDir()
			p, err := Prepare(context.Background(), Spec{
				Profile: claude{},
				Sandbox: SandboxLocal,
				Mounts:  []agentdir.Mount{{Src: ws, Dst: ws}},
				Home:    t.TempDir(),
				EnvDir:  t.TempDir(),
				Env:     tc.env,
			})
			if err != nil {
				t.Fatalf("Prepare() error = %v", err)
			}
			var got []string
			for _, kv := range p.Env {
				if strings.HasPrefix(kv, EnvTimeout+"=") {
					got = append(got, kv)
				}
			}
			if diff := cmp.Diff([]string{tc.want}, got); diff != "" {
				t.Fatalf("Prepare() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestPrepareRejectsInvalidSpecs(t *testing.T) {
	t.Parallel()
	ws := t.TempDir()
	tests := []struct {
		name    string
		spec    Spec
		wantErr string
	}{
		{name: "no workspace", spec: Spec{Sandbox: SandboxLocal}, wantErr: "no workspace"},
		{name: "moved workspace", spec: Spec{Sandbox: SandboxLocal, Mounts: []agentdir.Mount{{Src: ws, Dst: "/work"}}}, wantErr: "must keep its path"},
		{name: "unknown sandbox", spec: Spec{Sandbox: "vm", Mounts: []agentdir.Mount{{Src: ws, Dst: ws}}}, wantErr: "unknown sandbox"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.spec.Profile = claude{}
			tc.spec.Home = t.TempDir()
			if _, err := Prepare(context.Background(), tc.spec); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Prepare() error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func tmpfsMounts(args []string) []string {
	var out []string
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--mount" {
			out = append(out, args[i+1])
		}
	}
	return out
}

func TestPrepareDockerHidesNestedAgents(t *testing.T) {
	fakeDocker(t, runningDocker)
	ws, docs := t.TempDir(), t.TempDir()
	own := mkdir(t, filepath.Join(ws, agentdir.Name))
	mkdir(t, filepath.Join(ws, "sub", "deeper", agentdir.Name, "state"))
	mkdir(t, filepath.Join(docs, "x", agentdir.Name))

	p, err := startDocker(t, Spec{
		Mounts: []agentdir.Mount{{Src: ws, Dst: ws}, {Src: docs, Dst: "/docs", ReadOnly: true}},
		Hidden: []string{own},
		Home:   t.TempDir(),
		EnvDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}

	want := []string{
		"type=tmpfs,dst=" + own + ",readonly",
		"type=tmpfs,dst=" + filepath.Join(ws, "sub", "deeper", agentdir.Name) + ",readonly",
		"type=tmpfs,dst=/docs/x/" + agentdir.Name + ",readonly",
	}
	if diff := cmp.Diff(want, tmpfsMounts(p.Args)); diff != "" {
		t.Fatalf("Prepare() hidden mounts wrong result (-want +got) diff = %s", diff)
	}
	wantWarnings := []string{"found other agents in " + filepath.Join(ws, "sub", "deeper") + ", " + filepath.Join(docs, "x") + "; the sandbox hides their .a2a directories"}
	if diff := cmp.Diff(wantWarnings, p.Warnings); diff != "" {
		t.Fatalf("Prepare() warnings wrong result (-want +got) diff = %s", diff)
	}
	if err := p.Check(); err != nil {
		t.Fatalf("Prepared.Check() error = %v, want nil", err)
	}
	mkdir(t, filepath.Join(ws, "later", agentdir.Name))
	if err := p.Check(); err == nil || !strings.Contains(err.Error(), "a2a-serve restart") {
		t.Fatalf("Prepared.Check() error = %v, want a restart for the agent created later", err)
	}
}

func TestPrepareDockerSkipsAllowedBroadMount(t *testing.T) {
	fakeDocker(t, runningDocker)
	ws := t.TempDir()
	own := mkdir(t, filepath.Join(ws, agentdir.Name))

	p, err := startDocker(t, Spec{
		Mounts: []agentdir.Mount{{Src: ws, Dst: ws}, {Src: "/", Dst: "/host", ReadOnly: true}},
		Hidden: []string{own},
		Home:   t.TempDir(),
		EnvDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}

	want := []string{"type=tmpfs,dst=" + own + ",readonly", "type=tmpfs,dst=" + filepath.Join("/host", own) + ",readonly"}
	if diff := cmp.Diff(want, tmpfsMounts(p.Args)); diff != "" {
		t.Fatalf("Prepare() hidden mounts wrong result (-want +got) diff = %s", diff)
	}
}

func TestPrepareLocalWarnsAboutNestedAgents(t *testing.T) {
	t.Parallel()
	ws := t.TempDir()
	mkdir(t, filepath.Join(ws, "sub", agentdir.Name))

	p, err := Prepare(context.Background(), Spec{
		Profile: claude{},
		Sandbox: SandboxLocal,
		Mounts:  []agentdir.Mount{{Src: ws, Dst: ws}},
		Home:    t.TempDir(),
		EnvDir:  t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}

	want := []string{"found other agents in " + filepath.Join(ws, "sub") + "; --sandbox local doesn't hide their .a2a directories, so the agent can read their secrets"}
	if diff := cmp.Diff(want, p.Warnings); diff != "" {
		t.Fatalf("Prepare() warnings wrong result (-want +got) diff = %s", diff)
	}
}

func TestPrepareDockerMasksMCP(t *testing.T) {
	fakeDocker(t, runningDocker)
	tests := []struct {
		name     string
		existing string
		want     string
	}{
		{name: "existing .mcp.json", existing: `{"mcpServers": {"github": {"command": "gh-mcp"}}}`, want: `{"mcpServers": {"github": {"command": "gh-mcp"}}}`},
		{name: "no .mcp.json", want: emptyMCP},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ws, home, state := t.TempDir(), t.TempDir(), t.TempDir()
			mcpFile := filepath.Join(ws, project.MCPFile)
			if tc.existing != "" {
				if err := os.WriteFile(mcpFile, []byte(tc.existing), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			mask := filepath.Join(state, "mcp-mask.json")
			p, err := Prepare(context.Background(), Spec{
				Profile: claude{},
				Mounts:  []agentdir.Mount{{Src: ws, Dst: ws}, {Src: ws, Dst: "/copy", ReadOnly: true}},
				Home:    home,
				EnvDir:  t.TempDir(),
				Name:    "a2a-test",
				MCPMask: mask,
			})
			if err != nil {
				t.Fatalf("Prepare() error = %v", err)
			}
			t.Cleanup(func() {
				if err := p.Stop(context.Background()); err != nil {
					t.Errorf("Stop() error = %v", err)
				}
			})
			var mounts []string
			for i := 0; i+1 < len(p.Args); i++ {
				if p.Args[i] == "-v" {
					mounts = append(mounts, p.Args[i+1])
				}
			}
			wantMounts := []string{ws + ":" + ws, ws + ":/copy:ro", mask + ":" + mcpFile + ":ro", mask + ":/copy/" + project.MCPFile + ":ro", home + ":" + ContainerHome}
			if diff := cmp.Diff(wantMounts, mounts); diff != "" {
				t.Fatalf("Prepare() mounts wrong result (-want +got) diff = %s", diff)
			}
			got, err := os.ReadFile(mcpFile)
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.want, string(got)); diff != "" {
				t.Fatalf("Prepare() changed %s (-want +got) diff = %s", project.MCPFile, diff)
			}
			gotMask, err := os.ReadFile(mask)
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(emptyMCP, string(gotMask)); diff != "" {
				t.Fatalf("Prepare() wrong mask (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestPrepareDockerMasksAgentConfig(t *testing.T) {
	fakeDocker(t, runningDocker)
	tests := []struct {
		name    string
		profile Profile
		copy    bool
		// want returns the mounts, tmpfs mounts and files the workspace
		// gets, of the workspace ws, the read-only mount docs and the
		// private copy of the workspace.
		want func(ws, docs, copyDir, mask string) (mounts, tmpfs, created []string)
		// wantWarnings are the files of ws named in warnings.
		wantWarnings []string
	}{
		{
			name:    "opencode",
			profile: opencode{},
			want: func(ws, docs, _, mask string) ([]string, []string, []string) {
				return []string{
						ws + ":" + ws,
						docs + ":/docs:ro",
						mask + ":" + filepath.Join(ws, "opencode.json") + ":ro",
						mask + ":" + filepath.Join(ws, "opencode.jsonc") + ":ro",
						mask + ":/docs/opencode.json:ro",
						filepath.Join(ws, ".opencode", "skills") + ":" + filepath.Join(ws, ".opencode", "skills"),
					},
					[]string{"type=tmpfs,dst=" + filepath.Join(ws, ".opencode")},
					[]string{filepath.Join(ws, "opencode.jsonc")}
			},
			wantWarnings: []string{"opencode.json"},
		},
		{
			name:    "claude",
			profile: claude{},
			want: func(ws, docs, _, mask string) ([]string, []string, []string) {
				return []string{
						ws + ":" + ws,
						docs + ":/docs:ro",
						mask + ":" + filepath.Join(ws, ".claude", "settings.json") + ":ro",
						mask + ":" + filepath.Join(ws, ".claude", "settings.local.json") + ":ro",
						mask + ":/docs/.claude/settings.json:ro",
						filepath.Join(ws, ".claude") + ":" + filepath.Join(ws, ".claude"),
						filepath.Join(docs, ".claude") + ":/docs/.claude:ro",
					},
					nil,
					[]string{filepath.Join(ws, ".claude", "settings.json"), filepath.Join(ws, ".claude", "settings.local.json")}
			},
		},
		{
			name:    "claude with a private copy of the workspace",
			profile: claude{},
			copy:    true,
			want: func(ws, docs, copyDir, mask string) ([]string, []string, []string) {
				return []string{
						copyDir + ":" + ws,
						docs + ":/docs:ro",
						mask + ":" + filepath.Join(ws, ".claude", "settings.json") + ":ro",
						mask + ":" + filepath.Join(ws, ".claude", "settings.local.json") + ":ro",
						mask + ":/docs/.claude/settings.json:ro",
						filepath.Join(copyDir, ".claude") + ":" + filepath.Join(ws, ".claude"),
						filepath.Join(docs, ".claude") + ":/docs/.claude:ro",
					},
					nil,
					[]string{filepath.Join(copyDir, ".claude", "settings.json")}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ws, docs, state := t.TempDir(), t.TempDir(), t.TempDir()
			hidden := mkdir(t, filepath.Join(ws, agentdir.Name))
			copyDir := mkdir(t, filepath.Join(hidden, "state", "workspaces", "ben"))
			mask := filepath.Join(state, "config-mask.json")
			writeTestFile(t, filepath.Join(ws, "opencode.json"), `{"permission": {"bash": "allow"}}`)
			mkdir(t, filepath.Join(ws, ".opencode", "skills", "triage"))
			writeTestFile(t, filepath.Join(docs, ".claude", "settings.json"), `{"permissions": {"allow": ["Bash"]}}`)
			writeTestFile(t, filepath.Join(docs, "opencode.json"), `{}`)
			spec := Spec{
				Profile:    tc.profile,
				Name:       "a2a-test",
				Mounts:     []agentdir.Mount{{Src: ws, Dst: ws}, {Src: docs, Dst: "/docs", ReadOnly: true}},
				Hidden:     []string{hidden},
				Home:       t.TempDir(),
				EnvDir:     t.TempDir(),
				ConfigMask: mask,
			}
			if tc.copy {
				spec.WorkspaceSrc = copyDir
			}

			p, err := Prepare(context.Background(), spec)
			if err != nil {
				t.Fatalf("Prepare() error = %v", err)
			}
			t.Cleanup(func() {
				if err := p.Stop(context.Background()); err != nil {
					t.Errorf("Stop() error = %v", err)
				}
			})

			wantMounts, wantTmpfs, wantCreated := tc.want(ws, docs, copyDir, mask)
			var mounts []string
			for _, m := range bindMounts(p.Args) {
				if !strings.HasSuffix(m, ":"+ContainerHome) {
					mounts = append(mounts, m)
				}
			}
			if diff := cmp.Diff(wantMounts, mounts); diff != "" {
				t.Fatalf("Prepare() mounts wrong result (-want +got) diff = %s", diff)
			}
			wantTmpfs = append([]string{"type=tmpfs,dst=" + hidden + ",readonly"}, wantTmpfs...)
			if diff := cmp.Diff(wantTmpfs, tmpfsMounts(p.Args)); diff != "" {
				t.Fatalf("Prepare() tmpfs mounts wrong result (-want +got) diff = %s", diff)
			}
			for _, file := range wantCreated {
				if got, err := os.ReadFile(file); err != nil || string(got) != emptyConfig {
					t.Fatalf("Prepare() created %s = %q, %v, want %q", file, got, err, emptyConfig)
				}
			}
			var warned []string
			for _, w := range p.Warnings {
				for _, name := range []string{"opencode.json", filepath.Join(".claude", "settings.json")} {
					if strings.Contains(w, filepath.Join(ws, name)+" ") {
						warned = append(warned, name)
					}
				}
			}
			if diff := cmp.Diff(tc.wantWarnings, warned); diff != "" {
				t.Fatalf("Prepare() warnings wrong result (-want +got) diff = %s, warnings %q", diff, p.Warnings)
			}
		})
	}
}

func TestPrepareDockerMasksAgentConfigAboveTheWorkspace(t *testing.T) {
	fakeDocker(t, runningDocker)
	repo, state := t.TempDir(), t.TempDir()
	ws := mkdir(t, filepath.Join(repo, "services", "app"))
	mask := filepath.Join(state, "config-mask.json")
	writeTestFile(t, filepath.Join(repo, "services", "opencode.json"), `{"permission": {"bash": "allow"}}`)

	p, err := Prepare(context.Background(), Spec{
		Profile:    opencode{},
		Name:       "a2a-test",
		Mounts:     []agentdir.Mount{{Src: ws, Dst: ws}, {Src: repo, Dst: repo}},
		Home:       t.TempDir(),
		EnvDir:     t.TempDir(),
		ConfigMask: mask,
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	t.Cleanup(func() {
		if err := p.Stop(context.Background()); err != nil {
			t.Errorf("Stop() error = %v", err)
		}
	})

	if want := mask + ":" + filepath.Join(repo, "services", "opencode.json") + ":ro"; !slices.Contains(bindMounts(p.Args), want) {
		t.Fatalf("Prepare() mounts = %q, want %q", bindMounts(p.Args), want)
	}
	if err := p.Check(); err != nil {
		t.Fatalf("Prepared.Check() error = %v, want nil", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "opencode.json")); !os.IsNotExist(err) {
		t.Fatalf("Prepare() created opencode.json above the workspace: %v", err)
	}
	writeTestFile(t, filepath.Join(repo, "opencode.json"), `{"permission": {"bash": "allow"}}`)
	if err := p.Check(); err == nil || !strings.Contains(err.Error(), "a2a-serve restart") {
		t.Fatalf("Prepared.Check() error = %v, want a restart for the opencode.json created later", err)
	}
}

func TestPrepareDockerAgentConfigSymlinksInMounts(t *testing.T) {
	fakeDocker(t, runningDocker)
	repo, dotfiles := t.TempDir(), t.TempDir()
	ws := mkdir(t, filepath.Join(repo, "app"))
	mkdir(t, filepath.Join(dotfiles, "claude"))
	symlink(t, "claude", filepath.Join(dotfiles, ".claude"))
	spec := Spec{
		Profile:    claude{},
		Name:       "a2a-test",
		Mounts:     []agentdir.Mount{{Src: ws, Dst: ws}, {Src: dotfiles, Dst: "/dotfiles"}},
		Home:       t.TempDir(),
		EnvDir:     t.TempDir(),
		ConfigMask: filepath.Join(t.TempDir(), "config-mask.json"),
	}

	p, err := Prepare(context.Background(), spec)
	if err != nil {
		t.Fatalf("Prepare() with a symlinked .claude in a mount error = %v, want nil", err)
	}
	t.Cleanup(func() {
		if err := p.Stop(context.Background()); err != nil {
			t.Errorf("Stop() error = %v", err)
		}
	})
	writeTestFile(t, filepath.Join(dotfiles, "claude", "settings.json"), `{}`)
	if err := p.Check(); err != nil {
		t.Fatalf("Prepared.Check() after a file appeared in a mount error = %v, want nil", err)
	}

	symlink(t, filepath.Join(dotfiles, ".claude"), filepath.Join(repo, ".claude"))
	symlink(t, filepath.Join(dotfiles, "claude"), filepath.Join(repo, ".opencode"))
	spec.Mounts = append(spec.Mounts, agentdir.Mount{Src: repo, Dst: repo})
	above, err := Prepare(context.Background(), spec)
	if err != nil {
		t.Fatalf("Prepare() for Claude Code with a symlinked .claude above the workspace error = %v, want nil", err)
	}
	t.Cleanup(func() {
		if err := above.Stop(context.Background()); err != nil {
			t.Errorf("Stop() error = %v", err)
		}
	})
	spec.Profile = opencode{}
	_, err = Prepare(context.Background(), spec)
	if want := filepath.Join(repo, ".opencode") + " is a symlink, in a folder above the workspace"; err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Prepare() for opencode with a symlinked .opencode above the workspace error = %v, want it to contain %q", err, want)
	}
}

func TestPrepareDockerRefusesAgentConfigSymlinks(t *testing.T) {
	fakeDocker(t, runningDocker)
	tests := []struct {
		name    string
		profile Profile
		setup   func(t *testing.T, ws, outside string)
		want    string
	}{
		{
			name:    "file symlink out of the workspace",
			profile: opencode{},
			setup: func(t *testing.T, ws, outside string) {
				symlink(t, filepath.Join(outside, "x.json"), filepath.Join(ws, "opencode.json"))
			},
			want: "opencode.json is a symlink; the agent could use it to change its own permissions, so remove it",
		},
		{
			name:    "folder symlink in the workspace",
			profile: claude{},
			setup: func(t *testing.T, ws, _ string) {
				mkdir(t, filepath.Join(ws, "cfg"))
				symlink(t, "cfg", filepath.Join(ws, ".claude"))
			},
			want: ".claude is a symlink",
		},
		{
			name:    "folder in place of a file",
			profile: opencode{},
			setup:   func(t *testing.T, ws, _ string) { mkdir(t, filepath.Join(ws, "opencode.jsonc")) },
			want:    "opencode.jsonc isn't a file",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ws, outside := t.TempDir(), t.TempDir()
			tc.setup(t, ws, outside)

			_, err := Prepare(context.Background(), Spec{
				Profile:    tc.profile,
				Name:       "a2a-test",
				Mounts:     []agentdir.Mount{{Src: ws, Dst: ws}},
				Home:       t.TempDir(),
				EnvDir:     t.TempDir(),
				ConfigMask: filepath.Join(t.TempDir(), "config-mask.json"),
			})

			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Prepare() error = %v, want it to contain %q", err, tc.want)
			}
			entries, err := os.ReadDir(outside)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) > 0 {
				t.Fatalf("Prepare() wrote %s out of the workspace", entries[0].Name())
			}
		})
	}
}

func TestPrepareDockerLocksHomeConfig(t *testing.T) {
	fakeDocker(t, runningDocker)
	claudeWant := func(home string) []string {
		return []string{
			filepath.Join(home, ".claude") + ":" + filepath.Join(ContainerHome, ".claude"),
			filepath.Join(home, ".claude", "settings.json") + ":" + filepath.Join(ContainerHome, ".claude", "settings.json") + ":ro",
			filepath.Join(home, ".claude", "agents") + ":" + filepath.Join(ContainerHome, ".claude", "agents") + ":ro",
			filepath.Join(home, ".claude", "commands") + ":" + filepath.Join(ContainerHome, ".claude", "commands") + ":ro",
			filepath.Join(home, ".claude", "skills") + ":" + filepath.Join(ContainerHome, ".claude", "skills") + ":ro",
		}
	}
	opencodeWant := func(home string) []string {
		return []string{
			filepath.Join(home, ".config") + ":" + filepath.Join(ContainerHome, ".config"),
			filepath.Join(home, ".config", "opencode") + ":" + filepath.Join(ContainerHome, ".config", "opencode") + ":ro",
			filepath.Join(home, ".opencode") + ":" + filepath.Join(ContainerHome, ".opencode") + ":ro",
			filepath.Join(home, ".local") + ":" + filepath.Join(ContainerHome, ".local"),
			filepath.Join(home, ".local", "share") + ":" + filepath.Join(ContainerHome, ".local", "share"),
			filepath.Join(home, ".local", "share", "opencode") + ":" + filepath.Join(ContainerHome, ".local", "share", "opencode"),
			filepath.Join(home, ".local", "share", "opencode", "auth.json") + ":" + filepath.Join(ContainerHome, ".local", "share", "opencode", "auth.json") + ":ro",
		}
	}
	tests := []struct {
		name    string
		profile Profile
		lock    bool
		want    func(home string) []string
		// wantCreated are the files the home gets, relative to it.
		wantCreated []string
	}{
		{
			name:    "opencode",
			profile: opencode{},
			lock:    true,
			want:    opencodeWant,
			wantCreated: []string{
				filepath.Join(".config", "opencode", ".gitignore"),
				filepath.Join(".opencode", ".gitignore"),
				filepath.Join(".local", "share", "opencode", "auth.json"),
			},
		},
		{
			name:        "claude",
			profile:     claude{},
			lock:        true,
			want:        claudeWant,
			wantCreated: []string{filepath.Join(".claude", "agents"), filepath.Join(".claude", "commands"), filepath.Join(".claude", "skills")},
		},
		{
			name:    "agent of unknown kind",
			profile: custom{name: "acp", argv: []string{"fakeacp"}},
			lock:    true,
			want: func(home string) []string {
				return append(claudeWant(home), opencodeWant(home)...)
			},
			wantCreated: []string{filepath.Join(".claude", "settings.json"), filepath.Join(".opencode", ".gitignore")},
		},
		{
			name:    "owner's home",
			profile: claude{},
			want:    func(string) []string { return nil },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ws, home := t.TempDir(), t.TempDir()
			spec := Spec{
				Profile:  tc.profile,
				Name:     "a2a-test",
				Mounts:   []agentdir.Mount{{Src: ws, Dst: ws}},
				Home:     home,
				EnvDir:   t.TempDir(),
				Image:    "img:1",
				LockHome: tc.lock,
			}

			p, err := Prepare(context.Background(), spec)
			if err != nil {
				t.Fatalf("Prepare() error = %v", err)
			}
			t.Cleanup(func() {
				if err := p.Stop(context.Background()); err != nil {
					t.Errorf("Stop() error = %v", err)
				}
			})

			var mounts []string
			for _, m := range bindMounts(p.Args) {
				if m != ws+":"+ws && m != home+":"+ContainerHome {
					mounts = append(mounts, m)
				}
			}
			if diff := cmp.Diff(tc.want(home), mounts); diff != "" {
				t.Fatalf("Prepare() mounts wrong result (-want +got) diff = %s", diff)
			}
			for _, name := range tc.wantCreated {
				if _, err := os.Lstat(filepath.Join(home, name)); err != nil {
					t.Fatalf("Prepare() didn't create %s in the home: %v", name, err)
				}
			}
		})
	}
}

func TestPrepareDockerResetsLockedHomePermissions(t *testing.T) {
	fakeDocker(t, runningDocker)
	tests := []struct {
		name    string
		profile Profile
		file    string
		content string
		key     string
		want    string
		kept    string
	}{
		{
			name:    "opencode",
			profile: opencode{},
			file:    filepath.Join(".config", "opencode", "opencode.json"),
			content: `{"permission": {"*": "ask", "bash": "allow"}, "provider": {"stub": {}}}`,
			key:     "permission",
			want:    opencodePermission,
			kept:    "provider",
		},
		{
			name:    "opencode with comments",
			profile: opencode{},
			file:    filepath.Join(".config", "opencode", "opencode.json"),
			content: "{\n  // the stub model\n  \"provider\": {\"stub\": {}},\n  \"permission\": {\"bash\": \"allow\"},\n}",
			key:     "permission",
			want:    opencodePermission,
			kept:    "provider",
		},
		{
			name:    "claude",
			profile: claude{},
			file:    filepath.Join(".claude", "settings.json"),
			content: `{"permissions": {"defaultMode": "acceptEdits", "allow": ["Bash"]}, "model": "opus"}`,
			key:     "permissions",
			want:    `{"defaultMode": "default"}`,
			kept:    "model",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ws, home := t.TempDir(), t.TempDir()
			writeTestFile(t, filepath.Join(home, tc.file), tc.content)

			p, err := Prepare(context.Background(), Spec{
				Profile:  tc.profile,
				Name:     "a2a-test",
				Mounts:   []agentdir.Mount{{Src: ws, Dst: ws}},
				Home:     home,
				EnvDir:   t.TempDir(),
				LockHome: true,
			})
			if err != nil {
				t.Fatalf("Prepare() error = %v", err)
			}
			t.Cleanup(func() {
				if err := p.Stop(context.Background()); err != nil {
					t.Errorf("Stop() error = %v", err)
				}
			})

			data, err := os.ReadFile(filepath.Join(home, tc.file))
			if err != nil {
				t.Fatal(err)
			}
			var got, want map[string]any
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("json.Unmarshal(%s) error = %v", tc.file, err)
			}
			if err := json.Unmarshal([]byte(tc.want), &want); err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(want, got[tc.key]); diff != "" {
				t.Fatalf("Prepare() %s of %s wrong result (-want +got) diff = %s", tc.key, tc.file, diff)
			}
			if _, ok := got[tc.kept]; !ok {
				t.Fatalf("Prepare() left %s = %s, want %s kept", tc.file, data, tc.kept)
			}
		})
	}
}

func TestPrepareDockerRefusesHomeConfigSymlinks(t *testing.T) {
	fakeDocker(t, runningDocker)
	ws, home := t.TempDir(), t.TempDir()
	mkdir(t, filepath.Join(home, "agents"))
	mkdir(t, filepath.Join(home, ".claude"))
	symlink(t, filepath.Join("..", "agents"), filepath.Join(home, ".claude", "agents"))

	_, err := Prepare(context.Background(), Spec{
		Profile:  claude{},
		Name:     "a2a-test",
		Mounts:   []agentdir.Mount{{Src: ws, Dst: ws}},
		Home:     home,
		EnvDir:   t.TempDir(),
		LockHome: true,
	})

	if want := filepath.Join(home, ".claude", "agents") + " is a symlink"; err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Prepare() error = %v, want it to contain %q", err, want)
	}
}

func bindMounts(args []string) []string {
	var out []string
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-v" {
			out = append(out, args[i+1])
		}
	}
	return out
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	mkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestWithoutEgressCA(t *testing.T) {
	t.Parallel()
	got := withoutEgressCA([]string{"A=1", sandboxnet.EnvEgressCA + "=from-a-recipe", "B=2"})
	if diff := cmp.Diff([]string{"A=1", "B=2"}, got); diff != "" {
		t.Fatalf("withoutEgressCA() wrong result (-want +got) diff = %s", diff)
	}
}

// relayDocker runs the egress container, and runs the check with check.
func relayDocker(check string) string {
	return `case "$1 $2" in
"info "*) echo "[name=seccomp,profile=builtin]" ;;
"image inspect") echo "amd64 null" ;;
"rm "*|"run -d") ;;
"logs "*) echo "` + sandboxnet.RelayReady + `" ;;
"inspect "*) echo "true 0 0" ;;
"run --name") ` + check + ` ;;
*) echo "fake docker: $1 failed" >&2; exit 1 ;;
esac
`
}

func TestPrepareDockerEgressCheck(t *testing.T) {
	tests := []struct {
		name        string
		check       string
		wantEgress  bool
		wantProblem string
		wantErr     string
	}{
		{name: "check passes", check: "exit 0", wantEgress: true},
		{name: "launcher can't set up the network", check: launcherNoTUN, wantProblem: "egress control is off: the agent's container can't set up its network: no tun"},
		{name: "daemon error", check: daemonError, wantErr: "failed to create task for container: EOF"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fakeDocker(t, relayDocker(tc.check))
			t.Setenv(EnvForceRelay, "1")
			t.Setenv(EnvRelayImage, "example.com/egress:test")
			ws, home, sockDir := t.TempDir(), t.TempDir(), t.TempDir()
			var closed atomic.Bool
			p, err := Prepare(context.Background(), Spec{
				Profile: claude{},
				Mounts:  []agentdir.Mount{{Src: ws, Dst: ws}},
				Home:    home,
				EnvDir:  t.TempDir(),
				Name:    "a2a-test",
				Socket:  filepath.Join(sockDir, "a2a.sock"),
				CLI:     "/opt/a2a/bin/a2a",
				OpenEgress: func(HostGatewayFunc, bool) (EgressEndpoint, error) {
					return EgressEndpoint{RelayAddr: "tcp://127.0.0.1:1", RelayKey: "key", Close: func() error {
						closed.Store(true)
						return nil
					}}, nil
				},
			})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Prepare() error = %v, want %q", err, tc.wantErr)
				}
				if !closed.Load() {
					t.Fatalf("Prepare() error = %v left the egress boundary open", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Prepare() error = %v", err)
			}
			t.Cleanup(func() {
				if err := p.Stop(context.Background()); err != nil {
					t.Errorf("Stop() error = %v", err)
				}
			})
			if p.Egress != tc.wantEgress || !strings.HasPrefix(p.EgressProblem, tc.wantProblem) || (tc.wantProblem == "") != (p.EgressProblem == "") {
				t.Fatalf("Prepare() = egress %v, problem %q, want egress %v, problem %q", p.Egress, p.EgressProblem, tc.wantEgress, tc.wantProblem)
			}
		})
	}
}
