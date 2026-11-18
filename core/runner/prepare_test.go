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

package runner

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/core/agent"
	"github.com/a2aproject/a2a-serve/internal/projectfiles"
)

// fakeProcesses describes the processes of fixed keys.
type fakeProcesses map[string]Process

func (p fakeProcesses) Key(context.Context, string) (string, error) { return "", nil }

func (p fakeProcesses) Process(_ context.Context, key string) (Process, error) {
	proc, ok := p[key]
	if !ok {
		return Process{}, errors.New("unknown key " + key)
	}
	return proc, nil
}

// privateDir returns a temporary directory only its user may enter, as the
// socket directory needs.
func privateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func newTestRunner(t *testing.T, cfg Config) *Runner {
	t.Helper()
	if cfg.Dirs.Sockets == "" {
		cfg.Dirs.Sockets = filepath.Join(privateDir(t), "s")
	}
	if cfg.Secrets == nil {
		cfg.Secrets = func() map[string]string { return nil }
	}
	r, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := r.Release(context.Background()); err != nil {
			t.Errorf("Runner.Release() error = %v", err)
		}
	})
	return r
}

func TestPrepareRefusesReplacedAgentDirectoryBeforeWritingHome(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		key  string
		pool string
	}{
		{name: "main", key: ""},
		{name: "pool", key: "guest", pool: "guests"},
		{name: "child", key: "user:anna", pool: "users"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := filepath.Join(t.TempDir(), "proj")
			agentDir := filepath.Join(root, projectfiles.AgentDir)
			state := filepath.Join(agentDir, "state")
			home := filepath.Join(state, "home")
			if err := os.MkdirAll(home, 0o700); err != nil {
				t.Fatal(err)
			}
			pins, err := agent.NewPins(root, agentDir, state, home)
			if err != nil {
				t.Fatalf("agent.NewPins() error = %v", err)
			}
			if err := os.Rename(root, root+".old"); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(home, 0o700); err != nil {
				t.Fatal(err)
			}
			procHome := home
			if tc.key != "" {
				procHome = filepath.Join(state, "users", tc.name)
			}
			r := newTestRunner(t, Config{
				Dirs: Dirs{
					Policies:   filepath.Join(state, "policy"),
					Workspaces: filepath.Join(state, "workspaces"),
					MCPMask:    filepath.Join(state, "mcp-mask.json"),
					ConfigMask: filepath.Join(state, "config-mask.json"),
					Hidden:     []string{agentDir},
					Pins:       pins,
				},
				Spec: func() (Spec, error) {
					return Spec{Name: "a2a-coder", Agent: "fake", Command: []string{"fake-acp"}, Sandbox: agent.SandboxDocker, Mounts: []agent.Mount{{Src: root, Dst: root}}}, nil
				},
				Processes: fakeProcesses{tc.key: {Pool: tc.pool, Name: tc.name, CopyName: tc.name, Home: procHome, LockHome: tc.key != "", AgentAPI: true}},
			})

			_, err = r.prepare(context.Background(), io.Discard, io.Discard, tc.key)

			if err == nil || !strings.Contains(err.Error(), "was replaced") {
				t.Fatalf("Runner.prepare() error = %v, want the agent directory replaced", err)
			}
			entries, err := os.ReadDir(state)
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, e := range entries {
				names = append(names, e.Name())
			}
			if diff := cmp.Diff([]string{"home"}, names); diff != "" {
				t.Fatalf("Runner.prepare() wrote into the state directory (-want +got) diff = %s", diff)
			}
			got, err := os.ReadDir(home)
			if err != nil || len(got) > 0 {
				t.Fatalf("Runner.prepare() wrote %v into the home, error = %v", got, err)
			}
		})
	}
}

func TestPruneEnvFilesRemovesLeftoversOfEarlierRun(t *testing.T) {
	t.Parallel()
	envFiles := t.TempDir()
	leftover := filepath.Join(envFiles, "a2a-env-123")
	if err := os.WriteFile(leftover, []byte("KEY=secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := newTestRunner(t, Config{Dirs: Dirs{EnvFiles: envFiles}, Spec: func() (Spec, error) { return Spec{}, nil }, Processes: fakeProcesses{}})

	r.pruneEnvFiles()

	if _, err := os.Stat(leftover); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("os.Stat(%s) error = %v, want %v after Runner.pruneEnvFiles()", leftover, err, os.ErrNotExist)
	}
}
