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
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
)

func TestReadFrom(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		content    string
		offset     int64
		wantLines  []string
		wantOffset int64
	}{
		{name: "from start", content: "a\nb\n", wantLines: []string{"a", "b"}, wantOffset: 4},
		{name: "after offset", content: "a\nb\n", offset: 2, wantLines: []string{"b"}, wantOffset: 4},
		{name: "partial line", content: "a\nb", wantLines: []string{"a"}, wantOffset: 2},
		{name: "replaced file", content: "new\n", offset: 100, wantLines: []string{"new"}, wantOffset: 4},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "server.log")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
			lines, offset, err := readFrom(path, tc.offset)
			if err != nil {
				t.Fatalf("readFrom() error = %v", err)
			}
			if diff := cmp.Diff(tc.wantLines, lines); diff != "" {
				t.Fatalf("readFrom() wrong result (-want +got) diff = %s", diff)
			}
			if offset != tc.wantOffset {
				t.Fatalf("readFrom() offset = %d, want %d", offset, tc.wantOffset)
			}
		})
	}
}

func TestStartForegroundWhileRunningLeavesTheLogAlone(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("advisory locks are no-ops on this platform")
	}
	dir, err := agentdir.Open(t.TempDir())
	if err != nil {
		t.Fatalf("agentdir.Open() error = %v", err)
	}
	if err := dir.SaveConfig(&agentdir.Config{Name: "coder", Harness: "opencode"}); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}
	release, err := fsutil.TryLock(dir.LockPath())
	if err != nil {
		t.Fatalf("fsutil.TryLock() error = %v", err)
	}
	t.Cleanup(func() {
		if err := release(); err != nil {
			t.Errorf("release() error = %v", err)
		}
	})
	big := strings.Repeat("x", maxLogSize+1)
	if err := os.WriteFile(dir.LogPath(), []byte(big), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	err = Start(context.Background(), dir, StartOptions{Out: io.Discard, Quiet: true})
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("Start() error = %v, want an already running error", err)
	}
	data, err := os.ReadFile(dir.LogPath())
	if err != nil {
		t.Fatalf("reading the log error = %v", err)
	}
	if string(data) != big {
		t.Fatalf("Start() changed the running agent's log to %d bytes, want %d", len(data), len(big))
	}
	if _, err := os.Stat(dir.LogPath() + ".1"); !os.IsNotExist(err) {
		t.Fatalf("Start() rotated the running agent's log: os.Stat() error = %v", err)
	}
}

func TestStartDetachedWhileRunning(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets are not used on this platform")
	}
	dir := newAgent(t)
	serveControl(t, dir, &fakeControl{})

	var out strings.Builder
	if err := Start(context.Background(), dir, StartOptions{Mode: Detached, Quiet: true, Out: &out}); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if want := "✓ coder is already running (a2a-serve status)\n"; out.String() != want {
		t.Fatalf("Start() output = %q, want %q", out.String(), want)
	}
}
