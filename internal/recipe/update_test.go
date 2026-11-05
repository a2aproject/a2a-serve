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
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestPlanFiles(t *testing.T) {
	t.Parallel()
	v := func(s string) File { return File{Hash: sha(s)} }
	tests := []struct {
		name     string
		base     Files
		local    Files
		upstream Files
		want     []FileChange
	}{
		{
			name:     "unchanged everywhere",
			base:     Files{"a": v("1")},
			local:    Files{"a": v("1")},
			upstream: Files{"a": v("1")},
		},
		{
			name:     "upstream changed",
			base:     Files{"a": v("1")},
			local:    Files{"a": v("1")},
			upstream: Files{"a": v("2")},
			want:     []FileChange{{Path: "a", Action: FileUpdate}},
		},
		{
			name:     "upstream added",
			upstream: Files{"a": v("1")},
			want:     []FileChange{{Path: "a", Action: FileAdd}},
		},
		{
			name:  "upstream removed",
			base:  Files{"a": v("1")},
			local: Files{"a": v("1")},
			want:  []FileChange{{Path: "a", Action: FileRemove}},
		},
		{
			name:     "local changed",
			base:     Files{"a": v("1")},
			local:    Files{"a": v("mine")},
			upstream: Files{"a": v("1")},
		},
		{
			name:     "same change on both sides",
			base:     Files{"a": v("1")},
			local:    Files{"a": v("2")},
			upstream: Files{"a": v("2")},
		},
		{
			name:     "both changed",
			base:     Files{"a": v("1")},
			local:    Files{"a": v("mine")},
			upstream: Files{"a": v("2")},
			want:     []FileChange{{Path: "a", Action: FileConflict}},
		},
		{
			name:     "removed locally, changed upstream",
			base:     Files{"a": v("1")},
			upstream: Files{"a": v("2")},
			want:     []FileChange{{Path: "a", Action: FileKeepRemoved}},
		},
		{
			name:  "changed locally, removed upstream",
			base:  Files{"a": v("1")},
			local: Files{"a": v("mine")},
			want:  []FileChange{{Path: "a", Action: FileKeepChanged}},
		},
		{
			name:     "added on both sides",
			local:    Files{"a": v("mine")},
			upstream: Files{"a": v("2")},
			want:     []FileChange{{Path: "a", Action: FileConflict}},
		},
		{
			name:     "executable bit changed upstream",
			base:     Files{"a": v("1")},
			local:    Files{"a": v("1")},
			upstream: Files{"a": {Hash: sha("1"), Exec: true}},
			want:     []FileChange{{Path: "a", Action: FileUpdate}},
		},
		{
			name:     "rerun after a partial update",
			base:     Files{"a": v("1"), "b": v("1")},
			local:    Files{"a": v("2"), "b": v("1")},
			upstream: Files{"a": v("2"), "b": v("2")},
			want:     []FileChange{{Path: "b", Action: FileUpdate}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(tc.want, PlanFiles(tc.base, tc.local, tc.upstream)); diff != "" {
				t.Fatalf("PlanFiles() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestApplyFiles(t *testing.T) {
	t.Parallel()
	agent := t.TempDir()
	writeFile(t, agent, "AGENTS.md", "v1")
	writeFile(t, agent, "ENVIRONMENT.md", "mine")
	writeFile(t, agent, ".agents/skills/old/SKILL.md", "old")
	writeFile(t, agent, "NOTES.md", "notes")
	writeFile(t, agent, "README.md", "v2")
	writeFile(t, agent, "README.md.recipe", "v2")
	base := Files{
		"AGENTS.md":                   {Hash: sha("v1")},
		"ENVIRONMENT.md":              {Hash: sha("template v1")},
		".agents/skills/old/SKILL.md": {Hash: sha("old")},
		"README.md":                   {Hash: sha("v1")},
	}
	upstream := t.TempDir()
	writeFile(t, upstream, "AGENTS.md", "v2")
	writeFile(t, upstream, "ENVIRONMENT.md", "template v2")
	writeFile(t, upstream, ".agents/skills/new/SKILL.md", "new")
	writeFile(t, upstream, "README.md", "v2")
	up, err := Scan(upstream)
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	local, err := LocalFiles(agent, append(base.Paths(), up.Paths()...))
	if err != nil {
		t.Fatalf("LocalFiles() error = %v", err)
	}
	changes := PlanFiles(base, local, up)
	want := []FileChange{
		{Path: ".agents/skills/new/SKILL.md", Action: FileAdd},
		{Path: ".agents/skills/old/SKILL.md", Action: FileRemove},
		{Path: "AGENTS.md", Action: FileUpdate},
		{Path: "ENVIRONMENT.md", Action: FileConflict},
	}
	if diff := cmp.Diff(want, changes); diff != "" {
		t.Fatalf("PlanFiles() wrong result (-want +got) diff = %s", diff)
	}
	if !TouchesSkills(changes) {
		t.Fatalf("TouchesSkills() = false, want true")
	}

	if err := ApplyFiles(upstream, agent, up, changes); err != nil {
		t.Fatalf("ApplyFiles() error = %v", err)
	}

	for name, want := range map[string]string{
		"AGENTS.md":                   "v2",
		"ENVIRONMENT.md":              "mine",
		"ENVIRONMENT.md.recipe":       "template v2",
		".agents/skills/new/SKILL.md": "new",
		"NOTES.md":                    "notes",
	} {
		data, err := os.ReadFile(filepath.Join(agent, filepath.FromSlash(name)))
		if err != nil || string(data) != want {
			t.Fatalf("%s = %q, %v, want %q", name, data, err, want)
		}
	}
	for _, name := range []string{".agents/skills/old", "README.md.recipe"} {
		if _, err := os.Lstat(filepath.Join(agent, filepath.FromSlash(name))); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("Lstat(%s) error = %v, want it removed", name, err)
		}
	}
	outstanding, err := Outstanding(agent, up)
	if err != nil {
		t.Fatalf("Outstanding() error = %v", err)
	}
	if diff := cmp.Diff([]string{"ENVIRONMENT.md"}, outstanding); diff != "" {
		t.Fatalf("Outstanding() wrong result (-want +got) diff = %s", diff)
	}
}

func TestLocalEdits(t *testing.T) {
	t.Parallel()
	base := Files{".mcp.json": {Hash: sha("1")}, "opencode.json": {Hash: sha("1")}, ".opencode/agent/x.md": {Hash: sha("1")}, "AGENTS.md": {Hash: sha("1")}}
	local := Files{".mcp.json": {Hash: sha("2")}, "opencode.json": {Hash: sha("1")}, "AGENTS.md": {Hash: sha("2")}}
	if diff := cmp.Diff([]string{".mcp.json", ".opencode/agent/x.md"}, LocalEdits(base, local)); diff != "" {
		t.Fatalf("LocalEdits() wrong result (-want +got) diff = %s", diff)
	}
}
