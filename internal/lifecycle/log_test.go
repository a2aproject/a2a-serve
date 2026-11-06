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
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
)

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

			if err := FollowLog(context.Background(), dir, tc.n, false, &out, io.Discard); err != nil {
				t.Fatalf("FollowLog() error = %v", err)
			}

			if diff := cmp.Diff(tc.want, out.String()); diff != "" {
				t.Fatalf("FollowLog() wrong output (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestFollowLogNotesStoppedAgent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		running  bool
		wantNote string
	}{
		{name: "stopped", wantNote: "note: the agent is not running; waiting for it to write to its log (Ctrl-C to stop)\n"},
		{name: "running", running: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := newAgent(t)
			if err := os.WriteFile(dir.LogPath(), []byte("a\n"), 0o600); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
			if tc.running {
				release, err := fsutil.TryLock(dir.LockPath())
				if err != nil {
					t.Fatalf("fsutil.TryLock() error = %v", err)
				}
				t.Cleanup(func() {
					if err := release(); err != nil {
						t.Errorf("release() error = %v", err)
					}
				})
			}
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			var out, notes bytes.Buffer

			if err := FollowLog(ctx, dir, 10, true, &out, &notes); err != nil {
				t.Fatalf("FollowLog() error = %v", err)
			}

			if got := notes.String(); got != tc.wantNote {
				t.Fatalf("FollowLog() notes = %q, want %q", got, tc.wantNote)
			}
		})
	}
}
