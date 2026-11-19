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
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"golang.org/x/sys/unix"
)

// resolveNoSymlinks makes openat2 stay under its directory and refuse
// every symlink on the way, also one that replaces a directory while it
// resolves.
const resolveNoSymlinks = unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS

// maxResolveRetries bounds the retries of openat2, which fails with EAGAIN
// when a rename races its resolution.
const maxResolveRetries = 64

// hasOpenat2 reports whether this kernel has openat2, which Linux added in
// 5.6, and a seccomp profile lets through.
var hasOpenat2 = sync.OnceValue(func() bool {
	fd, err := unix.Openat2(unix.AT_FDCWD, "/", &unix.OpenHow{Flags: unix.O_PATH | unix.O_CLOEXEC})
	if err != nil {
		return false
	}
	return unix.Close(fd) == nil
})

// openHostRoot opens the host directory at path, as an [fdRoot] where the
// kernel has openat2, and as an os.Root otherwise.
func openHostRoot(path string) (hostRoot, error) {
	if !hasOpenat2() {
		r, err := os.OpenRoot(path)
		if err != nil {
			return nil, err
		}
		return r, nil
	}
	fd, err := unix.Open(path, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	return &fdRoot{fd: fd}, nil
}

// fdRoot is a host directory whose methods resolve every path with
// openat2 under it, refusing symlinks, and act on the last element with
// the *at system calls, which don't follow it.
type fdRoot struct{ fd int }

var _ hostRoot = (*fdRoot)(nil)

// openat2 opens name under the root with flags, and mode for O_CREAT.
func (r *fdRoot) openat2(op, name string, flags int, mode uint32) (int, error) {
	how := unix.OpenHow{Flags: uint64(flags | unix.O_CLOEXEC), Resolve: resolveNoSymlinks}
	if flags&unix.O_CREAT != 0 {
		how.Mode = uint64(mode)
	}
	for range maxResolveRetries {
		fd, err := unix.Openat2(r.fd, name, &how)
		switch {
		case err == nil:
			return fd, nil
		case errors.Is(err, unix.EAGAIN), errors.Is(err, unix.EINTR):
			continue
		case errors.Is(err, unix.ELOOP), errors.Is(err, unix.EXDEV):
			// A symlink on the way, as Memory has no entry under one.
			err = errNotDir
		}
		return -1, &fs.PathError{Op: op, Path: name, Err: err}
	}
	return -1, &fs.PathError{Op: op, Path: name, Err: unix.EAGAIN}
}

// parent opens the directory of name and returns it with the last element
// of name.
func (r *fdRoot) parent(op, name string) (int, string, error) {
	fd, err := r.openat2(op, filepath.Dir(name), unix.O_PATH|unix.O_DIRECTORY, 0)
	return fd, filepath.Base(name), err
}

// at runs f on the last element of name in its directory.
func (r *fdRoot) at(op, name string, f func(dir int, base string) error) error {
	dir, base, err := r.parent(op, name)
	if err != nil {
		return err
	}
	err = f(dir, base)
	if cerr := unix.Close(dir); cerr != nil && err == nil {
		err = cerr
	}
	if err != nil {
		return &fs.PathError{Op: op, Path: name, Err: err}
	}
	return nil
}

func (r *fdRoot) Lstat(name string) (fs.FileInfo, error) {
	fd, err := r.openat2("lstat", name, unix.O_PATH|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), filepath.Base(name))
	fi, err := f.Stat()
	return fi, errors.Join(err, f.Close())
}

func (r *fdRoot) Readlink(name string) (target string, err error) {
	err = r.at("readlink", name, func(dir int, base string) error {
		buf := make([]byte, maxSymlinkTarget)
		n, err := unix.Readlinkat(dir, base, buf)
		target = string(buf[:n])
		return err
	})
	return target, err
}

// maxSymlinkTarget bounds the target of a symlink that Readlink reads.
const maxSymlinkTarget = 4096

func (r *fdRoot) Open(name string) (*os.File, error) {
	return r.OpenFile(name, os.O_RDONLY, 0)
}

func (r *fdRoot) OpenFile(name string, flag int, perm fs.FileMode) (*os.File, error) {
	fd, err := r.openat2("open", name, flag|unix.O_NOFOLLOW, uint32(perm.Perm()))
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), filepath.Base(name)), nil
}

// Chmod changes the mode of the entry at name through its descriptor,
// since Linux's fchmodat follows a symlink, and refuses a symlink.
func (r *fdRoot) Chmod(name string, mode fs.FileMode) error {
	fd, err := r.openat2("chmod", name, unix.O_PATH|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := unix.Close(fd); cerr != nil && err == nil {
			err = cerr
		}
	}()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return &fs.PathError{Op: "chmod", Path: name, Err: err}
	}
	if st.Mode&unix.S_IFMT == unix.S_IFLNK {
		return &fs.PathError{Op: "chmod", Path: name, Err: unix.ELOOP}
	}
	// A descriptor opened with O_PATH changes its file through /proc.
	if err := unix.Chmod("/proc/self/fd/"+strconv.Itoa(fd), uint32(mode.Perm())); err != nil {
		return &fs.PathError{Op: "chmod", Path: name, Err: err}
	}
	return nil
}

func (r *fdRoot) Mkdir(name string, perm fs.FileMode) error {
	return r.at("mkdir", name, func(dir int, base string) error {
		return unix.Mkdirat(dir, base, uint32(perm.Perm()))
	})
}

// Rename moves the entry at oldname to newname. As os.Rename, it doesn't
// replace a directory.
func (r *fdRoot) Rename(oldname, newname string) error {
	return r.at("rename", oldname, func(from int, oldBase string) error {
		return r.at("rename", newname, func(to int, newBase string) error {
			var st unix.Stat_t
			if err := unix.Fstatat(to, newBase, &st, unix.AT_SYMLINK_NOFOLLOW); err == nil && st.Mode&unix.S_IFMT == unix.S_IFDIR {
				return unix.EEXIST
			}
			return unix.Renameat(from, oldBase, to, newBase)
		})
	})
}

func (r *fdRoot) Symlink(oldname, newname string) error {
	return r.at("symlink", newname, func(dir int, base string) error {
		return unix.Symlinkat(oldname, dir, base)
	})
}

// Remove removes the file or the empty directory at name.
func (r *fdRoot) Remove(name string) error {
	return r.at("remove", name, func(dir int, base string) error {
		err := unix.Unlinkat(dir, base, 0)
		if errors.Is(err, unix.EISDIR) {
			err = unix.Unlinkat(dir, base, unix.AT_REMOVEDIR)
		}
		return err
	})
}

// RemoveAll removes the entry at name, a directory with its contents. Each
// entry resolves anew from the root, so a directory that turns into a
// symlink loses the symlink and keeps its target.
func (r *fdRoot) RemoveAll(name string) error {
	fi, err := r.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.IsDir() {
		f, err := r.OpenFile(name, unix.O_RDONLY|unix.O_DIRECTORY, 0)
		if err != nil {
			return err
		}
		names, err := f.Readdirnames(-1)
		if err = errors.Join(err, f.Close()); err != nil {
			return err
		}
		for _, n := range names {
			if err := r.RemoveAll(filepath.Join(name, n)); err != nil {
				return err
			}
		}
	}
	err = r.Remove(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func (r *fdRoot) Close() error { return unix.Close(r.fd) }
