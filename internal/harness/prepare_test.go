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
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
			`CLOUDCODE_CONFIG_CONTENT={"experimental":{"continue_loop_on_deny":true},"model":"openrouter/a/b"}`,
		}},
		{name: "opencode", want: []string{
			`OPENCODE_CONFIG_CONTENT={"experimental":{"continue_loop_on_deny":true}}`,
			`CLOUDCODE_CONFIG_CONTENT={"experimental":{"continue_loop_on_deny":true}}`,
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

func TestWithoutEgressCA(t *testing.T) {
	t.Parallel()
	got := withoutEgressCA([]string{"A=1", sandboxnet.EnvEgressCA + "=from-a-recipe", "B=2"})
	if diff := cmp.Diff([]string{"A=1", "B=2"}, got); diff != "" {
		t.Fatalf("withoutEgressCA() wrong result (-want +got) diff = %s", diff)
	}
}
