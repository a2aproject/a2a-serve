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
	"bytes"
	"context"
	"io/fs"
	"iter"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/core/fsys"
)

// filePartOf is what a test compares of a file part.
type filePartOf struct {
	Name, Content, Mode string
}

func TestChangesArtifactStaysWithinItsLimits(t *testing.T) {
	t.Parallel()
	limits := changesLimits{FileBytes: 4, TotalBytes: 6, Listed: 5}
	tests := []struct {
		name        string
		changes     func(t *testing.T, tree fsys.FS) []fsys.Info
		wantFiles   []filePartOf
		wantSummary map[string]any
	}{
		{
			name: "files, symlinks and removals",
			changes: func(t *testing.T, tree fsys.FS) []fsys.Info {
				return []fsys.Info{
					write(t, tree, "a.sh", "ab", 0o755),
					{Path: "dir", Mode: fs.ModeDir | 0o755},
					{Path: "link", Mode: fs.ModeSymlink | 0o777, Target: "a.sh"},
					{Path: "old.txt", Removed: true},
				}
			},
			wantFiles:   []filePartOf{{Name: "a.sh", Content: "ab", Mode: "0755"}},
			wantSummary: map[string]any{"removed": []any{"old.txt"}, "symlinks": []any{map[string]any{"path": "link", "target": "a.sh"}}},
		},
		{
			name: "a file over the file limit",
			changes: func(t *testing.T, tree fsys.FS) []fsys.Info {
				return []fsys.Info{write(t, tree, "big", "12345", 0o644)}
			},
			wantSummary: map[string]any{"removed": []any{}, "omitted": []any{map[string]any{"path": "big", "size": int64(5), "reason": omittedFileTooLarge}}},
		},
		{
			name: "files over the total limit",
			changes: func(t *testing.T, tree fsys.FS) []fsys.Info {
				return []fsys.Info{write(t, tree, "a", "1234", 0o644), write(t, tree, "b", "123", 0o644), write(t, tree, "c", "12", 0o644)}
			},
			wantFiles:   []filePartOf{{Name: "a", Content: "1234", Mode: "0644"}, {Name: "c", Content: "12", Mode: "0644"}},
			wantSummary: map[string]any{"removed": []any{}, "omitted": []any{map[string]any{"path": "b", "size": int64(3), "reason": omittedTotalTooLarge}}},
		},
		{
			name: "a file that another writer changed since the sync",
			changes: func(t *testing.T, tree fsys.FS) []fsys.Info {
				synced := write(t, tree, "a", "ab", 0o644)
				write(t, tree, "a", "cd", 0o644)
				return []fsys.Info{synced}
			},
			wantSummary: map[string]any{"removed": []any{}, "omitted": []any{map[string]any{"path": "a", "size": int64(2), "reason": omittedChanged}}},
		},
		{
			name: "a file that is gone",
			changes: func(t *testing.T, tree fsys.FS) []fsys.Info {
				return []fsys.Info{{Path: "gone", Mode: 0o644, Size: 1}}
			},
			wantSummary: map[string]any{"removed": []any{}, "omitted": []any{map[string]any{"path": "gone", "size": int64(1), "reason": omittedUnreadable}}},
		},
		{
			name: "more paths than listed",
			changes: func(t *testing.T, tree fsys.FS) []fsys.Info {
				var out []fsys.Info
				for p := range strings.SplitSeq("a b c d e f g", " ") {
					out = append(out, fsys.Info{Path: p, Removed: true})
				}
				return out
			},
			wantSummary: map[string]any{"removed": []any{"a", "b", "c", "d", "e"}, "unlisted": 2},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tree := fsys.Memory()
			c := newChangesets(tree, limits, newMemory(defaultMemoryLimits, time.Now), slog.New(slog.DiscardHandler))

			art := c.artifact(context.Background(), tc.changes(t, tree))

			if art.Name != ChangesArtifact {
				t.Fatalf("artifact name = %q, want %q", art.Name, ChangesArtifact)
			}
			if diff := cmp.Diff(tc.wantSummary, art.Parts[0].Data()); diff != "" {
				t.Fatalf("artifact summary wrong result (-want +got) diff = %s", diff)
			}
			var files []filePartOf
			for _, p := range art.Parts[1:] {
				files = append(files, filePartOf{Name: p.Filename, Content: string(p.Raw()), Mode: p.Metadata["mode"].(string)})
			}
			if diff := cmp.Diff(tc.wantFiles, files); diff != "" {
				t.Fatalf("artifact files wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestChangesGoOutBeforeTheFinalEventOfTheirTask(t *testing.T) {
	t.Parallel()
	tree := fsys.Memory()
	store := newMemory(defaultMemoryLimits, time.Now)
	c := newChangesets(tree, defaultChangesLimits, store, slog.New(slog.DiscardHandler))
	task := &a2asrv.ExecutorContext{TaskID: "task-1", ContextID: "ctx"}
	inner := fakeExecutor(func(ctx context.Context, yield func(a2a.Event, error) bool) {
		if !yield(a2a.NewStatusUpdateEvent(task, a2a.TaskStateWorking, nil), nil) {
			return
		}
		c.synced(ctx, "ctx", []fsys.Info{write(t, tree, "a.txt", "a", 0o644)})
		yield(a2a.NewStatusUpdateEvent(task, a2a.TaskStateCompleted, nil), nil)
	})

	var got []string
	for ev, err := range c.middleware(inner).Execute(context.Background(), task) {
		if err != nil {
			t.Fatalf("Execute() error = %v", err)
		}
		switch ev := ev.(type) {
		case *a2a.TaskStatusUpdateEvent:
			got = append(got, string(ev.Status.State))
		case *a2a.TaskArtifactUpdateEvent:
			got = append(got, ev.Artifact.Name)
		}
	}

	want := []string{string(a2a.TaskStateWorking), ChangesArtifact, string(a2a.TaskStateCompleted)}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("Execute() events wrong result (-want +got) diff = %s", diff)
	}
	if art := store.takeArtifact(task.TaskID); art != nil {
		t.Fatalf("memory.takeArtifact() after the final event = %v, want nil", art)
	}
}

func TestChangesLeaveOutTheGitDirectory(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		changes   []string
		wantFiles []string
	}{
		{name: "only git internals", changes: []string{".git/opencode", ".git/objects/ab/cdef"}},
		{name: "git internals and a file", changes: []string{".git/opencode", "main.go", "sub/.git/HEAD"}, wantFiles: []string{"main.go"}},
		{name: "a file named like the git directory", changes: []string{".gitignore"}, wantFiles: []string{".gitignore"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tree := fsys.Memory()
			store := newMemory(defaultMemoryLimits, time.Now)
			c := newChangesets(tree, defaultChangesLimits, store, slog.New(slog.DiscardHandler))
			var changes []fsys.Info
			for _, p := range tc.changes {
				changes = append(changes, write(t, tree, p, "x", 0o644))
			}
			ctx := context.WithValue(context.Background(), taskKey{}, a2a.TaskID("task-1"))

			c.synced(ctx, "ctx", changes)

			art := store.takeArtifact("task-1")
			var got []string
			if art != nil {
				for _, p := range art.Parts[1:] {
					got = append(got, p.Filename)
				}
			}
			if diff := cmp.Diff(tc.wantFiles, got); diff != "" {
				t.Fatalf("changesets.synced() artifact files wrong result (-want +got) diff = %s", diff)
			}
			if len(tc.wantFiles) == 0 && art != nil {
				t.Fatalf("changesets.synced() artifact = %v, want none", art.Name)
			}
		})
	}
}

func TestChangesWithoutATurnAreLogged(t *testing.T) {
	t.Parallel()
	tree := fsys.Memory()
	var logs bytes.Buffer
	c := newChangesets(tree, defaultChangesLimits, newMemory(defaultMemoryLimits, time.Now), slog.New(slog.NewTextHandler(&logs, nil)))

	c.synced(context.Background(), "ctx-1", []fsys.Info{write(t, tree, "late.txt", "a", 0o644), {Path: "gone.txt", Removed: true}})

	if got := logs.String(); !strings.Contains(got, "context=ctx-1") || !strings.Contains(got, "late.txt") || !strings.Contains(got, "gone.txt") {
		t.Fatalf("log of changes without a turn = %q, want the context and the paths", got)
	}
}

func write(t *testing.T, tree fsys.FS, p, data string, mode fs.FileMode) fsys.Info {
	t.Helper()
	info, err := tree.Write(context.Background(), p, strings.NewReader(data), mode, "")
	if err != nil {
		t.Fatalf("FS.Write(%q) error = %v", p, err)
	}
	return info
}

// fakeExecutor is an executor whose Execute runs the function.
type fakeExecutor func(ctx context.Context, yield func(a2a.Event, error) bool)

func (f fakeExecutor) Execute(ctx context.Context, _ *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) { f(ctx, yield) }
}

func (f fakeExecutor) Cancel(context.Context, *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(func(a2a.Event, error) bool) {}
}
