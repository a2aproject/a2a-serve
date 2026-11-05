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
	"os"
	"path/filepath"
	"testing"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/clierr"
)

func TestHarnessDirFrom(t *testing.T) {
	t.Parallel()
	parent := newTestAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode"})
	sub := filepath.Join(parent.Root, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatalf("os.Mkdir() error = %v", err)
	}
	empty := t.TempDir()
	tests := []struct {
		name     string
		wd       string
		dir      string
		wantRoot string
		wantCode string
	}{
		{name: "new agent", wd: empty, wantRoot: empty},
		{name: "existing agent", wd: parent.Root, wantRoot: parent.Root},
		{name: "inside an existing agent", wd: sub, wantCode: clierr.CodeUsage},
		{name: "inside an existing agent with --dir", wd: sub, dir: sub, wantRoot: sub},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, err := harnessDirFrom(tc.wd, tc.dir)
			if tc.wantCode != "" {
				if got := clierr.Classify(err); got == nil || got.Code != tc.wantCode || got.Exit != 2 {
					t.Fatalf("harnessDirFrom(%q, %q) error = %v, want %s (exit 2)", tc.wd, tc.dir, err, tc.wantCode)
				}
				return
			}
			if err != nil {
				t.Fatalf("harnessDirFrom(%q, %q) error = %v, want nil", tc.wd, tc.dir, err)
			}
			if d.Root != tc.wantRoot {
				t.Fatalf("harnessDirFrom(%q, %q) = %s, want %s", tc.wd, tc.dir, d.Root, tc.wantRoot)
			}
		})
	}
}
