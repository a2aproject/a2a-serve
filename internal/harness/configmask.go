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
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
	"github.com/a2aproject/a2a-serve/internal/project"
)

// emptyConfig is an agent configuration file that changes nothing.
const emptyConfig = "{}\n"

// configReader is a profile which reads some of the project configuration
// files of [project.AgentConfigFiles].
type configReader interface {
	projectConfigFiles() []string
}

func (opencode) projectConfigFiles() []string { return project.OpencodeConfigFiles }
func (claude) projectConfigFiles() []string   { return project.ClaudeConfigFiles }

// configFiles returns the project configuration files p reads: all of them
// for an agent of unknown kind.
func configFiles(p Profile) []string {
	if c, ok := p.(custom); ok && c.base != nil {
		p = c.base
	}
	if r, ok := p.(configReader); ok {
		return r.projectConfigFiles()
	}
	return project.AgentConfigFiles
}

// opencodeOwnFiles are what opencode writes into a configuration folder
// itself, which aren't settings.
var opencodeOwnFiles = []string{".gitignore", "package.json", "bun.lock", "node_modules", "skills"}

// configMasks are the mounts that hide the agent's own configuration files.
type configMasks struct {
	mounts []agentdir.Mount
	// scratch are the container paths of the configuration folders, which
	// appear empty.
	scratch []string
	// warnings name the hidden files that hold settings.
	warnings []string
	// check refuses a container start after a configuration file appeared
	// where it wasn't masked.
	check func() error
}

// maskAgentConfig writes the empty configuration file of spec and returns
// the mounts that put it over the agent's configuration files: those of the
// workspace, and the existing ones in the mounts and in the folders of
// mounts that hold the workspace, which opencode also reads.
//
// The workspace gets the files and folders it lacks, since docker would
// otherwise create them as root and the agent could create them in a
// running container. A folder holding a masked file is mounted onto itself,
// so that the agent can't rename it and put another file in its place for
// the processes that the running containers start.
//
// The configuration folders stay writable, since opencode exits when it
// can't write its own files there. What the agent writes stays in its
// container, and opencode has read its configuration before the agent's
// first tool call. The skills folder of a masked .opencode stays visible.
func maskAgentConfig(spec Spec) (configMasks, error) {
	if err := fsutil.WriteAtomic(spec.ConfigMask, []byte(emptyConfig), 0o644); err != nil {
		return configMasks{}, fmt.Errorf("writing %s: %w", spec.ConfigMask, err)
	}
	mounts := slices.Clone(spec.Mounts)
	if spec.WorkspaceSrc != "" {
		mounts[0].Src = spec.WorkspaceSrc
	}
	entries := configFiles(spec.Profile)
	var cm configMasks
	var files, dirs, unmasked []string
	add := func(path string, isDir bool) {
		if isDir {
			dirs = append(dirs, path)
		} else {
			files = append(files, path)
		}
	}
	ws := resolvePath(mounts[0].Src)
	for _, entry := range entries {
		name, isDir := strings.CutSuffix(entry, "/")
		path := filepath.Join(ws, name)
		if err := prepareWorkspaceConfig(ws, name, isDir); err != nil {
			return configMasks{}, err
		}
		if hasSettings(path, isDir) {
			cm.warnings = append(cm.warnings, fmt.Sprintf("the sandbox hides %s from the agent, so its settings don't apply; put them in the agent home directories", path))
		}
		add(path, isDir)
	}
	for _, dir := range configDirs(mounts) {
		for _, entry := range entries {
			name, isDir := strings.CutSuffix(entry, "/")
			path := filepath.Join(dir, name)
			if !isConfig(dir, name, isDir) {
				unmasked = append(unmasked, path)
				continue
			}
			add(path, isDir)
		}
	}
	for _, file := range files {
		for _, m := range exposures(mounts, file) {
			cm.mounts = append(cm.mounts, agentdir.Mount{Src: spec.ConfigMask, Dst: m.Dst, ReadOnly: true})
		}
	}
	var parents []string
	for _, file := range files {
		if parent := filepath.Dir(file); !slices.Contains(parents, parent) && !slices.ContainsFunc(mounts, func(m agentdir.Mount) bool { return resolvePath(m.Src) == parent }) {
			parents = append(parents, parent)
			cm.mounts = append(cm.mounts, exposures(mounts, parent)...)
		}
	}
	for _, dir := range dirs {
		skills := filepath.Join(dir, "skills")
		fi, err := os.Lstat(skills)
		keep := err == nil && fi.IsDir()
		for _, m := range exposures(mounts, dir) {
			if slices.Contains(cm.scratch, m.Dst) {
				continue
			}
			cm.scratch = append(cm.scratch, m.Dst)
			if keep {
				cm.mounts = append(cm.mounts, agentdir.Mount{Src: skills, Dst: filepath.Join(m.Dst, "skills"), ReadOnly: m.ReadOnly})
			}
		}
	}
	cm.check = func() error {
		for _, path := range unmasked {
			if _, err := os.Lstat(path); err == nil {
				return fmt.Errorf("%s appeared after this agent started, and its sandbox doesn't hide it yet; restart this agent with `a2a-serve restart`", path)
			}
		}
		return nil
	}
	return cm, nil
}

// configDirs returns the folders besides the workspace where the agent's
// configuration is masked: the mount sources, and the folders of mounts
// between a mount that holds the workspace and the workspace.
func configDirs(mounts []agentdir.Mount) []string {
	var out []string
	add := func(dir string) {
		if dir = resolvePath(dir); !slices.Contains(out, dir) && dir != resolvePath(mounts[0].Src) {
			out = append(out, dir)
		}
	}
	ws := mounts[0].Dst
	for _, m := range mounts[1:] {
		add(m.Src)
		if m.Dst == ws || !within(m.Dst, ws) {
			continue
		}
		rel, err := filepath.Rel(m.Dst, filepath.Dir(ws))
		if err != nil || rel == "." {
			continue
		}
		dir := m.Src
		for _, part := range strings.Split(rel, string(filepath.Separator)) {
			dir = filepath.Join(dir, part)
			add(dir)
		}
	}
	return out
}

// prepareWorkspaceConfig makes name in the workspace ws a file or folder,
// creating it when it is missing. It refuses symlinks on the way, which
// the agent could repoint after the server masked their target.
func prepareWorkspaceConfig(ws, name string, isDir bool) error {
	refuse := func(path, why string) error {
		return fmt.Errorf("%s %s; the agent could use it to change its own permissions, so remove it", path, why)
	}
	path := ws
	parts := strings.Split(name, "/")
	for i, part := range parts {
		path = filepath.Join(path, part)
		last := i == len(parts)-1
		fi, err := os.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) {
			if err := createConfig(path, isDir || !last); err != nil {
				return fmt.Errorf("creating %s, which keeps the agent from changing its own permissions: %w", path, err)
			}
			continue
		}
		if err != nil {
			return err
		}
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			return refuse(path, "is a symlink")
		case (isDir || !last) && !fi.IsDir():
			return refuse(path, "isn't a folder")
		case !isDir && last && !fi.Mode().IsRegular():
			return refuse(path, "isn't a file")
		}
	}
	return nil
}

func createConfig(path string, isDir bool) error {
	if isDir {
		return os.Mkdir(path, 0o755)
	}
	return writeIfMissing(path, []byte(emptyConfig))
}

// isConfig reports whether name in dir is a configuration file or folder,
// with no symlink on the way.
func isConfig(dir, name string, isDir bool) bool {
	path := dir
	parts := strings.Split(name, "/")
	for i, part := range parts {
		path = filepath.Join(path, part)
		fi, err := os.Lstat(path)
		if err != nil || fi.Mode()&fs.ModeSymlink != 0 {
			return false
		}
		if i == len(parts)-1 {
			return isDir == fi.IsDir() && (isDir || fi.Mode().IsRegular())
		}
	}
	return false
}

// hasSettings reports whether the configuration file or folder at path
// holds settings that the mask hides.
func hasSettings(path string, isDir bool) bool {
	if !isDir {
		data, err := os.ReadFile(path)
		return err == nil && !slices.Contains([]string{"", "{}"}, string(bytes.TrimSpace(data)))
	}
	entries, err := os.ReadDir(path)
	return err == nil && slices.ContainsFunc(entries, func(e fs.DirEntry) bool { return !slices.Contains(opencodeOwnFiles, e.Name()) })
}

// exposures returns the mounts of the host path wherever mounts expose it.
func exposures(mounts []agentdir.Mount, path string) []agentdir.Mount {
	var out []agentdir.Mount
	for _, m := range mounts {
		src := resolvePath(m.Src)
		if !within(src, path) {
			continue
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			continue
		}
		dst := filepath.Join(m.Dst, rel)
		if !slices.ContainsFunc(out, func(o agentdir.Mount) bool { return o.Dst == dst }) {
			out = append(out, agentdir.Mount{Src: path, Dst: dst, ReadOnly: m.ReadOnly})
		}
	}
	return out
}

// within reports whether path is root or in it.
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
