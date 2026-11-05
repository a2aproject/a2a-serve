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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
)

// limits bound the files a recipe may hold.
type limits struct {
	files int
	bytes int64
}

var defaultLimits = limits{files: 10_000, bytes: 100 << 20}

// File is a file or a symlink of a recipe.
type File struct {
	// Hash is the SHA-256 of a file's content, in hex.
	Hash string
	Exec bool
	// Link is the target of a symlink.
	Link string
	Size int64
}

// String formats f for the record: sha256:<hex>, exec:sha256:<hex> for an
// executable file, or link:<target>.
func (f File) String() string {
	switch {
	case f.Link != "":
		return "link:" + f.Link
	case f.Exec:
		return "exec:sha256:" + f.Hash
	}
	return "sha256:" + f.Hash
}

// ParseFile parses the form of [File.String]. The size isn't recorded.
func ParseFile(s string) (File, error) {
	if target, ok := strings.CutPrefix(s, "link:"); ok && target != "" {
		return File{Link: target}, nil
	}
	exec := false
	if rest, ok := strings.CutPrefix(s, "exec:"); ok {
		s, exec = rest, true
	}
	if hash, ok := strings.CutPrefix(s, "sha256:"); ok && len(hash) == sha256.Size*2 {
		return File{Hash: hash, Exec: exec}, nil
	}
	return File{}, fmt.Errorf("invalid file entry %q", s)
}

// Same reports whether f and g have the same content, link target and
// executable bit.
func (f File) Same(g File) bool {
	return f.Hash == g.Hash && f.Exec == g.Exec && f.Link == g.Link
}

// Files are the files of a recipe by slash-separated path.
type Files map[string]File

// Paths returns the paths of the files, sorted.
func (f Files) Paths() []string {
	return slices.Sorted(maps.Keys(f))
}

// Scan lists the files of the recipe in dir that an agent directory takes:
// everything but .git and .a2a, whose config.yaml and recipe.yaml are read
// on their own. A symlink must be relative and stay inside dir and outside
// .a2a, so that it can't give the agent a host path. Other symlinks and
// special files fail the scan.
func Scan(dir string) (Files, error) {
	return scan(dir, defaultLimits)
}

func scan(dir string, lim limits) (_ Files, err error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	out := Files{}
	var total int64
	err = fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if name == "." {
			return nil
		}
		if d.Name() == ".git" || name == agentdir.Name {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if len(out) == lim.files {
			return fmt.Errorf("the recipe has more than %d files", lim.files)
		}
		f, err := scanFile(root, name, d.Type(), lim.bytes)
		if err != nil {
			return err
		}
		total += f.Size
		if total > lim.bytes {
			return fmt.Errorf("the recipe's files are larger than %s", formatBytes(lim.bytes))
		}
		out[name] = f
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reading the recipe in %s: %w", dir, err)
	}
	return out, nil
}

func formatBytes(n int64) string {
	if n >= 1<<20 {
		return fmt.Sprintf("%d MB", n>>20)
	}
	return fmt.Sprintf("%d bytes", n)
}

func scanFile(root *os.Root, name string, mode fs.FileMode, maxBytes int64) (File, error) {
	switch {
	case mode&fs.ModeSymlink != 0:
		target, err := root.Readlink(name)
		if err != nil {
			return File{}, err
		}
		if err := checkLink(name, target); err != nil {
			return File{}, err
		}
		return File{Link: target}, nil
	case !mode.IsRegular():
		return File{}, fmt.Errorf("%s is not a regular file, directory or symlink", name)
	}
	return hashFile(root, name, maxBytes)
}

// checkLink checks that the symlink name, pointing at target, stays inside
// the recipe and outside .a2a.
func checkLink(name, target string) error {
	if filepath.IsAbs(target) || path.IsAbs(target) {
		return fmt.Errorf("symlink %s points at the absolute path %s; a recipe's symlinks must be relative and stay inside it", name, target)
	}
	resolved := path.Join(path.Dir(name), filepath.ToSlash(target))
	if resolved == ".." || strings.HasPrefix(resolved, "../") {
		return fmt.Errorf("symlink %s points at %s, outside the recipe", name, target)
	}
	if resolved == agentdir.Name || strings.HasPrefix(resolved, agentdir.Name+"/") {
		return fmt.Errorf("symlink %s points into %s", name, agentdir.Name)
	}
	return nil
}

func hashFile(root *os.Root, name string, maxBytes int64) (_ File, err error) {
	r, err := root.Open(name)
	if err != nil {
		return File{}, err
	}
	defer func() { err = errors.Join(err, r.Close()) }()
	fi, err := r.Stat()
	if err != nil {
		return File{}, err
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(r, maxBytes+1))
	if err != nil {
		return File{}, err
	}
	return File{Hash: hex.EncodeToString(h.Sum(nil)), Exec: fi.Mode()&0o111 != 0, Size: n}, nil
}

// Copy copies the files at paths of the recipe in src into the directory
// dst, creating parent directories, without following symlinks out of dst.
// It checks each file against files, as [Scan] listed it, so that a file
// changed since doesn't slip through.
func Copy(src, dst string, files Files, paths []string) (err error) {
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
	for _, name := range paths {
		f, ok := files[name]
		if !ok {
			return fmt.Errorf("%s is not a file of the recipe", name)
		}
		if err := copyFile(from, to, name, f); err != nil {
			return fmt.Errorf("copying %s: %w", name, err)
		}
	}
	return nil
}

func copyFile(from, to *os.Root, name string, f File) error {
	if dir := path.Dir(name); dir != "." {
		if err := to.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	if f.Link != "" {
		if err := to.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return to.Symlink(f.Link, name)
	}
	data, err := from.ReadFile(name)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != f.Hash {
		return errors.New("the file changed while it was copied")
	}
	perm := os.FileMode(0o644)
	if f.Exec {
		perm = 0o755
	}
	return fsutil.WriteAtomicIn(to, name, data, perm)
}
