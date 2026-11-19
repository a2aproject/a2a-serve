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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	errIsDir   = errors.New("is a directory")
	errNotDir  = errors.New("not a directory")
	errNotFile = errors.New("not a regular file")
)

// MemoryLimits bound a tree in memory. Zero leaves a limit off.
type MemoryLimits struct {
	// MaxBytes bounds the contents of the regular files.
	MaxBytes int64
	// MaxEntries bounds the entries besides the root.
	MaxEntries int
}

// Memory returns an empty tree in memory. Its feed keeps the last 10000
// changes. The tree doesn't bound the files it holds.
func Memory() FS { return newMemory(defaultMaxChanges) }

// MemoryWithLimits returns an empty tree in memory, as [Memory] does, which
// refuses a change that takes it past limits with an error that wraps
// [ErrFull].
func MemoryWithLimits(limits MemoryLimits) FS {
	m := newMemory(defaultMaxChanges)
	m.limits = limits
	return m
}

func newMemory(maxChanges int) *memory {
	return &memory{
		nodes: map[string]*memNode{".": {mode: fs.ModeDir | 0o755, mod: time.Now(), children: map[string]bool{}}},
		log:   newChangeLog(maxChanges),
	}
}

type memory struct {
	log    *changeLog
	limits MemoryLimits

	mu    sync.Mutex
	nodes map[string]*memNode
	seq   uint64
	// bytes and entries are what the tree holds, as limits count them.
	bytes   int64
	entries int
}

// memNode is an entry. Its data is never changed in place, so a reader
// can keep it.
type memNode struct {
	mode     fs.FileMode
	data     []byte
	target   string
	mod      time.Time
	version  string
	children map[string]bool
}

func (m *memory) infoLocked(p string) Info {
	n := m.nodes[p]
	return Info{Path: p, Mode: n.mode, Size: int64(len(n.data)), ModTime: n.mod, Version: n.version, Target: n.target}
}

// versionLocked returns a new version, which no other tree gives.
func (m *memory) versionLocked() string {
	m.seq++
	return m.log.epoch + "-" + strconv.FormatUint(m.seq, 10)
}

// putLocked puts n at p, under its existing parent, and returns its info.
func (m *memory) putLocked(p string, n *memNode) Info {
	if old, ok := m.nodes[p]; ok {
		m.bytes -= int64(len(old.data))
	} else {
		m.entries++
	}
	m.bytes += int64(len(n.data))
	n.mod = time.Now()
	n.version = m.versionLocked()
	m.nodes[p] = n
	m.nodes[path.Dir(p)].children[path.Base(p)] = true
	return m.infoLocked(p)
}

// mkdirsLocked creates the missing directories of dir and returns them.
func (m *memory) mkdirsLocked(op, dir string) ([]Info, error) {
	if n, ok := m.nodes[dir]; ok {
		if !n.mode.IsDir() {
			return nil, &fs.PathError{Op: op, Path: dir, Err: errNotDir}
		}
		return nil, nil
	}
	created, err := m.mkdirsLocked(op, path.Dir(dir))
	if err != nil {
		return nil, err
	}
	return append(created, m.putLocked(dir, &memNode{mode: fs.ModeDir | 0o755, children: map[string]bool{}})), nil
}

// missingLocked counts the directories of dir that don't exist.
func (m *memory) missingLocked(dir string) int {
	n := 0
	for ; dir != "."; dir = path.Dir(dir) {
		if _, ok := m.nodes[dir]; ok {
			break
		}
		n++
	}
	return n
}

// fitLocked fails with an error that wraps ErrFull if the tree can't take
// size more bytes and entries more entries.
func (m *memory) fitLocked(op, p string, size int64, entries int) error {
	if m.limits.MaxBytes > 0 && size > 0 && m.bytes+size > m.limits.MaxBytes ||
		m.limits.MaxEntries > 0 && entries > 0 && m.entries+entries > m.limits.MaxEntries {
		return &fs.PathError{Op: op, Path: p, Err: fmt.Errorf("%w: %d bytes and %d entries at most", ErrFull, m.limits.MaxBytes, m.limits.MaxEntries)}
	}
	return nil
}

// treeLocked returns p and the paths under it, parents first.
func (m *memory) treeLocked(p string) []string {
	out := []string{p}
	if n := m.nodes[p]; n.mode.IsDir() {
		for _, name := range slices.Sorted(maps.Keys(n.children)) {
			out = append(out, m.treeLocked(path.Join(p, name))...)
		}
	}
	return out
}

// removeLocked removes p and the paths under it, and returns them as
// removed, children first.
func (m *memory) removeLocked(p string) []Info {
	var out []Info
	for _, q := range slices.Backward(m.treeLocked(p)) {
		m.bytes -= int64(len(m.nodes[q].data))
		m.entries--
		delete(m.nodes, q)
		out = append(out, Info{Path: q, Removed: true})
	}
	delete(m.nodes[path.Dir(p)].children, path.Base(p))
	return out
}

func (m *memory) Stat(ctx context.Context, p string) (Info, error) {
	if err := checkPath("stat", p); err != nil {
		return Info{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.nodes[p]; !ok {
		return Info{}, &fs.PathError{Op: "stat", Path: p, Err: fs.ErrNotExist}
	}
	return m.infoLocked(p), nil
}

func (m *memory) ReadDir(ctx context.Context, p string) ([]Info, error) {
	if err := checkPath("readdir", p); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.nodes[p]
	switch {
	case !ok:
		return nil, &fs.PathError{Op: "readdir", Path: p, Err: fs.ErrNotExist}
	case !n.mode.IsDir():
		return nil, &fs.PathError{Op: "readdir", Path: p, Err: errNotDir}
	}
	out := make([]Info, 0, len(n.children))
	for _, name := range slices.Sorted(maps.Keys(n.children)) {
		out = append(out, m.infoLocked(path.Join(p, name)))
	}
	return out, nil
}

func (m *memory) Open(ctx context.Context, p string) (io.ReadCloser, error) {
	if err := checkPath("open", p); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.nodes[p]
	switch {
	case !ok:
		return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist}
	case !n.mode.IsRegular():
		return nil, &fs.PathError{Op: "open", Path: p, Err: errNotFile}
	}
	return io.NopCloser(bytes.NewReader(n.data)), nil
}

func (m *memory) Write(ctx context.Context, p string, r io.Reader, mode fs.FileMode, ifVersion string) (Info, error) {
	if err := checkEntry("write", p); err != nil {
		return Info{}, err
	}
	if m.limits.MaxBytes > 0 {
		r = io.LimitReader(r, m.limits.MaxBytes+1)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return Info{}, &fs.PathError{Op: "write", Path: p, Err: err}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	n, exists := m.nodes[p]
	switch {
	case exists && n.mode.IsDir():
		return Info{}, &fs.PathError{Op: "write", Path: p, Err: errIsDir}
	case ifVersion == IfMissing && exists, ifVersion != "" && ifVersion != IfMissing && (!exists || n.version != ifVersion):
		return Info{}, &fs.PathError{Op: "write", Path: p, Err: ErrConflict}
	}
	grow, added := int64(len(data)), m.missingLocked(path.Dir(p))
	if exists {
		grow -= int64(len(n.data))
	} else {
		added++
	}
	if err := m.fitLocked("write", p, grow, added); err != nil {
		return Info{}, err
	}
	changes, err := m.mkdirsLocked("write", path.Dir(p))
	if err != nil {
		return Info{}, err
	}
	info := m.putLocked(p, &memNode{mode: mode & fs.ModePerm, data: data})
	m.log.add(append(changes, info)...)
	return info, nil
}

func (m *memory) Mkdir(ctx context.Context, p string, mode fs.FileMode) error {
	if err := checkPath("mkdir", p); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if n, ok := m.nodes[p]; ok {
		if n.mode.IsDir() {
			return nil
		}
		return &fs.PathError{Op: "mkdir", Path: p, Err: fs.ErrExist}
	}
	if err := m.fitLocked("mkdir", p, 0, m.missingLocked(path.Dir(p))+1); err != nil {
		return err
	}
	changes, err := m.mkdirsLocked("mkdir", path.Dir(p))
	if err != nil {
		return err
	}
	info := m.putLocked(p, &memNode{mode: fs.ModeDir | mode&fs.ModePerm, children: map[string]bool{}})
	m.log.add(append(changes, info)...)
	return nil
}

func (m *memory) Symlink(ctx context.Context, target, p string) error {
	if err := checkEntry("symlink", p); err != nil {
		return err
	}
	if target == "" || strings.ContainsRune(target, 0) {
		return &fs.PathError{Op: "symlink", Path: p, Err: fmt.Errorf("%w: invalid target %q", fs.ErrInvalid, target)}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.nodes[p]; ok {
		return &fs.PathError{Op: "symlink", Path: p, Err: fs.ErrExist}
	}
	if err := m.fitLocked("symlink", p, 0, m.missingLocked(path.Dir(p))+1); err != nil {
		return err
	}
	changes, err := m.mkdirsLocked("symlink", path.Dir(p))
	if err != nil {
		return err
	}
	info := m.putLocked(p, &memNode{mode: fs.ModeSymlink | 0o777, target: target})
	m.log.add(append(changes, info)...)
	return nil
}

func (m *memory) Rename(ctx context.Context, from, to string) error {
	if err := checkEntry("rename", from); err != nil {
		return err
	}
	if err := checkEntry("rename", to); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	src, ok := m.nodes[from]
	if !ok {
		return &fs.PathError{Op: "rename", Path: from, Err: fs.ErrNotExist}
	}
	if from == to {
		return nil
	}
	if strings.HasPrefix(to, from+"/") {
		return &fs.PathError{Op: "rename", Path: to, Err: fmt.Errorf("%w: inside %s", fs.ErrInvalid, from)}
	}
	added := m.missingLocked(path.Dir(to))
	if _, ok := m.nodes[to]; ok {
		added--
	}
	if err := m.fitLocked("rename", to, 0, added); err != nil {
		return err
	}
	var changes []Info
	if dst, ok := m.nodes[to]; ok {
		switch {
		case dst.mode.IsDir():
			return &fs.PathError{Op: "rename", Path: to, Err: fs.ErrExist}
		case src.mode.IsDir():
			return &fs.PathError{Op: "rename", Path: to, Err: errNotDir}
		}
		changes = m.removeLocked(to)
	}
	created, err := m.mkdirsLocked("rename", path.Dir(to))
	if err != nil {
		return err
	}
	changes = append(changes, created...)
	moved := m.treeLocked(from)
	nodes := make([]*memNode, len(moved))
	for i, q := range moved {
		nodes[i] = m.nodes[q]
	}
	changes = append(changes, m.removeLocked(from)...)
	for i, q := range moved {
		changes = append(changes, m.putLocked(to+strings.TrimPrefix(q, from), nodes[i]))
	}
	m.log.add(changes...)
	return nil
}

func (m *memory) Remove(ctx context.Context, p string) error {
	if err := checkEntry("remove", p); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.nodes[p]; !ok {
		return &fs.PathError{Op: "remove", Path: p, Err: fs.ErrNotExist}
	}
	m.log.add(m.removeLocked(p)...)
	return nil
}

func (m *memory) Changes(ctx context.Context, cursor string) ([]Info, string, error) {
	return waitChanges(ctx, m.log, cursor, nil, 0)
}
