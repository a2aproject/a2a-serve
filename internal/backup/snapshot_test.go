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

package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/fsutil"
)

// hookWriter calls onHeader with the entry name of every tar header block
// written to it, so that a test can change files between entries.
type hookWriter struct {
	buf      bytes.Buffer
	names    []string
	onHeader func(name string)
}

func (w *hookWriter) Write(p []byte) (int, error) {
	if len(p) == 512 {
		name, _, _ := bytes.Cut(p[:100], []byte{0})
		for _, n := range w.names {
			if string(name) == n {
				w.onHeader(n)
			}
		}
	}
	return w.buf.Write(p)
}

func readTar(t *testing.T, data []byte) map[string]string {
	t.Helper()
	out := map[string]string{}
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("reading the archive: %v", err)
		}
		content, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("reading %s: %v", hdr.Name, err)
		}
		out[hdr.Name] = string(content)
	}
}

func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func newTestSnapshotter(t *testing.T, tw *tar.Writer, log *slog.Logger, afterCopy func(name string, attempt int)) *snapshotter {
	t.Helper()
	stage, err := os.CreateTemp(t.TempDir(), "stage")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := stage.Close(); err != nil {
			t.Errorf("closing the staging file: %v", err)
		}
	})
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return newSnapshotter(tw, stage, log, afterCopy)
}

func TestAddTreeFilesDeletedDuringWalk(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"a": "1", "b": "2", "c": "3"})
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	s := newTestSnapshotter(t, tw, nil, func(name string, _ int) {
		if name != "home/a" {
			return
		}
		if err := os.Remove(filepath.Join(root, "b")); err != nil {
			t.Error(err)
		}
	})
	if err := s.addTree(context.Background(), root, "home"); err != nil {
		t.Fatalf("snapshotter.addTree() error = %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"home/": "", "home/a": "1", "home/c": "3"}
	if diff := cmp.Diff(want, readTar(t, buf.Bytes())); diff != "" {
		t.Fatalf("snapshotter.addTree() wrong entries (-want +got) diff = %s", diff)
	}
}

func TestAddTreeSkipsFileReplacedDuringCopy(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		replace func(t *testing.T, path string) error
	}{
		{name: "FIFO", replace: func(_ *testing.T, path string) error { return syscall.Mkfifo(path, 0o600) }},
		{name: "symlink out of the tree", replace: func(t *testing.T, path string) error { return os.Symlink(t.TempDir(), path) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			writeFiles(t, root, map[string]string{"a": "1", "f": "2"})
			var logs bytes.Buffer
			var buf bytes.Buffer
			tw := tar.NewWriter(&buf)
			s := newTestSnapshotter(t, tw, slog.New(slog.NewTextHandler(&logs, nil)), func(name string, attempt int) {
				if name != "home/f" || attempt != 1 {
					return
				}
				path := filepath.Join(root, "f")
				if err := os.Remove(path); err != nil {
					t.Error(err)
				}
				if err := tc.replace(t, path); err != nil {
					t.Error(err)
				}
			})

			done := make(chan error, 1)
			go func() { done <- s.addTree(context.Background(), root, "home") }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("snapshotter.addTree() error = %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("snapshotter.addTree() blocked")
			}
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}

			want := map[string]string{"home/": "", "home/a": "1"}
			if diff := cmp.Diff(want, readTar(t, buf.Bytes())); diff != "" {
				t.Fatalf("snapshotter.addTree() wrong entries (-want +got) diff = %s", diff)
			}
			if !strings.Contains(logs.String(), "home/f") {
				t.Fatalf("snapshotter.addTree() logged %q, want a warning naming home/f", logs.String())
			}
		})
	}
}

func TestAddTreeMissingRoot(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	s := newTestSnapshotter(t, tw, nil, nil)
	if err := s.addTree(context.Background(), filepath.Join(t.TempDir(), "gone"), "home"); err != nil {
		t.Fatalf("snapshotter.addTree() error = %v, want nil for a missing root", err)
	}
}

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func writeFileAt(t *testing.T, path, content string, mtime time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotterAddFileCopiesFilesChangingDuringCopy(t *testing.T) {
	t.Parallel()
	const secret = "s3cret-content"
	tests := []struct {
		name         string
		mutate       func(t *testing.T, path string, attempt int)
		want         string
		wantAttempts int
		wantWarning  bool
		wantMissing  bool
	}{
		{
			name:         "unchanged",
			mutate:       func(*testing.T, string, int) {},
			want:         "abc",
			wantAttempts: 1,
		},
		{
			name: "grows once",
			mutate: func(t *testing.T, path string, attempt int) {
				if attempt == 1 {
					writeFileAt(t, path, "abcdefgh", epoch)
				}
			},
			want:         "abcdefgh",
			wantAttempts: 2,
		},
		{
			name: "shrinks once",
			mutate: func(t *testing.T, path string, attempt int) {
				if attempt == 1 {
					writeFileAt(t, path, "a", epoch)
				}
			},
			want:         "a",
			wantAttempts: 2,
		},
		{
			name: "same size with a new mtime",
			mutate: func(t *testing.T, path string, attempt int) {
				if attempt == 1 {
					writeFileAt(t, path, "xyz", epoch.Add(time.Hour))
				}
			},
			want:         "xyz",
			wantAttempts: 2,
		},
		{
			name: "replaced by a file of the same size and mtime",
			mutate: func(t *testing.T, path string, attempt int) {
				if attempt != 1 {
					return
				}
				next := path + ".next"
				writeFileAt(t, next, "xyz", epoch)
				if err := os.Rename(next, path); err != nil {
					t.Fatal(err)
				}
			},
			want:         "xyz",
			wantAttempts: 2,
		},
		{
			name: "removed",
			mutate: func(t *testing.T, path string, _ int) {
				if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
			},
			wantAttempts: 1,
			wantMissing:  true,
		},
		{
			name: "changes on every attempt",
			mutate: func(t *testing.T, path string, attempt int) {
				writeFileAt(t, path, strings.Repeat(secret[:1], 3+attempt), epoch)
			},
			want:         "sssss",
			wantAttempts: maxCopyAttempts,
			wantWarning:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "f")
			writeFileAt(t, path, "abc", epoch)
			var logs bytes.Buffer
			var attempts int
			var buf bytes.Buffer
			tw := tar.NewWriter(&buf)
			s := newTestSnapshotter(t, tw, slog.New(slog.NewTextHandler(&logs, nil)), func(_ string, attempt int) {
				attempts = attempt
				tt.mutate(t, path, attempt)
			})

			err := s.addFile(path, "home/f")
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}

			wantEntries := map[string]string{"home/f": tt.want}
			if tt.wantMissing {
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("snapshotter.addFile() error = %v, want %v", err, os.ErrNotExist)
				}
				wantEntries = map[string]string{}
			} else if err != nil {
				t.Fatalf("snapshotter.addFile() error = %v", err)
			}
			if diff := cmp.Diff(wantEntries, readTar(t, buf.Bytes())); diff != "" {
				t.Fatalf("snapshotter.addFile() wrong entries (-want +got) diff = %s", diff)
			}
			if attempts != tt.wantAttempts {
				t.Fatalf("snapshotter.addFile() copied the file %d times, want %d", attempts, tt.wantAttempts)
			}
			warned := strings.Contains(logs.String(), "home/f")
			if warned != tt.wantWarning {
				t.Fatalf("snapshotter.addFile() logged %q, want a warning naming the file = %v", logs.String(), tt.wantWarning)
			}
			if strings.Contains(logs.String(), "sss") {
				t.Fatalf("snapshotter.addFile() logged file content: %q", logs.String())
			}
		})
	}
}

func TestSnapshotterAddFileBoundsCopyOfGrowingFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "f")
	writeFileAt(t, path, "abc", epoch)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	s := newTestSnapshotter(t, tw, nil, func(_ string, attempt int) {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.WriteString(strings.Repeat("x", 1<<16))
		if err := errors.Join(err, f.Close()); err != nil {
			t.Fatal(err)
		}
	})

	if err := s.addFile(path, "home/f"); err != nil {
		t.Fatalf("snapshotter.addFile() error = %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if got := len(readTar(t, buf.Bytes())["home/f"]); got != 3+2<<16 {
		t.Fatalf("snapshotter.addFile() stored %d bytes, want %d", got, 3+2<<16)
	}
}

func TestSnapshotterAddFileRefusesSymlinkSwappedIn(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	writeFileAt(t, path, "abc", epoch)
	writeFileAt(t, filepath.Join(dir, "other"), "other file", epoch)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	s := newTestSnapshotter(t, tw, nil, func(_ string, attempt int) {
		if attempt != 1 {
			return
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("other", path); err != nil {
			t.Fatal(err)
		}
	})

	err := s.addFile(path, "home/f")
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	if !errors.Is(err, fsutil.ErrNotRegular) {
		t.Fatalf("snapshotter.addFile() error = %v, want %v", err, fsutil.ErrNotRegular)
	}
	if diff := cmp.Diff(map[string]string{}, readTar(t, buf.Bytes())); diff != "" {
		t.Fatalf("snapshotter.addFile() wrong entries (-want +got) diff = %s", diff)
	}
}

// archiveFiles returns the entries of the gzipped tar archive in data.
func archiveFiles(t *testing.T, data []byte) map[string]string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gzip.NewReader() error = %v", err)
	}
	plain, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("reading the archive: %v", err)
	}
	return readTar(t, plain)
}

func TestWriteArchiveCopiesDatabaseOfPathWithURICharacters(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"hash#dir", "query?dir", "percent%41dir"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := filepath.Join(t.TempDir(), name)
			if err := os.Mkdir(root, 0o755); err != nil {
				t.Fatal(err)
			}
			d := newAgentAt(t, root)
			var buf bytes.Buffer

			if err := writeArchive(context.Background(), d, &buf); err != nil {
				t.Fatalf("writeArchive() error = %v", err)
			}

			copyPath := filepath.Join(t.TempDir(), "copy.db")
			if err := os.WriteFile(copyPath, []byte(archiveFiles(t, buf.Bytes())[pathDB]), 0o600); err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", copyPath)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := db.Close(); err != nil {
					t.Errorf("closing the database copy: %v", err)
				}
			}()
			var text string
			if err := db.QueryRow(`SELECT text FROM notes`).Scan(&text); err != nil || text != "remember me" {
				t.Fatalf("database copy note = %q, %v, want %q", text, err, "remember me")
			}
		})
	}
}

func TestWriteArchiveStagesDatabaseCopyInStateDir(t *testing.T) {
	d := newAgent(t)
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))

	if err := writeArchive(context.Background(), d, io.Discard); err != nil {
		t.Fatalf("writeArchive() error = %v", err)
	}
}

func TestWriteArchiveRemovesStagedFiles(t *testing.T) {
	d := newAgent(t)

	if err := writeArchive(context.Background(), d, io.Discard); err != nil {
		t.Fatalf("writeArchive() error = %v", err)
	}

	left, err := filepath.Glob(filepath.Join(d.StateDir(), "backup-*"))
	if err != nil || len(left) != 0 {
		t.Fatalf("writeArchive() left %v in the state directory, %v, want nothing", left, err)
	}
}

// blockingWriter blocks the first write until release is closed.
type blockingWriter struct {
	once    sync.Once
	started chan struct{}
	release chan struct{}
}

func (w *blockingWriter) Write(p []byte) (int, error) {
	w.once.Do(func() {
		close(w.started)
		<-w.release
	})
	return len(p), nil
}

func TestWriteArchiveRemovesLeftoverStagedFiles(t *testing.T) {
	t.Parallel()
	d := newAgent(t)
	leftover := filepath.Join(d.StateDir(), "backup-1")
	if err := os.Mkdir(leftover, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, leftover, map[string]string{"server.db": "plaintext"})
	running := &blockingWriter{started: make(chan struct{}), release: make(chan struct{})}
	first := make(chan error, 1)
	go func() { first <- writeArchive(context.Background(), d, running) }()
	<-running.started

	err := writeArchive(context.Background(), d, io.Discard)
	left, globErr := filepath.Glob(filepath.Join(d.StateDir(), "backup-*"))
	close(running.release)
	if firstErr := <-first; firstErr != nil {
		t.Fatalf("writeArchive() of the running backup error = %v", firstErr)
	}

	if err != nil {
		t.Fatalf("writeArchive() error = %v", err)
	}
	if globErr != nil || len(left) != 1 || left[0] == leftover {
		t.Fatalf("writeArchive() left %v in the state directory, %v, want only the running backup", left, globErr)
	}
}

// failingDest fails every Put after it read the header of the snapshot,
// while the archive is still being written.
type failingDest struct{ Dest }

func (failingDest) Put(_ context.Context, _ string, r io.Reader) error {
	if _, err := io.ReadFull(r, make([]byte, len(magic)+saltLen+prefixLen)); err != nil {
		return err
	}
	return errors.New("destination unreachable")
}

func (failingDest) Delete(context.Context, string) error { return nil }

func TestTakeJoinsArchiveWriterOnFailure(t *testing.T) {
	d := newAgent(t)

	if _, err := Take(context.Background(), d, failingDest{}, Config{Agent: "coder", Key: "k", Retention: time.Hour}, time.Now()); err == nil {
		t.Fatalf("Take() error = nil, want the destination error")
	}

	buf := make([]byte, 1<<20)
	if stacks := string(buf[:runtime.Stack(buf, true)]); strings.Contains(stacks, "backup.writeArchive") {
		t.Fatalf("Take() returned while the archive writer still ran:\n%s", stacks)
	}
}

func TestAddTreeDoesNotFollowSymlinksSwappedInDuringWalk(t *testing.T) {
	t.Parallel()
	root, outside := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, filepath.Join(root, "sub"), map[string]string{"a": "1", "b": "2"})
	writeFiles(t, outside, map[string]string{"b": "host secret"})
	w := &hookWriter{names: []string{"home/sub/a"}}
	w.onHeader = func(string) {
		if err := os.Rename(filepath.Join(root, "sub"), filepath.Join(root, "old")); err != nil {
			t.Error(err)
		}
		if err := os.Symlink(outside, filepath.Join(root, "sub")); err != nil {
			t.Error(err)
		}
	}
	tw := tar.NewWriter(w)
	s := newTestSnapshotter(t, tw, nil, nil)
	err := s.addTree(context.Background(), root, "home")
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	for name, content := range readTar(t, w.buf.Bytes()) {
		if content == "host secret" {
			t.Fatalf("snapshotter.addTree() archived %s with the content of a file out of the tree (error = %v)", name, err)
		}
	}
}
