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

// Package fsutil writes files that several processes share: atomically,
// with exact permissions, under an advisory lock.
package fsutil

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// WriteAtomic replaces the file at path with data, readable as perm. Readers
// see the old or the new content, never a partial write, also after a crash.
func WriteAtomic(path string, data []byte, perm os.FileMode) (err error) {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, removeIfExists(f.Name()))
		}
	}()
	if err := writeAndClose(f, data, perm); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

// WriteAtomicIn is [WriteAtomic] for the file name inside root, which it
// refuses to leave through symlinks or "..".
func WriteAtomicIn(root *os.Root, name string, data []byte, perm os.FileMode) (err error) {
	tmp := filepath.Join(filepath.Dir(name), "."+filepath.Base(name)+"."+rand.Text()+".tmp")
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if err == nil {
			return
		}
		if rmErr := root.Remove(tmp); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			err = errors.Join(err, rmErr)
		}
	}()
	if err := writeAndClose(f, data, perm); err != nil {
		return err
	}
	return root.Rename(tmp, name)
}

func writeAndClose(f *os.File, data []byte, perm os.FileMode) error {
	if _, err := f.Write(data); err != nil {
		return errors.Join(err, f.Close())
	}
	if err := f.Chmod(perm); err != nil {
		return errors.Join(err, f.Close())
	}
	if err := f.Sync(); err != nil {
		return errors.Join(err, f.Close())
	}
	return f.Close()
}

// WithLock runs fn while holding an exclusive advisory lock on path+".lock",
// so that read-modify-write cycles of several processes don't lose updates.
func WithLock(path string, fn func() error) (err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	if err := lock(f); err != nil {
		return fmt.Errorf("locking %s: %w", path, err)
	}
	defer func() { err = errors.Join(err, unlock(f)) }()
	return fn()
}

// WithDirLock runs fn while holding an exclusive advisory lock on the
// directory dir itself, which leaves no lock file in it.
func WithDirLock(dir string, fn func() error) (err error) {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	if err := lock(f); err != nil {
		return fmt.Errorf("locking %s: %w", dir, err)
	}
	defer func() { err = errors.Join(err, unlock(f)) }()
	return fn()
}

// ErrLocked is returned by TryLock when another holder has the lock.
var ErrLocked = errors.New("locked by another process")

// TryLock takes an exclusive advisory lock on path without waiting, and
// returns the function that releases it.
func TryLock(path string) (release func() error, err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := tryLock(f); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return func() error { return errors.Join(unlock(f), f.Close()) }, nil
}

// MkdirReal creates dir with mode 0700 if it is missing, in a parent that
// exists, and refuses a symlink or another file in its place. A cloned
// repository can ship such a symlink to have the server write elsewhere.
func MkdirReal(dir string) error {
	err := os.Mkdir(dir, 0o700)
	if err == nil || !errors.Is(err, os.ErrExist) {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink; remove it so the server can create the directory", dir)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	return nil
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
