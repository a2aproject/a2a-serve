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

package agentrun

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/core/sandbox"
	"github.com/a2aproject/a2a-serve/core/workspace"
	"github.com/a2aproject/a2a-serve/internal/agentprofile"
	"github.com/a2aproject/a2a-serve/internal/projectfiles"
)

// fakeCallers describes the processes of fixed keys, and admits every
// turn, as admit does if set.
type fakeCallers struct {
	envs  map[string]Environment
	admit func(ctx context.Context, key string, execCtx *a2asrv.ExecutorContext) (func(), error)
}

func (c fakeCallers) Key(context.Context, string) (string, error) { return "", nil }

func (c fakeCallers) Environment(_ context.Context, key string) (Environment, error) {
	env, ok := c.envs[key]
	if !ok {
		return Environment{}, errors.New("unknown key " + key)
	}
	return env, nil
}

func (c fakeCallers) Admit(ctx context.Context, key string, execCtx *a2asrv.ExecutorContext) (func(), error) {
	if c.admit != nil {
		return c.admit(ctx, key, execCtx)
	}
	return func() {}, nil
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
	if cfg.Options.SocketDir == "" {
		cfg.Options.SocketDir = filepath.Join(privateDir(t), "s")
	}
	if cfg.Profile == nil {
		cfg.Profile = agentprofile.Custom("fake", []string{"fake-acp"})
	}
	if cfg.Runtime == nil {
		cfg.Runtime = sandbox.Docker(sandbox.DockerOptions{})
	}
	if cfg.StateDir == "" {
		cfg.StateDir = t.TempDir()
	}
	r, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := r.Close(context.Background()); err != nil {
			t.Errorf("Runner.Close() error = %v", err)
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
			pins, err := agentprofile.NewPins(root, agentDir, state, home)
			if err != nil {
				t.Fatalf("agentprofile.NewPins() error = %v", err)
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
			env := Environment{
				Workspace: workspace.Dir(root),
				Name:      "a2a-coder-" + tc.name,
				Home:      procHome,
				Hide:      []string{agentDir},
				Pool:      tc.pool,
				Process:   &ProcessOptions{LockHome: tc.key != ""},
			}
			r := newTestRunner(t, Config{
				StateDir: state,
				Callers:  fakeCallers{envs: map[string]Environment{tc.key: env}},
				Pools:    map[string]Pool{"guests": {}, "users": {}},
				AgentAPI: http.NotFoundHandler(),
				Options:  Options{Pins: pins, Mounts: []agentprofile.Mount{{Src: root, Dst: root}}},
			})

			_, err = r.prepare(context.Background(), io.Discard, io.Discard, tc.key, env)

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
	state := t.TempDir()
	envFiles := filepath.Join(state, "run", "env")
	if err := os.MkdirAll(envFiles, 0o700); err != nil {
		t.Fatal(err)
	}
	leftover := filepath.Join(envFiles, "a2a-env-123")
	if err := os.WriteFile(leftover, []byte("KEY=secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := newTestRunner(t, Config{StateDir: state})

	r.pruneEnvFiles()

	if _, err := os.Stat(leftover); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("os.Stat(%s) error = %v, want %v after Runner.pruneEnvFiles()", leftover, err, os.ErrNotExist)
	}
}
