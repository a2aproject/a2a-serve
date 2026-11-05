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

package logfile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestRotatingLogRotatesWhileWriting(t *testing.T) {
	t.Parallel()
	const limit = 200
	path := filepath.Join(t.TempDir(), "server.log")
	w, err := Open(path, limit)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	var wg sync.WaitGroup
	for g := range 4 {
		wg.Go(func() {
			for i := range 50 {
				if _, err := fmt.Fprintf(w, "writer %d line %d\n", g, i); err != nil {
					t.Errorf("Write() error = %v", err)
					return
				}
			}
		})
	}
	wg.Wait()
	if _, err := w.Write([]byte("last line\n")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the log error = %v", err)
	}
	old, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("reading the rotated log error = %v", err)
	}
	if len(got) > limit+len("writer 0 line 0\n") {
		t.Fatalf("log size = %d, want at most about %d", len(got), limit)
	}
	if !strings.HasSuffix(string(got), "last line\n") {
		t.Fatalf("log = %q, want it to end with the last line", got)
	}
	for _, data := range [][]byte{got, old} {
		for line := range strings.SplitSeq(strings.TrimSuffix(string(data), "\n"), "\n") {
			if line != "last line" && !strings.HasPrefix(line, "writer ") {
				t.Fatalf("log has the broken line %q", line)
			}
		}
	}
}

func TestRotatingLogKeepsSharedFileWritable(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "server.log")
	stdout, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("OpenFile() error = %v", err)
	}
	t.Cleanup(func() {
		if err := stdout.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	w, err := Open(path, 8, stdout)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := w.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	if _, err := w.Write([]byte("0123456789\n")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	if _, err := w.Write([]byte("new\n")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if _, err := stdout.WriteString("from stdout\n"); err != nil {
		t.Fatalf("WriteString() error = %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the log error = %v", err)
	}
	if diff := cmp.Diff("new\nfrom stdout\n", string(got)); diff != "" {
		t.Fatalf("log wrong content (-want +got) diff = %s", diff)
	}
	old, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("reading the rotated log error = %v", err)
	}
	if diff := cmp.Diff("0123456789\n", string(old)); diff != "" {
		t.Fatalf("rotated log wrong content (-want +got) diff = %s", diff)
	}
}

func TestRotateLog(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		content   string
		stdoutLog bool
		wantLog   string
		wantOld   string
	}{
		{name: "small", content: "abc\n", wantLog: "abc\nnew\n"},
		{name: "oversized", content: "0123456789\n", wantLog: "new\n", wantOld: "0123456789\n"},
		{name: "oversized while it is stdout", content: "0123456789\n", stdoutLog: true, wantLog: "new\n", wantOld: "0123456789\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "server.log")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
			stdout, err := os.OpenFile(filepath.Join(t.TempDir(), "other"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
			if tc.stdoutLog {
				stdout, err = os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
			}
			if err != nil {
				t.Fatalf("OpenFile() error = %v", err)
			}
			t.Cleanup(func() {
				if err := stdout.Close(); err != nil {
					t.Errorf("Close() error = %v", err)
				}
			})

			if err := Rotate(path, 8, stdout); err != nil {
				t.Fatalf("Rotate() error = %v", err)
			}
			w := stdout
			if !tc.stdoutLog {
				if w, err = os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err != nil {
					t.Fatalf("OpenFile() error = %v", err)
				}
				defer func() {
					if err := w.Close(); err != nil {
						t.Errorf("Close() error = %v", err)
					}
				}()
			}
			if _, err := w.WriteString("new\n"); err != nil {
				t.Fatalf("WriteString() error = %v", err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile() error = %v", err)
			}
			if diff := cmp.Diff(tc.wantLog, string(got)); diff != "" {
				t.Fatalf("Rotate() wrong log (-want +got) diff = %s", diff)
			}
			old, err := os.ReadFile(path + ".1")
			if err != nil && !os.IsNotExist(err) {
				t.Fatalf("ReadFile() error = %v", err)
			}
			if diff := cmp.Diff(tc.wantOld, string(old)); diff != "" {
				t.Fatalf("Rotate() wrong rotated log (-want +got) diff = %s", diff)
			}
		})
	}
}
