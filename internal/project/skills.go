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

package project

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/a2aproject/a2a-serve/internal/fsutil"
)

// SkillDirs are the project folders harnesses read skills from, each skill
// being a <name>/SKILL.md inside one.
var SkillDirs = []string{".claude/skills", ".opencode/skills", ".agents/skills"}

// AgentConfigFiles are the project files of the agents' own configuration,
// which can change what the agent runs and approves. Directories end with a
// slash.
var AgentConfigFiles = []string{".claude/settings.json", ".claude/settings.local.json", "opencode.json", "opencode.jsonc", ".opencode/"}

// Skill is a skill of a project.
type Skill struct {
	Name string `json:"name"`
	// Dir is the skill folder holding it, one of SkillDirs.
	Dir string `json:"dir"`
}

// Path returns the skill's directory in the project at root.
func (s Skill) Path(root string) string {
	return filepath.Join(root, s.Dir, s.Name)
}

// Skills lists the skills in the skill folders of the project at root, in
// the order of SkillDirs, then by name. The agent can write the project, so
// folders and skills behind symlinks that lead out of root are left out.
func Skills(root string) (out []Skill, err error) {
	r, err := os.OpenRoot(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, r.Close()) }()
	for _, dir := range SkillDirs {
		entries, err := readDir(r, dir)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			s := Skill{Name: e.Name(), Dir: dir}
			if _, err := r.Stat(filepath.Join(dir, s.Name, "SKILL.md")); err != nil {
				continue
			}
			out = append(out, s)
		}
	}
	return out, nil
}

// readDir lists the folder dir of r. A missing folder, or a symlink r
// refuses to follow, has no entries.
func readDir(r *os.Root, dir string) ([]os.DirEntry, error) {
	entries, err := fsutil.ReadDirIn(r, dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		if fi, lerr := r.Lstat(dir); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
			return nil, nil
		}
		return nil, err
	}
	slices.SortFunc(entries, func(a, b os.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return entries, nil
}
