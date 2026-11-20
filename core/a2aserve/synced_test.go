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

package a2aserve_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/core/a2aserve"
	"github.com/a2aproject/a2a-serve/core/fsys"
)

// snapshot describes the entries of tree: "dir", "-> target", or the mode
// and content of a file.
func snapshot(t *testing.T, tree fsys.FS) map[string]string {
	t.Helper()
	out := map[string]string{}
	var visit func(dir string)
	visit = func(dir string) {
		entries, err := tree.ReadDir(context.Background(), dir)
		if err != nil {
			t.Fatalf("FS.ReadDir(%q) error = %v", dir, err)
		}
		for _, e := range entries {
			switch {
			case e.Mode.IsDir():
				out[e.Path] = "dir"
				visit(e.Path)
			case e.Mode&fs.ModeSymlink != 0:
				out[e.Path] = "-> " + e.Target
			default:
				rc, err := tree.Open(context.Background(), e.Path)
				if err != nil {
					t.Fatalf("FS.Open(%q) error = %v", e.Path, err)
				}
				data, err := io.ReadAll(rc)
				if err != nil {
					t.Fatalf("reading %q error = %v", e.Path, err)
				}
				if err := rc.Close(); err != nil {
					t.Fatalf("closing %q error = %v", e.Path, err)
				}
				out[e.Path] = fmt.Sprintf("%v %s", e.Mode.Perm(), data)
			}
		}
	}
	visit(".")
	return out
}

func put(t *testing.T, tree fsys.FS, path, data string, mode fs.FileMode) {
	t.Helper()
	if _, err := tree.Write(context.Background(), path, strings.NewReader(data), mode, ""); err != nil {
		t.Fatalf("FS.Write(%q) error = %v", path, err)
	}
}

func symlink(t *testing.T, tree fsys.FS, target, path string) {
	t.Helper()
	if err := tree.Symlink(context.Background(), target, path); err != nil {
		t.Fatalf("FS.Symlink(%q) error = %v", path, err)
	}
}

func remove(t *testing.T, tree fsys.FS, path string) {
	t.Helper()
	if err := tree.Remove(context.Background(), path); err != nil {
		t.Fatalf("FS.Remove(%q) error = %v", path, err)
	}
}

// attachSynced attaches the directory of key in a TurnSynced workspace of
// tree, whose directories are in root.
func attachSynced(t *testing.T, tree fsys.FS, root, key string) (a2aserve.Attachment, fsys.FS) {
	t.Helper()
	att, err := a2aserve.TurnSynced(tree, a2aserve.TurnSyncOptions{Dir: root}).Attach(context.Background(), key)
	if err != nil {
		t.Fatalf("TurnSynced().Attach() error = %v", err)
	}
	return att, fsys.Dir(att.Dir)
}

func TestTurnSyncedFillsDirectory(t *testing.T) {
	t.Parallel()
	tree := fsys.Memory()
	put(t, tree, "main.go", "package main", 0o644)
	put(t, tree, "bin/run.sh", "#!/bin/sh", 0o755)
	symlink(t, tree, "bin/run.sh", "run")
	put(t, tree, "sub/.a2a/config.yaml", "x", 0o644)

	att, local := attachSynced(t, tree, t.TempDir(), "a")

	want := map[string]string{
		"main.go":    "-rw-r--r-- package main",
		"bin":        "dir",
		"bin/run.sh": "-rwxr-xr-x #!/bin/sh",
		"run":        "-> bin/run.sh",
		"sub":        "dir",
	}
	if diff := cmp.Diff(want, snapshot(t, local)); diff != "" {
		t.Fatalf("TurnSynced().Attach() directory wrong entries (-want +got) diff = %s", diff)
	}
	if err := att.Release(); err != nil {
		t.Fatalf("Release() error = %v", err)
	}
}

func TestTurnSyncedSyncsAtTurnBoundaries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		treeEdit  func(t *testing.T, tree fsys.FS)
		localEdit func(t *testing.T, local fsys.FS)
		want      map[string]string
	}{
		{
			name:     "tree adds a file",
			treeEdit: func(t *testing.T, tree fsys.FS) { put(t, tree, "new.txt", "new", 0o600) },
			want:     map[string]string{"keep.txt": "-rw-r--r-- keep", "dir": "dir", "dir/a.txt": "-rw-r--r-- a", "new.txt": "-rw------- new"},
		},
		{
			name:     "tree removes a directory",
			treeEdit: func(t *testing.T, tree fsys.FS) { remove(t, tree, "dir") },
			want:     map[string]string{"keep.txt": "-rw-r--r-- keep"},
		},
		{
			name:      "agent adds an executable file",
			localEdit: func(t *testing.T, local fsys.FS) { put(t, local, "tools/build.sh", "#!/bin/sh", 0o755) },
			want:      map[string]string{"keep.txt": "-rw-r--r-- keep", "dir": "dir", "dir/a.txt": "-rw-r--r-- a", "tools": "dir", "tools/build.sh": "-rwxr-xr-x #!/bin/sh"},
		},
		{
			name:      "agent changes a file",
			localEdit: func(t *testing.T, local fsys.FS) { put(t, local, "keep.txt", "changed", 0o644) },
			want:      map[string]string{"keep.txt": "-rw-r--r-- changed", "dir": "dir", "dir/a.txt": "-rw-r--r-- a"},
		},
		{
			name:      "agent removes a directory",
			localEdit: func(t *testing.T, local fsys.FS) { remove(t, local, "dir") },
			want:      map[string]string{"keep.txt": "-rw-r--r-- keep"},
		},
		{
			name: "agent replaces a file with a symlink",
			localEdit: func(t *testing.T, local fsys.FS) {
				remove(t, local, "keep.txt")
				symlink(t, local, "dir/a.txt", "keep.txt")
			},
			want: map[string]string{"keep.txt": "-> dir/a.txt", "dir": "dir", "dir/a.txt": "-rw-r--r-- a"},
		},
		{
			name: "placeholders and agent directories stay",
			localEdit: func(t *testing.T, local fsys.FS) {
				put(t, local, "opencode.json", "{}\n", 0o644)
				put(t, local, ".claude/settings.json", "{}\n", 0o644)
				put(t, local, ".mcp.json", "{}\n", 0o644)
				put(t, local, ".a2a/state", "x", 0o644)
			},
			want: map[string]string{"keep.txt": "-rw-r--r-- keep", "dir": "dir", "dir/a.txt": "-rw-r--r-- a"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tree := fsys.Memory()
			put(t, tree, "keep.txt", "keep", 0o644)
			put(t, tree, "dir/a.txt", "a", 0o644)
			att, local := attachSynced(t, tree, t.TempDir(), "a")
			ctx := context.Background()

			if tc.treeEdit != nil {
				tc.treeEdit(t, tree)
			}
			if err := att.BeforeTurn(ctx); err != nil {
				t.Fatalf("BeforeTurn() error = %v", err)
			}
			if tc.localEdit != nil {
				tc.localEdit(t, local)
			}
			if err := att.AfterTurn(ctx); err != nil {
				t.Fatalf("AfterTurn() error = %v", err)
			}

			if diff := cmp.Diff(tc.want, snapshot(t, tree)); diff != "" {
				t.Fatalf("tree after the turn wrong entries (-want +got) diff = %s", diff)
			}
			got := snapshot(t, local)
			for p := range got {
				if strings.HasPrefix(p, ".a2a") || strings.HasPrefix(p, ".claude") || p == "opencode.json" || p == ".mcp.json" {
					delete(got, p)
				}
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("directory after the turn wrong entries (-want +got) diff = %s", diff)
			}
			if err := att.Release(); err != nil {
				t.Fatalf("Release() error = %v", err)
			}
		})
	}
}

func TestTurnSyncedKeepsTheTreesVersionOnConflict(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		treeEdit  func(t *testing.T, tree fsys.FS)
		localEdit func(t *testing.T, local fsys.FS)
		// betweenTurns makes both edits before BeforeTurn.
		betweenTurns bool
		want         map[string]string
		wantPaths    []string
	}{
		{
			name:         "both change a file between turns",
			treeEdit:     func(t *testing.T, tree fsys.FS) { put(t, tree, "f.txt", "theirs", 0o644) },
			localEdit:    func(t *testing.T, local fsys.FS) { put(t, local, "f.txt", "mine", 0o644) },
			betweenTurns: true,
			want:         map[string]string{"f.txt": "-rw-r--r-- theirs", "d": "dir", "d/a.txt": "-rw-r--r-- a"},
			wantPaths:    []string{"f.txt"},
		},
		{
			name:         "tree removes a directory the agent added to between turns",
			treeEdit:     func(t *testing.T, tree fsys.FS) { remove(t, tree, "d") },
			localEdit:    func(t *testing.T, local fsys.FS) { put(t, local, "d/mine.txt", "mine", 0o644) },
			betweenTurns: true,
			want:         map[string]string{"f.txt": "-rw-r--r-- base"},
			wantPaths:    []string{"d"},
		},
		{
			name:         "tree replaces a directory the agent added to between turns",
			treeEdit:     func(t *testing.T, tree fsys.FS) { remove(t, tree, "d"); put(t, tree, "d", "theirs", 0o644) },
			localEdit:    func(t *testing.T, local fsys.FS) { put(t, local, "d/mine.txt", "mine", 0o644) },
			betweenTurns: true,
			want:         map[string]string{"f.txt": "-rw-r--r-- base", "d": "-rw-r--r-- theirs"},
			wantPaths:    []string{"d"},
		},
		{
			name:      "tree replaces a directory the agent added to",
			treeEdit:  func(t *testing.T, tree fsys.FS) { remove(t, tree, "d"); put(t, tree, "d", "theirs", 0o644) },
			localEdit: func(t *testing.T, local fsys.FS) { put(t, local, "d/mine.txt", "mine", 0o644) },
			want:      map[string]string{"f.txt": "-rw-r--r-- base", "d": "-rw-r--r-- theirs"},
			wantPaths: []string{"d"},
		},
		{
			name:      "agent replaces a directory the tree added to",
			treeEdit:  func(t *testing.T, tree fsys.FS) { put(t, tree, "d/new.txt", "new", 0o644) },
			localEdit: func(t *testing.T, local fsys.FS) { remove(t, local, "d"); put(t, local, "d", "mine", 0o644) },
			want:      map[string]string{"f.txt": "-rw-r--r-- base", "d": "dir", "d/a.txt": "-rw-r--r-- a", "d/new.txt": "-rw-r--r-- new"},
			wantPaths: []string{"d"},
		},
		{
			name:      "both change a file",
			treeEdit:  func(t *testing.T, tree fsys.FS) { put(t, tree, "f.txt", "theirs", 0o644) },
			localEdit: func(t *testing.T, local fsys.FS) { put(t, local, "f.txt", "mine", 0o644) },
			want:      map[string]string{"f.txt": "-rw-r--r-- theirs", "d": "dir", "d/a.txt": "-rw-r--r-- a"},
			wantPaths: []string{"f.txt"},
		},
		{
			name:      "tree removes what the agent changed",
			treeEdit:  func(t *testing.T, tree fsys.FS) { remove(t, tree, "f.txt") },
			localEdit: func(t *testing.T, local fsys.FS) { put(t, local, "f.txt", "mine", 0o644) },
			want:      map[string]string{"d": "dir", "d/a.txt": "-rw-r--r-- a"},
			wantPaths: []string{"f.txt"},
		},
		{
			name:      "agent removes what the tree changed",
			treeEdit:  func(t *testing.T, tree fsys.FS) { put(t, tree, "f.txt", "theirs", 0o644) },
			localEdit: func(t *testing.T, local fsys.FS) { remove(t, local, "f.txt") },
			want:      map[string]string{"f.txt": "-rw-r--r-- theirs", "d": "dir", "d/a.txt": "-rw-r--r-- a"},
			wantPaths: []string{"f.txt"},
		},
		{
			name:      "both create a file",
			treeEdit:  func(t *testing.T, tree fsys.FS) { put(t, tree, "new.txt", "theirs", 0o644) },
			localEdit: func(t *testing.T, local fsys.FS) { put(t, local, "new.txt", "mine", 0o755) },
			want:      map[string]string{"f.txt": "-rw-r--r-- base", "d": "dir", "d/a.txt": "-rw-r--r-- a", "new.txt": "-rw-r--r-- theirs"},
			wantPaths: []string{"new.txt"},
		},
		{
			name:      "both create a file of the same size",
			treeEdit:  func(t *testing.T, tree fsys.FS) { put(t, tree, "new.txt", "abcd", 0o644) },
			localEdit: func(t *testing.T, local fsys.FS) { put(t, local, "new.txt", "abce", 0o644) },
			want:      map[string]string{"f.txt": "-rw-r--r-- base", "d": "dir", "d/a.txt": "-rw-r--r-- a", "new.txt": "-rw-r--r-- abcd"},
			wantPaths: []string{"new.txt"},
		},
		{
			name:         "both change a file to the same size between turns",
			treeEdit:     func(t *testing.T, tree fsys.FS) { put(t, tree, "f.txt", "ours", 0o644) },
			localEdit:    func(t *testing.T, local fsys.FS) { put(t, local, "f.txt", "mine", 0o644) },
			betweenTurns: true,
			want:         map[string]string{"f.txt": "-rw-r--r-- ours", "d": "dir", "d/a.txt": "-rw-r--r-- a"},
			wantPaths:    []string{"f.txt"},
		},
		{
			name:      "both create a file with the same bytes and other permissions",
			treeEdit:  func(t *testing.T, tree fsys.FS) { put(t, tree, "new.sh", "same", 0o644) },
			localEdit: func(t *testing.T, local fsys.FS) { put(t, local, "new.sh", "same", 0o755) },
			want:      map[string]string{"f.txt": "-rw-r--r-- base", "d": "dir", "d/a.txt": "-rw-r--r-- a", "new.sh": "-rw-r--r-- same"},
			wantPaths: []string{"new.sh"},
		},
		{
			name:      "both create a symlink to other targets",
			treeEdit:  func(t *testing.T, tree fsys.FS) { symlink(t, tree, "f.txt", "link") },
			localEdit: func(t *testing.T, local fsys.FS) { symlink(t, local, "d/a.txt", "link") },
			want:      map[string]string{"f.txt": "-rw-r--r-- base", "d": "dir", "d/a.txt": "-rw-r--r-- a", "link": "-> f.txt"},
			wantPaths: []string{"link"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tree := fsys.Memory()
			put(t, tree, "f.txt", "base", 0o644)
			put(t, tree, "d/a.txt", "a", 0o644)
			att, local := attachSynced(t, tree, t.TempDir(), "a")
			ctx := context.Background()
			if !tc.betweenTurns {
				if err := att.BeforeTurn(ctx); err != nil {
					t.Fatalf("BeforeTurn() error = %v", err)
				}
			}
			tc.localEdit(t, local)
			tc.treeEdit(t, tree)
			if tc.betweenTurns {
				if err := att.BeforeTurn(ctx); err != nil {
					t.Fatalf("BeforeTurn() error = %v", err)
				}
			}

			err := att.AfterTurn(ctx)

			conflict, ok := errors.AsType[*a2aserve.SyncConflictError](err)
			if !ok {
				t.Fatalf("AfterTurn() error = %v, want a SyncConflictError", err)
			}
			if diff := cmp.Diff(tc.wantPaths, conflict.Paths); diff != "" {
				t.Fatalf("AfterTurn() wrong conflicts (-want +got) diff = %s", diff)
			}
			if diff := cmp.Diff(tc.want, snapshot(t, tree)); diff != "" {
				t.Fatalf("tree after the conflict wrong entries (-want +got) diff = %s", diff)
			}
			if diff := cmp.Diff(tc.want, snapshot(t, local)); diff != "" {
				t.Fatalf("directory after the conflict wrong entries (-want +got) diff = %s", diff)
			}
			if err := att.BeforeTurn(ctx); err != nil {
				t.Fatalf("BeforeTurn() after the conflict error = %v", err)
			}
			if err := att.AfterTurn(ctx); err != nil {
				t.Fatalf("AfterTurn() of the next turn error = %v, want nil", err)
			}
			if err := att.Release(); err != nil {
				t.Fatalf("Release() error = %v", err)
			}
		})
	}
}

func TestTurnSyncedTakesIdenticalChangesWithoutConflict(t *testing.T) {
	t.Parallel()
	same := func(t *testing.T, tree fsys.FS) {
		put(t, tree, "f.txt", "same", 0o644)
		put(t, tree, "new/run.sh", "#!/bin/sh", 0o755)
		symlink(t, tree, "f.txt", "link")
	}
	tests := []struct {
		name string
		// betweenTurns makes both edits before BeforeTurn.
		betweenTurns bool
	}{
		{name: "during a turn"},
		{name: "between turns", betweenTurns: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tree := fsys.Memory()
			put(t, tree, "f.txt", "base", 0o644)
			var reports [][]fsys.Info
			opts := a2aserve.TurnSyncOptions{Dir: t.TempDir(), Synced: func(_ context.Context, _ string, changes []fsys.Info) {
				reports = append(reports, changes)
			}}
			att, err := a2aserve.TurnSynced(tree, opts).Attach(context.Background(), "a")
			if err != nil {
				t.Fatalf("TurnSynced().Attach() error = %v", err)
			}
			local := fsys.Dir(att.Dir)
			ctx := context.Background()
			if !tc.betweenTurns {
				if err := att.BeforeTurn(ctx); err != nil {
					t.Fatalf("BeforeTurn() error = %v", err)
				}
			}
			same(t, local)
			same(t, tree)
			if tc.betweenTurns {
				if err := att.BeforeTurn(ctx); err != nil {
					t.Fatalf("BeforeTurn() error = %v", err)
				}
			}

			if err := att.AfterTurn(ctx); err != nil {
				t.Fatalf("AfterTurn() error = %v, want nil", err)
			}

			want := map[string]string{"f.txt": "-rw-r--r-- same", "new": "dir", "new/run.sh": "-rwxr-xr-x #!/bin/sh", "link": "-> f.txt"}
			if diff := cmp.Diff(want, snapshot(t, tree)); diff != "" {
				t.Fatalf("tree after the turn wrong entries (-want +got) diff = %s", diff)
			}
			if diff := cmp.Diff(want, snapshot(t, local)); diff != "" {
				t.Fatalf("directory after the turn wrong entries (-want +got) diff = %s", diff)
			}
			if len(reports) > 0 {
				t.Fatalf("Synced() reports = %v, want none", reports)
			}
			if err := att.BeforeTurn(ctx); err != nil {
				t.Fatalf("BeforeTurn() of the next turn error = %v", err)
			}
			if err := att.AfterTurn(ctx); err != nil {
				t.Fatalf("AfterTurn() of the next turn error = %v, want nil", err)
			}
			if err := att.Release(); err != nil {
				t.Fatalf("Release() error = %v", err)
			}
		})
	}
}

func TestTurnSyncedKeysWritingTheSameNewFilesDontConflict(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// pullFirst syncs b in before its turn ends, after a wrote back.
		pullFirst bool
	}{
		{name: "both turns end"},
		{name: "second turn starts after the first ends", pullFirst: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tree := fsys.Memory()
			put(t, tree, "README.md", "hi", 0o644)
			var mu sync.Mutex
			reported := map[string][]string{}
			opts := a2aserve.TurnSyncOptions{Dir: t.TempDir(), Synced: func(_ context.Context, key string, changes []fsys.Info) {
				mu.Lock()
				defer mu.Unlock()
				for _, c := range changes {
					reported[key] = append(reported[key], c.Path)
				}
			}}
			w := a2aserve.TurnSynced(tree, opts)
			ctx := context.Background()
			atts := map[string]a2aserve.Attachment{}
			for _, key := range []string{"a", "b"} {
				att, err := w.Attach(ctx, key)
				if err != nil {
					t.Fatalf("TurnSynced().Attach(%q) error = %v", key, err)
				}
				atts[key] = att
				if !tc.pullFirst || key == "a" {
					if err := att.BeforeTurn(ctx); err != nil {
						t.Fatalf("BeforeTurn() of %q error = %v", key, err)
					}
				}
				local := fsys.Dir(att.Dir)
				put(t, local, ".git/opencode", "0123abcd", 0o444)
				put(t, local, ".git/objects/69/fecf", "blob", 0o444)
			}

			if err := atts["a"].AfterTurn(ctx); err != nil {
				t.Fatalf("AfterTurn() of a error = %v, want nil", err)
			}
			if tc.pullFirst {
				if err := atts["b"].BeforeTurn(ctx); err != nil {
					t.Fatalf("BeforeTurn() of b error = %v", err)
				}
			}
			if err := atts["b"].AfterTurn(ctx); err != nil {
				t.Fatalf("AfterTurn() of b error = %v, want nil", err)
			}

			want := map[string][]string{"a": {".git", ".git/objects", ".git/objects/69", ".git/objects/69/fecf", ".git/opencode"}}
			mu.Lock()
			diff := cmp.Diff(want, reported)
			mu.Unlock()
			if diff != "" {
				t.Fatalf("Synced() reports wrong result (-want +got) diff = %s", diff)
			}
			for key, att := range atts {
				if err := att.Release(); err != nil {
					t.Fatalf("Release() of %q error = %v", key, err)
				}
			}
		})
	}
}

func TestTurnSyncedDoesntWriteBackAnIdenticalRewrite(t *testing.T) {
	t.Parallel()
	tree := fsys.Memory()
	put(t, tree, "f.txt", "base", 0o644)
	var reports [][]fsys.Info
	opts := a2aserve.TurnSyncOptions{Dir: t.TempDir(), Synced: func(_ context.Context, _ string, changes []fsys.Info) {
		reports = append(reports, changes)
	}}
	att, err := a2aserve.TurnSynced(tree, opts).Attach(context.Background(), "a")
	if err != nil {
		t.Fatalf("TurnSynced().Attach() error = %v", err)
	}
	ctx := context.Background()
	before, err := tree.Stat(ctx, "f.txt")
	if err != nil {
		t.Fatalf("FS.Stat() error = %v", err)
	}
	if err := att.BeforeTurn(ctx); err != nil {
		t.Fatalf("BeforeTurn() error = %v", err)
	}
	put(t, fsys.Dir(att.Dir), "f.txt", "base", 0o644)

	if err := att.AfterTurn(ctx); err != nil {
		t.Fatalf("AfterTurn() error = %v, want nil", err)
	}

	after, err := tree.Stat(ctx, "f.txt")
	if err != nil {
		t.Fatalf("FS.Stat() error = %v", err)
	}
	if after.Version != before.Version {
		t.Fatalf("FS.Stat() version after the turn = %q, want %q, the file unwritten", after.Version, before.Version)
	}
	if len(reports) > 0 {
		t.Fatalf("Synced() reports = %v, want none", reports)
	}
	if err := att.Release(); err != nil {
		t.Fatalf("Release() error = %v", err)
	}
}

// syncedChange is what a test compares of a change that Synced reports.
type syncedChange struct {
	Path    string
	Mode    fs.FileMode
	Removed bool
}

func TestTurnSyncedReportsWhatTheTurnWrote(t *testing.T) {
	t.Parallel()
	tree := fsys.Memory()
	put(t, tree, "keep.txt", "keep", 0o644)
	put(t, tree, "old.txt", "old", 0o644)
	put(t, tree, "conflict.txt", "base", 0o644)
	type report struct {
		key     string
		value   any
		changes []syncedChange
	}
	var reports []report
	type ctxKey struct{}
	opts := a2aserve.TurnSyncOptions{Dir: t.TempDir(), Synced: func(ctx context.Context, key string, changes []fsys.Info) {
		r := report{key: key, value: ctx.Value(ctxKey{})}
		for _, c := range changes {
			r.changes = append(r.changes, syncedChange{Path: c.Path, Mode: c.Mode, Removed: c.Removed})
		}
		reports = append(reports, r)
	}}
	att, err := a2aserve.TurnSynced(tree, opts).Attach(context.Background(), "a")
	if err != nil {
		t.Fatalf("TurnSynced().Attach() error = %v", err)
	}
	local := fsys.Dir(att.Dir)
	ctx := context.WithValue(context.Background(), ctxKey{}, "turn 1")

	if err := att.BeforeTurn(ctx); err != nil {
		t.Fatalf("BeforeTurn() error = %v", err)
	}
	put(t, local, "new/run.sh", "#!/bin/sh", 0o755)
	remove(t, local, "old.txt")
	put(t, local, "conflict.txt", "mine", 0o644)
	put(t, local, "opencode.json", "{}", 0o644)
	put(t, tree, "conflict.txt", "theirs", 0o644)
	if _, ok := errors.AsType[*a2aserve.SyncConflictError](att.AfterTurn(ctx)); !ok {
		t.Fatalf("AfterTurn() error, want a SyncConflictError")
	}
	quiet := context.WithValue(context.Background(), ctxKey{}, "turn 2")
	if err := att.BeforeTurn(quiet); err != nil {
		t.Fatalf("BeforeTurn() error = %v", err)
	}
	if err := att.AfterTurn(quiet); err != nil {
		t.Fatalf("AfterTurn() error = %v", err)
	}

	want := []report{{key: "a", value: "turn 1", changes: []syncedChange{
		{Path: "new", Mode: fs.ModeDir | 0o755},
		{Path: "new/run.sh", Mode: 0o755},
		{Path: "old.txt", Removed: true},
	}}}
	if diff := cmp.Diff(want, reports, cmp.AllowUnexported(report{})); diff != "" {
		t.Fatalf("Synced() reports wrong result (-want +got) diff = %s", diff)
	}
	if err := att.Release(); err != nil {
		t.Fatalf("Release() error = %v", err)
	}
}

func TestTurnSyncedReportsWhatReleaseWrote(t *testing.T) {
	t.Parallel()
	tree := fsys.Memory()
	var got []syncedChange
	opts := a2aserve.TurnSyncOptions{Dir: t.TempDir(), Synced: func(_ context.Context, key string, changes []fsys.Info) {
		for _, c := range changes {
			got = append(got, syncedChange{Path: key + ":" + c.Path, Mode: c.Mode, Removed: c.Removed})
		}
	}}
	att, err := a2aserve.TurnSynced(tree, opts).Attach(context.Background(), "a")
	if err != nil {
		t.Fatalf("TurnSynced().Attach() error = %v", err)
	}
	put(t, fsys.Dir(att.Dir), "late.txt", "after the turn", 0o644)

	if err := att.Release(); err != nil {
		t.Fatalf("Release() error = %v", err)
	}

	if diff := cmp.Diff([]syncedChange{{Path: "a:late.txt", Mode: 0o644}}, got); diff != "" {
		t.Fatalf("Synced() of Release wrong result (-want +got) diff = %s", diff)
	}
}

func TestTurnSyncedRunsATurnPastItsLimitsAndRecovers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tests := []struct {
		name string
		// treeBytes bounds the tree; zero leaves it unbounded.
		treeBytes int64
		// grow takes the workspace past the sync's limit of 10 bytes, or
		// fills the tree, and shrink takes it back.
		grow, shrink func(t *testing.T, tree, local fsys.FS)
		wantErr      error
		// wantSkipped is set when the turn runs without the tree's change.
		wantSkipped string
	}{
		{
			name:        "tree grows between turns",
			grow:        func(t *testing.T, tree, _ fsys.FS) { put(t, tree, "big.txt", "0123456789", 0o644) },
			shrink:      func(t *testing.T, tree, _ fsys.FS) { remove(t, tree, "big.txt") },
			wantErr:     a2aserve.ErrWorkspaceTooLarge,
			wantSkipped: "big.txt",
		},
		{
			name:    "agent fills the directory",
			grow:    func(t *testing.T, _, local fsys.FS) { put(t, local, "big.txt", "0123456789", 0o644) },
			shrink:  func(t *testing.T, _, local fsys.FS) { remove(t, local, "big.txt") },
			wantErr: a2aserve.ErrWorkspaceTooLarge,
		},
		{
			name:      "agent fills the tree",
			treeBytes: 6,
			grow: func(t *testing.T, _, local fsys.FS) {
				put(t, local, "new.txt", "01234", 0o644)
			},
			shrink:  func(t *testing.T, _, local fsys.FS) { remove(t, local, "new.txt") },
			wantErr: fsys.ErrFull,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tree := fsys.MemoryWithLimits(fsys.MemoryLimits{MaxBytes: tc.treeBytes})
			put(t, tree, "keep.txt", "keep", 0o644)
			opts := a2aserve.TurnSyncOptions{Dir: t.TempDir(), MaxBytes: 10}
			att, err := a2aserve.TurnSynced(tree, opts).Attach(ctx, "a")
			if err != nil {
				t.Fatalf("TurnSynced().Attach() error = %v", err)
			}
			local := fsys.Dir(att.Dir)

			tc.grow(t, tree, local)
			if err := att.BeforeTurn(ctx); err != nil {
				t.Fatalf("BeforeTurn() past the limits error = %v, want nil so that the agent can remove files", err)
			}
			if tc.wantSkipped != "" {
				if _, err := local.Stat(ctx, tc.wantSkipped); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("Stat(%q) of the directory past the limits error = %v, want %v", tc.wantSkipped, err, fs.ErrNotExist)
				}
			}
			if err := att.AfterTurn(ctx); !errors.Is(err, tc.wantErr) {
				t.Fatalf("AfterTurn() past the limits error = %v, want %v", err, tc.wantErr)
			}
			tc.shrink(t, tree, local)
			if err := att.BeforeTurn(ctx); err != nil {
				t.Fatalf("BeforeTurn() back within the limits error = %v", err)
			}
			if err := att.AfterTurn(ctx); err != nil {
				t.Fatalf("AfterTurn() back within the limits error = %v, want nil", err)
			}

			if diff := cmp.Diff(snapshot(t, tree), snapshot(t, local)); diff != "" {
				t.Fatalf("directory after the recovery wrong entries (-tree +directory) diff = %s", diff)
			}
			if err := att.Release(); err != nil {
				t.Fatalf("Release() error = %v", err)
			}
		})
	}
}

func TestTurnSyncedRemovesBeforeItWritesToAFullTree(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tree := fsys.MemoryWithLimits(fsys.MemoryLimits{MaxBytes: 6})
	put(t, tree, "old.txt", "0123", 0o644)
	att, local := attachSynced(t, tree, t.TempDir(), "a")
	if err := att.BeforeTurn(ctx); err != nil {
		t.Fatalf("BeforeTurn() error = %v", err)
	}
	remove(t, local, "old.txt")
	put(t, local, "new.txt", "01234", 0o644)

	if err := att.AfterTurn(ctx); err != nil {
		t.Fatalf("AfterTurn() error = %v, want nil", err)
	}

	if diff := cmp.Diff(map[string]string{"new.txt": "-rw-r--r-- 01234"}, snapshot(t, tree)); diff != "" {
		t.Fatalf("tree after the turn wrong entries (-want +got) diff = %s", diff)
	}
	if err := att.Release(); err != nil {
		t.Fatalf("Release() error = %v", err)
	}
}

func TestTurnSyncedReleaseWritesBackAndRemovesTheDirectory(t *testing.T) {
	t.Parallel()
	tree, root := fsys.Memory(), t.TempDir()
	att, local := attachSynced(t, tree, root, "a")
	put(t, local, "late.txt", "late", 0o644)

	if err := att.Release(); err != nil {
		t.Fatalf("Release() error = %v", err)
	}

	if diff := cmp.Diff(map[string]string{"late.txt": "-rw-r--r-- late"}, snapshot(t, tree)); diff != "" {
		t.Fatalf("tree after Release() wrong entries (-want +got) diff = %s", diff)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("os.ReadDir() error = %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("os.ReadDir() after Release() = %v, want no directory or state left", entries)
	}
	if err := att.AfterTurn(context.Background()); err != nil {
		t.Fatalf("AfterTurn() after Release() error = %v, want nil", err)
	}
}

func TestTurnSyncedReleaseReportsConflicts(t *testing.T) {
	t.Parallel()
	tree := fsys.Memory()
	put(t, tree, "f.txt", "base", 0o644)
	att, local := attachSynced(t, tree, t.TempDir(), "a")
	if err := att.BeforeTurn(context.Background()); err != nil {
		t.Fatalf("BeforeTurn() error = %v", err)
	}
	put(t, local, "f.txt", "mine", 0o644)
	put(t, tree, "f.txt", "theirs", 0o644)

	err := att.Release()

	conflict, ok := errors.AsType[*a2aserve.SyncConflictError](err)
	if !ok {
		t.Fatalf("Release() error = %v, want a SyncConflictError", err)
	}
	if diff := cmp.Diff([]string{"f.txt"}, conflict.Paths); diff != "" {
		t.Fatalf("Release() wrong conflicts (-want +got) diff = %s", diff)
	}
	if got := snapshot(t, tree)["f.txt"]; got != "-rw-r--r-- theirs" {
		t.Fatalf("tree f.txt after Release() = %q, want the tree's version", got)
	}
}

func TestTurnSyncedWritesBackADirectoryLeftByAStop(t *testing.T) {
	t.Parallel()
	tree, root := fsys.Memory(), t.TempDir()
	put(t, tree, "f.txt", "base", 0o644)
	_, local := attachSynced(t, tree, root, "a")
	put(t, local, "f.txt", "mine", 0o644)

	att, _ := attachSynced(t, tree, root, "a")

	if diff := cmp.Diff(map[string]string{"f.txt": "-rw-r--r-- mine"}, snapshot(t, tree)); diff != "" {
		t.Fatalf("tree after the next Attach() wrong entries (-want +got) diff = %s", diff)
	}
	if err := att.Release(); err != nil {
		t.Fatalf("Release() error = %v", err)
	}
}

func TestTurnSyncedRefusesAttach(t *testing.T) {
	t.Parallel()
	big := fsys.Memory()
	for _, name := range []string{"a", "b", "c"} {
		put(t, big, name, "x", 0o644)
	}
	tests := []struct {
		name    string
		w       a2aserve.Workspace
		wantErr error
		want    string
	}{
		{name: "no directory outside an Agent", w: a2aserve.TurnSynced(fsys.Memory(), a2aserve.TurnSyncOptions{}), want: "attaches only in an Agent"},
		{name: "too many entries", w: a2aserve.TurnSynced(big, a2aserve.TurnSyncOptions{Dir: t.TempDir(), MaxEntries: 2}), wantErr: a2aserve.ErrWorkspaceTooLarge},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := tc.w.Attach(context.Background(), "a")
			if err == nil || tc.wantErr != nil && !errors.Is(err, tc.wantErr) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("TurnSynced().Attach() error = %v, want %v %q", err, tc.wantErr, tc.want)
			}
		})
	}

}

func TestTurnSyncedAttachWaitsForRelease(t *testing.T) {
	t.Parallel()
	w := a2aserve.TurnSynced(fsys.Memory(), a2aserve.TurnSyncOptions{Dir: t.TempDir()})
	att, err := w.Attach(context.Background(), "a")
	if err != nil {
		t.Fatalf("TurnSynced().Attach() error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := w.Attach(ctx, "a"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("TurnSynced().Attach() of an attached key error = %v, want %v", err, context.DeadlineExceeded)
	}

	attached := make(chan error, 1)
	go func() {
		next, err := w.Attach(context.Background(), "a")
		if err == nil {
			err = next.Release()
		}
		attached <- err
	}()
	if err := att.Release(); err != nil {
		t.Fatalf("Release() error = %v", err)
	}
	if err := <-attached; err != nil {
		t.Fatalf("TurnSynced().Attach() after Release() error = %v", err)
	}
}

// syncedDir returns the directory of the only process of a TurnSynced
// workspace in root.
func syncedDir(t *testing.T, root string) string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("os.ReadDir() error = %v", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			return filepath.Join(root, e.Name())
		}
	}
	t.Fatalf("os.ReadDir(%s) = %v, want a synced directory", root, entries)
	return ""
}

// waitFile waits for the file path of tree to hold data.
func waitFile(t *testing.T, tree fsys.FS, path, data string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := snapshot(t, tree)[path]
		if strings.HasSuffix(got, " "+data) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("tree %s = %q, want %q", path, got, data)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestTurnSyncedAgentWritesBackOnEveryTurnEnd(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		turn      func(t *testing.T, a *a2aserve.Agent, store taskstore.Store, dir string) a2a.TaskState
		wantState a2a.TaskState
		path      string
		want      string
	}{
		{
			name: "completed",
			turn: func(t *testing.T, a *a2aserve.Agent, _ taskstore.Store, _ string) a2a.TaskState {
				return sendTask(t, client(t, a), "", "write out.txt").Status.State
			},
			wantState: a2a.TaskStateCompleted,
			path:      "out.txt",
			want:      "written by fakeacp\n",
		},
		{
			name: "agent exited",
			turn: func(t *testing.T, a *a2aserve.Agent, _ taskstore.Store, dir string) a2a.TaskState {
				writeFile(t, filepath.Join(dir, "before-crash.txt"), "kept")
				return sendTask(t, client(t, a), "", "crash").Status.State
			},
			wantState: a2a.TaskStateFailed,
			path:      "before-crash.txt",
			want:      "kept",
		},
		{
			name: "canceled",
			turn: func(t *testing.T, a *a2aserve.Agent, store taskstore.Store, dir string) a2a.TaskState {
				c := client(t, a)
				msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("slow"))
				res, err := c.SendMessage(context.Background(), &a2a.SendMessageRequest{Message: msg, Config: &a2a.SendMessageConfig{ReturnImmediately: true}})
				if err != nil {
					t.Fatalf("Client.SendMessage(slow) error = %v", err)
				}
				task := res.(*a2a.Task)
				waitWorkingOutput(t, store, task.ID)
				writeFile(t, filepath.Join(dir, "mid-turn.txt"), "kept")
				canceled, err := c.CancelTask(context.Background(), &a2a.CancelTaskRequest{ID: task.ID})
				if err != nil {
					t.Fatalf("Client.CancelTask() error = %v", err)
				}
				return canceled.Status.State
			},
			wantState: a2a.TaskStateCanceled,
			path:      "mid-turn.txt",
			want:      "kept",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tree, root := fsys.Memory(), t.TempDir()
			store := taskstore.NewInMemory(nil)
			a := newAgent(t, a2aserve.Config{
				Main: a2aserve.Environment{Workspace: a2aserve.TurnSynced(tree, a2aserve.TurnSyncOptions{Dir: root}), Home: filepath.Join(t.TempDir(), "home")},
				A2A:  a2aserve.A2A{TaskStore: store},
			})

			if got := tc.turn(t, a, store, syncedDir(t, root)); got != tc.wantState {
				t.Fatalf("task state = %v, want %v", got, tc.wantState)
			}

			waitFile(t, tree, tc.path, tc.want)
		})
	}
}

func TestTurnSyncedAgentFailsATurnWithConflicts(t *testing.T) {
	t.Parallel()
	tree, root := fsys.Memory(), t.TempDir()
	put(t, tree, "f.txt", "base", 0o644)
	var mu sync.Mutex
	var explained error
	a := newAgent(t, a2aserve.Config{
		Main: a2aserve.Environment{Workspace: a2aserve.TurnSynced(tree, a2aserve.TurnSyncOptions{Dir: root}), Home: filepath.Join(t.TempDir(), "home")},
		Hooks: a2aserve.Hooks{
			Turn: func(context.Context, *a2asrv.ExecutorContext) func() {
				put(t, tree, "f.txt", "theirs", 0o644)
				return func() {}
			},
			Explain: func(_ context.Context, _ *a2asrv.ExecutorContext, err error) []*a2a.Part {
				mu.Lock()
				defer mu.Unlock()
				explained = err
				return []*a2a.Part{a2a.NewTextPart("explained: " + err.Error())}
			},
		},
	})

	task := sendTask(t, client(t, a), "", "write f.txt")

	if task.Status.State != a2a.TaskStateFailed || task.Status.Message == nil {
		t.Fatalf("Client.SendMessage() status = %+v, want failed with a message", task.Status)
	}
	if diff := cmp.Diff([]any{"f.txt"}, task.Status.Message.Metadata[a2aserve.ConflictsKey]); diff != "" {
		t.Fatalf("status message metadata %s wrong value (-want +got) diff = %s", a2aserve.ConflictsKey, diff)
	}
	if got := task.Status.Message.Parts[0].Text(); !strings.HasPrefix(got, "explained: ") {
		t.Fatalf("status message = %q, want the one of Hooks.Explain", got)
	}
	mu.Lock()
	_, ok := errors.AsType[*a2aserve.SyncConflictError](explained)
	mu.Unlock()
	if !ok {
		t.Fatalf("Hooks.Explain() error = %v, want a SyncConflictError", explained)
	}
	if got := snapshot(t, tree)["f.txt"]; got != "-rw-r--r-- theirs" {
		t.Fatalf("tree f.txt = %q, want the tree's version", got)
	}
	if got := readFile(t, filepath.Join(syncedDir(t, root), "f.txt")); got != "theirs" {
		t.Fatalf("directory f.txt = %q, want the tree's version", got)
	}
}

// turnLog is a workspace in a shared directory that records the turn
// boundaries it sees.
type turnLog struct {
	dir string

	mu     sync.Mutex
	events []string
}

func (w *turnLog) Attach(context.Context, string) (a2aserve.Attachment, error) {
	return a2aserve.Attachment{
		Dir:        w.dir,
		Release:    func() error { return nil },
		BeforeTurn: func(context.Context) error { w.add("before"); return nil },
		AfterTurn:  func(context.Context) error { w.add("after"); return nil },
	}, nil
}

func (w *turnLog) add(event string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.events = append(w.events, event)
}

func (w *turnLog) log() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.events)
}

// sendAsync sends text in the conversation contextID without waiting for
// its result, and returns its task.
func sendAsync(t *testing.T, c *a2aclient.Client, contextID, text string) *a2a.Task {
	t.Helper()
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))
	msg.ContextID = contextID
	res, err := c.SendMessage(context.Background(), &a2a.SendMessageRequest{Message: msg, Config: &a2a.SendMessageConfig{ReturnImmediately: true}})
	if err != nil {
		t.Fatalf("Client.SendMessage(%q) error = %v", text, err)
	}
	return res.(*a2a.Task)
}

func cancelTask(t *testing.T, c *a2aclient.Client, id a2a.TaskID) {
	t.Helper()
	if _, err := c.CancelTask(context.Background(), &a2a.CancelTaskRequest{ID: id}); err != nil {
		t.Fatalf("Client.CancelTask() error = %v", err)
	}
}

func TestTurnBoundariesOfAKeyDontOverlap(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// first and second are the conversations of the turn that runs and
		// of the turn sent meanwhile.
		first, second string
		pool          bool
	}{
		{name: "same conversation", first: "c1", second: "c1"},
		{name: "another conversation of the key", first: "c1", second: "c2"},
		{name: "two conversations of a pool process", first: "a-1", second: "a-2", pool: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := &turnLog{dir: t.TempDir()}
			store := taskstore.NewInMemory(nil)
			cfg := a2aserve.Config{
				Main: a2aserve.Environment{Workspace: w, Home: filepath.Join(t.TempDir(), "home")},
				A2A:  a2aserve.A2A{TaskStore: store},
			}
			if tc.pool {
				cfg.Main = environment(t, "")
				cfg.Callers = keyedCallers{envs: map[string]a2aserve.Environment{"a": {Workspace: w, Home: filepath.Join(t.TempDir(), "home"), Pool: "users"}}}
				cfg.Pools = map[string]a2aserve.Pool{"users": {MaxProcesses: 1}}
			}
			a := newAgent(t, cfg)
			c := client(t, a)
			first := sendAsync(t, c, tc.first, "slow 500ms")
			waitStored(t, store, first.ID, a2a.TaskStateWorking)

			if got := sendTask(t, c, tc.second, "hello").Status.State; got != a2a.TaskStateCompleted {
				t.Fatalf("second task state = %v, want %v", got, a2a.TaskStateCompleted)
			}

			waitStored(t, store, first.ID, a2a.TaskStateCompleted)
			if diff := cmp.Diff([]string{"before", "after", "before", "after"}, w.log()); diff != "" {
				t.Fatalf("turn boundaries wrong order (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestCanceledWaitingTurnLetsTheNextTurnRun(t *testing.T) {
	t.Parallel()
	w := &turnLog{dir: t.TempDir()}
	store := taskstore.NewInMemory(nil)
	a := newAgent(t, a2aserve.Config{
		Main: a2aserve.Environment{Workspace: w, Home: filepath.Join(t.TempDir(), "home")},
		A2A:  a2aserve.A2A{TaskStore: store},
	})
	c := client(t, a)
	first := sendAsync(t, c, "c1", "slow")
	waitWorkingOutput(t, store, first.ID)
	waiting := sendAsync(t, c, "c2", "hello")

	cancelTask(t, c, waiting.ID)
	waitStored(t, store, waiting.ID, a2a.TaskStateCanceled)
	cancelTask(t, c, first.ID)
	waitStored(t, store, first.ID, a2a.TaskStateCanceled)

	if got := sendTask(t, c, "c3", "hello").Status.State; got != a2a.TaskStateCompleted {
		t.Fatalf("next task state = %v, want %v", got, a2a.TaskStateCompleted)
	}
	if diff := cmp.Diff([]string{"before", "after", "before", "after"}, w.log()); diff != "" {
		t.Fatalf("turn boundaries wrong order (-want +got) diff = %s", diff)
	}
}

// failingRelease is a workspace in a shared directory whose Release fails.
type failingRelease struct{ dir string }

func (w failingRelease) Attach(context.Context, string) (a2aserve.Attachment, error) {
	return a2aserve.Attachment{Dir: w.dir, Release: func() error { return errors.New("release failed") }}, nil
}

func TestAgentLogsAFailedRelease(t *testing.T) {
	t.Parallel()
	var logs syncBuffer
	a, cancel := startAgent(t, a2aserve.Config{
		Main: a2aserve.Environment{Workspace: failingRelease{dir: t.TempDir()}, Home: filepath.Join(t.TempDir(), "home")},
		Log:  slog.New(slog.NewTextHandler(&logs, nil)),
	})
	defer cancel()

	if err := a.Close(context.Background()); err == nil || !strings.Contains(err.Error(), "release failed") {
		t.Fatalf("Agent.Close() error = %v, want the error of Release", err)
	}

	if got := logs.String(); !strings.Contains(got, "release failed") {
		t.Fatalf("Agent logs = %q, want the error of Release", got)
	}
}

// syncBuffer is a bytes.Buffer safe for concurrent use.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
