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

package fsutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// RemoveAll deletes path with everything in it, as [os.RemoveAll] does,
// and also deletes the entries of directories that lack write or search
// permission, such as a Go module cache: it adds owner permissions to each
// directory first. It doesn't follow symbolic links, so it changes nothing
// outside path. It deletes as much as it can and joins the errors. A path
// that doesn't exist is not an error.
func RemoveAll(path string) error {
	path = filepath.Clean(path)
	parent, err := os.OpenRoot(filepath.Dir(path))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return errors.Join(removeEntry(parent, filepath.Base(path), path), parent.Close())
}

// removeEntry deletes the entry name of root, which is displayed as path
// in errors.
func removeEntry(root *os.Root, name, path string) error {
	info, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return at(path, err)
	}
	if !info.IsDir() {
		return at(path, ignoreNotExist(root.Remove(name)))
	}
	// A directory that another user owns can't be changed, but its entries
	// may still be deletable. The deletes below report what isn't.
	if err := root.Chmod(name, info.Mode().Perm()|0o700); err != nil && !errors.Is(err, fs.ErrPermission) {
		return at(path, err)
	}
	dir, err := root.OpenRoot(name)
	if err != nil {
		return at(path, err)
	}
	if err := errors.Join(removeEntries(dir, path), at(path, dir.Close())); err != nil {
		return err
	}
	return at(path, ignoreNotExist(root.Remove(name)))
}

// removeEntries deletes what dir contains.
func removeEntries(dir *os.Root, path string) error {
	f, err := dir.Open(".")
	if err != nil {
		return at(path, err)
	}
	entries, err := f.ReadDir(-1)
	errs := []error{at(path, err), at(path, f.Close())}
	for _, e := range entries {
		errs = append(errs, removeEntry(dir, e.Name(), filepath.Join(path, e.Name())))
	}
	return errors.Join(errs...)
}

// at reports the failure err of an operation in a root with the path of
// the entry from the caller's point of view.
func at(path string, err error) error {
	var perr *fs.PathError
	if errors.As(err, &perr) {
		return &fs.PathError{Op: perr.Op, Path: path, Err: perr.Err}
	}
	return err
}

func ignoreNotExist(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
