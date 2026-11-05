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
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
)

func TestRecordRoundTrip(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	writeFile(t, src, ".a2a/config.yaml", testConfig)
	writeFile(t, src, ".mcp.json", testMCP)
	writeFile(t, src, "AGENTS.md", "instructions")
	symlink(t, src, "CLAUDE.md", "AGENTS.md")
	r, err := Load(src)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	fetched := &Fetched{
		Source: Source{Repo: "https://github.com/acme/agents.git", Ref: "main", Path: "experts/o11y"},
		Commit: strings.Repeat("ab", 20),
	}
	d, err := agentdir.Open(t.TempDir())
	if err != nil {
		t.Fatalf("agentdir.Open() error = %v", err)
	}
	if err := d.Init(); err != nil {
		t.Fatalf("Dir.Init() error = %v", err)
	}

	rec := NewRecord(fetched, r)
	if err := WriteRecord(d, rec); err != nil {
		t.Fatalf("WriteRecord() error = %v", err)
	}
	got, err := ReadRecord(d)
	if err != nil {
		t.Fatalf("ReadRecord() error = %v", err)
	}
	want := &Record{
		Repo:   "https://github.com/acme/agents.git",
		Ref:    "main",
		Path:   "experts/o11y",
		Commit: strings.Repeat("ab", 20),
		MCP:    []string{"grafana", "kubernetes"},
		Files: map[string]string{
			".mcp.json": "sha256:" + sha(testMCP),
			"AGENTS.md": "sha256:" + sha("instructions"),
			"CLAUDE.md": "link:AGENTS.md",
		},
		Config: testConfig,
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("ReadRecord() wrong result (-want +got) diff = %s", diff)
	}
	if diff := cmp.Diff(fetched.Source, got.Source(), cmp.AllowUnexported(Source{})); diff != "" {
		t.Fatalf("Record.Source() wrong result (-want +got) diff = %s", diff)
	}
	files, err := got.RecipeFiles()
	if err != nil {
		t.Fatalf("Record.RecipeFiles() error = %v", err)
	}
	for name, f := range r.Files {
		if !files[name].Same(f) {
			t.Fatalf("Record.RecipeFiles()[%s] = %v, want %v", name, files[name], f)
		}
	}
	data, err := os.ReadFile(d.RecipeLockPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), "# Written by `a2a-serve recipe`") || !strings.Contains(string(data), "config: |") {
		t.Fatalf("recipe.lock = %s, want the header and config as a literal block", data)
	}
}

func TestReadRecordErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		content string
		wantErr string
	}{
		{name: "no source", content: "commit: abc\n", wantErr: "names neither a repository nor a directory"},
		{name: "both sources", content: "repo: x\ndir: /y\n", wantErr: "names neither a repository nor a directory"},
		{name: "not yaml", content: "repo: [\n", wantErr: "parsing"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, err := agentdir.Open(t.TempDir())
			if err != nil {
				t.Fatalf("agentdir.Open() error = %v", err)
			}
			writeFile(t, d.Root, ".a2a/recipe.lock", tc.content)
			_, err = ReadRecord(d)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ReadRecord() error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestReadRecordMissing(t *testing.T) {
	t.Parallel()
	d, err := agentdir.Open(t.TempDir())
	if err != nil {
		t.Fatalf("agentdir.Open() error = %v", err)
	}
	if _, err := ReadRecord(d); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReadRecord() error = %v, want %v", err, os.ErrNotExist)
	}
}
