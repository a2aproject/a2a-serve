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

//go:build unix

package fsutil

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
}

func chmodTree(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("Chmod(%s) error = %v", path, err)
	}
}

func TestRemoveAll(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(t *testing.T, root string) (target string)
	}{
		{
			name: "missing path",
			setup: func(t *testing.T, root string) string {
				return filepath.Join(root, "missing")
			},
		},
		{
			name: "file",
			setup: func(t *testing.T, root string) string {
				target := filepath.Join(root, "file")
				writeFile(t, target)
				return target
			},
		},
		{
			name: "tree",
			setup: func(t *testing.T, root string) string {
				target := filepath.Join(root, "home")
				if err := os.MkdirAll(filepath.Join(target, "a", "b"), 0o700); err != nil {
					t.Fatal(err)
				}
				writeFile(t, filepath.Join(target, "a", "b", "f"))
				return target
			},
		},
		{
			name: "read-only subtree",
			setup: func(t *testing.T, root string) string {
				target := filepath.Join(root, "home")
				mod := filepath.Join(target, "go", "pkg", "mod", "x@v1")
				if err := os.MkdirAll(mod, 0o700); err != nil {
					t.Fatal(err)
				}
				writeFile(t, filepath.Join(mod, "go.mod"))
				chmodTree(t, mod, 0o500)
				chmodTree(t, filepath.Join(target, "go", "pkg", "mod"), 0o500)
				return target
			},
		},
		{
			name: "directory without any permission",
			setup: func(t *testing.T, root string) string {
				target := filepath.Join(root, "home")
				locked := filepath.Join(target, "locked")
				if err := os.MkdirAll(locked, 0o700); err != nil {
					t.Fatal(err)
				}
				writeFile(t, filepath.Join(locked, "f"))
				chmodTree(t, locked, 0)
				return target
			},
		},
		{
			name: "read-only top directory",
			setup: func(t *testing.T, root string) string {
				target := filepath.Join(root, "home")
				if err := os.MkdirAll(target, 0o700); err != nil {
					t.Fatal(err)
				}
				writeFile(t, filepath.Join(target, "f"))
				chmodTree(t, target, 0o500)
				return target
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			target := tc.setup(t, root)

			err := RemoveAll(target)

			if err != nil {
				t.Fatalf("RemoveAll() error = %v, want nil", err)
			}
			if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("Lstat() after RemoveAll() error = %v, want %v", err, os.ErrNotExist)
			}
		})
	}
}

func TestRemoveAllKeepsWhatSymlinksPointTo(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	outside := filepath.Join(root, "outside")
	if err := os.Mkdir(outside, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { chmodTree(t, outside, 0o700) })
	kept := filepath.Join(outside, "kept")
	chmodTree(t, outside, 0o700)
	writeFile(t, kept)
	chmodTree(t, outside, 0o500)
	home := filepath.Join(root, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(home, "link")); err != nil {
		t.Fatal(err)
	}

	if err := RemoveAll(home); err != nil {
		t.Fatalf("RemoveAll() error = %v, want nil", err)
	}

	if _, err := os.Stat(kept); err != nil {
		t.Fatalf("Stat() of the file behind the symlink error = %v, want it kept", err)
	}
	info, err := os.Stat(outside)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o500 {
		t.Fatalf("mode of the directory behind the symlink = %v, want it unchanged at 0500", got)
	}
}

func TestRemoveAllRemovesASymlinkToADirectoryWithoutFollowingIt(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	outside := filepath.Join(root, "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(outside, "kept"))
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}

	if err := RemoveAll(link); err != nil {
		t.Fatalf("RemoveAll() error = %v, want nil", err)
	}

	if _, err := os.Lstat(link); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Lstat() of the link error = %v, want %v", err, os.ErrNotExist)
	}
	if _, err := os.Stat(filepath.Join(outside, "kept")); err != nil {
		t.Fatalf("Stat() of the file behind the link error = %v, want it kept", err)
	}
}
