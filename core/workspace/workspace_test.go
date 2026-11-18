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

package workspace_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/a2aproject/a2a-serve/core/workspace"
	"github.com/google/go-cmp/cmp"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newRoot(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "private")
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func attach(t *testing.T, w workspace.Workspace, key string) workspace.Attachment {
	t.Helper()
	a, err := w.Attach(context.Background(), key)
	if err != nil {
		t.Fatalf("Attach(%q) error = %v", key, err)
	}
	if a.Release == nil {
		t.Fatalf("Attach(%q).Release = nil, want a function", key)
	}
	return a
}

func TestDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w := workspace.Dir(dir)
	got := attach(t, w, "a")
	want := workspace.Attachment{Dir: dir}
	if diff := cmp.Diff(want, got, cmp.FilterPath(func(p cmp.Path) bool { return p.Last().String() == ".Release" }, cmp.Ignore())); diff != "" {
		t.Fatalf("Dir().Attach() wrong attachment (-want +got) diff = %s", diff)
	}
	if err := got.Release(); err != nil {
		t.Fatalf("Release() error = %v", err)
	}
	if other := attach(t, w, "b"); other.Dir != dir {
		t.Fatalf("Dir().Attach(b).Dir = %q, want the shared %q", other.Dir, dir)
	}
}

func TestReadOnly(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tests := []struct {
		name string
		w    workspace.Workspace
		want workspace.Attachment
	}{
		{name: "dir", w: workspace.ReadOnly(workspace.Dir(dir)), want: workspace.Attachment{Dir: dir, ReadOnly: true}},
		{name: "empty", w: workspace.ReadOnly(workspace.Empty(newRoot(t), "/work")), want: workspace.Attachment{Path: "/work", ReadOnly: true}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := attach(t, tc.w, "k")
			if tc.want.Dir == "" {
				tc.want.Dir = got.Dir
			}
			opt := cmp.FilterPath(func(p cmp.Path) bool { return p.Last().String() == ".Release" }, cmp.Ignore())
			if diff := cmp.Diff(tc.want, got, opt); diff != "" {
				t.Fatalf("Attach() wrong attachment (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestCopiesCopyOnceAndKeepEdits(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "main.go"), "package main")
	writeFile(t, filepath.Join(src, "sub", "a.txt"), "a")
	writeFile(t, filepath.Join(src, ".a2a", "state", "secrets"), "KEY=v")
	w := workspace.Copies(src, workspace.CopyOptions{Dir: newRoot(t)})

	first := attach(t, w, "user:pair:anna#0123")
	if first.Path != src {
		t.Fatalf("Attach().Path = %q, want %q", first.Path, src)
	}
	if first.ReadOnly {
		t.Fatalf("Attach().ReadOnly = true, want false")
	}
	if first.Dir == src {
		t.Fatalf("Attach().Dir = %q, want a copy", first.Dir)
	}
	if got := readFile(t, filepath.Join(first.Dir, "sub", "a.txt")); got != "a" {
		t.Fatalf("copy of sub/a.txt = %q, want a", got)
	}
	if _, err := os.Lstat(filepath.Join(first.Dir, ".a2a")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Lstat(copy/.a2a) error = %v, want not exist", err)
	}
	if err := first.Release(); err != nil {
		t.Fatalf("Release() error = %v", err)
	}

	writeFile(t, filepath.Join(first.Dir, "main.go"), "edited")
	writeFile(t, filepath.Join(src, "new.go"), "new")
	second := attach(t, w, "user:pair:anna#0123")
	if second.Dir != first.Dir {
		t.Fatalf("second Attach().Dir = %q, want %q", second.Dir, first.Dir)
	}
	if got := readFile(t, filepath.Join(second.Dir, "main.go")); got != "edited" {
		t.Fatalf("copy of main.go = %q, want the edit", got)
	}
	if _, err := os.Lstat(filepath.Join(second.Dir, "new.go")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Lstat(copy/new.go) error = %v, want not exist: the copy is made once", err)
	}
	if got := readFile(t, filepath.Join(src, "main.go")); got != "package main" {
		t.Fatalf("src main.go = %q, want unchanged", got)
	}
}

func TestCopiesKeysGetOwnCopies(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "main.go"), "package main")
	w := workspace.Copies(src, workspace.CopyOptions{Dir: newRoot(t)})
	keys := []string{"plain", "user:pair:anna#0123abcd", "a/b/../c", "../escape", "..", ".", "ключ", "with space", "a_b", "a:b", strings.Repeat("x", 300), strings.Repeat("x", 301)}
	dirs := map[string]string{}
	for _, key := range keys {
		a := attach(t, w, key)
		if prev, ok := dirs[a.Dir]; ok {
			t.Fatalf("Attach(%q).Dir = %q, want a directory other than the one of %q", key, a.Dir, prev)
		}
		dirs[a.Dir] = key
		if filepath.Dir(a.Dir) == a.Dir || !strings.HasPrefix(a.Dir, filepath.Dir(filepath.Dir(a.Dir))) {
			t.Fatalf("Attach(%q).Dir = %q, want a path under the copies directory", key, a.Dir)
		}
		writeFile(t, filepath.Join(a.Dir, "mark"), key)
	}
	for _, key := range keys {
		a := attach(t, w, key)
		if got := readFile(t, filepath.Join(a.Dir, "mark")); got != key {
			t.Fatalf("Attach(%q) mark = %q, want %q", key, got, key)
		}
	}
}

func TestCopiesDirectoryNameIsDeterministic(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "main.go"), "package main")
	root := newRoot(t)
	a := attach(t, workspace.Copies(src, workspace.CopyOptions{Dir: root}), "user:pair:anna#0123")
	b := attach(t, workspace.Copies(src, workspace.CopyOptions{Dir: root}), "user:pair:anna#0123")
	if a.Dir != b.Dir {
		t.Fatalf("Attach().Dir = %q and %q, want the same for the same key", a.Dir, b.Dir)
	}
}

func TestCopiesLimits(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "big"), strings.Repeat("x", 2048))
	writeFile(t, filepath.Join(src, "small"), "x")
	tests := []struct {
		name string
		opts workspace.CopyOptions
		keys []string
		want error
	}{
		{name: "bytes", opts: workspace.CopyOptions{MaxBytes: 1024}, keys: []string{"a"}, want: workspace.ErrTooLarge},
		{name: "entries", opts: workspace.CopyOptions{MaxEntries: 1}, keys: []string{"a"}, want: workspace.ErrTooLarge},
		{name: "copies", opts: workspace.CopyOptions{MaxCopies: 1}, keys: []string{"a", "b"}, want: workspace.ErrTooMany},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.opts.Dir = newRoot(t)
			w := workspace.Copies(src, tc.opts)
			var err error
			for i, key := range tc.keys {
				_, err = w.Attach(context.Background(), key)
				if i < len(tc.keys)-1 && err != nil {
					t.Fatalf("Attach(%q) error = %v, want nil", key, err)
				}
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("Attach() error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestCopiesFailedCopyLeavesNone(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "big"), strings.Repeat("x", 2048))
	root := newRoot(t)
	w := workspace.Copies(src, workspace.CopyOptions{Dir: root, MaxBytes: 1024})
	a, err := w.Attach(context.Background(), "k")
	if !errors.Is(err, workspace.ErrTooLarge) {
		t.Fatalf("Attach() = %+v, %v, want %v", a, err, workspace.ErrTooLarge)
	}
	if err := os.Remove(filepath.Join(src, "big")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Attach(context.Background(), "k"); err != nil {
		t.Fatalf("Attach() after the source shrank error = %v, want nil", err)
	}
}

func TestCopiesAttachConcurrently(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	for i := range 20 {
		writeFile(t, filepath.Join(src, "d", string(rune('a'+i))), "x")
	}
	w := workspace.Copies(src, workspace.CopyOptions{Dir: newRoot(t)})
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			a, err := w.Attach(context.Background(), "same")
			if err == nil {
				err = a.Release()
			}
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Attach() error = %v, want nil", err)
		}
	}
}

func TestCopiesMissingSource(t *testing.T) {
	t.Parallel()
	w := workspace.Copies(filepath.Join(t.TempDir(), "missing"), workspace.CopyOptions{Dir: newRoot(t)})
	if a, err := w.Attach(context.Background(), "k"); err == nil {
		t.Fatalf("Attach() = %+v, nil, want an error", a)
	}
}

func TestAttachEmptyKey(t *testing.T) {
	t.Parallel()
	for name, w := range map[string]workspace.Workspace{
		"copies": workspace.Copies(t.TempDir(), workspace.CopyOptions{Dir: newRoot(t)}),
		"empty":  workspace.Empty(newRoot(t), ""),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if a, err := w.Attach(context.Background(), ""); err == nil {
				t.Fatalf("Attach() = %+v, nil, want an error", a)
			}
		})
	}
}

func TestEmpty(t *testing.T) {
	t.Parallel()
	root := newRoot(t)
	tests := []struct {
		name     string
		at       string
		key      string
		wantPath func(dir string) string
	}{
		{name: "at path", at: "/work", key: "user:pair:ben#ff00", wantPath: func(string) string { return "/work" }},
		{name: "no path", key: "a/b", wantPath: func(string) string { return "" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := workspace.Empty(filepath.Join(root, tc.name), tc.at)
			a := attach(t, w, tc.key)
			if want := tc.wantPath(a.Dir); a.Path != want {
				t.Fatalf("Attach().Path = %q, want %q", a.Path, want)
			}
			entries, err := os.ReadDir(a.Dir)
			if err != nil || len(entries) != 0 {
				t.Fatalf("ReadDir(%q) = %v, %v, want an empty directory", a.Dir, entries, err)
			}
			writeFile(t, filepath.Join(a.Dir, "note"), "kept")
			if err := a.Release(); err != nil {
				t.Fatalf("Release() error = %v", err)
			}
			again := attach(t, w, tc.key)
			if again.Dir != a.Dir || readFile(t, filepath.Join(again.Dir, "note")) != "kept" {
				t.Fatalf("second Attach() = %+v, want the same directory with its files", again)
			}
			other := attach(t, w, tc.key+"x")
			if other.Dir == a.Dir {
				t.Fatalf("Attach(%q).Dir = %q, want a directory of its own", tc.key+"x", other.Dir)
			}
		})
	}
}
