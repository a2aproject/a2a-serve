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
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

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
