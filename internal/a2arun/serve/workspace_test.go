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

package serve

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-serve/core/a2aserve"
	"github.com/a2aproject/a2a-serve/core/fsys"
)

func TestCheckWorkspaceSize(t *testing.T) {
	t.Parallel()
	limits := workspaceLimits{bytes: 10, entries: 3}
	tests := []struct {
		name    string
		files   map[string]string
		wantErr bool
	}{
		{name: "within the limits", files: map[string]string{"a": "12345", "d/b": "12345"}},
		{name: "agent directories don't count", files: map[string]string{"a": "1", ".a2a/x": "12345678901", ".a2a/y": "1", ".a2a/z": "1"}},
		{name: "past the bytes", files: map[string]string{"a": "12345", "b": "123456"}, wantErr: true},
		{name: "past the entries", files: map[string]string{"a": "1", "d/b": "1", "d/c": "1"}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			for p, data := range tc.files {
				if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, p), []byte(data), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			err := checkWorkspaceSize(dir, limits)

			if gotErr := err != nil; gotErr != tc.wantErr {
				t.Fatalf("checkWorkspaceSize() error = %v, want error %t", err, tc.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "--workspace-sync live") {
				t.Fatalf("checkWorkspaceSize() error = %q, want it to name --workspace-sync live", err)
			}
		})
	}
}

func TestExplainWordsWorkspaceFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		ws   workspace
		err  error
		want []string
	}{
		{
			name: "conflict",
			ws:   workspace{kind: WorkspaceDir, sync: SyncTurn},
			err:  &a2aserve.SyncConflictError{Paths: []string{"a.go"}},
			want: []string{"dropped the agent's: a.go", a2aserve.ConflictsKey},
		},
		{
			name: "dir past the limits",
			ws:   workspace{kind: WorkspaceDir, sync: SyncTurn},
			err:  fmt.Errorf("writing back: %w", a2aserve.ErrWorkspaceTooLarge),
			want: []string{"limits", "remove files", "--workspace-sync live"},
		},
		{
			name: "full memory workspace",
			ws:   workspace{kind: WorkspaceMemory, sync: SyncTurn},
			err:  fmt.Errorf("write a: %w", fsys.ErrFull),
			want: []string{"limits", "remove files"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			parts := tc.ws.explain(context.Background(), nil, tc.err)

			if len(parts) != 1 {
				t.Fatalf("workspace.explain() = %v, want one part", parts)
			}
			for _, w := range tc.want {
				if !strings.Contains(parts[0].Text(), w) {
					t.Fatalf("workspace.explain() = %q, want it to contain %q", parts[0].Text(), w)
				}
			}
		})
	}
	if parts := (workspace{kind: WorkspaceDir}).explain(context.Background(), nil, errors.New("other")); parts != nil {
		t.Fatalf("workspace.explain() of another error = %v, want nil", parts)
	}
}
