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

// Package fsys is a file tree that an agent's workspace can come from: in
// memory, in a host directory, or held by another program. Unlike io/fs it
// writes, and it keeps executable bits, symlinks and a feed of changes.
//
// Paths are slash-separated, relative to the root of the tree and cleaned,
// such as "src/main.go"; "." is the root. Methods refuse other paths, such
// as ones with "..", a leading "/" or NUL, with an error that wraps
// [fs.ErrInvalid].
package fsys

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"time"
)

// Info describes an entry of the tree.
type Info struct {
	Path string
	// Mode has the type bits, such as fs.ModeDir and fs.ModeSymlink, and
	// the permission bits, including the executable ones.
	Mode    fs.FileMode
	Size    int64
	ModTime time.Time
	// Version changes on every write of the entry. Write takes it to
	// refuse a write over a change it didn't see.
	Version string
	// Target is the target of a symlink.
	Target string
	// Removed marks an entry that Changes reports as gone; only Path is
	// set then.
	Removed bool
}

// FS is a file tree. Its methods are safe for concurrent use. Write,
// Mkdir, Symlink and Rename create the missing parent directories of their
// path.
type FS interface {
	// Stat describes the entry at path. It doesn't follow a symlink.
	Stat(ctx context.Context, path string) (Info, error)
	// ReadDir describes the entries of the directory at path, sorted by
	// name.
	ReadDir(ctx context.Context, path string) ([]Info, error)
	// Open reads the regular file at path.
	Open(ctx context.Context, path string) (io.ReadCloser, error)
	// Write replaces the regular file at path with the contents of r and
	// the permission bits of mode, or creates it. With ifVersion "" it
	// writes unconditionally, with [IfMissing] only if path doesn't exist,
	// and otherwise only if the entry at path has that Version; a refused
	// write fails with an error that wraps [ErrConflict].
	Write(ctx context.Context, path string, r io.Reader, mode fs.FileMode, ifVersion string) (Info, error)
	// Mkdir creates the directory at path with the permission bits of
	// mode. A directory already there isn't an error.
	Mkdir(ctx context.Context, path string, mode fs.FileMode) error
	// Symlink creates a symlink at path to target. It fails with an error
	// that wraps fs.ErrExist if path exists.
	Symlink(ctx context.Context, target, path string) error
	// Rename moves the entry at from, with its contents, to to. A file
	// replaces a file at to; other entries there fail it.
	Rename(ctx context.Context, from, to string) error
	// Remove removes the entry at path, a directory with its contents.
	Remove(ctx context.Context, path string) error
	// Changes returns the entries that changed since cursor, the latest
	// state of each, and the cursor to pass next. Cursor "" returns no
	// changes and the current cursor. When nothing changed, Changes waits
	// for a change until ctx ends, and returns no changes and cursor if
	// ctx's deadline passed. When the feed no longer reaches cursor, it
	// fails with an error that wraps [ErrResync] and returns the current
	// cursor: read the tree again, then continue from that cursor.
	Changes(ctx context.Context, cursor string) ([]Info, string, error)
}

// IfMissing is the ifVersion of a Write that only creates a file.
const IfMissing = "\x00missing"

var (
	// ErrConflict is wrapped by the error of a Write whose ifVersion
	// doesn't match the entry.
	ErrConflict = errors.New("the file changed since the version the write expected")
	// ErrFull is wrapped by the error of a change that would take a tree
	// past its limits.
	ErrFull = errors.New("the tree is full")
	// ErrResync is wrapped by the error of Changes when the feed no
	// longer reaches its cursor.
	ErrResync = errors.New("the change feed no longer reaches the cursor; read the tree again")
)
