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

package projectfiles

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

// ClaudeConfigFiles, OpencodeConfigFiles, CopilotConfigFiles and
// AgyConfigFiles are the project files of the agents' own configuration,
// which can change what the agent runs and approves. Copilot also reads the
// hooks of Claude Code's files. Antigravity CLI reads the same folder under
// four names, and .gemini/config.json can point it to another one.
// Directories end with a slash.
var (
	ClaudeConfigFiles   = []string{".claude/settings.json", ".claude/settings.local.json"}
	OpencodeConfigFiles = []string{"opencode.json", "opencode.jsonc", ".opencode/"}
	CopilotConfigFiles  = []string{
		".github/copilot/", ".github/hooks/", ".github/extensions/", ".github/mcp.json", ".github/lsp.json",
		".claude/settings.json", ".claude/settings.local.json",
	}
	AgyConfigFiles = []string{".agents/", ".agent/", "_agents/", "_agent/", ".gemini/config.json"}
)

// AgentConfigFiles are the configuration files of every agent.
var AgentConfigFiles = agentConfigFiles(ClaudeConfigFiles, OpencodeConfigFiles, CopilotConfigFiles, AgyConfigFiles)

// IsAgentConfig reports whether p, a slash-separated path in a project, is
// an entry of [AgentConfigFiles] or in one of its folders. The skills
// folders of those folders don't count: the sandbox hides the rest of the
// folder from the agent and keeps its skills.
func IsAgentConfig(p string) bool {
	for _, f := range AgentConfigFiles {
		dir, isDir := strings.CutSuffix(f, "/")
		if p == dir && !isDir {
			return true
		}
		if rest, ok := strings.CutPrefix(p, dir+"/"); ok && isDir && rest != "skills" && !strings.HasPrefix(rest, "skills/") {
			return true
		}
	}
	return false
}

func agentConfigFiles(lists ...[]string) []string {
	var out []string
	for _, f := range slices.Concat(lists...) {
		if !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	return out
}

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
