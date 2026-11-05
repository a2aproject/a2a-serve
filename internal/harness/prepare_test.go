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
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/project"
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
