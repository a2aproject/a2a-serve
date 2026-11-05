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

package recipe

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func symlink(t *testing.T, dir, name, target string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, p); err != nil {
		t.Fatal(err)
	}
}

func TestScan(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "AGENTS.md", "instructions")
	writeFile(t, dir, ".mcp.json", "{}")
	writeFile(t, dir, ".agents/skills/triage/SKILL.md", "skill")
	writeFile(t, dir, ".agents/skills/triage/run.sh", "#!/bin/sh")
	if err := os.Chmod(filepath.Join(dir, ".agents/skills/triage/run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, ".a2a/config.yaml", "agent: opencode\n")
	writeFile(t, dir, ".a2a/state/secrets", "TOKEN=x")
	writeFile(t, dir, ".git/HEAD", "ref: refs/heads/main")
	writeFile(t, dir, "vendor/lib/.git", "gitdir: ../.git/modules/lib")
	symlink(t, dir, "CLAUDE.md", "AGENTS.md")
	symlink(t, dir, ".claude/skills", "../.agents/skills")

	got, err := Scan(dir)
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	want := Files{
		"AGENTS.md":                      {Hash: sha("instructions"), Size: 12},
		".mcp.json":                      {Hash: sha("{}"), Size: 2},
		".agents/skills/triage/SKILL.md": {Hash: sha("skill"), Size: 5},
		".agents/skills/triage/run.sh":   {Hash: sha("#!/bin/sh"), Exec: true, Size: 9},
		"CLAUDE.md":                      {Link: "AGENTS.md"},
		".claude/skills":                 {Link: "../.agents/skills"},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("Scan() wrong result (-want +got) diff = %s", diff)
	}
}

func TestScanRefuses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		setup   func(t *testing.T, dir string)
		lim     limits
		wantErr string
	}{
		{
			name:    "absolute symlink",
			setup:   func(t *testing.T, dir string) { symlink(t, dir, "notes", "/etc") },
			wantErr: "symlink notes points at the absolute path /etc",
		},
		{
			name:    "symlink out of the recipe",
			setup:   func(t *testing.T, dir string) { symlink(t, dir, "skills/x", "../../outside") },
			wantErr: "symlink skills/x points at ../../outside, outside the recipe",
		},
		{
			name:    "symlink into .a2a",
			setup:   func(t *testing.T, dir string) { symlink(t, dir, "docs/secrets", "../.a2a/state/secrets") },
			wantErr: "symlink docs/secrets points into .a2a",
		},
		{
			name: "named pipe",
			setup: func(t *testing.T, dir string) {
				if err := syscall.Mkfifo(filepath.Join(dir, "pipe"), 0o644); err != nil {
					t.Skipf("mkfifo: %v", err)
				}
			},
			wantErr: "pipe is not a regular file, directory or symlink",
		},
		{
			name: "too many files",
			setup: func(t *testing.T, dir string) {
				for _, name := range []string{"a", "b", "c"} {
					writeFile(t, dir, name, "x")
				}
			},
			lim:     limits{files: 2, bytes: 100},
			wantErr: "more than 2 files",
		},
		{
			name: "too large",
			setup: func(t *testing.T, dir string) {
				writeFile(t, dir, "a", "12345")
				writeFile(t, dir, "b", "67890")
			},
			lim:     limits{files: 10, bytes: 8},
			wantErr: "larger than 8 bytes",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			tc.setup(t, dir)
			lim := tc.lim
			if lim == (limits{}) {
				lim = defaultLimits
			}
			_, err := scan(dir, lim)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Scan() error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestCopy(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	writeFile(t, src, "AGENTS.md", "instructions")
	writeFile(t, src, ".agents/skills/triage/run.sh", "#!/bin/sh")
	if err := os.Chmod(filepath.Join(src, ".agents/skills/triage/run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	symlink(t, src, "CLAUDE.md", "AGENTS.md")
	files, err := Scan(src)
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	dst := t.TempDir()
	writeFile(t, dst, "CLAUDE.md", "replaced by the link")

	if err := Copy(src, dst, files, files.Paths()); err != nil {
		t.Fatalf("Copy() error = %v", err)
	}
	got, err := Scan(dst)
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	if diff := cmp.Diff(files, got); diff != "" {
		t.Fatalf("Copy() wrong files (-want +got) diff = %s", diff)
	}
}

func TestCopyRefuses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		setup   func(t *testing.T, src, dst string)
		wantErr string
	}{
		{
			name: "file changed after the scan",
			setup: func(t *testing.T, src, _ string) {
				writeFile(t, src, "AGENTS.md", "changed")
			},
			wantErr: "the file changed while it was copied",
		},
		{
			name: "symlink out of the destination",
			setup: func(t *testing.T, _, dst string) {
				symlink(t, dst, ".agents", t.TempDir())
			},
			wantErr: "copying .agents/skills/x/SKILL.md",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			src, dst := t.TempDir(), t.TempDir()
			writeFile(t, src, "AGENTS.md", "instructions")
			writeFile(t, src, ".agents/skills/x/SKILL.md", "skill")
			files, err := Scan(src)
			if err != nil {
				t.Fatalf("Scan() error = %v", err)
			}
			tc.setup(t, src, dst)
			err = Copy(src, dst, files, files.Paths())
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Copy() error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestFileString(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		file File
		want string
	}{
		{name: "file", file: File{Hash: sha("a")}, want: "sha256:" + sha("a")},
		{name: "executable", file: File{Hash: sha("a"), Exec: true}, want: "exec:sha256:" + sha("a")},
		{name: "symlink", file: File{Link: "AGENTS.md"}, want: "link:AGENTS.md"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.file.String(); got != tc.want {
				t.Fatalf("File.String() = %q, want %q", got, tc.want)
			}
			got, err := ParseFile(tc.want)
			if err != nil {
				t.Fatalf("ParseFile(%q) error = %v", tc.want, err)
			}
			if diff := cmp.Diff(tc.file, got); diff != "" {
				t.Fatalf("ParseFile(%q) wrong result (-want +got) diff = %s", tc.want, diff)
			}
		})
	}
	for _, in := range []string{"", "md5:abc", "sha256:abc", "link:", "exec:link:x"} {
		if _, err := ParseFile(in); err == nil {
			t.Fatalf("ParseFile(%q) error = nil, want an error", in)
		}
	}
}
