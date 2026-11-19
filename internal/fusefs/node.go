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
	"bytes"
	"context"
	"errors"
	"io/fs"
	"slices"
	"strings"
	"sync"
	"syscall"

	gofs "github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/a2aproject/a2a-serve/core/fsys"
)

// node is an entry of the tree. Its path is where the tree of inodes has
// it, which the kernel's renames keep up to date.
type node struct {
	gofs.Inode
	m *mountState

	mu sync.Mutex
	// handles are the open handles of a file that write it.
	handles []*handle
}

var (
	_ gofs.NodeLookuper   = (*node)(nil)
	_ gofs.NodeGetattrer  = (*node)(nil)
	_ gofs.NodeSetattrer  = (*node)(nil)
	_ gofs.NodeReaddirer  = (*node)(nil)
	_ gofs.NodeOpener     = (*node)(nil)
	_ gofs.NodeCreater    = (*node)(nil)
	_ gofs.NodeMkdirer    = (*node)(nil)
	_ gofs.NodeSymlinker  = (*node)(nil)
	_ gofs.NodeReadlinker = (*node)(nil)
	_ gofs.NodeUnlinker   = (*node)(nil)
	_ gofs.NodeRmdirer    = (*node)(nil)
	_ gofs.NodeRenamer    = (*node)(nil)
	_ gofs.NodeStatfser   = (*node)(nil)
)

// path returns the path of n in the tree, or false when n was removed.
func (n *node) path() (string, bool) {
	root := n.Root()
	var names []string
	for p := &n.Inode; p != root; {
		name, parent := p.Parent()
		if parent == nil {
			return "", false
		}
		names = append(names, name)
		p = parent
	}
	if len(names) == 0 {
		return ".", true
	}
	slices.Reverse(names)
	return strings.Join(names, "/"), true
}

// child returns the path of the entry name in the directory n.
func (n *node) child(name string) (string, syscall.Errno) {
	p, ok := n.path()
	if !ok {
		return "", syscall.ENOENT
	}
	if p == "." {
		return name, 0
	}
	return p + "/" + name, 0
}

// inode returns the inode of the entry that info describes, named name in
// the directory n, the one it already has when its type didn't change.
func (n *node) inode(ctx context.Context, name string, info fsys.Info, out *fuse.EntryOut) *gofs.Inode {
	n.m.fill(info, &out.Attr)
	mode := typeBits(info.Mode)
	if ch := n.GetChild(name); ch != nil && ch.Mode() == mode {
		return ch
	}
	return n.NewInode(ctx, &node{m: n.m}, gofs.StableAttr{Mode: mode})
}

func (n *node) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*gofs.Inode, syscall.Errno) {
	p, e := n.child(name)
	if e != 0 {
		return nil, e
	}
	info, err := n.m.tree.Stat(ctx, p)
	if err != nil {
		return nil, errno(err)
	}
	ch := n.inode(ctx, name, info, out)
	ch.Operations().(*node).sizeOverride(&out.Attr)
	return ch, 0
}

// sizeOverride gives out the size of the buffer of a handle that writes
// the file, which the tree doesn't have yet.
func (n *node) sizeOverride(out *fuse.Attr) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, h := range n.handles {
		if size, ok := h.bufferedSize(); ok {
			out.Size = uint64(size)
			out.Blocks = (out.Size + 511) / 512
		}
	}
}

// Getattr describes n. It describes the root without the tree, since the
// kernel asks for it at the mount, before the launcher can reach the tree.
func (n *node) Getattr(ctx context.Context, f gofs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	if &n.Inode == n.Root() {
		n.m.fill(fsys.Info{Path: ".", Mode: fs.ModeDir | 0o755, ModTime: n.m.mounted}, &out.Attr)
		return 0
	}
	p, ok := n.path()
	if !ok {
		return syscall.ENOENT
	}
	info, err := n.m.tree.Stat(ctx, p)
	if err != nil {
		return errno(err)
	}
	n.m.fill(info, &out.Attr)
	n.sizeOverride(&out.Attr)
	return 0
}

// Setattr changes the size and the permission bits of a file. Outside an
// open handle that writes the file, it writes the file again, since the
// tree takes the bits only with the contents. Other changes, such as of
// times and owners, and changes of the bits of directories, don't apply.
func (n *node) Setattr(ctx context.Context, f gofs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	size, hasSize := in.GetSize()
	mode, hasMode := in.GetMode()
	ch := change{size: int64(size), hasSize: hasSize, mode: fs.FileMode(mode).Perm(), hasMode: hasMode}
	if !hasSize && !hasMode {
		return n.Getattr(ctx, f, out)
	}
	if h, ok := f.(*handle); ok && h.writable {
		if e := h.apply(ctx, ch); e != 0 {
			return e
		}
		return n.Getattr(ctx, f, out)
	}
	if e := n.rewrite(ctx, ch); e != 0 {
		return e
	}
	return n.Getattr(ctx, f, out)
}

// rewrite applies ch to the file n by writing it again.
func (n *node) rewrite(ctx context.Context, ch change) syscall.Errno {
	p, ok := n.path()
	if !ok {
		return syscall.ENOENT
	}
	info, err := n.m.tree.Stat(ctx, p)
	if err != nil {
		return errno(err)
	}
	if !info.Mode.IsRegular() {
		return 0
	}
	h := n.newHandle(info.Mode.Perm(), true)
	e := h.apply(ctx, ch)
	if e == 0 {
		e = h.Flush(ctx)
	}
	return release(ctx, h, e)
}

// release releases h and returns e, or the error of the release.
func release(ctx context.Context, h *handle, e syscall.Errno) syscall.Errno {
	if re := h.Release(ctx); e == 0 {
		return re
	}
	return e
}

func (n *node) Readdir(ctx context.Context) (gofs.DirStream, syscall.Errno) {
	p, ok := n.path()
	if !ok {
		return nil, syscall.ENOENT
	}
	infos, err := n.m.tree.ReadDir(ctx, p)
	if err != nil {
		return nil, errno(err)
	}
	entries := make([]fuse.DirEntry, 0, len(infos))
	for _, info := range infos {
		name := info.Path[strings.LastIndexByte(info.Path, '/')+1:]
		entries = append(entries, fuse.DirEntry{Name: name, Mode: typeBits(info.Mode)})
	}
	return gofs.NewListDirStream(entries), 0
}

func (n *node) Open(ctx context.Context, flags uint32) (gofs.FileHandle, uint32, syscall.Errno) {
	p, ok := n.path()
	if !ok {
		return nil, 0, syscall.ENOENT
	}
	info, err := n.m.tree.Stat(ctx, p)
	if err != nil {
		return nil, 0, errno(err)
	}
	if !info.Mode.IsRegular() {
		return nil, 0, syscall.EINVAL
	}
	writable := flags&syscall.O_ACCMODE != syscall.O_RDONLY
	h := n.newHandle(info.Mode.Perm(), writable)
	if writable && flags&syscall.O_TRUNC != 0 {
		if e := h.truncateEmpty(); e != 0 {
			return nil, 0, release(ctx, h, e)
		}
		h.dirty = true
	}
	return h, 0, 0
}

func (n *node) Create(ctx context.Context, name string, flags, mode uint32, out *fuse.EntryOut) (*gofs.Inode, gofs.FileHandle, uint32, syscall.Errno) {
	p, e := n.child(name)
	if e != 0 {
		return nil, nil, 0, e
	}
	perm := fs.FileMode(mode).Perm()
	info, err := n.m.tree.Write(ctx, p, bytes.NewReader(nil), perm, fsys.IfMissing)
	existed := errors.Is(err, fsys.ErrConflict)
	switch {
	case existed && flags&syscall.O_EXCL != 0:
		return nil, nil, 0, syscall.EEXIST
	case existed:
		info, err = n.m.tree.Stat(ctx, p)
	}
	if err != nil {
		return nil, nil, 0, errno(err)
	}
	ch := n.inode(ctx, name, info, out)
	h := ch.Operations().(*node).newHandle(info.Mode.Perm(), true)
	if !existed || flags&syscall.O_TRUNC != 0 {
		if e := h.truncateEmpty(); e != 0 {
			return nil, nil, 0, release(ctx, h, e)
		}
		h.dirty = existed
	}
	return ch, h, 0, 0
}

func (n *node) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*gofs.Inode, syscall.Errno) {
	p, e := n.child(name)
	if e != 0 {
		return nil, e
	}
	if _, err := n.m.tree.Stat(ctx, p); err == nil {
		return nil, syscall.EEXIST
	}
	if err := n.m.tree.Mkdir(ctx, p, fs.FileMode(mode).Perm()); err != nil {
		return nil, errno(err)
	}
	info, err := n.m.tree.Stat(ctx, p)
	if err != nil {
		return nil, errno(err)
	}
	return n.inode(ctx, name, info, out), 0
}

func (n *node) Symlink(ctx context.Context, target, name string, out *fuse.EntryOut) (*gofs.Inode, syscall.Errno) {
	p, e := n.child(name)
	if e != 0 {
		return nil, e
	}
	if err := n.m.tree.Symlink(ctx, target, p); err != nil {
		return nil, errno(err)
	}
	info, err := n.m.tree.Stat(ctx, p)
	if err != nil {
		return nil, errno(err)
	}
	return n.inode(ctx, name, info, out), 0
}

func (n *node) Readlink(ctx context.Context) ([]byte, syscall.Errno) {
	p, ok := n.path()
	if !ok {
		return nil, syscall.ENOENT
	}
	info, err := n.m.tree.Stat(ctx, p)
	if err != nil {
		return nil, errno(err)
	}
	if info.Mode&fs.ModeSymlink == 0 {
		return nil, syscall.EINVAL
	}
	return []byte(info.Target), 0
}

func (n *node) Unlink(ctx context.Context, name string) syscall.Errno {
	p, e := n.child(name)
	if e != 0 {
		return e
	}
	info, err := n.m.tree.Stat(ctx, p)
	if err != nil {
		return errno(err)
	}
	if info.Mode.IsDir() {
		return syscall.EISDIR
	}
	return errno(n.m.tree.Remove(ctx, p))
}

func (n *node) Rmdir(ctx context.Context, name string) syscall.Errno {
	p, e := n.child(name)
	if e != 0 {
		return e
	}
	if e := n.removableDir(ctx, p); e != 0 {
		return e
	}
	return errno(n.m.tree.Remove(ctx, p))
}

// removableDir returns the error of rmdir of p unless p is an empty
// directory, which the tree removes all at once.
func (n *node) removableDir(ctx context.Context, p string) syscall.Errno {
	info, err := n.m.tree.Stat(ctx, p)
	if err != nil {
		return errno(err)
	}
	if !info.Mode.IsDir() {
		return syscall.ENOTDIR
	}
	infos, err := n.m.tree.ReadDir(ctx, p)
	if err != nil {
		return errno(err)
	}
	if len(infos) > 0 {
		return syscall.ENOTEMPTY
	}
	return 0
}

// renameExchange and renameNoReplace are the flags of renameat2.
const (
	renameNoReplace = 1 << 0
	renameExchange  = 1 << 1
)

func (n *node) Rename(ctx context.Context, name string, newParent gofs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	if flags&renameExchange != 0 {
		return syscall.EINVAL
	}
	from, e := n.child(name)
	if e != 0 {
		return e
	}
	dst, ok := newParent.(*node)
	if !ok {
		return syscall.EXDEV
	}
	to, e := dst.child(newName)
	if e != 0 {
		return e
	}
	src, err := n.m.tree.Stat(ctx, from)
	if err != nil {
		return errno(err)
	}
	switch target, err := n.m.tree.Stat(ctx, to); {
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return errno(err)
	case err != nil:
	case flags&renameNoReplace != 0:
		return syscall.EEXIST
	case src.Mode.IsDir() && !target.Mode.IsDir():
		return syscall.ENOTDIR
	case !src.Mode.IsDir() && target.Mode.IsDir():
		return syscall.EISDIR
	case target.Mode.IsDir():
		// A directory replaces an empty one.
		if e := n.removableDir(ctx, to); e != 0 {
			return e
		}
		if err := n.m.tree.Remove(ctx, to); err != nil {
			return errno(err)
		}
	}
	return errno(n.m.tree.Rename(ctx, from, to))
}

func (n *node) Statfs(ctx context.Context, out *fuse.StatfsOut) syscall.Errno {
	const blocks = 1 << 28
	out.Bsize, out.Frsize = 4096, 4096
	out.Blocks, out.Bfree, out.Bavail = blocks, blocks, blocks
	out.Files, out.Ffree = 1<<24, 1<<24
	out.NameLen = 255
	return 0
}
