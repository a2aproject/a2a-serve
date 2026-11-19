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

// Package fsysmask lays the masks of a sandbox over an fsys tree that the
// sandbox shows live, as the bind mounts of a container lay them over a
// host directory: files shown with other contents, directories private to
// the sandbox, names hidden at any depth, and a read-only view.
package fsysmask

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"path"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/a2aproject/a2a-serve/core/fsys"
)

// Rules are the masks of a tree.
type Rules struct {
	// ReadOnly refuses every change outside the Private directories.
	ReadOnly bool
	// Files are the paths of files whose contents the tree shows in place
	// of their own, read-only, where the tree has an entry. Creating,
	// changing or removing them fails.
	Files map[string][]byte
	// Private are the paths of directories that start empty and keep
	// what the sandbox writes there to themselves. They always appear.
	Private []string
	// Hidden reports the names hidden at any depth: they don't appear,
	// can't be created, and a directory that holds one can't be removed.
	Hidden func(name string) bool
}

// maxRemoveScan bounds the entries that a removal of a directory looks
// through for hidden ones.
const maxRemoveScan = 200_000

// New returns tree with the masks of r.
func New(tree fsys.FS, r Rules) fsys.FS {
	m := &masked{tree: tree, rules: r, private: map[string]fsys.FS{}, created: time.Now()}
	for _, p := range r.Private {
		m.private[p] = fsys.Memory()
	}
	return m
}

type masked struct {
	tree    fsys.FS
	rules   Rules
	private map[string]fsys.FS
	created time.Time
}

var (
	_ fsys.FS          = (*masked)(nil)
	_ fsys.RangeOpener = (*masked)(nil)
)

// kind is what a path of the tree is under the masks.
type kind int

const (
	plain kind = iota
	hidden
	fixed
	privateRoot
	private
)

// place is where a path of the tree is under the masks: its kind and, in
// a Private directory, the tree that keeps it, the directory and its path
// there.
type place struct {
	kind kind
	tree fsys.FS
	root string
	rel  string
}

// rebase returns info, which the tree of pl describes, with its path in
// the masked tree.
func (pl place) rebase(info fsys.Info) fsys.Info {
	if info.Path == "." {
		info.Path = pl.root
	} else {
		info.Path = pl.root + "/" + info.Path
	}
	return info
}

func (m *masked) classify(p string) place {
	if m.rules.Hidden != nil && p != "." {
		for name := range strings.SplitSeq(p, "/") {
			if m.rules.Hidden(name) {
				return place{kind: hidden}
			}
		}
	}
	for root, tree := range m.private {
		if p == root {
			return place{kind: privateRoot, tree: tree, root: root, rel: "."}
		}
		if rel, ok := strings.CutPrefix(p, root+"/"); ok {
			return place{kind: private, tree: tree, root: root, rel: rel}
		}
	}
	if _, ok := m.rules.Files[p]; ok {
		return place{kind: fixed}
	}
	return place{kind: plain}
}

func notExist(op, p string) error { return &fs.PathError{Op: op, Path: p, Err: fs.ErrNotExist} }

func denied(op, p, why string) error {
	return &fs.PathError{Op: op, Path: p, Err: fmt.Errorf("%w: %s", fs.ErrPermission, why)}
}

// refuseChange returns the error of a change of p of kind k, or nil.
func (m *masked) refuseChange(op, p string, k kind) error {
	switch k {
	case hidden:
		return denied(op, p, "the sandbox hides this name")
	case fixed:
		return denied(op, p, "the sandbox masks this file")
	case privateRoot:
		return denied(op, p, "the sandbox keeps this directory")
	case plain:
		if m.rules.ReadOnly {
			return denied(op, p, "the workspace is read-only")
		}
	}
	return nil
}

func (m *masked) rootInfo(p string) fsys.Info {
	return fsys.Info{Path: p, Mode: fs.ModeDir | 0o755, ModTime: m.created, Version: "private"}
}

// fixedInfo describes the file at p, which the tree describes as info.
func (m *masked) fixedInfo(info fsys.Info) fsys.Info {
	data := m.rules.Files[info.Path]
	return fsys.Info{Path: info.Path, Mode: 0o444, Size: int64(len(data)), ModTime: info.ModTime, Version: "masked"}
}

func (m *masked) Stat(ctx context.Context, p string) (fsys.Info, error) {
	pl := m.classify(p)
	switch pl.kind {
	case hidden:
		return fsys.Info{}, notExist("stat", p)
	case privateRoot:
		return m.rootInfo(p), nil
	case private:
		info, err := pl.tree.Stat(ctx, pl.rel)
		if err != nil {
			return fsys.Info{}, err
		}
		return pl.rebase(info), nil
	}
	info, err := m.tree.Stat(ctx, p)
	if err != nil || pl.kind != fixed {
		return info, err
	}
	return m.fixedInfo(info), nil
}

func (m *masked) ReadDir(ctx context.Context, p string) ([]fsys.Info, error) {
	pl := m.classify(p)
	switch pl.kind {
	case hidden:
		return nil, notExist("readdir", p)
	case privateRoot, private:
		infos, err := pl.tree.ReadDir(ctx, pl.rel)
		if err != nil {
			return nil, err
		}
		for i := range infos {
			infos[i] = pl.rebase(infos[i])
		}
		return infos, nil
	case fixed:
		return nil, &fs.PathError{Op: "readdir", Path: p, Err: syscall.ENOTDIR}
	}
	infos, err := m.tree.ReadDir(ctx, p)
	if err != nil {
		return nil, err
	}
	out := infos[:0]
	for _, info := range infos {
		switch m.classify(info.Path).kind {
		case hidden, privateRoot:
			continue
		case fixed:
			info = m.fixedInfo(info)
		}
		out = append(out, info)
	}
	for root := range m.private {
		if path.Dir(root) == p {
			out = append(out, m.rootInfo(root))
		}
	}
	slices.SortFunc(out, func(a, b fsys.Info) int { return strings.Compare(a.Path, b.Path) })
	return out, nil
}

func (m *masked) Open(ctx context.Context, p string) (io.ReadCloser, error) {
	return m.OpenRange(ctx, p, 0, -1)
}

// OpenRange implements [fsys.RangeOpener].
func (m *masked) OpenRange(ctx context.Context, p string, offset, length int64) (io.ReadCloser, error) {
	pl := m.classify(p)
	switch pl.kind {
	case hidden:
		return nil, notExist("open", p)
	case privateRoot:
		return nil, &fs.PathError{Op: "open", Path: p, Err: syscall.EISDIR}
	case private:
		return fsys.OpenRange(ctx, pl.tree, pl.rel, offset, length)
	case fixed:
		if _, err := m.tree.Stat(ctx, p); err != nil {
			return nil, err
		}
		data := m.rules.Files[p]
		data = data[min(offset, int64(len(data))):]
		if length >= 0 {
			data = data[:min(length, int64(len(data)))]
		}
		return io.NopCloser(bytes.NewReader(data)), nil
	}
	return fsys.OpenRange(ctx, m.tree, p, offset, length)
}

func (m *masked) Write(ctx context.Context, p string, r io.Reader, mode fs.FileMode, ifVersion string) (fsys.Info, error) {
	pl := m.classify(p)
	if err := m.refuseChange("write", p, pl.kind); err != nil {
		return fsys.Info{}, err
	}
	if pl.kind != private {
		return m.tree.Write(ctx, p, r, mode, ifVersion)
	}
	info, err := pl.tree.Write(ctx, pl.rel, r, mode, ifVersion)
	if err != nil {
		return fsys.Info{}, err
	}
	return pl.rebase(info), nil
}

func (m *masked) Mkdir(ctx context.Context, p string, mode fs.FileMode) error {
	pl := m.classify(p)
	if pl.kind == privateRoot {
		return nil
	}
	if err := m.refuseChange("mkdir", p, pl.kind); err != nil {
		return err
	}
	if pl.kind == private {
		return pl.tree.Mkdir(ctx, pl.rel, mode)
	}
	return m.tree.Mkdir(ctx, p, mode)
}

func (m *masked) Symlink(ctx context.Context, target, p string) error {
	pl := m.classify(p)
	if err := m.refuseChange("symlink", p, pl.kind); err != nil {
		return err
	}
	if pl.kind == private {
		return pl.tree.Symlink(ctx, target, pl.rel)
	}
	return m.tree.Symlink(ctx, target, p)
}

func (m *masked) Rename(ctx context.Context, from, to string) error {
	src, dst := m.classify(from), m.classify(to)
	if src.kind == hidden {
		return notExist("rename", from)
	}
	if err := m.refuseChange("rename", from, src.kind); err != nil {
		return err
	}
	if err := m.refuseChange("rename", to, dst.kind); err != nil {
		return err
	}
	switch {
	case src.kind == private && dst.kind == private && src.root == dst.root:
		return src.tree.Rename(ctx, src.rel, dst.rel)
	case src.kind == private || dst.kind == private:
		return &fs.PathError{Op: "rename", Path: from, Err: syscall.EXDEV}
	}
	return m.tree.Rename(ctx, from, to)
}

func (m *masked) Remove(ctx context.Context, p string) error {
	pl := m.classify(p)
	if pl.kind == hidden {
		return notExist("remove", p)
	}
	if err := m.refuseChange("remove", p, pl.kind); err != nil {
		return err
	}
	if pl.kind == private {
		return pl.tree.Remove(ctx, pl.rel)
	}
	if err := m.checkRemovable(ctx, p); err != nil {
		return err
	}
	return m.tree.Remove(ctx, p)
}

// checkRemovable refuses the removal of a directory that holds a hidden
// entry, a masked file or a Private directory, which the removal would
// take with it.
func (m *masked) checkRemovable(ctx context.Context, p string) error {
	if m.rules.Hidden == nil && len(m.rules.Files) == 0 && len(m.private) == 0 {
		return nil
	}
	info, err := m.tree.Stat(ctx, p)
	if err != nil || !info.Mode.IsDir() {
		return err
	}
	queue, seen := []string{p}, 0
	for len(queue) > 0 {
		dir := queue[0]
		queue = queue[1:]
		infos, err := m.tree.ReadDir(ctx, dir)
		if err != nil {
			return err
		}
		for _, info := range infos {
			if seen++; seen > maxRemoveScan {
				return denied("remove", p, fmt.Sprintf("it holds more than %d entries to check for hidden ones", maxRemoveScan))
			}
			switch k := m.classify(info.Path).kind; {
			case k == hidden || k == fixed || k == privateRoot:
				return denied("remove", p, "it holds an entry that the sandbox hides or masks")
			case info.Mode.IsDir():
				queue = append(queue, info.Path)
			}
		}
	}
	return nil
}

func (m *masked) Changes(ctx context.Context, cursor string) ([]fsys.Info, string, error) {
	changes, next, err := m.tree.Changes(ctx, cursor)
	out := changes[:0]
	for _, c := range changes {
		switch m.classify(c.Path).kind {
		case plain:
			out = append(out, c)
		case fixed:
			if !c.Removed {
				c = m.fixedInfo(c)
			}
			out = append(out, c)
		}
	}
	return out, next, err
}
