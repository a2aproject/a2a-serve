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
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/a2aproject/a2a-serve/internal/project"
)

// skillReader is a profile which reads skills from some of the project
// skill folders, and from a folder in its home.
type skillReader interface {
	// projectSkillDirs are the entries of [project.SkillDirs] it reads.
	projectSkillDirs() []string
	// homeSkillDir is the folder in its home it reads, relative to the home.
	homeSkillDir() string
}

func (opencode) projectSkillDirs() []string { return project.SkillDirs }
func (opencode) homeSkillDir() string       { return filepath.Join(".config", "opencode", "skills") }

func (claude) projectSkillDirs() []string { return []string{".claude/skills"} }
func (claude) homeSkillDir() string       { return filepath.Join(".claude", "skills") }

// LinkSkills links the project skills which p doesn't read from the
// project into the skill folder of its home, so every harness gets every
// project skill. It removes the links of skills which are gone, and leaves
// the skills the user put there alone. The harness can change its home, so
// symlinks that lead out of it are refused.
func LinkSkills(p Profile, ws Workspace) (err error) {
	if c, ok := p.(custom); ok {
		p = c.base
	}
	r, ok := p.(skillReader)
	if !ok {
		return nil
	}
	skills, err := project.Skills(ws.Dir)
	if err != nil {
		return err
	}
	reads := r.projectSkillDirs()
	want := map[string]string{}
	native := map[string]bool{}
	for _, s := range skills {
		if slices.Contains(reads, s.Dir) {
			native[s.Name] = true
			continue
		}
		if _, ok := want[s.Name]; !ok {
			want[s.Name] = s.Path(ws.Dir)
		}
	}
	for name := range native {
		delete(want, name)
	}
	root, err := os.OpenRoot(ws.Home)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	home := r.homeSkillDir()
	if err := removeStaleSkillLinks(root, home, ws.Dir, want); err != nil {
		return err
	}
	for _, name := range slices.Sorted(maps.Keys(want)) {
		link := filepath.Join(home, name)
		if _, err := root.Lstat(link); err == nil {
			continue
		}
		if err := root.MkdirAll(home, 0o700); err != nil {
			return err
		}
		if err := root.Symlink(want[name], link); err != nil {
			return fmt.Errorf("linking skill %s: %w", name, err)
		}
	}
	return nil
}

// removeStaleSkillLinks removes the links in the folder home of root to
// project skill folders of dir which aren't wanted any more.
func removeStaleSkillLinks(root *os.Root, home, dir string, want map[string]string) error {
	f, err := root.Open(home)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	entries, err := f.ReadDir(-1)
	if err := errors.Join(err, f.Close()); err != nil {
		return err
	}
	for _, e := range entries {
		if e.Type()&os.ModeSymlink == 0 {
			continue
		}
		link := filepath.Join(home, e.Name())
		target, err := root.Readlink(link)
		if err != nil {
			return err
		}
		if !isProjectSkill(dir, target) || want[e.Name()] == target {
			continue
		}
		if err := root.Remove(link); err != nil {
			return err
		}
	}
	return nil
}

func isProjectSkill(root, path string) bool {
	for _, dir := range project.SkillDirs {
		if strings.HasPrefix(path, filepath.Join(root, dir)+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
