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
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
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

func TestAddTreeFilesDeletedDuringWalk(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"a": "1", "b": "2", "c": "3"})
	w := &hookWriter{names: []string{"home/a"}}
	w.onHeader = func(string) {
		if err := os.Remove(filepath.Join(root, "b")); err != nil {
			t.Error(err)
		}
	}
	tw := tar.NewWriter(w)
	if err := addTree(context.Background(), tw, root, "home"); err != nil {
		t.Fatalf("addTree() error = %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"home/": "", "home/a": "1", "home/c": "3"}
	if diff := cmp.Diff(want, readTar(t, w.buf.Bytes())); diff != "" {
		t.Fatalf("addTree() wrong entries (-want +got) diff = %s", diff)
	}
}

func TestAddTreeMissingRoot(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := addTree(context.Background(), tw, filepath.Join(t.TempDir(), "gone"), "home"); err != nil {
		t.Fatalf("addTree() error = %v, want nil for a missing root", err)
	}
}

func TestAddTreeFilesChangingSize(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"grow": "abc", "shrink": "defgh"})
	w := &hookWriter{names: []string{"home/grow", "home/shrink"}}
	w.onHeader = func(name string) {
		path := filepath.Join(root, filepath.Base(name))
		var err error
		switch name {
		case "home/grow":
			err = os.WriteFile(path, []byte("abcdefgh"), 0o600)
		case "home/shrink":
			err = os.WriteFile(path, []byte("de"), 0o600)
		}
		if err != nil {
			t.Error(err)
		}
	}
	tw := tar.NewWriter(w)
	if err := addTree(context.Background(), tw, root, "home"); err != nil {
		t.Fatalf("addTree() error = %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"home/": "", "home/grow": "abc", "home/shrink": "de\x00\x00\x00"}
	if diff := cmp.Diff(want, readTar(t, w.buf.Bytes())); diff != "" {
		t.Fatalf("addTree() wrong entries (-want +got) diff = %s", diff)
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
