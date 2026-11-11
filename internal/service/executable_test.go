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

package service

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestStableExecutable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(t *testing.T, root string) (self string, lookPath func(string) (string, error))
		want  func(root string) string
	}{
		{
			name: "symlink on PATH to the running binary",
			setup: func(t *testing.T, root string) (string, func(string) (string, error)) {
				self := filepath.Join(root, "Cellar", "a2a-serve", "1.0.0", "bin", "a2a-serve")
				writeBinary(t, self)
				link := filepath.Join(root, "bin", "a2a-serve")
				symlink(t, filepath.Join("..", "Cellar", "a2a-serve", "1.0.0", "bin", "a2a-serve"), link)
				return self, lookIn(filepath.Join(root, "bin"))
			},
			want: func(root string) string { return filepath.Join(root, "bin", "a2a-serve") },
		},
		{
			name: "running binary itself on PATH",
			setup: func(t *testing.T, root string) (string, func(string) (string, error)) {
				self := filepath.Join(root, "bin", "a2a-serve")
				writeBinary(t, self)
				return self, lookIn(filepath.Join(root, "bin"))
			},
			want: func(root string) string { return filepath.Join(root, "bin", "a2a-serve") },
		},
		{
			name: "another copy on PATH",
			setup: func(t *testing.T, root string) (string, func(string) (string, error)) {
				self := filepath.Join(root, "build", "a2a-serve")
				writeBinary(t, self)
				writeBinary(t, filepath.Join(root, "bin", "a2a-serve"))
				return self, lookIn(filepath.Join(root, "bin"))
			},
			want: func(root string) string { return filepath.Join(root, "build", "a2a-serve") },
		},
		{
			name: "not on PATH",
			setup: func(t *testing.T, root string) (string, func(string) (string, error)) {
				self := filepath.Join(root, "build", "a2a-serve")
				writeBinary(t, self)
				return self, func(string) (string, error) { return "", errors.New("not found") }
			},
			want: func(root string) string { return filepath.Join(root, "build", "a2a-serve") },
		},
		{
			name: "dangling link on PATH",
			setup: func(t *testing.T, root string) (string, func(string) (string, error)) {
				self := filepath.Join(root, "build", "a2a-serve")
				writeBinary(t, self)
				symlink(t, filepath.Join(root, "gone"), filepath.Join(root, "bin", "a2a-serve"))
				return self, lookIn(filepath.Join(root, "bin"))
			},
			want: func(root string) string { return filepath.Join(root, "build", "a2a-serve") },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			self, lookPath := tt.setup(t, root)
			if got, want := StableExecutable(self, lookPath), tt.want(root); got != want {
				t.Fatalf("StableExecutable(%q) = %q, want %q", self, got, want)
			}
		})
	}
}

func lookIn(dir string) func(string) (string, error) {
	return func(name string) (string, error) { return filepath.Join(dir, name), nil }
}

func writeBinary(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("os.MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatalf("os.MkdirAll() error = %v", err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("os.Symlink() error = %v", err)
	}
}
