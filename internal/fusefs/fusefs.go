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

//go:build linux

package fusefs

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
	"time"

	gofs "github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/a2aproject/a2a-serve/core/fsys"
)

const (
	defaultMaxOpenBytes = 2 << 30
	// cacheTimeout is how long the kernel keeps what it learned of an
	// entry, so changes of the tree show after at most this long.
	cacheTimeout = time.Second
	// releaseTimeout bounds the write of a file when it is closed and
	// its last flush didn't write it.
	releaseTimeout = 5 * time.Minute
)

// Mount mounts tree at dir, which it creates, with the mount system call,
// which takes CAP_SYS_ADMIN, and serves it until the process exits. Only
// the launcher of a container mounts this way; the processes of every user
// in the container reach the mount.
func Mount(dir string, tree fsys.FS, opts Options) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating the mount point %s: %w", dir, err)
	}
	_, err := mount(dir, tree, opts, true)
	return err
}

// mount mounts tree at dir; direct mounts it with the mount system call,
// and otherwise with fusermount, for the tests.
func mount(dir string, tree fsys.FS, opts Options, direct bool) (*fuse.Server, error) {
	opts.MaxFileBytes = cmp.Or(opts.MaxFileBytes, fsys.MaxFileSize)
	opts.MaxOpenBytes = cmp.Or(opts.MaxOpenBytes, defaultMaxOpenBytes)
	m := &mountState{tree: detached{tree}, opts: opts, budget: &budget{max: opts.MaxOpenBytes}, mounted: time.Now()}
	timeout, negative := cacheTimeout, time.Duration(0)
	mo := fuse.MountOptions{
		FsName:            "a2a-workspace",
		Name:              "a2a",
		AllowOther:        direct,
		DirectMount:       direct,
		DirectMountStrict: direct,
		DisableXAttrs:     true,
		MaxWrite:          1 << 20,
		Options:           []string{"default_permissions"},
	}
	if opts.ReadOnly {
		mo.Options = append(mo.Options, "ro")
	}
	server, err := gofs.Mount(dir, &node{m: m}, &gofs.Options{
		MountOptions:    mo,
		EntryTimeout:    &timeout,
		AttrTimeout:     &timeout,
		NegativeTimeout: &negative,
		UID:             opts.UID,
		GID:             opts.GID,
	})
	if err != nil {
		return nil, fmt.Errorf("mounting the workspace at %s: %w", dir, err)
	}
	return server, nil
}

type mountState struct {
	tree    fsys.FS
	opts    Options
	budget  *budget
	mounted time.Time
}

// fill describes info in out.
func (m *mountState) fill(info fsys.Info, out *fuse.Attr) {
	out.Mode = typeBits(info.Mode) | uint32(info.Mode.Perm())
	out.Size = uint64(max(info.Size, 0))
	if info.Mode&fs.ModeSymlink != 0 {
		out.Size = uint64(len(info.Target))
	}
	out.Blocks = (out.Size + 511) / 512
	out.Blksize = 4096
	out.Nlink = 1
	if info.Mode.IsDir() {
		out.Nlink = 2
	}
	t := info.ModTime
	out.SetTimes(&t, &t, &t)
	out.Owner = fuse.Owner{Uid: m.opts.UID, Gid: m.opts.GID}
}

func typeBits(mode fs.FileMode) uint32 {
	switch {
	case mode.IsDir():
		return syscall.S_IFDIR
	case mode&fs.ModeSymlink != 0:
		return syscall.S_IFLNK
	}
	return syscall.S_IFREG
}

// errno returns the error number of err.
func errno(err error) syscall.Errno {
	if err == nil {
		return 0
	}
	if e, ok := errors.AsType[syscall.Errno](err); ok {
		return e
	}
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return syscall.ENOENT
	case errors.Is(err, fs.ErrExist), errors.Is(err, fsys.ErrConflict):
		return syscall.EEXIST
	case errors.Is(err, fs.ErrPermission):
		return syscall.EACCES
	case errors.Is(err, fs.ErrInvalid):
		return syscall.EINVAL
	case errors.Is(err, context.Canceled):
		return syscall.EINTR
	}
	return syscall.EIO
}
