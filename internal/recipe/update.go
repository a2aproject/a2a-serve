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
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"slices"
	"strings"

	"github.com/a2aproject/a2a-serve/internal/project"
)

// RecipeSuffix is added to the name of the recipe's version of a file that
// changed both locally and in the recipe.
const RecipeSuffix = ".recipe"

// FileAction is what an update does with a file of the recipe.
type FileAction int

// File actions.
const (
	// FileAdd writes a file the recipe added.
	FileAdd FileAction = iota
	// FileUpdate writes the recipe's new version of an unchanged file.
	FileUpdate
	// FileRemove removes an unchanged file the recipe removed.
	FileRemove
	// FileConflict keeps a file changed both locally and in the recipe, and
	// saves the recipe's version next to it with [RecipeSuffix].
	FileConflict
	// FileKeepRemoved leaves out a file removed locally that the recipe
	// changed.
	FileKeepRemoved
	// FileKeepChanged keeps a file changed locally that the recipe removed.
	FileKeepChanged
)

// FileChange is what an update does with the file at Path.
type FileChange struct {
	Path   string
	Action FileAction
}

// PlanFiles merges each file three ways: base is the recipe at the recorded
// commit, local the agent's directory, upstream the newly fetched recipe. A
// file local didn't change takes upstream's version; a file upstream didn't
// change keeps the local one. A file both changed differently stays as it
// is locally. Files outside the recipe aren't part of the plan.
func PlanFiles(base, local, upstream Files) []FileChange {
	paths := slices.Sorted(maps.Keys(base))
	for p := range upstream {
		if _, ok := base[p]; !ok {
			paths = append(paths, p)
		}
	}
	slices.Sort(paths)
	var out []FileChange
	for _, p := range paths {
		b, inBase := base[p]
		l, inLocal := local[p]
		u, inUpstream := upstream[p]
		same := func(x File, inX bool, y File, inY bool) bool {
			return inX == inY && (!inX || x.Same(y))
		}
		var action FileAction
		switch {
		case same(u, inUpstream, b, inBase), same(l, inLocal, u, inUpstream):
			continue
		case same(l, inLocal, b, inBase) && !inUpstream:
			action = FileRemove
		case same(l, inLocal, b, inBase) && !inLocal:
			action = FileAdd
		case same(l, inLocal, b, inBase):
			action = FileUpdate
		case !inLocal:
			action = FileKeepRemoved
		case !inUpstream:
			action = FileKeepChanged
		default:
			action = FileConflict
		}
		out = append(out, FileChange{Path: p, Action: action})
	}
	return out
}

// LocalFiles returns the files at paths in the agent directory dir, in the
// form [Scan] lists them. A path that holds something else, such as a
// directory, is listed with a hash that matches no file.
func LocalFiles(dir string, paths []string) (_ Files, err error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	out := Files{}
	for _, p := range paths {
		f, err := entryOf(root, p)
		if err != nil {
			return nil, err
		}
		if f != nil {
			out[p] = *f
		}
	}
	return out, nil
}

// ApplyFiles makes the changes of PlanFiles in the agent directory dst,
// copying files from the fetched recipe in src, as upstream lists them. It
// also removes the [RecipeSuffix] copies that now match their file.
func ApplyFiles(src, dst string, upstream Files, changes []FileChange) (err error) {
	from, err := os.OpenRoot(src)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, from.Close()) }()
	to, err := os.OpenRoot(dst)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, to.Close()) }()
	for _, c := range changes {
		var err error
		switch c.Action {
		case FileAdd, FileUpdate:
			err = copyFile(from, to, c.Path, c.Path, upstream[c.Path])
		case FileConflict:
			err = copyFile(from, to, c.Path, c.Path+RecipeSuffix, upstream[c.Path])
		case FileRemove:
			err = removeFile(to, c.Path)
		}
		if err != nil {
			return fmt.Errorf("updating %s: %w", c.Path, err)
		}
	}
	return removeResolved(to, upstream)
}

// removeFile removes name and the directories its removal leaves empty.
func removeFile(root *os.Root, name string) error {
	if err := root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for dir := path.Dir(name); dir != "."; dir = path.Dir(dir) {
		entries, err := fs.ReadDir(root.FS(), dir)
		if err != nil || len(entries) > 0 {
			return nil
		}
		if err := root.Remove(dir); err != nil {
			return err
		}
	}
	return nil
}

// RemoveResolved removes the [RecipeSuffix] copies in the agent directory
// dir that match their file, whose conflict the owner resolved.
func RemoveResolved(dir string, upstream Files) (err error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	return removeResolved(root, upstream)
}

// removeResolved removes the recipe copies that match their file, whose
// conflict the owner resolved.
func removeResolved(root *os.Root, upstream Files) error {
	for _, p := range upstream.Paths() {
		copied, err := entryOf(root, p+RecipeSuffix)
		if err != nil || copied == nil {
			continue
		}
		local, err := entryOf(root, p)
		if err != nil || local == nil || !local.Same(*copied) {
			continue
		}
		if err := root.Remove(p + RecipeSuffix); err != nil {
			return err
		}
	}
	return nil
}

// Outstanding returns the files of upstream in the agent directory dir
// whose recipe copy differs from them, which the owner hasn't resolved yet.
func Outstanding(dir string, upstream Files) (_ []string, err error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	var out []string
	for _, p := range upstream.Paths() {
		copied, err := entryOf(root, p+RecipeSuffix)
		if err != nil {
			return nil, err
		}
		if copied == nil {
			continue
		}
		if local, err := entryOf(root, p); err != nil || local == nil || !local.Same(*copied) {
			out = append(out, p)
		}
	}
	return out, nil
}

// notAFile is the hash of an entry that is neither a file nor a symlink,
// which matches no file.
const notAFile = "not a file"

// entryOf returns the file or symlink name in root, or nil if there is
// nothing there.
func entryOf(root *os.Root, name string) (*File, error) {
	fi, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		target, err := root.Readlink(name)
		if err != nil {
			return nil, err
		}
		return &File{Link: target}, nil
	case fi.Mode().IsRegular():
		f, err := hashFile(root, name, defaultLimits.bytes)
		return &f, err
	}
	return &File{Hash: notAFile}, nil
}

// riskyFiles decide what the agent runs and approves: the MCP servers and
// the agent's own configuration.
var riskyFiles = append([]string{project.MCPFile}, agentConfigFiles...)

// IsRisky reports whether the file at p decides what the agent runs.
func IsRisky(p string) bool {
	return slices.ContainsFunc(riskyFiles, func(r string) bool {
		dir, isDir := strings.CutSuffix(r, "/")
		return p == r || isDir && strings.HasPrefix(p, dir+"/")
	})
}

// LocalEdits returns the files of the recipe at base that decide what the
// agent runs and that changed in the agent directory, as local lists them.
func LocalEdits(base, local Files) []string {
	var out []string
	for _, p := range base.Paths() {
		l, ok := local[p]
		if IsRisky(p) && (!ok || !l.Same(base[p])) {
			out = append(out, p)
		}
	}
	return out
}

// TouchesSkills reports whether the changes add, change or remove a skill,
// which the server links into the agent's home when an agent process
// starts.
func TouchesSkills(changes []FileChange) bool {
	return slices.ContainsFunc(changes, func(c FileChange) bool {
		if c.Action != FileAdd && c.Action != FileUpdate && c.Action != FileRemove {
			return false
		}
		return slices.ContainsFunc(project.SkillDirs, func(dir string) bool { return strings.HasPrefix(c.Path, dir+"/") })
	})
}
