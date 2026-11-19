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
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"

	gofs "github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"golang.org/x/sys/unix"

	"github.com/a2aproject/a2a-serve/core/fsys"
)

// handle is an open file. A handle that writes keeps the whole file in a
// buffer from its first change, and writes the buffer to the tree when it
// is flushed. Reads without a buffer stream the file from the tree.
type handle struct {
	n        *node
	writable bool

	mu   sync.Mutex
	mode fs.FileMode
	// buf holds the file from the first change, size bytes of it, and
	// reserved is what it takes of the budget.
	buf      *os.File
	size     int64
	reserved int64
	dirty    bool
	// rc streams the file from pos.
	rc  io.ReadCloser
	pos int64
}

var (
	_ gofs.FileReader   = (*handle)(nil)
	_ gofs.FileWriter   = (*handle)(nil)
	_ gofs.FileFlusher  = (*handle)(nil)
	_ gofs.FileFsyncer  = (*handle)(nil)
	_ gofs.FileReleaser = (*handle)(nil)
)

// change is a change of the size or the permission bits of a file.
type change struct {
	size    int64
	hasSize bool
	mode    fs.FileMode
	hasMode bool
}

func (n *node) newHandle(mode fs.FileMode, writable bool) *handle {
	h := &handle{n: n, writable: writable, mode: mode}
	if writable {
		n.mu.Lock()
		n.handles = append(n.handles, h)
		n.mu.Unlock()
	}
	return h
}

// bufferedSize returns the size of the buffer of h, or false without one.
func (h *handle) bufferedSize() (int64, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.size, h.buf != nil
}

// truncateEmpty gives h an empty buffer, as for a file opened with
// O_TRUNC or created.
func (h *handle) truncateEmpty() syscall.Errno {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.buf == nil {
		buf, err := newBuffer(h.n.m.opts.TempDir)
		if err != nil {
			return errno(err)
		}
		h.buf = buf
	}
	h.size = 0
	return errno(h.buf.Truncate(0))
}

// loadLocked fills the buffer with the file in the tree, if h has none.
func (h *handle) loadLocked(ctx context.Context) syscall.Errno {
	if h.buf != nil {
		return 0
	}
	p, ok := h.n.path()
	if !ok {
		return syscall.ENOENT
	}
	info, err := h.n.m.tree.Stat(ctx, p)
	if err != nil {
		return errno(err)
	}
	if err := h.growLocked(info.Size); err != nil {
		return errno(err)
	}
	buf, err := newBuffer(h.n.m.opts.TempDir)
	if err != nil {
		return errno(err)
	}
	rc, err := h.n.m.tree.Open(ctx, p)
	if err != nil {
		return errno(errors.Join(err, buf.Close()))
	}
	n, err := io.Copy(buf, io.LimitReader(rc, h.n.m.opts.MaxFileBytes+1))
	err = errors.Join(err, rc.Close())
	if err == nil && n > h.n.m.opts.MaxFileBytes {
		err = syscall.EFBIG
	}
	if err == nil {
		err = h.growLocked(n)
	}
	if err != nil {
		return errno(errors.Join(err, buf.Close()))
	}
	h.buf, h.size = buf, n
	return 0
}

// growLocked takes what a buffer of size bytes needs of the budget.
func (h *handle) growLocked(size int64) error {
	if size > h.n.m.opts.MaxFileBytes {
		return syscall.EFBIG
	}
	if size <= h.reserved {
		return nil
	}
	if !h.n.m.budget.reserve(size - h.reserved) {
		return syscall.ENOSPC
	}
	h.reserved = size
	return nil
}

func (h *handle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.buf != nil {
		n, err := h.buf.ReadAt(dest[:max(min(int64(len(dest)), h.size-off), 0)], off)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, errno(err)
		}
		return fuse.ReadResultData(dest[:n]), 0
	}
	if h.rc == nil || h.pos != off {
		if e := h.closeStreamLocked(); e != 0 {
			return nil, e
		}
		p, ok := h.n.path()
		if !ok {
			return nil, syscall.ENOENT
		}
		rc, err := fsys.OpenRange(ctx, h.n.m.tree, p, off, -1)
		if err != nil {
			return nil, errno(err)
		}
		h.rc, h.pos = rc, off
	}
	n, err := io.ReadFull(h.rc, dest)
	h.pos += int64(n)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, errno(errors.Join(err, errorOf(h.closeStreamLocked())))
	}
	return fuse.ReadResultData(dest[:n]), 0
}

func (h *handle) closeStreamLocked() syscall.Errno {
	if h.rc == nil {
		return 0
	}
	err := h.rc.Close()
	h.rc = nil
	return errno(err)
}

func (h *handle) Write(ctx context.Context, data []byte, off int64) (uint32, syscall.Errno) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.writable {
		return 0, syscall.EBADF
	}
	if e := h.loadLocked(ctx); e != 0 {
		return 0, e
	}
	end := off + int64(len(data))
	if err := h.growLocked(end); err != nil {
		return 0, errno(err)
	}
	if _, err := h.buf.WriteAt(data, off); err != nil {
		return 0, errno(err)
	}
	h.size, h.dirty = max(h.size, end), true
	return uint32(len(data)), 0
}

// apply applies ch to the buffer of h.
func (h *handle) apply(ctx context.Context, ch change) syscall.Errno {
	h.mu.Lock()
	defer h.mu.Unlock()
	if e := h.loadLocked(ctx); e != 0 {
		return e
	}
	if ch.hasSize {
		if err := h.growLocked(ch.size); err != nil {
			return errno(err)
		}
		if err := h.buf.Truncate(ch.size); err != nil {
			return errno(err)
		}
		h.size = ch.size
	}
	if ch.hasMode {
		h.mode = ch.mode
	}
	h.dirty = true
	return 0
}

func (h *handle) Flush(ctx context.Context) syscall.Errno {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.flushLocked(ctx)
}

func (h *handle) Fsync(ctx context.Context, flags uint32) syscall.Errno {
	return h.Flush(ctx)
}

// flushLocked writes the buffer to the tree, unless it is written or the
// file was removed.
func (h *handle) flushLocked(ctx context.Context) syscall.Errno {
	if !h.dirty {
		return 0
	}
	p, ok := h.n.path()
	if !ok {
		h.dirty = false
		return 0
	}
	var r io.Reader = io.NewSectionReader(h.buf, 0, h.size)
	if h.buf == nil {
		r = strings.NewReader("")
	}
	if _, err := h.n.m.tree.Write(ctx, p, r, h.mode, ""); err != nil {
		return errno(err)
	}
	h.dirty = false
	return 0
}

func (h *handle) Release(ctx context.Context) syscall.Errno {
	if h.writable {
		h.n.mu.Lock()
		h.n.handles = slices.DeleteFunc(h.n.handles, func(o *handle) bool { return o == h })
		h.n.mu.Unlock()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()
	e := h.flushLocked(ctx)
	var err error
	if h.buf != nil {
		err = h.buf.Close()
		h.buf = nil
	}
	h.n.m.budget.release(h.reserved)
	h.reserved = 0
	if ce := h.closeStreamLocked(); err == nil {
		err = errorOf(ce)
	}
	if e != 0 {
		return e
	}
	return errno(err)
}

func errorOf(e syscall.Errno) error {
	if e == 0 {
		return nil
	}
	return e
}

// newBuffer returns an empty buffer for a file: an unlinked file in dir,
// or in memory where dir can't hold one, such as an image without /tmp.
func newBuffer(dir string) (*os.File, error) {
	if f, err := os.CreateTemp(dir, "a2a-fuse-*"); err == nil {
		if err := os.Remove(f.Name()); err == nil {
			return f, nil
		}
		if err := f.Close(); err != nil {
			return nil, err
		}
	}
	fd, err := unix.MemfdCreate("a2a-fuse", unix.MFD_CLOEXEC)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), "a2a-fuse"), nil
}

// budget bounds the bytes that the buffers of a mount hold together.
type budget struct {
	mu        sync.Mutex
	used, max int64
}

func (b *budget) reserve(n int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used+n > b.max {
		return false
	}
	b.used += n
	return true
}

func (b *budget) release(n int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.used -= n
}
