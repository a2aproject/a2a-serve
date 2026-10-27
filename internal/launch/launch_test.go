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

package launch

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

func TestForegroundWhileRunningLeavesTheLogAlone(t *testing.T) {
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

	err = Foreground(context.Background(), dir, io.Discard, false, true, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("Foreground() error = %v, want an already running error", err)
	}
	data, err := os.ReadFile(dir.LogPath())
	if err != nil {
		t.Fatalf("reading the log error = %v", err)
	}
	if string(data) != big {
		t.Fatalf("Foreground() changed the running agent's log to %d bytes, want %d", len(data), len(big))
	}
	if _, err := os.Stat(dir.LogPath() + ".1"); !os.IsNotExist(err) {
		t.Fatalf("Foreground() rotated the running agent's log: os.Stat() error = %v", err)
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

			if err := rotateLog(path, 8, stdout); err != nil {
				t.Fatalf("rotateLog() error = %v", err)
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
				t.Fatalf("rotateLog() wrong log (-want +got) diff = %s", diff)
			}
			old, err := os.ReadFile(path + ".1")
			if err != nil && !os.IsNotExist(err) {
				t.Fatalf("ReadFile() error = %v", err)
			}
			if diff := cmp.Diff(tc.wantOld, string(old)); diff != "" {
				t.Fatalf("rotateLog() wrong rotated log (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestServiceNameIgnoresAgentName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir, err := agentdir.Open(t.TempDir())
	if err != nil {
		t.Fatalf("agentdir.Open() error = %v", err)
	}
	want := "a2a-" + dir.ID()
	for _, name := range []string{"coder", "renamed"} {
		if got := ServiceName(dir, &agentdir.Config{Name: name}); got != want {
			t.Fatalf("ServiceName(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestServiceNameFindsLegacyUnit(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("writes a systemd user unit")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir, err := agentdir.Open(t.TempDir())
	if err != nil {
		t.Fatalf("agentdir.Open() error = %v", err)
	}
	legacy := "a2a-old-name-" + dir.ID()
	units := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(units, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(units, legacy+".service"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ServiceName(dir, &agentdir.Config{Name: "coder"}); got != legacy {
		t.Fatalf("ServiceName() = %q, want %q", got, legacy)
	}
	if err := os.WriteFile(filepath.Join(units, "a2a-"+dir.ID()+".service"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, want := ServiceName(dir, &agentdir.Config{Name: "coder"}), "a2a-"+dir.ID(); got != want {
		t.Fatalf("ServiceName() = %q, want %q", got, want)
	}
}

func TestInstalledServicesListsLegacyUnits(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("writes systemd user units")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir, err := agentdir.Open(t.TempDir())
	if err != nil {
		t.Fatalf("agentdir.Open() error = %v", err)
	}
	units := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(units, 0o700); err != nil {
		t.Fatal(err)
	}
	want := []string{"a2a-" + dir.ID(), "a2a-first-" + dir.ID(), "a2a-second-" + dir.ID()}
	for _, name := range want {
		if err := os.WriteFile(filepath.Join(units, name+".service"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	got := InstalledServices(dir)

	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("InstalledServices() wrong result (-want +got) diff = %s", diff)
	}
}
