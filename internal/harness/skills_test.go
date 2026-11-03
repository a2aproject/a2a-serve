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

package harness

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func addSkill(t *testing.T, root, dir, name string) {
	t.Helper()
	path := filepath.Join(root, dir, name)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("os.MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, "SKILL.md"), []byte("---\nname: "+name+"\n---\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
}

// homeSkills maps the entries of a harness's home skill folder to their link
// targets, or to "" for entries which aren't links.
func homeSkills(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return map[string]string{}
	}
	if err != nil {
		t.Fatalf("os.ReadDir() error = %v", err)
	}
	out := map[string]string{}
	for _, e := range entries {
		target, err := os.Readlink(filepath.Join(dir, e.Name()))
		if err != nil {
			target = ""
		}
		out[e.Name()] = target
	}
	return out
}

func TestLinkSkills(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		harness string
		command []string
		dir     string
		want    func(root string) map[string]string
	}{
		{
			name:    "claude gets the skills of other folders",
			harness: "claude",
			dir:     ".claude/skills",
			want: func(root string) map[string]string {
				return map[string]string{
					"deploy": filepath.Join(root, ".agents/skills/deploy"),
					"lint":   filepath.Join(root, ".opencode/skills/lint"),
					"mine":   "",
				}
			},
		},
		{
			name:    "claude with a custom command",
			harness: "claude",
			command: []string{"claude-agent-acp", "--debug"},
			dir:     ".claude/skills",
			want: func(root string) map[string]string {
				return map[string]string{
					"deploy": filepath.Join(root, ".agents/skills/deploy"),
					"lint":   filepath.Join(root, ".opencode/skills/lint"),
					"mine":   "",
				}
			},
		},
		{
			name:    "opencode reads every folder",
			harness: "opencode",
			dir:     ".config/opencode/skills",
			want:    func(string) map[string]string { return map[string]string{"mine": ""} },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root, home := t.TempDir(), t.TempDir()
			addSkill(t, root, ".claude/skills", "review")
			addSkill(t, root, ".agents/skills", "review")
			addSkill(t, root, ".agents/skills", "deploy")
			addSkill(t, root, ".opencode/skills", "lint")
			addSkill(t, home, tc.dir, "mine")
			p, err := Lookup(tc.harness, tc.command)
			if err != nil {
				t.Fatalf("Lookup() error = %v", err)
			}
			ws := Workspace{Dir: root, Home: home}

			if err := LinkSkills(p, ws); err != nil {
				t.Fatalf("LinkSkills() error = %v", err)
			}
			if err := LinkSkills(p, ws); err != nil {
				t.Fatalf("LinkSkills() again error = %v", err)
			}
			if diff := cmp.Diff(tc.want(root), homeSkills(t, filepath.Join(home, tc.dir))); diff != "" {
				t.Fatalf("LinkSkills() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestLinkSkillsRemovesGoneSkills(t *testing.T) {
	t.Parallel()
	root, home := t.TempDir(), t.TempDir()
	addSkill(t, root, ".agents/skills", "deploy")
	addSkill(t, root, ".agents/skills", "lint")
	p, err := Lookup("claude", nil)
	if err != nil {
		t.Fatalf("Lookup() error = %v", err)
	}
	ws := Workspace{Dir: root, Home: home}
	if err := LinkSkills(p, ws); err != nil {
		t.Fatalf("LinkSkills() error = %v", err)
	}
	if err := os.RemoveAll(filepath.Join(root, ".agents/skills/lint")); err != nil {
		t.Fatalf("os.RemoveAll() error = %v", err)
	}
	addSkill(t, root, ".claude/skills", "deploy")

	if err := LinkSkills(p, ws); err != nil {
		t.Fatalf("LinkSkills() error = %v", err)
	}
	if diff := cmp.Diff(map[string]string{}, homeSkills(t, filepath.Join(home, ".claude/skills"))); diff != "" {
		t.Fatalf("LinkSkills() wrong result (-want +got) diff = %s", diff)
	}
}

func TestLinkSkillsCustomHarness(t *testing.T) {
	t.Parallel()
	root, home := t.TempDir(), t.TempDir()
	addSkill(t, root, ".agents/skills", "deploy")
	p, err := Lookup("gemini", []string{"gemini", "--acp"})
	if err != nil {
		t.Fatalf("Lookup() error = %v", err)
	}
	if err := LinkSkills(p, Workspace{Dir: root, Home: home}); err != nil {
		t.Fatalf("LinkSkills() error = %v", err)
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatalf("os.ReadDir() error = %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("LinkSkills() for a harness without a profile wrote %v, want nothing", entries)
	}
}
