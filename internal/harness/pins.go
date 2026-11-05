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
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/a2aproject/a2a-serve/internal/fsutil"
)

// Pins are the identities of the directories that container starts rely
// on, such as the agent directory and the homes, as the server first saw
// them. A directory that the agent renamed and recreated through a
// writable mount has a new identity.
type Pins struct {
	mu   sync.Mutex
	dirs map[string]os.FileInfo
	// base are checked before every container start, other dirs before the
	// starts that use them as their home.
	base []string
}

// NewPins pins dirs, which every container start relies on.
func NewPins(dirs ...string) (*Pins, error) {
	p := &Pins{dirs: map[string]os.FileInfo{}, base: slices.Clone(dirs)}
	for _, dir := range dirs {
		if err := p.Pin(dir); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// Pin pins dir when it is new to p, and otherwise checks that it is the
// directory pinned.
func (p *Pins) Pin(dir string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.dirs[dir]; ok {
		return p.checkLocked(dir)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	// Windows reads the file ID on first comparison, so read it now.
	os.SameFile(fi, fi)
	p.dirs[dir] = fi
	return nil
}

// Unpin forgets dir, such as the home of a removed user.
func (p *Pins) Unpin(dir string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.dirs, dir)
}

// check checks the base directories and home, if pinned.
func (p *Pins) check(home string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, dir := range append(slices.Clone(p.base), home) {
		if _, ok := p.dirs[dir]; !ok {
			continue
		}
		if err := p.checkLocked(dir); err != nil {
			return err
		}
	}
	return nil
}

func (p *Pins) checkLocked(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil && os.SameFile(p.dirs[dir], fi) {
		return nil
	}
	return fmt.Errorf("%s was replaced after the agent server first used it; the agent may have renamed it through a writable mount, so check it and restart the agent server", dir)
}

// PrepareHome makes the home of spec a real directory and pins it. With
// Docker, it first refuses paths that the agent could have redirected, as
// [Prepared.Check] does. Call it before writing into the home.
func PrepareHome(spec Spec) error {
	if spec.Sandbox == "" || spec.Sandbox == SandboxDocker {
		if err := checkPaths(spec); err != nil {
			return err
		}
	}
	// The parent is state/homes for the homes of users.
	for _, dir := range []string{filepath.Dir(spec.Home), spec.Home} {
		if err := fsutil.MkdirReal(dir); err != nil {
			return err
		}
	}
	if spec.Pins == nil {
		return nil
	}
	return spec.Pins.Pin(spec.Home)
}

// checkPaths refuses a container start that docker could follow out of
// the sandbox: a mount source, hidden path or home that goes through a
// symlink in a writable mount, which the agent can repoint, or a pinned
// directory the agent replaced.
func checkPaths(spec Spec) error {
	var roots []writableRoot
	paths := slices.Clone(spec.Hidden)
	for _, m := range spec.Mounts {
		paths = append(paths, m.Src)
		if !m.ReadOnly {
			roots = append(roots, writableRoot{src: m.Src, resolved: resolvePath(m.Src)})
		}
	}
	if spec.Home != "" {
		paths = append(paths, spec.Home)
		roots = append(roots, writableRoot{src: spec.Home, resolved: resolvePath(spec.Home)})
	}
	for _, path := range paths {
		abs, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		w := pathWalk{roots: roots}
		_, err = w.walk(abs)
		var se *symlinkError
		if errors.As(err, &se) {
			return fmt.Errorf("%s goes through the symlink %s in the writable mount %s, which the agent can repoint; use the resolved path %s instead", path, se.link, se.mount, resolvePath(path))
		}
		if err != nil {
			return err
		}
	}
	if spec.Pins == nil {
		return nil
	}
	return spec.Pins.check(spec.Home)
}

// writableRoot is the source of a writable mount and the path it resolves
// to, which docker mounts.
type writableRoot struct {
	src, resolved string
}

type symlinkError struct {
	link, mount string
}

func (e *symlinkError) Error() string {
	return fmt.Sprintf("%s is a symlink in the writable mount %s", e.link, e.mount)
}

// pathWalk resolves paths one component at a time with Lstat, as docker
// would, and stops at a symlink in a directory that a writable mount
// exposes.
type pathWalk struct {
	roots []writableRoot
	hops  int
}

// walk returns p with symlinks resolved. Of a path that doesn't exist, it
// resolves the part that does.
func (w *pathWalk) walk(p string) (string, error) {
	cur := filepath.VolumeName(p) + string(filepath.Separator)
	names := strings.Split(strings.TrimPrefix(p, cur), string(filepath.Separator))
	for i, name := range names {
		if name == "" {
			continue
		}
		next := filepath.Join(cur, name)
		fi, err := os.Lstat(next)
		if errors.Is(err, os.ErrNotExist) {
			return filepath.Join(append([]string{cur}, names[i:]...)...), nil
		}
		if err != nil {
			return "", err
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			cur = next
			continue
		}
		if mount, ok := w.writable(cur); ok {
			return "", &symlinkError{link: next, mount: mount}
		}
		if w.hops++; w.hops > maxSymlinks {
			return "", fmt.Errorf("%s: too many levels of symlinks", p)
		}
		target, err := os.Readlink(next)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(cur, target)
		}
		if cur, err = w.walk(filepath.Clean(target)); err != nil {
			return "", err
		}
	}
	return cur, nil
}

// writable returns the source of a writable mount that exposes dir, where
// the agent can change entries.
func (w *pathWalk) writable(dir string) (string, bool) {
	for _, r := range w.roots {
		rel, err := filepath.Rel(r.resolved, dir)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return r.src, true
		}
	}
	return "", false
}
