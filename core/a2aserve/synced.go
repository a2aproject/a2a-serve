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

package a2aserve

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-serve/core/fsys"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
	"github.com/a2aproject/a2a-serve/internal/projectfiles"
)

// ConflictsKey is the key of the status message metadata of a task whose
// turn ended with a [*SyncConflictError]. Its value lists the paths of the
// conflicts as strings, a []any.
const ConflictsKey = "a2a-serve/conflicts"

// maxConflicts bounds the paths a [*SyncConflictError] lists.
const maxConflicts = 1000

// SyncConflictError is the error of a turn after which a [TurnSynced]
// workspace didn't write the agent's changes of Paths to its tree, since
// the tree changed them too. The directory of the process has the tree's
// version of each. Omitted counts the conflicts past the first 1000.
type SyncConflictError struct {
	Paths   []string
	Omitted int
}

func (e *SyncConflictError) Error() string {
	list := strings.Join(e.Paths, ", ")
	if e.Omitted > 0 {
		list += fmt.Sprintf(" and %d more", e.Omitted)
	}
	return "the workspace changed while the agent worked, so the agent's changes of these paths weren't saved: " + list
}

// TurnSyncOptions configures [TurnSynced].
type TurnSyncOptions struct {
	// Dir holds the directory of each key, named as the copies of Copies
	// are, next to a file with its sync state. Empty uses the directory
	// "synced" in the StateDir of the Agent, which Attach outside an Agent
	// can't.
	Dir string
	// At is where the process sees its directory; its place on the host
	// if empty. The local sandbox runs the agent at its place on the host,
	// so leave At empty there.
	At string
	// MaxBytes bounds the regular files and MaxEntries the files,
	// directories and symlinks that a sync reads on either side. Zero
	// takes the default: 1 GiB and 200000 entries.
	MaxBytes   int64
	MaxEntries int
}

const (
	defaultSyncBytes   = 1 << 30
	defaultSyncEntries = 200_000
	// releaseTimeout bounds the last sync of a directory when its process
	// stops.
	releaseTimeout = 5 * time.Minute
)

// TurnSynced returns the workspace that gives each key a private directory
// and keeps it in step with tree at the turn boundaries. Attach fills the
// directory from tree. Before each turn, the tree's changes are written
// into the directory. After each turn, whether the agent finished, failed,
// was canceled or its process exited, the directory's changes are written
// back to tree, each only over the version of the tree it came from.
//
// Before a turn, a path that the agent changed since the last sync keeps
// the agent's version. After the turn, when the tree changed a path that
// the agent changed too, the tree's version wins: the agent's version isn't
// written, the directory gets the tree's, and the task fails with a
// [*SyncConflictError], whose paths its status message lists under
// [ConflictsKey]. A canceled task stays canceled, and a failed one failed,
// with the conflicts in its status message when its final event comes
// after the sync. A change of the directory between turns, such as by a
// process the agent left running, goes back at the end of the next turn.
// The turns of the process run one at a time, as [Attachment] describes.
//
// The sync skips the agent directories (.a2a) at any depth. It doesn't
// write back the agent's configuration files and .mcp.json at the root,
// which a sandbox hides from the agent and puts empty placeholders for.
// Release writes the last changes back and removes the directory; if that
// fails, the directory and its state stay, and the next Attach of the key
// writes them back first. Release returns a [*SyncConflictError] for the
// conflicts of changes made after the last turn. Attach of a key waits for
// the Release of its last attachment.
func TurnSynced(tree fsys.FS, opts TurnSyncOptions) Workspace {
	opts.MaxBytes = cmp.Or(opts.MaxBytes, defaultSyncBytes)
	opts.MaxEntries = cmp.Or(opts.MaxEntries, defaultSyncEntries)
	return &syncedWorkspace{tree: tree, opts: opts, attached: map[string]chan struct{}{}}
}

type syncedWorkspace struct {
	tree fsys.FS
	opts TurnSyncOptions

	mu sync.Mutex
	// attached are the directories attached now, each with the channel
	// that its Release closes.
	attached map[string]chan struct{}
}

func (s *syncedWorkspace) Attach(ctx context.Context, key string) (Attachment, error) {
	return s.attachIn(ctx, key, "")
}

func (s *syncedWorkspace) attachIn(ctx context.Context, key, stateDir string) (Attachment, error) {
	root := s.opts.Dir
	if root == "" {
		if stateDir == "" {
			return Attachment{}, errors.New("a2aserve: TurnSynced without TurnSyncOptions.Dir attaches only in an Agent")
		}
		root = filepath.Join(stateDir, "synced")
	}
	name := dirName(key)
	if err := s.claim(ctx, name); err != nil {
		return Attachment{}, fmt.Errorf("a2aserve: waiting for the release of the synced directory of key %q: %w", key, err)
	}
	st := &syncState{
		tree:      s.tree,
		dir:       filepath.Join(root, name),
		stateFile: filepath.Join(root, name+".json"),
		limits:    syncLimits{bytes: s.opts.MaxBytes, entries: s.opts.MaxEntries},
	}
	st.local = fsys.Dir(st.dir)
	if err := st.open(ctx); err != nil {
		s.unclaim(name)
		return Attachment{}, err
	}
	return Attachment{
		Dir:        st.dir,
		Path:       s.opts.At,
		BeforeTurn: st.beforeTurn,
		AfterTurn:  st.afterTurn,
		Release: func() error {
			defer s.unclaim(name)
			return st.release()
		},
	}, nil
}

// claim attaches the directory name, once the attachment before it, such
// as of a process that exited, released it.
func (s *syncedWorkspace) claim(ctx context.Context, name string) error {
	for {
		s.mu.Lock()
		released, ok := s.attached[name]
		if !ok {
			s.attached[name] = make(chan struct{})
			s.mu.Unlock()
			return nil
		}
		s.mu.Unlock()
		select {
		case <-released:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (s *syncedWorkspace) unclaim(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	close(s.attached[name])
	delete(s.attached, name)
}

type syncLimits struct {
	bytes   int64
	entries int
}

// syncState keeps the directory of one process in step with the tree.
type syncState struct {
	tree, local    fsys.FS
	dir, stateFile string
	limits         syncLimits

	mu       sync.Mutex
	released bool
	base     syncBase
	// conflicts are the paths that the next AfterTurn reports, and omitted
	// counts those past maxConflicts.
	conflicts []string
	omitted   int
}

// syncBase is what both sides had at the last sync, stored in the state
// file.
type syncBase struct {
	// Cursor is the cursor of the tree's changes after the last sync.
	Cursor  string               `json:"cursor"`
	Entries map[string]syncEntry `json:"entries"`
}

// syncEntry is the version of a path in the tree and in the directory.
type syncEntry struct {
	Tree  string `json:"tree"`
	Local string `json:"local"`
}

// open fills a new directory from the tree, or writes the changes of a
// directory that a stopped server left back first.
func (st *syncState) open(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(st.dir), 0o700); err != nil {
		return err
	}
	if data, err := os.ReadFile(st.stateFile); err == nil {
		if err := json.Unmarshal(data, &st.base); err != nil {
			return fmt.Errorf("reading the sync state %s: %w", st.stateFile, err)
		}
		if st.base.Entries == nil {
			st.base.Entries = map[string]syncEntry{}
		}
		if _, err := os.Stat(st.dir); err == nil {
			if err := st.syncOut(ctx); err != nil {
				return err
			}
			return st.syncIn(ctx)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.RemoveAll(st.dir); err != nil {
		return err
	}
	if err := os.Mkdir(st.dir, 0o755); err != nil {
		return err
	}
	if err := os.Chmod(st.dir, 0o755); err != nil {
		return err
	}
	st.base = syncBase{Entries: map[string]syncEntry{}}
	_, cursor, err := changesNow(ctx, st.tree, "")
	if err != nil {
		return err
	}
	return st.resync(ctx, cursor)
}

func (st *syncState) beforeTurn(ctx context.Context) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.released {
		return nil
	}
	return st.syncIn(ctx)
}

func (st *syncState) afterTurn(ctx context.Context) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	var err error
	if !st.released {
		err = st.syncOut(ctx)
	}
	if len(st.conflicts) > 0 {
		err = errors.Join(err, &SyncConflictError{Paths: st.conflicts, Omitted: st.omitted})
		st.conflicts, st.omitted = nil, 0
	}
	return err
}

// release writes the last changes back and removes the directory and its
// state. It returns the conflicts that no AfterTurn reported, such as of
// changes the agent made after its last turn.
func (st *syncState) release() error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.released {
		return nil
	}
	st.released = true
	ctx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
	defer cancel()
	if err := st.syncOut(ctx); err != nil {
		return fmt.Errorf("writing the changes of %s back: %w", st.dir, err)
	}
	err := errors.Join(os.RemoveAll(st.dir), os.Remove(st.stateFile))
	if len(st.conflicts) > 0 {
		err = errors.Join(err, &SyncConflictError{Paths: st.conflicts, Omitted: st.omitted})
		st.conflicts, st.omitted = nil, 0
	}
	return err
}

func (st *syncState) conflict(p string) {
	if len(st.conflicts) >= maxConflicts {
		st.omitted++
		return
	}
	if !slices.Contains(st.conflicts, p) {
		st.conflicts = append(st.conflicts, p)
	}
}

// save stores the state.
func (st *syncState) save() error {
	data, err := json.Marshal(st.base)
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(st.stateFile, data, 0o600)
}

// changesNow returns the changes of tree after cursor without waiting.
func changesNow(ctx context.Context, tree fsys.FS, cursor string) ([]fsys.Info, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 0)
	defer cancel()
	return tree.Changes(ctx, cursor)
}

// syncIn writes the tree's changes into the directory.
func (st *syncState) syncIn(ctx context.Context) error {
	changes, next, err := changesNow(ctx, st.tree, st.base.Cursor)
	if errors.Is(err, fsys.ErrResync) {
		return st.resync(ctx, next)
	}
	if err != nil {
		return err
	}
	// Removals go first, children before parents, then the rest, parents
	// before children.
	slices.SortFunc(changes, func(a, b fsys.Info) int {
		switch {
		case a.Removed && b.Removed:
			return strings.Compare(b.Path, a.Path)
		case a.Removed:
			return -1
		case b.Removed:
			return 1
		}
		return strings.Compare(a.Path, b.Path)
	})
	for _, c := range changes {
		switch {
		case skipped(c.Path):
		case c.Removed:
			if err := st.pullRemoved(ctx, c.Path); err != nil {
				return err
			}
		case st.base.Entries[c.Path].Tree == version(c):
		default:
			if err := st.pull(ctx, c); err != nil {
				return err
			}
		}
	}
	st.base.Cursor = next
	return st.save()
}

// resync makes the directory match the whole tree, and continues from
// cursor.
func (st *syncState) resync(ctx context.Context, cursor string) error {
	remote, err := walk(ctx, st.tree, st.limits)
	if err != nil {
		return err
	}
	local, err := walk(ctx, st.local, st.limits)
	if err != nil {
		return err
	}
	for _, p := range slices.Backward(slices.Sorted(maps.Keys(local))) {
		b, ok := st.base.Entries[p]
		if _, there := remote[p]; there || !ok || version(local[p]) != b.Local {
			continue
		}
		if err := st.removeLocal(p); err != nil {
			return err
		}
	}
	for _, p := range slices.Sorted(maps.Keys(remote)) {
		r := remote[p]
		b, ok := st.base.Entries[p]
		l, there := local[p]
		if ok && there && b.Tree == version(r) && b.Local == version(l) {
			continue
		}
		if err := st.pull(ctx, r); err != nil {
			return err
		}
	}
	for p := range st.base.Entries {
		if _, ok := remote[p]; !ok {
			delete(st.base.Entries, p)
		}
	}
	st.base.Cursor = cursor
	return st.save()
}

// pull writes the tree's entry info into the directory, unless the agent
// changed the entry since the last sync: then the sync out finds the
// conflict, and the turn that made the change reports it.
func (st *syncState) pull(ctx context.Context, info fsys.Info) error {
	p := info.Path
	b, ok := st.base.Entries[p]
	l, err := st.local.Stat(ctx, p)
	there := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	bothDirs := there && l.Mode.IsDir() && info.Mode.IsDir()
	if !bothDirs && (there && (!ok || version(l) != b.Local) || !there && ok) {
		return nil
	}
	if there && l.Mode.IsDir() && !info.Mode.IsDir() && st.changedUnder(ctx, p) {
		return nil
	}
	switch {
	case info.Mode.IsDir():
		if there && !l.Mode.IsDir() {
			if err := st.local.Remove(ctx, p); err != nil {
				return err
			}
		}
		err = st.local.Mkdir(ctx, p, info.Mode.Perm())
	case info.Mode.IsRegular():
		err = st.copyFile(ctx, st.tree, st.local, p, info.Mode, there && l.Mode.IsDir())
	case info.Mode&fs.ModeSymlink != 0:
		err = st.replaceSymlink(ctx, st.local, p, info.Target, there)
	default:
		return nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		// The tree removed it since; its removal comes with the next sync.
		return nil
	}
	if err != nil {
		return err
	}
	l, err = st.local.Stat(ctx, p)
	if err != nil {
		return err
	}
	st.base.Entries[p] = syncEntry{Tree: version(info), Local: version(l)}
	return nil
}

// pullRemoved removes from the directory what the tree removed, unless
// the agent changed it since the last sync.
func (st *syncState) pullRemoved(ctx context.Context, p string) error {
	l, err := st.local.Stat(ctx, p)
	if errors.Is(err, fs.ErrNotExist) {
		st.forget(p)
		return nil
	}
	if err != nil {
		return err
	}
	if b, ok := st.base.Entries[p]; !ok || version(l) != b.Local || l.Mode.IsDir() && st.changedUnder(ctx, p) {
		return nil
	}
	return st.removeLocal(p)
}

// changedUnder reports whether the agent changed an entry under the
// directory p since the last sync, or couldn't tell.
func (st *syncState) changedUnder(ctx context.Context, p string) bool {
	entries, err := st.local.ReadDir(ctx, p)
	if err != nil {
		return true
	}
	for _, e := range entries {
		b, ok := st.base.Entries[e.Path]
		if !ok || version(e) != b.Local || e.Mode.IsDir() && st.changedUnder(ctx, e.Path) {
			return true
		}
	}
	return false
}

func (st *syncState) removeLocal(p string) error {
	if err := st.local.Remove(context.Background(), p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	st.forget(p)
	return nil
}

// forget drops p and the paths under it from the state.
func (st *syncState) forget(p string) {
	for q := range st.base.Entries {
		if q == p || strings.HasPrefix(q, p+"/") {
			delete(st.base.Entries, q)
		}
	}
}

// treeChangedUnder reports whether the tree changed an entry under the
// directory p since the last sync. The entries it didn't change go with
// the directory, which the directory of the process no longer has.
func (st *syncState) treeChangedUnder(ctx context.Context, p string) (bool, error) {
	entries, err := st.tree.ReadDir(ctx, p)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if skipped(e.Path) {
			continue
		}
		b, ok := st.base.Entries[e.Path]
		if !ok || version(e) != b.Tree {
			return true, nil
		}
		if e.Mode.IsDir() {
			if changed, err := st.treeChangedUnder(ctx, e.Path); changed || err != nil {
				return changed, err
			}
		}
	}
	return false, nil
}

// syncOut writes the directory's changes to the tree.
func (st *syncState) syncOut(ctx context.Context) error {
	local, err := walk(ctx, st.local, st.limits)
	if err != nil {
		return err
	}
	placeholders := placeholderDirs(local)
	var lost []string
	under := func(p string) bool {
		return slices.ContainsFunc(lost, func(q string) bool { return strings.HasPrefix(p, q+"/") })
	}
	for _, p := range slices.Sorted(maps.Keys(local)) {
		l := local[p]
		b, ok := st.base.Entries[p]
		switch {
		case skippedOut(p), ok && version(l) == b.Local, !ok && placeholders[p], under(p):
			continue
		}
		gone, err := st.removedAncestor(ctx, p)
		if err != nil {
			return err
		}
		if gone != "" {
			if err := st.takeTree(ctx, gone); err != nil {
				return err
			}
			lost = append(lost, gone)
			continue
		}
		pushed, err := st.push(ctx, l, b, ok)
		if err != nil {
			return err
		}
		if !pushed {
			lost = append(lost, p)
		}
	}
	for _, p := range slices.Backward(slices.Sorted(maps.Keys(st.base.Entries))) {
		if _, there := local[p]; there || skippedOut(p) || under(p) {
			continue
		}
		if err := st.pushRemoved(ctx, p); err != nil {
			return err
		}
	}
	return st.save()
}

// removedAncestor returns the nearest directory above p that the last sync
// saw, if the tree removed it or replaced it with another kind of entry
// since, so that a change under it doesn't bring it back.
func (st *syncState) removedAncestor(ctx context.Context, p string) (string, error) {
	for dir := path.Dir(p); dir != "."; dir = path.Dir(dir) {
		if _, ok := st.base.Entries[dir]; !ok {
			continue
		}
		cur, err := st.tree.Stat(ctx, dir)
		if errors.Is(err, fs.ErrNotExist) || err == nil && !cur.Mode.IsDir() {
			return dir, nil
		}
		return "", err
	}
	return "", nil
}

// push writes the directory's entry l to the tree, unless the tree changed
// it since the last sync, b; then it takes the tree's entry and reports
// false.
func (st *syncState) push(ctx context.Context, l fsys.Info, b syncEntry, ok bool) (bool, error) {
	p := l.Path
	cur, err := st.tree.Stat(ctx, p)
	there := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	switch {
	case l.Mode.IsDir() && there && cur.Mode.IsDir():
		st.base.Entries[p] = syncEntry{Tree: version(cur), Local: version(l)}
		return true, nil
	case ok && (!there || version(cur) != b.Tree), !ok && there:
		return false, st.takeTree(ctx, p)
	}
	if there && cur.Mode.IsDir() {
		changed, err := st.treeChangedUnder(ctx, p)
		if err != nil {
			return false, err
		}
		if changed {
			return false, st.takeTree(ctx, p)
		}
	}
	ifVersion := fsys.IfMissing
	if there && cur.Mode.IsRegular() && l.Mode.IsRegular() {
		ifVersion = cur.Version
	} else if there {
		if err := st.tree.Remove(ctx, p); err != nil {
			return false, err
		}
	}
	switch {
	case l.Mode.IsDir():
		err = st.tree.Mkdir(ctx, p, l.Mode.Perm())
	case l.Mode.IsRegular():
		var info fsys.Info
		info, err = writeFrom(ctx, st.local, st.tree, p, l.Mode, ifVersion)
		if errors.Is(err, fsys.ErrConflict) {
			return false, st.takeTree(ctx, p)
		}
		if err == nil {
			st.base.Entries[p] = syncEntry{Tree: version(info), Local: version(l)}
			return true, nil
		}
	case l.Mode&fs.ModeSymlink != 0:
		err = st.tree.Symlink(ctx, l.Target, p)
	default:
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if cur, err = st.tree.Stat(ctx, p); err != nil {
		return false, err
	}
	st.base.Entries[p] = syncEntry{Tree: version(cur), Local: version(l)}
	return true, nil
}

// pushRemoved removes from the tree what the directory removed, unless the
// tree changed it since the last sync.
func (st *syncState) pushRemoved(ctx context.Context, p string) error {
	cur, err := st.tree.Stat(ctx, p)
	if errors.Is(err, fs.ErrNotExist) {
		st.forget(p)
		return nil
	}
	if err != nil {
		return err
	}
	if version(cur) != st.base.Entries[p].Tree {
		return st.takeTree(ctx, p)
	}
	if cur.Mode.IsDir() {
		changed, err := st.treeChangedUnder(ctx, p)
		if err != nil {
			return err
		}
		if changed {
			return st.takeTree(ctx, p)
		}
	}
	if err := st.tree.Remove(ctx, p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	st.forget(p)
	return nil
}

// takeTree records a conflict at p and puts the tree's entry at p, with
// what is under it, in the directory in place of the agent's.
func (st *syncState) takeTree(ctx context.Context, p string) error {
	st.conflict(p)
	if err := st.removeLocal(p); err != nil {
		return err
	}
	info, err := st.tree.Stat(ctx, p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return st.pullTree(ctx, info)
}

// pullTree puts the tree's entry info, with what is under it, in the
// directory, where nothing of the agent's is left at its path.
func (st *syncState) pullTree(ctx context.Context, info fsys.Info) error {
	if err := st.pull(ctx, info); err != nil || !info.Mode.IsDir() {
		return err
	}
	entries, err := st.tree.ReadDir(ctx, info.Path)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if skipped(e.Path) {
			continue
		}
		if err := st.pullTree(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

// copyFile copies the regular file p from src to dst, replacing a
// directory there if dir.
func (st *syncState) copyFile(ctx context.Context, src, dst fsys.FS, p string, mode fs.FileMode, dir bool) error {
	if dir {
		if err := dst.Remove(ctx, p); err != nil {
			return err
		}
	} else if fi, err := dst.Stat(ctx, p); err == nil && fi.Mode&fs.ModeSymlink != 0 {
		if err := dst.Remove(ctx, p); err != nil {
			return err
		}
	}
	_, err := writeFrom(ctx, src, dst, p, mode, "")
	return err
}

// writeFrom writes the file p of src to dst, over ifVersion.
func writeFrom(ctx context.Context, src, dst fsys.FS, p string, mode fs.FileMode, ifVersion string) (info fsys.Info, err error) {
	rc, err := src.Open(ctx, p)
	if err != nil {
		return fsys.Info{}, err
	}
	defer func() { err = errors.Join(err, rc.Close()) }()
	return dst.Write(ctx, p, rc, mode.Perm(), ifVersion)
}

func (st *syncState) replaceSymlink(ctx context.Context, dst fsys.FS, p, target string, there bool) error {
	if there {
		if err := dst.Remove(ctx, p); err != nil {
			return err
		}
	}
	return dst.Symlink(ctx, target, p)
}

// version is what a sync compares of an entry: the version of a file or
// symlink, and the permissions of a directory, whose version may change
// with its entries.
func version(info fsys.Info) string {
	if info.Mode.IsDir() {
		return "dir:" + info.Mode.Perm().String()
	}
	return info.Version
}

// walk describes the entries of tree, within limits, leaving out the
// skipped paths.
func walk(ctx context.Context, tree fsys.FS, limits syncLimits) (map[string]fsys.Info, error) {
	found := map[string]fsys.Info{}
	var size int64
	var visit func(dir string) error
	visit = func(dir string) error {
		entries, err := tree.ReadDir(ctx, dir)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if skipped(e.Path) {
				continue
			}
			if e.Mode.IsRegular() {
				size += e.Size
			}
			found[e.Path] = e
			if len(found) > limits.entries || size > limits.bytes {
				return fmt.Errorf("%w: more than %d entries or %d bytes", ErrWorkspaceTooLarge, limits.entries, limits.bytes)
			}
			if e.Mode.IsDir() {
				if err := visit(e.Path); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return found, visit(".")
}

// skipped reports whether p is in an agent directory, which the sync
// leaves out both ways.
func skipped(p string) bool {
	return slices.Contains(strings.Split(p, "/"), projectfiles.AgentDir)
}

// skippedOut reports whether p is a file that a sandbox hides from the
// agent and puts a placeholder for, which the sync doesn't write back.
func skippedOut(p string) bool {
	if p == projectfiles.MCPFile {
		return true
	}
	for _, f := range projectfiles.AgentConfigFiles {
		name, isDir := strings.CutSuffix(f, "/")
		if p == name || isDir && strings.HasPrefix(p, name+"/") {
			return true
		}
	}
	return false
}

// placeholderDirs returns the directories of local that hold placeholders
// and nothing else, such as a .claude with only its settings files.
func placeholderDirs(local map[string]fsys.Info) map[string]bool {
	out := map[string]bool{}
	for p := range local {
		if skippedOut(p) {
			for d := path.Dir(p); d != "."; d = path.Dir(d) {
				out[d] = true
			}
		}
	}
	for p := range local {
		if !skippedOut(p) {
			for d := path.Dir(p); d != "."; d = path.Dir(d) {
				delete(out, d)
			}
		}
	}
	return out
}
