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
	"fmt"
	"io"
	"os"
)

// MaxReadSize bounds [ReadRegular].
const MaxReadSize = 1 << 20

var (
	// ErrNotRegular reports a path that doesn't name a regular file, such
	// as a FIFO, a device or a directory.
	ErrNotRegular = errors.New("not a regular file")
	// ErrTooLarge reports a file larger than [MaxReadSize].
	ErrTooLarge = errors.New("larger than 1 MiB")
)

// OpenRegular opens the file name in root for reading, and fails with
// [ErrNotRegular] unless it is a regular file. It doesn't block on a FIFO,
// so it suits files that a sandboxed agent can replace.
func OpenRegular(root *os.Root, name string) (*os.File, error) {
	return openRegular(root, name, os.O_RDONLY, 0)
}

// ReadRegular reads the file name in root like [OpenRegular], and fails
// with [ErrTooLarge] for a file larger than [MaxReadSize].
func ReadRegular(root *os.Root, name string) (_ []byte, err error) {
	f, err := OpenRegular(root, name)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	data, err := io.ReadAll(io.LimitReader(f, MaxReadSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxReadSize {
		return nil, fmt.Errorf("%s is %w", name, ErrTooLarge)
	}
	return data, nil
}

// WriteRegular writes data to the file name in root, creating it with perm
// if it is missing. Like [OpenRegular], it doesn't block on a FIFO, and
// fails with [ErrNotRegular] unless name is a regular file.
func WriteRegular(root *os.Root, name string, data []byte, perm os.FileMode) (err error) {
	f, err := openRegular(root, name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	_, err = f.Write(data)
	return err
}

// ReadDirIn lists the directory name in root. Like [OpenRegular], it
// doesn't block on a FIFO.
func ReadDirIn(root *os.Root, name string) (_ []os.DirEntry, err error) {
	f, err := root.OpenFile(name, os.O_RDONLY|oNonblock, 0)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	return f.ReadDir(-1)
}

func openRegular(root *os.Root, name string, flag int, perm os.FileMode) (*os.File, error) {
	f, err := root.OpenFile(name, flag|oNonblock, perm)
	if isNoReader(err) {
		// Opening a FIFO without a reader for writing fails at once.
		return nil, fmt.Errorf("%s is %w", name, ErrNotRegular)
	}
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = fmt.Errorf("%s is %w", name, ErrNotRegular)
	}
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}
