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

package lifecycle

import (
	"errors"
	"io"
	"os"
	"slices"
	"sync"
)

// rotatingLog appends to the server log and rotates it when it grows past
// its limit, so that the continuous stderr of the agent can't fill the disk.
type rotatingLog struct {
	path   string
	limit  int64
	shared []*os.File

	mu sync.Mutex
	f  *os.File
}

var _ io.WriteCloser = (*rotatingLog)(nil)

// openRotatingLog opens the log at path for appending. Files in shared that
// other writers of this process have open on the log keep working after a
// rotation.
func openRotatingLog(path string, limit int64, shared ...*os.File) (*rotatingLog, error) {
	l := &rotatingLog{path: path, limit: limit, shared: shared}
	f, err := l.open()
	if err != nil {
		return nil, err
	}
	l.f = f
	return l, nil
}

func (l *rotatingLog) open() (*os.File, error) {
	return os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
}

// Write implements [io.Writer].
func (l *rotatingLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.rotateIfFull(); err != nil {
		return 0, err
	}
	return l.f.Write(p)
}

func (l *rotatingLog) rotateIfFull() error {
	info, err := l.f.Stat()
	if err != nil {
		return err
	}
	if info.Size() <= l.limit {
		return nil
	}
	if err := rotateLog(l.path, l.limit, l.shared...); err != nil {
		return err
	}
	// A rename leaves l.f on the rotated file.
	f, err := l.open()
	if err != nil {
		return err
	}
	old := l.f
	l.f = f
	return old.Close()
}

// Close implements [io.Closer].
func (l *rotatingLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}

// rotateLog moves the log at path to path.1 once it is larger than limit. A
// log this process already writes to through one of writers, as the stdout
// a detached start or the service manager opened, is copied and truncated
// instead, since a rename would leave those writes in path.1.
func rotateLog(path string, limit int64, writers ...*os.File) error {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Size() <= limit {
		return nil
	}
	if !slices.ContainsFunc(writers, func(f *os.File) bool { return sameFile(f, info) }) {
		return os.Rename(path, path+".1")
	}
	return copyTruncate(path, path+".1")
}

func sameFile(f *os.File, info os.FileInfo) bool {
	if f == nil {
		return false
	}
	fi, err := f.Stat()
	return err == nil && os.SameFile(fi, info)
}

func copyTruncate(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, in.Close()) }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		return errors.Join(err, out.Close())
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Truncate(src, 0)
}
