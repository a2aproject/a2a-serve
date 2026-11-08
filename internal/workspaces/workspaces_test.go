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

package workspaces

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
)

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func tree(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil || rel == "." {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			rel += " -> " + target
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func TestPrepareOwnCopiesOnce(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "main.go"), "package main")
	writeFile(t, filepath.Join(src, ".git", "HEAD"), "ref")
	writeFile(t, filepath.Join(src, ".a2a", "state", "secrets"), "KEY=v")
	writeFile(t, filepath.Join(src, "sub", ".a2a", "config.yaml"), "x")
	if err := os.Symlink("main.go", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	s := New(filepath.Join(t.TempDir(), "workspaces"))
	dir, err := s.Prepare(context.Background(), "u1", "pair:ben", agentdir.StorageOwn, src)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	want := []string{".git", ".git/HEAD", "link -> main.go", "main.go", "sub"}
	if diff := cmp.Diff(want, tree(t, dir)); diff != "" {
		t.Fatalf("Prepare() wrong copy (-want +got) diff = %s", diff)
	}
	writeFile(t, filepath.Join(dir, "main.go"), "changed")
	writeFile(t, filepath.Join(src, "new.go"), "new")
	if _, err := s.Prepare(context.Background(), "u1", "pair:ben", agentdir.StorageOwn, src); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "main.go"))
	if err != nil || string(data) != "changed" {
		t.Fatalf("Prepare() again = %q, %v, want the copy kept", data, err)
	}
	entries, err := s.List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(entries) != 1 || entries[0].Principal != "pair:ben" || entries[0].Used.IsZero() {
		t.Fatalf("List() = %+v, want one used entry of pair:ben", entries)
	}
	if diff := cmp.Diff([]string{agentdir.StorageOwn}, entries[0].Storage); diff != "" {
		t.Fatalf("List() wrong storage (-want +got) diff = %s", diff)
	}
}

func TestPrepareEmpty(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "main.go"), "package main")
	s := New(filepath.Join(t.TempDir(), "w"))
	dir, err := s.Prepare(context.Background(), "u1", "email:a@example.com", agentdir.StorageEmpty, src)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if got := tree(t, dir); len(got) != 0 {
		t.Fatalf("Prepare() = %v, want an empty directory", got)
	}
}

func TestPrepareOwnTooLarge(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		maxBytes   int64
		maxEntries int
	}{
		{name: "bytes", maxBytes: 10},
		{name: "entries", maxEntries: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			src := t.TempDir()
			writeFile(t, filepath.Join(src, "a"), "0123456789")
			writeFile(t, filepath.Join(src, "b"), "0123456789")
			writeFile(t, filepath.Join(src, "c"), "0")
			s := New(filepath.Join(t.TempDir(), "w"))
			s.MaxBytes, s.MaxEntries = tt.maxBytes, tt.maxEntries
			_, err := s.Prepare(context.Background(), "u1", "pair:ben", agentdir.StorageOwn, src)
			if !errors.Is(err, ErrTooLarge) {
				t.Fatalf("Prepare() error = %v, want %v", err, ErrTooLarge)
			}
			if got := tree(t, filepath.Join(s.Root, "u1")); !cmp.Equal(got, []string{"principal"}) {
				t.Fatalf("Prepare() left %v, want only the principal", got)
			}
		})
	}
}

func TestRemove(t *testing.T) {
	t.Parallel()
	s := New(filepath.Join(t.TempDir(), "w"))
	for _, storage := range []string{agentdir.StorageOwn, agentdir.StorageEmpty} {
		if _, err := s.Prepare(context.Background(), "u1", "pair:ben", storage, t.TempDir()); err != nil {
			t.Fatalf("Prepare() error = %v", err)
		}
	}
	if err := s.RemoveStorage("u1", agentdir.StorageOwn); err != nil {
		t.Fatalf("RemoveStorage() error = %v", err)
	}
	entries, err := s.List()
	if err != nil || len(entries) != 1 || !cmp.Equal(entries[0].Storage, []string{agentdir.StorageEmpty}) {
		t.Fatalf("List() = %+v, %v, want only the empty workspace", entries, err)
	}
	if err := s.Remove("u1"); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if entries, err := s.List(); err != nil || len(entries) != 0 {
		t.Fatalf("List() = %+v, %v, want none", entries, err)
	}
}
