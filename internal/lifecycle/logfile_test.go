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

package lifecycle

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
)

func TestRotatingLogRotatesWhileWriting(t *testing.T) {
	t.Parallel()
	const limit = 200
	path := filepath.Join(t.TempDir(), "server.log")
	w, err := openRotatingLog(path, limit)
	if err != nil {
		t.Fatalf("openRotatingLog() error = %v", err)
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
	w, err := openRotatingLog(path, 8, stdout)
	if err != nil {
		t.Fatalf("openRotatingLog() error = %v", err)
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

func TestFollowLogPrintsLastLines(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		content string
		n       int
		want    string
	}{
		{name: "fewer lines than asked", content: "a\nb\n", n: 5, want: "a\nb\n"},
		{name: "last two", content: "a\nb\nc\nd\n", n: 2, want: "c\nd\n"},
		{name: "all", content: "a\nb\nc\n", n: 0, want: "a\nb\nc\n"},
		{name: "partial last line", content: "a\nb\nc", n: 2, want: "b\nc\n"},
		{name: "trailing blank lines", content: "a\nb\n\n\n", n: 5, want: "a\nb\n"},
		{name: "inner blank line", content: "a\n\nb\n", n: 5, want: "a\n\nb\n"},
		{name: "empty", content: "", n: 5, want: "\n"},
		{name: "long log", content: strings.Repeat("filler line\n", 20000) + "x\ny\n", n: 3, want: "filler line\nx\ny\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, err := agentdir.Open(t.TempDir())
			if err != nil {
				t.Fatalf("agentdir.Open() error = %v", err)
			}
			if err := dir.Init(); err != nil {
				t.Fatalf("Init() error = %v", err)
			}
			if err := os.WriteFile(dir.LogPath(), []byte(tc.content), 0o600); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
			var out bytes.Buffer

			if err := FollowLog(context.Background(), dir, tc.n, false, &out); err != nil {
				t.Fatalf("FollowLog() error = %v", err)
			}

			if diff := cmp.Diff(tc.want, out.String()); diff != "" {
				t.Fatalf("FollowLog() wrong output (-want +got) diff = %s", diff)
			}
		})
	}
}
