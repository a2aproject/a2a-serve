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

package fsys_test

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/a2aproject/a2a-serve/core/fsys"
)

// impls are the trees under test, each with a feed of maxChanges changes.
var impls = map[string]func(t *testing.T, maxChanges int) fsys.FS{
	"memory": func(t *testing.T, maxChanges int) fsys.FS { return fsys.NewMemory(maxChanges) },
	"dir": func(t *testing.T, maxChanges int) fsys.FS {
		return fsys.NewDir(t.TempDir(), maxChanges, 1000, 20*time.Millisecond)
	},
}

// forEach runs test against every tree.
func forEach(t *testing.T, test func(t *testing.T, newFS func(maxChanges int) fsys.FS)) {
	t.Helper()
	for name, newFS := range impls {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			test(t, func(maxChanges int) fsys.FS { return newFS(t, maxChanges) })
		})
	}
}

func newTree(t *testing.T, newFS func(int) fsys.FS) fsys.FS {
	t.Helper()
	return newFS(100)
}

func write(t *testing.T, f fsys.FS, path, data string, mode fs.FileMode) fsys.Info {
	t.Helper()
	info, err := f.Write(context.Background(), path, strings.NewReader(data), mode, "")
	if err != nil {
		t.Fatalf("FS.Write(%q) error = %v", path, err)
	}
	return info
}

func read(t *testing.T, f fsys.FS, path string) string {
	t.Helper()
	rc, err := f.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("FS.Open(%q) error = %v", path, err)
	}
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("reading %q error = %v", path, err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("closing %q error = %v", path, err)
	}
	return string(data)
}

func stat(t *testing.T, f fsys.FS, path string) fsys.Info {
	t.Helper()
	info, err := f.Stat(context.Background(), path)
	if err != nil {
		t.Fatalf("FS.Stat(%q) error = %v", path, err)
	}
	return info
}

// entry is what the tests compare of an Info.
type entry struct {
	Path    string
	Mode    fs.FileMode
	Size    int64
	Target  string
	Removed bool
}

func entries(infos []fsys.Info) []entry {
	out := make([]entry, len(infos))
	for i, info := range infos {
		out[i] = entry{Path: info.Path, Mode: info.Mode, Size: info.Size, Target: info.Target, Removed: info.Removed}
	}
	return out
}

func TestWriteCreatesFileAndParents(t *testing.T) {
	t.Parallel()
	forEach(t, func(t *testing.T, newFS func(int) fsys.FS) {
		f := newTree(t, newFS)
		info := write(t, f, "a/b/c.txt", "hello", 0o644)

		if diff := cmp.Diff(entry{Path: "a/b/c.txt", Mode: 0o644, Size: 5}, entries([]fsys.Info{info})[0]); diff != "" {
			t.Fatalf("FS.Write() wrong info (-want +got) diff = %s", diff)
		}
		if info.Version == "" {
			t.Fatalf("FS.Write() version = %q, want one", info.Version)
		}
		if got := read(t, f, "a/b/c.txt"); got != "hello" {
			t.Fatalf("FS.Open() content = %q, want %q", got, "hello")
		}
		got, err := f.ReadDir(context.Background(), "a")
		if err != nil {
			t.Fatalf("FS.ReadDir(a) error = %v", err)
		}
		if diff := cmp.Diff([]entry{{Path: "a/b", Mode: fs.ModeDir | 0o755}}, entries(got)); diff != "" {
			t.Fatalf("FS.ReadDir(a) wrong entries (-want +got) diff = %s", diff)
		}
		if !stat(t, f, ".").Mode.IsDir() {
			t.Fatalf("FS.Stat(.) isn't a directory")
		}
	})
}

func TestWriteKeepsMode(t *testing.T) {
	t.Parallel()
	modes := []fs.FileMode{0o644, 0o755, 0o600, 0o700}
	forEach(t, func(t *testing.T, newFS func(int) fsys.FS) {
		f := newTree(t, newFS)
		for _, mode := range modes {
			path := "f-" + mode.String()
			write(t, f, path, "x", mode|fs.ModeSetuid)
			if got := stat(t, f, path).Mode; got != mode {
				t.Fatalf("FS.Stat(%q).Mode = %v, want %v", path, got, mode)
			}
		}
	})
}

func TestWriteChecksVersion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		setup     func(t *testing.T, f fsys.FS) (ifVersion string)
		path      string
		wantErr   error
		wantAfter string
	}{
		{name: "unconditional over a file", path: "f", setup: func(t *testing.T, f fsys.FS) string { write(t, f, "f", "old", 0o644); return "" }, wantAfter: "new"},
		{name: "matching version", path: "f", setup: func(t *testing.T, f fsys.FS) string { return write(t, f, "f", "old", 0o644).Version }, wantAfter: "new"},
		{
			name: "stale version",
			path: "f",
			setup: func(t *testing.T, f fsys.FS) string {
				v := write(t, f, "f", "old", 0o644).Version
				time.Sleep(10 * time.Millisecond)
				write(t, f, "f", "theirs", 0o644)
				return v
			},
			wantErr:   fsys.ErrConflict,
			wantAfter: "theirs",
		},
		{name: "version of a missing file", path: "f", setup: func(t *testing.T, f fsys.FS) string { return "1" }, wantErr: fsys.ErrConflict},
		{name: "only if missing, missing", path: "f", setup: func(t *testing.T, f fsys.FS) string { return fsys.IfMissing }, wantAfter: "new"},
		{
			name:      "only if missing, present",
			path:      "f",
			setup:     func(t *testing.T, f fsys.FS) string { write(t, f, "f", "theirs", 0o644); return fsys.IfMissing },
			wantErr:   fsys.ErrConflict,
			wantAfter: "theirs",
		},
	}
	forEach(t, func(t *testing.T, newFS func(int) fsys.FS) {
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				f := newTree(t, newFS)
				ifVersion := tc.setup(t, f)
				info, err := f.Write(context.Background(), tc.path, strings.NewReader("new"), 0o644, ifVersion)
				if !errors.Is(err, tc.wantErr) || (tc.wantErr == nil) != (err == nil) {
					t.Fatalf("FS.Write() error = %v, want %v", err, tc.wantErr)
				}
				if err == nil && info.Version == ifVersion {
					t.Fatalf("FS.Write() version = %q, want a new one", info.Version)
				}
				if tc.wantAfter == "" {
					if _, err := f.Stat(context.Background(), tc.path); !errors.Is(err, fs.ErrNotExist) {
						t.Fatalf("FS.Stat() after a refused write error = %v, want %v", err, fs.ErrNotExist)
					}
					return
				}
				if got := read(t, f, tc.path); got != tc.wantAfter {
					t.Fatalf("FS.Open() after the write = %q, want %q", got, tc.wantAfter)
				}
			})
		}
	})
}

func TestWriteRefusesDirectory(t *testing.T) {
	t.Parallel()
	forEach(t, func(t *testing.T, newFS func(int) fsys.FS) {
		f := newTree(t, newFS)
		write(t, f, "d/f", "x", 0o644)
		if _, err := f.Write(context.Background(), "d", strings.NewReader("x"), 0o644, ""); err == nil {
			t.Fatalf("FS.Write() over a directory error = nil, want one")
		}
		if _, err := f.Write(context.Background(), "d/f/g", strings.NewReader("x"), 0o644, ""); err == nil {
			t.Fatalf("FS.Write() under a file error = nil, want one")
		}
	})
}

func TestMkdir(t *testing.T) {
	t.Parallel()
	forEach(t, func(t *testing.T, newFS func(int) fsys.FS) {
		f := newTree(t, newFS)
		ctx := context.Background()
		if err := f.Mkdir(ctx, "a/b", 0o700); err != nil {
			t.Fatalf("FS.Mkdir(a/b) error = %v", err)
		}
		if got := stat(t, f, "a/b").Mode; got != fs.ModeDir|0o700 {
			t.Fatalf("FS.Stat(a/b).Mode = %v, want %v", got, fs.ModeDir|0o700)
		}
		if got := stat(t, f, "a").Mode; !got.IsDir() {
			t.Fatalf("FS.Stat(a).Mode = %v, want a directory", got)
		}
		if err := f.Mkdir(ctx, "a/b", 0o755); err != nil {
			t.Fatalf("FS.Mkdir() of an existing directory error = %v, want nil", err)
		}
		write(t, f, "file", "x", 0o644)
		if err := f.Mkdir(ctx, "file", 0o755); !errors.Is(err, fs.ErrExist) {
			t.Fatalf("FS.Mkdir() over a file error = %v, want %v", err, fs.ErrExist)
		}
	})
}

func TestSymlink(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	forEach(t, func(t *testing.T, newFS func(int) fsys.FS) {
		f := newTree(t, newFS)
		ctx := context.Background()
		write(t, f, "target.txt", "hello", 0o644)
		if err := f.Symlink(ctx, "../target.txt", "links/rel"); err != nil {
			t.Fatalf("FS.Symlink() error = %v", err)
		}
		if err := f.Symlink(ctx, "/etc/passwd", "links/abs"); err != nil {
			t.Fatalf("FS.Symlink() with an absolute target error = %v", err)
		}
		got, err := f.ReadDir(ctx, "links")
		if err != nil {
			t.Fatalf("FS.ReadDir(links) error = %v", err)
		}
		want := []entry{{Path: "links/abs", Mode: fs.ModeSymlink | 0o777, Size: int64(len("/etc/passwd")), Target: "/etc/passwd"}, {Path: "links/rel", Mode: fs.ModeSymlink | 0o777, Size: int64(len("../target.txt")), Target: "../target.txt"}}
		if diff := cmp.Diff(want, entries(got), cmpopts.IgnoreFields(entry{}, "Size")); diff != "" {
			t.Fatalf("FS.ReadDir(links) wrong entries (-want +got) diff = %s", diff)
		}
		if _, err := f.Open(ctx, "links/abs"); err == nil {
			t.Fatalf("FS.Open() of a symlink error = nil, want one")
		}
		if err := f.Symlink(ctx, "x", "target.txt"); !errors.Is(err, fs.ErrExist) {
			t.Fatalf("FS.Symlink() over a file error = %v, want %v", err, fs.ErrExist)
		}
		if err := f.Symlink(ctx, "", "empty"); !errors.Is(err, fs.ErrInvalid) {
			t.Fatalf("FS.Symlink() to an empty target error = %v, want %v", err, fs.ErrInvalid)
		}
	})
}

func TestRename(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		from, to string
		wantErr  bool
		want     map[string]string
		gone     []string
	}{
		{name: "file to a new directory", from: "a.txt", to: "new/b.txt", want: map[string]string{"new/b.txt": "a"}, gone: []string{"a.txt"}},
		{name: "file over a file", from: "a.txt", to: "dir/c.txt", want: map[string]string{"dir/c.txt": "a"}, gone: []string{"a.txt"}},
		{name: "directory with contents", from: "dir", to: "moved/dir", want: map[string]string{"moved/dir/c.txt": "c", "moved/dir/sub/d.txt": "d"}, gone: []string{"dir"}},
		{name: "directory over a directory", from: "dir", to: "empty", wantErr: true},
		{name: "directory over a file", from: "dir", to: "a.txt", wantErr: true},
		{name: "file over a directory", from: "a.txt", to: "dir", wantErr: true},
		{name: "directory into itself", from: "dir", to: "dir/sub/x", wantErr: true},
		{name: "missing", from: "missing", to: "x", wantErr: true},
	}
	forEach(t, func(t *testing.T, newFS func(int) fsys.FS) {
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				f := newTree(t, newFS)
				ctx := context.Background()
				write(t, f, "a.txt", "a", 0o644)
				write(t, f, "dir/c.txt", "c", 0o644)
				write(t, f, "dir/sub/d.txt", "d", 0o644)
				if err := f.Mkdir(ctx, "empty", 0o755); err != nil {
					t.Fatalf("FS.Mkdir() error = %v", err)
				}
				err := f.Rename(ctx, tc.from, tc.to)
				if (err != nil) != tc.wantErr {
					t.Fatalf("FS.Rename(%q, %q) error = %v, want error %v", tc.from, tc.to, err, tc.wantErr)
				}
				for path, data := range tc.want {
					if got := read(t, f, path); got != data {
						t.Fatalf("FS.Open(%q) after the rename = %q, want %q", path, got, data)
					}
				}
				for _, path := range tc.gone {
					if _, err := f.Stat(ctx, path); !errors.Is(err, fs.ErrNotExist) {
						t.Fatalf("FS.Stat(%q) after the rename error = %v, want %v", path, err, fs.ErrNotExist)
					}
				}
			})
		}
	})
}

func TestRemove(t *testing.T) {
	t.Parallel()
	forEach(t, func(t *testing.T, newFS func(int) fsys.FS) {
		f := newTree(t, newFS)
		ctx := context.Background()
		write(t, f, "dir/a/b.txt", "b", 0o644)
		write(t, f, "keep.txt", "k", 0o644)
		if err := f.Remove(ctx, "dir"); err != nil {
			t.Fatalf("FS.Remove(dir) error = %v", err)
		}
		if _, err := f.Stat(ctx, "dir/a/b.txt"); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("FS.Stat() under a removed directory error = %v, want %v", err, fs.ErrNotExist)
		}
		got, err := f.ReadDir(ctx, ".")
		if err != nil {
			t.Fatalf("FS.ReadDir(.) error = %v", err)
		}
		if diff := cmp.Diff([]entry{{Path: "keep.txt", Mode: 0o644, Size: 1}}, entries(got)); diff != "" {
			t.Fatalf("FS.ReadDir(.) after the removal wrong entries (-want +got) diff = %s", diff)
		}
		if err := f.Remove(ctx, "dir"); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("FS.Remove() of a missing path error = %v, want %v", err, fs.ErrNotExist)
		}
		if err := f.Remove(ctx, "."); !errors.Is(err, fs.ErrInvalid) {
			t.Fatalf("FS.Remove(.) error = %v, want %v", err, fs.ErrInvalid)
		}
	})
}

func TestRefusesInvalidPaths(t *testing.T) {
	t.Parallel()
	paths := []string{"", "/etc/passwd", "..", "../x", "a/../../x", "a/../b", "./a", "a/", "a//b", "a\x00b"}
	ops := map[string]func(f fsys.FS, p string) error{
		"Stat":    func(f fsys.FS, p string) error { _, err := f.Stat(context.Background(), p); return err },
		"ReadDir": func(f fsys.FS, p string) error { _, err := f.ReadDir(context.Background(), p); return err },
		"Open":    func(f fsys.FS, p string) error { _, err := f.Open(context.Background(), p); return err },
		"Write": func(f fsys.FS, p string) error {
			_, err := f.Write(context.Background(), p, strings.NewReader("x"), 0o644, "")
			return err
		},
		"Mkdir":       func(f fsys.FS, p string) error { return f.Mkdir(context.Background(), p, 0o755) },
		"Symlink":     func(f fsys.FS, p string) error { return f.Symlink(context.Background(), "x", p) },
		"Rename from": func(f fsys.FS, p string) error { return f.Rename(context.Background(), p, "x") },
		"Rename to":   func(f fsys.FS, p string) error { return f.Rename(context.Background(), "ok", p) },
		"Remove":      func(f fsys.FS, p string) error { return f.Remove(context.Background(), p) },
	}
	forEach(t, func(t *testing.T, newFS func(int) fsys.FS) {
		f := newTree(t, newFS)
		write(t, f, "ok", "x", 0o644)
		for name, op := range ops {
			for _, p := range paths {
				if err := op(f, p); !errors.Is(err, fs.ErrInvalid) {
					t.Fatalf("FS.%s(%q) error = %v, want %v", name, p, err, fs.ErrInvalid)
				}
			}
		}
	})
}

// changes returns the changes after cursor that are there now.
func changes(t *testing.T, f fsys.FS, cursor string) ([]fsys.Info, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()
	got, next, err := f.Changes(ctx, cursor)
	if err != nil {
		t.Fatalf("FS.Changes() error = %v", err)
	}
	return got, next
}

func TestChanges(t *testing.T) {
	t.Parallel()
	forEach(t, func(t *testing.T, newFS func(int) fsys.FS) {
		f := newTree(t, newFS)
		ctx := context.Background()
		write(t, f, "old.txt", "old", 0o644)
		got, cursor := changes(t, f, "")
		if len(got) != 0 || cursor == "" {
			t.Fatalf("FS.Changes(\"\") = %v, %q, want no changes and a cursor", got, cursor)
		}

		file := write(t, f, "new.txt", "new", 0o755)
		if err := f.Mkdir(ctx, "dir", 0o755); err != nil {
			t.Fatalf("FS.Mkdir() error = %v", err)
		}
		got, cursor = changes(t, f, cursor)
		if diff := cmp.Diff([]entry{{Path: "dir", Mode: fs.ModeDir | 0o755}, {Path: "new.txt", Mode: 0o755, Size: 3}}, entries(got), cmpopts.SortSlices(func(a, b entry) bool { return a.Path < b.Path })); diff != "" {
			t.Fatalf("FS.Changes() wrong changes (-want +got) diff = %s", diff)
		}
		for _, c := range got {
			if c.Path == "new.txt" && c.Version != file.Version {
				t.Fatalf("FS.Changes() version of new.txt = %q, want the version of its write %q", c.Version, file.Version)
			}
		}

		if err := f.Remove(ctx, "old.txt"); err != nil {
			t.Fatalf("FS.Remove() error = %v", err)
		}
		got, cursor = changes(t, f, cursor)
		if diff := cmp.Diff([]entry{{Path: "old.txt", Removed: true}}, entries(got)); diff != "" {
			t.Fatalf("FS.Changes() after a removal wrong changes (-want +got) diff = %s", diff)
		}
		if got, _ := changes(t, f, cursor); len(got) != 0 {
			t.Fatalf("FS.Changes() with no change = %v, want none", got)
		}
	})
}

func TestChangesWaitsForChange(t *testing.T) {
	t.Parallel()
	forEach(t, func(t *testing.T, newFS func(int) fsys.FS) {
		f := newTree(t, newFS)
		_, cursor := changes(t, f, "")
		type result struct {
			got []fsys.Info
			err error
		}
		done := make(chan result, 1)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			got, _, err := f.Changes(ctx, cursor)
			done <- result{got, err}
		}()
		time.Sleep(50 * time.Millisecond)
		write(t, f, "late.txt", "x", 0o644)
		r := <-done
		if r.err != nil {
			t.Fatalf("FS.Changes() error = %v", r.err)
		}
		if diff := cmp.Diff([]entry{{Path: "late.txt", Mode: 0o644, Size: 1}}, entries(r.got)); diff != "" {
			t.Fatalf("FS.Changes() wrong changes (-want +got) diff = %s", diff)
		}
	})
}

func TestChangesEndsWithContext(t *testing.T) {
	t.Parallel()
	forEach(t, func(t *testing.T, newFS func(int) fsys.FS) {
		f := newTree(t, newFS)
		_, cursor := changes(t, f, "")
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		got, next, err := f.Changes(ctx, cursor)
		if err != nil || len(got) != 0 || next != cursor {
			t.Fatalf("FS.Changes() past the deadline = %v, %q, %v, want no changes, %q, nil", got, next, err, cursor)
		}
		ctx, cancel = context.WithCancel(context.Background())
		cancel()
		if _, _, err := f.Changes(ctx, cursor); !errors.Is(err, context.Canceled) {
			t.Fatalf("FS.Changes() with a canceled context error = %v, want %v", err, context.Canceled)
		}
	})
}

func TestChangesAsksToResync(t *testing.T) {
	t.Parallel()
	forEach(t, func(t *testing.T, newFS func(int) fsys.FS) {
		f := newFS(2)
		_, start := changes(t, f, "")
		cursor := start
		for i := range 6 {
			write(t, f, "f"+string(rune('a'+i)), "x", 0o644)
			_, cursor = changes(t, f, cursor)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 0)
		defer cancel()
		_, next, err := f.Changes(ctx, start)
		if !errors.Is(err, fsys.ErrResync) {
			t.Fatalf("FS.Changes() of an old cursor error = %v, want %v", err, fsys.ErrResync)
		}
		if got, _ := changes(t, f, next); len(got) != 0 {
			t.Fatalf("FS.Changes() of the cursor of a resync = %v, want no changes", got)
		}
		for _, cursor := range []string{"garbage", "0.0"} {
			if _, _, err := f.Changes(ctx, cursor); !errors.Is(err, fsys.ErrResync) {
				t.Fatalf("FS.Changes(%q) error = %v, want %v", cursor, err, fsys.ErrResync)
			}
		}
		_, other := changes(t, newFS(2), "")
		if _, _, err := f.Changes(ctx, other); !errors.Is(err, fsys.ErrResync) {
			t.Fatalf("FS.Changes() with the cursor of another tree error = %v, want %v", err, fsys.ErrResync)
		}
	})
}

func TestDirSeesOutsideChanges(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	f := fsys.NewDir(root, 100, 1000, 20*time.Millisecond)
	_, cursor := changes(t, f, "")
	go func() {
		time.Sleep(50 * time.Millisecond)
		if err := os.WriteFile(filepath.Join(root, "outside.txt"), []byte("x"), 0o644); err != nil {
			t.Errorf("os.WriteFile() error = %v", err)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	got, _, err := f.Changes(ctx, cursor)
	if err != nil {
		t.Fatalf("FS.Changes() error = %v", err)
	}
	if diff := cmp.Diff([]entry{{Path: "outside.txt", Size: 1}}, entries(got), cmpopts.IgnoreFields(entry{}, "Mode")); diff != "" {
		t.Fatalf("FS.Changes() wrong changes (-want +got) diff = %s", diff)
	}
}

func TestDirStaysInRoot(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	root, outside := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "out")); err != nil {
		t.Fatal(err)
	}
	f := fsys.Dir(root)
	ctx := context.Background()
	if _, err := f.Open(ctx, "out/secret"); err == nil {
		t.Fatalf("FS.Open() through a symlink out of the root error = nil, want one")
	}
	if _, err := f.Write(ctx, "out/new", strings.NewReader("x"), 0o644, ""); err == nil {
		t.Fatalf("FS.Write() through a symlink out of the root error = nil, want one")
	}
	if _, err := f.ReadDir(ctx, "out"); err == nil {
		t.Fatalf("FS.ReadDir() of a symlink error = nil, want one")
	}
	if err := f.Rename(ctx, "out/secret", "stolen"); err == nil {
		t.Fatalf("FS.Rename() through a symlink out of the root error = nil, want one")
	}
	if _, err := os.Stat(filepath.Join(outside, "new")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("os.Stat() of a file written out of the root error = %v, want %v", err, fs.ErrNotExist)
	}
	if got := stat(t, f, "out"); got.Target != outside || got.Mode&fs.ModeSymlink == 0 {
		t.Fatalf("FS.Stat(out) = %+v, want the symlink to %s", got, outside)
	}
}

func TestDirBoundsEntries(t *testing.T) {
	t.Parallel()
	f := fsys.NewDir(t.TempDir(), 100, 3, time.Second)
	for _, name := range []string{"a", "b", "c", "d"} {
		write(t, f, name, "x", 0o644)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()
	if _, _, err := f.Changes(ctx, ""); !errors.Is(err, fsys.ErrTooManyEntries) {
		t.Fatalf("FS.Changes() of a directory over the limit error = %v, want %v", err, fsys.ErrTooManyEntries)
	}
}
