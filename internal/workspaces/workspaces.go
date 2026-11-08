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

// Package workspaces keeps the private workspaces of agent containers whose
// sandbox gives them their own storage: a copy of the workspace made at
// first use, or an empty directory. Each container has a directory named
// after it, which holds one workspace per storage mode, the principal it
// belongs to and when it was last used.
package workspaces

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
)

// Limits of a copy of the workspace.
const (
	// MaxBytes bounds the size of the regular files of a copy.
	MaxBytes = 1 << 30
	// MaxEntries bounds the files, directories and symlinks of a copy.
	MaxEntries = 200_000
	// MaxCopies bounds the copies of the workspace the store keeps.
	MaxCopies = 100
)

// Files in the directory of a container besides its workspaces.
const (
	principalFile = "principal"
	usedFile      = "used"
	tmpPrefix     = ".tmp-"
)

// skipped are the entries a copy leaves out at any depth: the directories of
// agents, which hold their secrets.
var skipped = []string{agentdir.Name}

// ErrTooLarge is returned for a workspace over the limits of a copy.
var ErrTooLarge = errors.New("the workspace is too large to copy")

// ErrTooMany is returned for a copy over MaxCopies.
var ErrTooMany = fmt.Errorf("the agent keeps %d copies of the workspace already", MaxCopies)

// Store keeps the private workspaces under Root.
type Store struct {
	Root string
	// MaxBytes and MaxEntries bound a copy; zero takes the package limits.
	MaxBytes   int64
	MaxEntries int

	// copyMu serializes the copies, so that concurrent first uses can't
	// pass MaxCopies.
	copyMu sync.Mutex
}

// New returns the store of the private workspaces under root.
func New(root string) *Store { return &Store{Root: root} }

// Dir returns the workspace of storage in the directory of the container
// name.
func (s *Store) Dir(name, storage string) string { return filepath.Join(s.Root, name, storage) }

// Prepare returns the private workspace of storage, own or empty, of the
// container name, which belongs to principal. The first use of own copies
// src into it. It marks the workspace as used.
func (s *Store) Prepare(ctx context.Context, name, principal, storage, src string) (string, error) {
	if storage != agentdir.StorageOwn && storage != agentdir.StorageEmpty {
		return "", fmt.Errorf("no private workspace for storage %q", storage)
	}
	if err := fsutil.MkdirPrivate(s.Root); err != nil {
		return "", err
	}
	base := filepath.Join(s.Root, name)
	if err := fsutil.MkdirPrivate(base); err != nil {
		return "", err
	}
	if err := fsutil.WriteAtomic(filepath.Join(base, principalFile), []byte(principal+"\n"), 0o600); err != nil {
		return "", err
	}
	dir := s.Dir(name, storage)
	switch _, err := os.Lstat(dir); {
	case err == nil:
	case !errors.Is(err, fs.ErrNotExist):
		return "", err
	case storage == agentdir.StorageEmpty:
		if err := os.Mkdir(dir, 0o755); err != nil {
			return "", err
		}
	default:
		if err := s.copyOnce(ctx, src, base, dir); err != nil {
			return "", err
		}
	}
	return dir, s.Touch(name)
}

// copyOnce copies src to dir, unless another copy made it meanwhile, while
// no other copy runs. It refuses a new copy once the store keeps MaxCopies.
func (s *Store) copyOnce(ctx context.Context, src, base, dir string) error {
	s.copyMu.Lock()
	defer s.copyMu.Unlock()
	if _, err := os.Lstat(dir); err == nil {
		return nil
	}
	if err := s.checkCopies(); err != nil {
		return err
	}
	return s.copyTree(ctx, src, base, dir)
}

// Used returns when the workspaces of the container name were last used.
func (s *Store) Used(name string) (time.Time, error) {
	fi, err := os.Stat(filepath.Join(s.Root, name, usedFile))
	if err != nil {
		return time.Time{}, err
	}
	return fi.ModTime(), nil
}

// checkCopies refuses a new copy once the store keeps MaxCopies.
func (s *Store) checkCopies() error {
	entries, err := s.List()
	if err != nil {
		return err
	}
	n := 0
	for _, e := range entries {
		if slices.Contains(e.Storage, agentdir.StorageOwn) {
			n++
		}
	}
	if n >= MaxCopies {
		return ErrTooMany
	}
	return nil
}

// Touch marks the workspaces of the container name as used now.
func (s *Store) Touch(name string) error {
	p := filepath.Join(s.Root, name, usedFile)
	now := time.Now()
	err := os.Chtimes(p, now, now)
	if errors.Is(err, fs.ErrNotExist) {
		return fsutil.WriteAtomic(p, nil, 0o600)
	}
	return err
}

// Remove deletes the workspaces of the container name.
func (s *Store) Remove(name string) error { return os.RemoveAll(filepath.Join(s.Root, name)) }

// RemoveStorage deletes the workspace of storage of the container name.
func (s *Store) RemoveStorage(name, storage string) error { return os.RemoveAll(s.Dir(name, storage)) }

// Entry is the directory of a container's workspaces.
type Entry struct {
	Name      string
	Principal string
	// Storage are the storage modes it holds a workspace of.
	Storage []string
	// Used is when the workspaces were last used.
	Used time.Time
}

// List returns the directories of the containers' workspaces.
func (s *Store) List() ([]Entry, error) {
	dirs, err := os.ReadDir(s.Root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		e := Entry{Name: d.Name()}
		base := filepath.Join(s.Root, d.Name())
		if data, err := os.ReadFile(filepath.Join(base, principalFile)); err == nil {
			e.Principal = strings.TrimSpace(string(data))
		}
		if fi, err := os.Stat(filepath.Join(base, usedFile)); err == nil {
			e.Used = fi.ModTime()
		}
		for _, storage := range []string{agentdir.StorageOwn, agentdir.StorageEmpty} {
			if _, err := os.Lstat(filepath.Join(base, storage)); err == nil {
				e.Storage = append(e.Storage, storage)
			}
		}
		out = append(out, e)
	}
	return out, nil
}

// copyTree copies src to dst through a temporary directory in base, so
// that dst holds a whole copy or nothing. Symlinks are copied as they are,
// and special files are left out.
func (s *Store) copyTree(ctx context.Context, src, base, dst string) (err error) {
	c := &copier{ctx: ctx, maxBytes: s.MaxBytes, maxEntries: s.MaxEntries}
	if c.maxBytes == 0 {
		c.maxBytes = MaxBytes
	}
	if c.maxEntries == 0 {
		c.maxEntries = MaxEntries
	}
	if err := removeTemps(base); err != nil {
		return err
	}
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		return err
	}
	tmp := filepath.Join(base, tmpPrefix+hex.EncodeToString(suffix))
	if err := os.Mkdir(tmp, 0o755); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, os.RemoveAll(tmp))
		}
	}()
	from, err := os.OpenRoot(src)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, from.Close()) }()
	to, err := os.OpenRoot(tmp)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, to.Close()) }()
	if err := c.copyDir(from, to); err != nil {
		return fmt.Errorf("copying the workspace: %w", err)
	}
	return os.Rename(tmp, dst)
}

// copier copies a workspace within its limits.
type copier struct {
	ctx        context.Context
	maxBytes   int64
	maxEntries int
	size       int64
	entries    int
}

// copyDir copies the entries of the directory from into to. Agents with a
// shared workspace can rename its entries during the copy, so each entry
// is opened by its name in the directory it was listed in, and checked to
// be the entry it listed: a directory replaced by a symlink can't redirect
// the copy elsewhere in the workspace, such as into .a2a.
func (c *copier) copyDir(from, to *os.Root) error {
	entries, err := fsutil.ReadDirIn(from, ".")
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := c.ctx.Err(); err != nil {
			return err
		}
		name := e.Name()
		if isSkipped(name) {
			continue
		}
		if c.entries++; c.entries > c.maxEntries {
			return fmt.Errorf("%w: it has over %d files", ErrTooLarge, c.maxEntries)
		}
		fi, err := from.Lstat(name)
		if err != nil {
			return err
		}
		switch mode := fi.Mode(); {
		case mode.IsDir():
			err = c.copySubdir(from, to, name, fi)
		case mode&fs.ModeSymlink != 0:
			var target string
			if target, err = from.Readlink(name); err == nil {
				err = to.Symlink(target, name)
			}
		case mode.IsRegular():
			err = c.copyFile(from, to, name, fi)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *copier) copySubdir(from, to *os.Root, name string, listed fs.FileInfo) (err error) {
	sub, err := from.OpenRoot(name)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, sub.Close()) }()
	if err := checkSame(sub.Stat("."))(name, listed); err != nil {
		return err
	}
	if err := to.Mkdir(name, 0o755); err != nil {
		return err
	}
	toSub, err := to.OpenRoot(name)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, toSub.Close()) }()
	return c.copyDir(sub, toSub)
}

// copyFile copies the regular file name of from to to, reading at most one
// byte over the size left.
func (c *copier) copyFile(from, to *os.Root, name string, listed fs.FileInfo) (err error) {
	in, err := fsutil.OpenRegular(from, name)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, in.Close()) }()
	if err := checkSame(in.Stat())(name, listed); err != nil {
		return err
	}
	perm := os.FileMode(0o644)
	if listed.Mode().Perm()&0o111 != 0 {
		perm = 0o755
	}
	out, err := to.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, io.LimitReader(in, c.maxBytes-c.size+1))
	c.size += n
	if err := errors.Join(err, out.Close()); err != nil {
		return err
	}
	if c.size > c.maxBytes {
		return fmt.Errorf("%w: it is over %d MiB", ErrTooLarge, c.maxBytes>>20)
	}
	return nil
}

// checkSame returns a check that the opened entry is the one listed as
// name.
func checkSame(opened fs.FileInfo, err error) func(name string, listed fs.FileInfo) error {
	return func(name string, listed fs.FileInfo) error {
		if err != nil {
			return err
		}
		if !os.SameFile(opened, listed) {
			return fmt.Errorf("%s changed while the workspace was copied; send the message again", name)
		}
		return nil
	}
}

func isSkipped(name string) bool { return slices.Contains(skipped, name) }

// removeTemps deletes the temporary directories of copies that didn't
// finish, such as those of a crash.
func removeTemps(base string) error {
	entries, err := os.ReadDir(base)
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tmpPrefix) {
			errs = append(errs, os.RemoveAll(filepath.Join(base, e.Name())))
		}
	}
	return errors.Join(errs...)
}
