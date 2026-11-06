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
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/clierr"
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

func TestWaitReadyFailsWhenServerStops(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets are not used on this platform")
	}
	dir := newAgent(t)
	var log bytes.Buffer
	log.WriteString("pulling the harness image\n")
	slog.New(slog.NewTextHandler(&log, nil)).Error(stopMsg, "error", errors.New("pulling the image: exit status 1"))
	if err := os.WriteFile(dir.LogPath(), log.Bytes(), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	err := waitReady(ctx, dir, 0, func() bool { return true }, io.Discard)

	if err == nil || !strings.Contains(err.Error(), "exit status 1") {
		t.Fatalf("waitReady() error = %v, want the error the server stopped with", err)
	}
}

func TestWaitReadyReportsServerErrorOnce(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets are not used on this platform")
	}
	stop := func(err string) string {
		var b bytes.Buffer
		slog.New(slog.NewTextHandler(&b, nil)).Error(stopMsg, "error", errors.New(err))
		return b.String()
	}
	type result struct {
		Out, Message, Hint string
	}
	tests := []struct {
		name string
		log  string
		want result
	}{
		{
			name: "error and hint the server printed",
			log: "✓ opencode is running (ACP)\n" +
				stop("listening on localhost:1: address already in use") +
				"Error: listening on localhost:1: address already in use\n" +
				"hint: pick another port\n",
			want: result{
				Out:     "✓ opencode is running (ACP)\n",
				Message: "the server exited: listening on localhost:1: address already in use",
				Hint:    "pick another port",
			},
		},
		{
			name: "error the server printed before logging",
			log:  "Error: the server is already running in /a (a2a-serve status)\n",
			want: result{Message: "the server exited: the server is already running in /a (a2a-serve status)"},
		},
		{
			name: "stop record only",
			log:  "pulling the harness image\n" + stop("pulling the image: exit status 1"),
			want: result{
				Out:     "pulling the harness image\n",
				Message: "the server exited: pulling the image: exit status 1",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := newAgent(t)
			if err := os.WriteFile(dir.LogPath(), []byte(tt.log), 0o600); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			var out bytes.Buffer

			err := waitReady(ctx, dir, 0, func() bool { return false }, &out)

			ce := clierr.Classify(err)
			if ce == nil {
				t.Fatalf("waitReady() error = nil, want the error the server exited with")
			}
			got := result{Out: out.String(), Message: ce.Message, Hint: ce.Hint}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Fatalf("waitReady() wrong result (-want +got) diff = %s", diff)
			}
		})
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
