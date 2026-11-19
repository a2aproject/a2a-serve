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

//go:build linux

package fusefs

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/core/fsys"
)

// closeQuietly closes f at the end of a test that may have failed with it
// open, which would keep its mount busy.
func closeQuietly(t *testing.T, f *os.File) {
	if err := f.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		t.Errorf("closing %s error = %v", f.Name(), err)
	}
}

func put(t *testing.T, tree fsys.FS, p, data string, mode fs.FileMode) {
	t.Helper()
	if _, err := tree.Write(context.Background(), p, strings.NewReader(data), mode, ""); err != nil {
		t.Fatalf("FS.Write(%q) error = %v", p, err)
	}
}

func get(t *testing.T, tree fsys.FS, p string) string {
	t.Helper()
	rc, err := tree.Open(context.Background(), p)
	if err != nil {
		t.Fatalf("FS.Open(%q) error = %v", p, err)
	}
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("reading %q error = %v", p, err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("closing %q error = %v", p, err)
	}
	return string(data)
}

func TestMountReadsAndWrites(t *testing.T) {
	t.Parallel()
	dir, tree := mountTree(t, Options{})
	put(t, tree, "src/main.go", "package main", 0o644)

	got, err := os.ReadFile(filepath.Join(dir, "src/main.go"))
	if err != nil || string(got) != "package main" {
		t.Fatalf("os.ReadFile(src/main.go) = %q, %v, want the tree's file", got, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src/new.go"), []byte("package new"), 0o644); err != nil {
		t.Fatalf("os.WriteFile(src/new.go) error = %v", err)
	}
	if got := get(t, tree, "src/new.go"); got != "package new" {
		t.Fatalf("the tree's src/new.go = %q, want what was written", got)
	}
	put(t, tree, "src/main.go", "package changed", 0o644)
	if got, err := os.ReadFile(filepath.Join(dir, "src/main.go")); err != nil || string(got) != "package changed" {
		t.Fatalf("os.ReadFile(src/main.go) after the tree changed it = %q, %v, want the change", got, err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "src"))
	if err != nil {
		t.Fatalf("os.ReadDir(src) error = %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if diff := cmp.Diff([]string{"main.go", "new.go"}, names); diff != "" {
		t.Fatalf("os.ReadDir(src) wrong names (-want +got) diff = %s", diff)
	}
}

// TestMountWritesWholeFilesOnClose runs alone: a fork in this process,
// such as by another test, closes the child's copy of the open file when
// it execs, which flushes the file to the tree, as every close does.
func TestMountWritesWholeFilesOnClose(t *testing.T) {
	dir, tree := mountTree(t, Options{})
	put(t, tree, "log", "one\n", 0o644)

	f, err := os.OpenFile(filepath.Join(dir, "log"), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatalf("os.OpenFile(log) error = %v", err)
	}
	t.Cleanup(func() { closeQuietly(t, f) })
	if _, err := f.WriteString("two\n"); err != nil {
		t.Fatalf("writing log error = %v", err)
	}
	if got := get(t, tree, "log"); got != "one\n" {
		t.Fatalf("the tree's log before the close = %q, want the old contents", got)
	}
	if fi, err := os.Stat(filepath.Join(dir, "log")); err != nil || fi.Size() != 8 {
		t.Fatalf("os.Stat(log) while it is written = %v, %v, want the buffered size 8", fi, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("closing log error = %v", err)
	}
	if got := get(t, tree, "log"); got != "one\ntwo\n" {
		t.Fatalf("the tree's log after the close = %q, want both lines", got)
	}
	if err := os.Truncate(filepath.Join(dir, "log"), 3); err != nil {
		t.Fatalf("os.Truncate(log) error = %v", err)
	}
	if got := get(t, tree, "log"); got != "one" {
		t.Fatalf("the tree's log after the truncate = %q, want %q", got, "one")
	}
}

func TestMountKeepsModesLinksAndRenames(t *testing.T) {
	t.Parallel()
	dir, tree := mountTree(t, Options{})
	ctx := context.Background()

	script := filepath.Join(dir, "run.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile(run.sh) error = %v", err)
	}
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatalf("os.Chmod(run.sh) error = %v", err)
	}
	if info, err := tree.Stat(ctx, "run.sh"); err != nil || info.Mode != 0o755 {
		t.Fatalf("the tree's run.sh = %+v, %v, want mode 0755", info, err)
	}
	if err := os.Symlink("run.sh", filepath.Join(dir, "link")); err != nil {
		t.Fatalf("os.Symlink() error = %v", err)
	}
	if target, err := os.Readlink(filepath.Join(dir, "link")); err != nil || target != "run.sh" {
		t.Fatalf("os.Readlink(link) = %q, %v, want run.sh", target, err)
	}
	if info, err := tree.Stat(ctx, "link"); err != nil || info.Target != "run.sh" {
		t.Fatalf("the tree's link = %+v, %v, want a symlink to run.sh", info, err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "a/b"), 0o755); err != nil {
		t.Fatalf("os.MkdirAll(a/b) error = %v", err)
	}
	if err := os.Rename(script, filepath.Join(dir, "a/b/run.sh")); err != nil {
		t.Fatalf("os.Rename() error = %v", err)
	}
	if got := get(t, tree, "a/b/run.sh"); got != "#!/bin/sh\n" {
		t.Fatalf("the tree's a/b/run.sh = %q, want the moved script", got)
	}
	if err := os.Remove(filepath.Join(dir, "a")); !errors.Is(err, syscall.ENOTEMPTY) {
		t.Fatalf("os.Remove() of a full directory error = %v, want %v", err, syscall.ENOTEMPTY)
	}
	if err := os.RemoveAll(filepath.Join(dir, "a")); err != nil {
		t.Fatalf("os.RemoveAll(a) error = %v", err)
	}
	if _, err := tree.Stat(ctx, "a"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the tree's a after the removal error = %v, want %v", err, fs.ErrNotExist)
	}
	if _, err := os.OpenFile(filepath.Join(dir, "link"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("os.OpenFile(O_EXCL) of an existing entry error = %v, want %v", err, fs.ErrExist)
	}
}

func TestMountDropsWritesOfRemovedFiles(t *testing.T) {
	t.Parallel()
	dir, tree := mountTree(t, Options{})
	f, err := os.Create(filepath.Join(dir, "tmp"))
	if err != nil {
		t.Fatalf("os.Create(tmp) error = %v", err)
	}
	t.Cleanup(func() { closeQuietly(t, f) })
	if _, err := f.WriteString("scratch"); err != nil {
		t.Fatalf("writing tmp error = %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "tmp")); err != nil {
		t.Fatalf("os.Remove(tmp) error = %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("closing tmp error = %v", err)
	}
	infos, err := tree.ReadDir(context.Background(), ".")
	if err != nil {
		t.Fatalf("FS.ReadDir(.) error = %v", err)
	}
	if len(infos) != 0 {
		t.Fatalf("FS.ReadDir(.) after closing a removed file = %v, want nothing", infos)
	}
}

func TestMountBoundsBuffers(t *testing.T) {
	t.Parallel()
	dir, _ := mountTree(t, Options{MaxFileBytes: 1 << 10, MaxOpenBytes: 1 << 20})
	err := os.WriteFile(filepath.Join(dir, "big"), make([]byte, 2<<10), 0o644)
	if !errors.Is(err, syscall.EFBIG) {
		t.Fatalf("os.WriteFile() past the limit error = %v, want %v", err, syscall.EFBIG)
	}
}

func TestMountReadOnly(t *testing.T) {
	t.Parallel()
	dir, tree := mountTree(t, Options{ReadOnly: true})
	put(t, tree, "f", "x", 0o644)
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("y"), 0o644); !errors.Is(err, syscall.EROFS) {
		t.Fatalf("os.WriteFile() on a read-only mount error = %v, want %v", err, syscall.EROFS)
	}
}

// cancelable is a tree whose writes fail once their context ends, as the
// client of a file server does.
type cancelable struct{ fsys.FS }

func (c cancelable) Write(ctx context.Context, p string, r io.Reader, mode fs.FileMode, ifVersion string) (fsys.Info, error) {
	if err := ctx.Err(); err != nil {
		return fsys.Info{}, err
	}
	return c.FS.Write(ctx, p, r, mode, ifVersion)
}

func TestInterruptsDontCancelOperations(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tree := fsys.Memory()
	if _, err := (detached{cancelable{tree}}).Write(ctx, "f", strings.NewReader("x"), 0o644, ""); err != nil {
		t.Fatalf("detached.Write() with an interrupted context error = %v, want nil", err)
	}
	if got := get(t, tree, "f"); got != "x" {
		t.Fatalf("the tree's f = %q, want %q", got, "x")
	}
}
