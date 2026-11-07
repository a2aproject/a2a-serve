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
	"syscall"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// withTimeout fails the test when fn doesn't return in time, such as an
// open that blocks on a FIFO.
func withTimeout(t *testing.T, fn func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("the call blocked")
		return nil
	}
}

func TestReadRegular(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		setup   func(t *testing.T, path string)
		want    string
		wantErr error
	}{
		{
			name: "regular file",
			setup: func(t *testing.T, path string) {
				if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: "hello",
		},
		{
			name: "at the limit",
			setup: func(t *testing.T, path string) {
				if err := os.WriteFile(path, make([]byte, MaxReadSize), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: string(make([]byte, MaxReadSize)),
		},
		{
			name:    "missing",
			setup:   func(*testing.T, string) {},
			wantErr: os.ErrNotExist,
		},
		{
			name: "FIFO",
			setup: func(t *testing.T, path string) {
				if err := syscall.Mkfifo(path, 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: ErrNotRegular,
		},
		{
			name: "directory",
			setup: func(t *testing.T, path string) {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: ErrNotRegular,
		},
		{
			name: "huge file",
			setup: func(t *testing.T, path string) {
				f, err := os.Create(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := f.Truncate(1 << 34); err != nil {
					t.Fatal(errors.Join(err, f.Close()))
				}
				if err := f.Close(); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: ErrTooLarge,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			tc.setup(t, filepath.Join(dir, "f"))
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := root.Close(); err != nil {
					t.Error(err)
				}
			})
			var got []byte
			err = withTimeout(t, func() error {
				var err error
				got, err = ReadRegular(root, "f")
				return err
			})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ReadRegular() error = %v, want %v", err, tc.wantErr)
			}
			if diff := cmp.Diff(tc.want, string(got)); diff != "" {
				t.Fatalf("ReadRegular() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestWriteRegular(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		setup   func(t *testing.T, path string)
		want    string
		wantErr error
	}{
		{name: "new file", setup: func(*testing.T, string) {}, want: "new"},
		{
			name: "replaces the content",
			setup: func(t *testing.T, path string) {
				if err := os.WriteFile(path, []byte("older content"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: "new",
		},
		{
			name: "FIFO",
			setup: func(t *testing.T, path string) {
				if err := syscall.Mkfifo(path, 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: ErrNotRegular,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "f")
			tc.setup(t, path)
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := root.Close(); err != nil {
					t.Error(err)
				}
			})
			err = withTimeout(t, func() error { return WriteRegular(root, "f", []byte("new"), 0o600) })
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("WriteRegular() error = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr != nil {
				return
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.want, string(got)); diff != "" {
				t.Fatalf("WriteRegular() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestWriteRegularToFIFOWithReader(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	reader, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reader.Close(); err != nil {
			t.Error(err)
		}
	})
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Error(err)
		}
	})
	err = withTimeout(t, func() error { return WriteRegular(root, "f", []byte("new"), 0o600) })
	if !errors.Is(err, ErrNotRegular) {
		t.Fatalf("WriteRegular() error = %v, want %v", err, ErrNotRegular)
	}
}

func TestReadDirIn(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "d"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "d", "a"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Error(err)
		}
	})

	var names []string
	err = withTimeout(t, func() error {
		entries, err := ReadDirIn(root, "d")
		for _, e := range entries {
			names = append(names, e.Name())
		}
		return err
	})
	if err != nil {
		t.Fatalf("ReadDirIn() error = %v", err)
	}
	if diff := cmp.Diff([]string{"a"}, names); diff != "" {
		t.Fatalf("ReadDirIn() wrong result (-want +got) diff = %s", diff)
	}

	err = withTimeout(t, func() error {
		_, err := ReadDirIn(root, "fifo")
		return err
	})
	if err == nil {
		t.Fatal("ReadDirIn() of a FIFO error = nil, want an error")
	}
}
