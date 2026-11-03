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
)

// SkillDirs are the project folders harnesses read skills from, each skill
// being a <name>/SKILL.md inside one.
var SkillDirs = []string{".claude/skills", ".opencode/skills", ".agents/skills"}

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
// the order of SkillDirs, then by name.
func Skills(root string) ([]Skill, error) {
	var out []Skill
	for _, dir := range SkillDirs {
		entries, err := os.ReadDir(filepath.Join(root, dir))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			s := Skill{Name: e.Name(), Dir: dir}
			if _, err := os.Stat(filepath.Join(s.Path(root), "SKILL.md")); err != nil {
				continue
			}
			out = append(out, s)
		}
	}
	return out, nil
}
