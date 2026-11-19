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

package fsys

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"syscall"
	"time"
)

// Limits of the wire protocol of [Handler] and [Client].
const (
	// MaxFileSize bounds the body of a write.
	MaxFileSize = 1 << 30
	// MaxEntries bounds the entries of a listing and of a batch of
	// changes.
	MaxEntries = 200_000
	// MaxChangesWait bounds how long a request for changes waits.
	MaxChangesWait = time.Minute

	// maxPath bounds a path or a symlink target.
	maxPath = 4096
	// maxRequestJSON bounds the JSON body of a request.
	maxRequestJSON = 64 << 10
	// maxInfoJSON bounds the answer to a stat or a write, and
	// maxListJSON a listing or a batch of changes.
	maxInfoJSON = 64 << 10
	maxListJSON = 128 << 20
	// maxInFlight bounds the requests a Handler serves at once.
	maxInFlight = 256
)

// wireInfo is an [Info] on the wire.
type wireInfo struct {
	Path    string    `json:"path"`
	Type    string    `json:"type,omitempty"`
	Mode    uint32    `json:"mode,omitempty"`
	Size    int64     `json:"size,omitempty"`
	ModTime time.Time `json:"modTime,omitzero"`
	Version string    `json:"version,omitempty"`
	Target  string    `json:"target,omitempty"`
	Removed bool      `json:"removed,omitempty"`
}

// The types of an entry on the wire.
const (
	typeFile    = "file"
	typeDir     = "dir"
	typeSymlink = "symlink"
)

func toWire(i Info) wireInfo {
	w := wireInfo{Path: i.Path, Removed: i.Removed}
	if i.Removed {
		return w
	}
	w.Mode, w.Size, w.ModTime, w.Version, w.Target = uint32(i.Mode.Perm()), i.Size, i.ModTime, i.Version, i.Target
	switch {
	case i.Mode.IsDir():
		w.Type = typeDir
	case i.Mode&fs.ModeSymlink != 0:
		w.Type = typeSymlink
	default:
		w.Type = typeFile
	}
	return w
}

// fromWire converts w, refusing a path outside the rules of the package
// and an unknown type.
func fromWire(w wireInfo) (Info, error) {
	if err := checkPath("decode", w.Path); err != nil {
		return Info{}, err
	}
	if w.Removed {
		return Info{Path: w.Path, Removed: true}, nil
	}
	i := Info{Path: w.Path, Mode: fs.FileMode(w.Mode) & fs.ModePerm, Size: w.Size, ModTime: w.ModTime, Version: w.Version, Target: w.Target}
	switch w.Type {
	case typeFile:
	case typeDir:
		i.Mode |= fs.ModeDir
	case typeSymlink:
		i.Mode |= fs.ModeSymlink
	default:
		return Info{}, fmt.Errorf("%w: entry %q has the unknown type %q", fs.ErrInvalid, w.Path, w.Type)
	}
	return i, nil
}

type wireList struct {
	Entries []wireInfo `json:"entries"`
}

type wireChanges struct {
	Changes []wireInfo `json:"changes"`
	Cursor  string     `json:"cursor"`
}

type wireMkdir struct {
	Path string `json:"path"`
	Mode uint32 `json:"mode"`
}

type wireSymlink struct {
	Target string `json:"target"`
	Path   string `json:"path"`
}

type wireRename struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// wireError is the body of a failed request. Cursor is set with the code
// resync.
type wireError struct {
	Error  string `json:"error"`
	Code   string `json:"code,omitempty"`
	Cursor string `json:"cursor,omitempty"`
}

// The codes of a failed request, each with its HTTP status.
const (
	codeInvalid    = "invalid"
	codeNotExist   = "not_exist"
	codeExist      = "exist"
	codePermission = "permission"
	codeConflict   = "conflict"
	codeResync     = "resync"
	codeIsDir      = "is_dir"
	codeNotDir     = "not_dir"
	codeNotEmpty   = "not_empty"
	codeCrossDir   = "cross_device"
	codeTooLarge   = "too_large"
	codeBusy       = "busy"
)

// codes are the codes in the order classify checks them, with the error
// each one wraps in a client.
var codes = []struct {
	code   string
	status int
	err    error
}{
	{codeConflict, 412, ErrConflict},
	{codeResync, 410, ErrResync},
	{codeNotExist, 404, fs.ErrNotExist},
	{codeExist, 409, fs.ErrExist},
	{codePermission, 403, fs.ErrPermission},
	{codeIsDir, 422, syscall.EISDIR},
	{codeNotDir, 422, syscall.ENOTDIR},
	{codeNotEmpty, 422, syscall.ENOTEMPTY},
	{codeCrossDir, 422, syscall.EXDEV},
	{codeTooLarge, 413, errTooLarge},
	{codeInvalid, 400, fs.ErrInvalid},
}

var errTooLarge = errors.New("too large")

// classify returns the code and the HTTP status of err.
func classify(err error) (string, int) {
	for _, c := range codes {
		if errors.Is(err, c.err) {
			return c.code, c.status
		}
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return "", 503
	case errors.Is(err, errBusy):
		return codeBusy, 503
	}
	return "", 500
}

var errBusy = errors.New("the file server is busy")

// errorOf returns the error of a failed request of op on path with body w.
func errorOf(op, path string, status int, w wireError) error {
	msg := strings.TrimSpace(w.Error)
	if msg == "" {
		msg = fmt.Sprintf("the file server answered %d", status)
	}
	for _, c := range codes {
		if c.code == w.Code {
			return &fs.PathError{Op: op, Path: path, Err: &remoteError{msg: msg, err: c.err}}
		}
	}
	return &fs.PathError{Op: op, Path: path, Err: errors.New(msg)}
}

// remoteError is an error that a file server reported, which wraps the
// error of its code.
type remoteError struct {
	msg string
	err error
}

func (e *remoteError) Error() string { return e.msg }
func (e *remoteError) Unwrap() error { return e.err }

// checkTarget refuses an invalid symlink target.
func checkTarget(op, path, target string) error {
	if target == "" || len(target) > maxPath || strings.ContainsRune(target, 0) {
		return &fs.PathError{Op: op, Path: path, Err: fmt.Errorf("%w: invalid target %q", fs.ErrInvalid, target)}
	}
	return nil
}

// RangeOpener is a tree that reads part of a file without reading the
// bytes before it, as [Client] does with an HTTP range request.
type RangeOpener interface {
	// OpenRange reads the regular file at path from offset, at most
	// length bytes, or to its end when length is negative.
	OpenRange(ctx context.Context, path string, offset, length int64) (io.ReadCloser, error)
}

// OpenRange reads the regular file at path of f from offset, at most
// length bytes, or to its end when length is negative. An offset past the
// end reads nothing. It uses f's own OpenRange when f is a
// [RangeOpener], and otherwise skips the bytes before offset.
func OpenRange(ctx context.Context, f FS, path string, offset, length int64) (io.ReadCloser, error) {
	if offset < 0 {
		return nil, &fs.PathError{Op: "open", Path: path, Err: fmt.Errorf("%w: negative offset", fs.ErrInvalid)}
	}
	if ro, ok := f.(RangeOpener); ok {
		return ro.OpenRange(ctx, path, offset, length)
	}
	rc, err := f.Open(ctx, path)
	if err != nil {
		return nil, err
	}
	if _, err := io.CopyN(io.Discard, rc, offset); err != nil && !errors.Is(err, io.EOF) {
		return nil, errors.Join(err, rc.Close())
	}
	if length < 0 {
		return rc, nil
	}
	return limitedReadCloser{Reader: io.LimitReader(rc, length), Closer: rc}, nil
}

type limitedReadCloser struct {
	io.Reader
	io.Closer
}
