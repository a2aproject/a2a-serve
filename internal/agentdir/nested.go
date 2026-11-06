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

package agentdir

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// FindBelow returns the .a2a entries in root and below it, directories or
// symlinks, without descending into them. It fails after visiting more
// than limit directories. Directories it may not read are skipped: the
// sandbox, which runs as the same user, can't read them either.
func FindBelow(root string, limit int) ([]string, error) {
	var out []string
	visited := 0
	err := filepath.WalkDir(root, func(path string, e fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrPermission) && path != root {
			return fs.SkipDir
		}
		if err != nil {
			return err
		}
		if e.Name() == Name && path != root && (e.IsDir() || e.Type()&fs.ModeSymlink != 0) {
			out = append(out, path)
			if e.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !e.IsDir() {
			return nil
		}
		if visited++; visited > limit {
			return fmt.Errorf("%s has more than %d directories, too many to search for other agents; mount a smaller directory", root, limit)
		}
		return nil
	})
	return out, err
}

// BroadMount reports whether a mount of src exposes the user's home
// directory: src is the home directory, / or a directory that holds it.
func BroadMount(src string) (bool, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return false, err
	}
	src = resolved(src)
	if src == string(filepath.Separator) {
		return true, nil
	}
	rel, err := filepath.Rel(src, resolved(home))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)), nil
}

// resolved returns p with symlinks resolved, or cleaned if it doesn't exist.
func resolved(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}
