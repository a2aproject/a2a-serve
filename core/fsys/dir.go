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
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	// defaultMaxEntries bounds the entries of a host directory that
	// Changes compares.
	defaultMaxEntries = 200_000
	// defaultPoll is how often Changes of a host directory looks for
	// changes made outside its methods while it waits.
	defaultPoll = time.Second
	// tempPrefix starts the names of the files that Write fills before it
	// renames them into place.
	tempPrefix = ".fsys-tmp-"
)

// ErrTooManyEntries is wrapped by the error of Changes of [Dir] when the
// directory holds more entries than it compares.
var ErrTooManyEntries = errors.New("the directory has too many entries to follow its changes")

// Dir returns the tree of the host directory at root. No method reaches
// outside it, also through symlinks, which it creates and describes as
// they are. The changes made outside its methods appear in its feed once
// Changes compares the directory with what it saw last: on every call, and
// every second while it waits. It compares up to 200000 entries.
func Dir(root string) FS { return newDir(root, defaultMaxChanges, defaultMaxEntries, defaultPoll) }

func newDir(root string, maxChanges, maxEntries int, poll time.Duration) *dir {
	return &dir{path: root, maxEntries: maxEntries, poll: poll, log: newChangeLog(maxChanges)}
}

type dir struct {
	path       string
	maxEntries int
	poll       time.Duration
	log        *changeLog

	// mu orders the checks of Write with its renames and the comparisons
	// of Changes, and guards seen.
	mu sync.Mutex
	// seen is the directory as Changes saw it last; nil before.
	seen map[string]Info
}

// in runs f with the root of the directory and the host form of p.
func (d *dir) in(op, p string, f func(r *os.Root, name string) error) (err error) {
	if err := checkPath(op, p); err != nil {
		return err
	}
	r, err := os.OpenRoot(d.path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, r.Close()) }()
	return f(r, filepath.FromSlash(p))
}

// infoOf describes the entry at p, which fi describes.
func infoOf(r *os.Root, p string, fi fs.FileInfo) (Info, error) {
	mode := fi.Mode() & (fs.ModeType | fs.ModePerm)
	info := Info{Path: p, Mode: mode, Size: fi.Size(), ModTime: fi.ModTime()}
	switch {
	case mode.IsDir():
		// The time and size of a directory change with its entries.
		info.Size, info.Version = 0, fmt.Sprintf("d%x.%x", uint32(mode), inode(fi))
		return info, nil
	case mode&fs.ModeSymlink != 0:
		target, err := r.Readlink(filepath.FromSlash(p))
		if err != nil {
			return Info{}, err
		}
		info.Target = target
	}
	info.Version = fmt.Sprintf("%x.%x.%x.%x", fi.ModTime().UnixNano(), fi.Size(), uint32(mode), inode(fi))
	return info, nil
}

func lstat(r *os.Root, p string) (Info, error) {
	fi, err := r.Lstat(filepath.FromSlash(p))
	if err != nil {
		return Info{}, err
	}
	return infoOf(r, p, fi)
}

func (d *dir) Stat(ctx context.Context, p string) (info Info, err error) {
	err = d.in("stat", p, func(r *os.Root, _ string) error {
		info, err = lstat(r, p)
		return err
	})
	return info, err
}

func (d *dir) ReadDir(ctx context.Context, p string) (out []Info, err error) {
	err = d.in("readdir", p, func(r *os.Root, name string) error {
		out, err = readDir(r, p)
		return err
	})
	return out, err
}

// readDir describes the entries of the directory p, sorted by name,
// leaving out the files that Write fills.
func readDir(r *os.Root, p string) ([]Info, error) {
	fi, err := r.Lstat(filepath.FromSlash(p))
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, &fs.PathError{Op: "readdir", Path: p, Err: errNotDir}
	}
	f, err := r.Open(filepath.FromSlash(p))
	if err != nil {
		return nil, err
	}
	entries, err := f.ReadDir(-1)
	err = errors.Join(err, f.Close())
	if err != nil {
		return nil, err
	}
	out := make([]Info, 0, len(entries))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tempPrefix) {
			continue
		}
		info, err := lstat(r, path.Join(p, e.Name()))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, info)
	}
	slices.SortFunc(out, func(a, b Info) int { return strings.Compare(a.Path, b.Path) })
	return out, nil
}

func (d *dir) Open(ctx context.Context, p string) (rc io.ReadCloser, err error) {
	err = d.in("open", p, func(r *os.Root, name string) error {
		fi, err := r.Lstat(name)
		if err != nil {
			return err
		}
		if !fi.Mode().IsRegular() {
			return &fs.PathError{Op: "open", Path: p, Err: errNotFile}
		}
		rc, err = r.Open(name)
		return err
	})
	return rc, err
}

func (d *dir) Write(ctx context.Context, p string, src io.Reader, mode fs.FileMode, ifVersion string) (info Info, err error) {
	if err := checkEntry("write", p); err != nil {
		return Info{}, err
	}
	if err := ctx.Err(); err != nil {
		return Info{}, err
	}
	err = d.in("write", p, func(r *os.Root, name string) error {
		if err := mkdirs(r, filepath.Dir(name)); err != nil {
			return err
		}
		tmp, err := fillTemp(r, filepath.Dir(name), src, mode)
		if err != nil {
			return err
		}
		info, err = d.replace(r, p, tmp, ifVersion)
		if err != nil {
			if rerr := r.Remove(tmp); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
				err = errors.Join(err, rerr)
			}
		}
		return err
	})
	if err == nil {
		d.log.notify()
	}
	return info, err
}

// mkdirs creates the missing directories of name, each with mode 0o755
// whatever the umask, as Memory does.
func mkdirs(r *os.Root, name string) error {
	if name == "." {
		return nil
	}
	fi, err := r.Lstat(name)
	switch {
	case err == nil && fi.IsDir():
		return nil
	case err == nil:
		return &fs.PathError{Op: "mkdir", Path: filepath.ToSlash(name), Err: errNotDir}
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	if err := mkdirs(r, filepath.Dir(name)); err != nil {
		return err
	}
	if err := r.Mkdir(name, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return r.Chmod(name, 0o755)
}

// fillTemp writes the contents of src with the permission bits of mode to
// a new file in the directory parent, and returns its name.
func fillTemp(r *os.Root, parent string, src io.Reader, mode fs.FileMode) (name string, err error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	name = filepath.Join(parent, tempPrefix+hex.EncodeToString(b))
	f, err := r.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	_, err = io.Copy(f, src)
	if err = errors.Join(err, f.Close()); err == nil {
		err = r.Chmod(name, mode&fs.ModePerm)
	}
	if err != nil {
		return "", errors.Join(err, r.Remove(name))
	}
	return name, nil
}

// replace renames tmp to p if the entry at p matches ifVersion.
func (d *dir) replace(r *os.Root, p, tmp, ifVersion string) (Info, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	cur, err := lstat(r, p)
	exists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Info{}, err
	}
	switch {
	case exists && cur.Mode.IsDir():
		return Info{}, &fs.PathError{Op: "write", Path: p, Err: errIsDir}
	case ifVersion == IfMissing && exists, ifVersion != "" && ifVersion != IfMissing && (!exists || cur.Version != ifVersion):
		return Info{}, &fs.PathError{Op: "write", Path: p, Err: ErrConflict}
	}
	if err := r.Rename(tmp, filepath.FromSlash(p)); err != nil {
		return Info{}, err
	}
	return lstat(r, p)
}

func (d *dir) Mkdir(ctx context.Context, p string, mode fs.FileMode) error {
	err := d.in("mkdir", p, func(r *os.Root, name string) error {
		fi, err := r.Lstat(name)
		switch {
		case err == nil && fi.IsDir():
			return nil
		case err == nil:
			return &fs.PathError{Op: "mkdir", Path: p, Err: fs.ErrExist}
		case !errors.Is(err, fs.ErrNotExist):
			return err
		}
		if err := mkdirs(r, filepath.Dir(name)); err != nil {
			return err
		}
		if err := r.Mkdir(name, mode&fs.ModePerm); err != nil {
			return err
		}
		return r.Chmod(name, mode&fs.ModePerm)
	})
	if err == nil {
		d.log.notify()
	}
	return err
}

func (d *dir) Symlink(ctx context.Context, target, p string) error {
	if err := checkEntry("symlink", p); err != nil {
		return err
	}
	if target == "" || strings.ContainsRune(target, 0) {
		return &fs.PathError{Op: "symlink", Path: p, Err: fmt.Errorf("%w: invalid target %q", fs.ErrInvalid, target)}
	}
	err := d.in("symlink", p, func(r *os.Root, name string) error {
		if _, err := r.Lstat(name); err == nil {
			return &fs.PathError{Op: "symlink", Path: p, Err: fs.ErrExist}
		}
		if err := mkdirs(r, filepath.Dir(name)); err != nil {
			return err
		}
		return r.Symlink(target, name)
	})
	if err == nil {
		d.log.notify()
	}
	return err
}

func (d *dir) Rename(ctx context.Context, from, to string) error {
	if err := checkEntry("rename", from); err != nil {
		return err
	}
	if err := checkEntry("rename", to); err != nil {
		return err
	}
	err := d.in("rename", from, func(r *os.Root, name string) error {
		if _, err := r.Lstat(name); err != nil {
			return err
		}
		if err := mkdirs(r, filepath.Dir(filepath.FromSlash(to))); err != nil {
			return err
		}
		return r.Rename(name, filepath.FromSlash(to))
	})
	if err == nil {
		d.log.notify()
	}
	return err
}

func (d *dir) Remove(ctx context.Context, p string) error {
	if err := checkEntry("remove", p); err != nil {
		return err
	}
	err := d.in("remove", p, func(r *os.Root, name string) error {
		if _, err := r.Lstat(name); err != nil {
			return err
		}
		return r.RemoveAll(name)
	})
	if err == nil {
		d.log.notify()
	}
	return err
}

func (d *dir) Changes(ctx context.Context, cursor string) ([]Info, string, error) {
	return waitChanges(ctx, d.log, cursor, d.compare, d.poll)
}

// compare adds the differences between the directory and what it was the
// last time to the feed. The first time, it only looks.
func (d *dir) compare() error {
	return d.in("changes", ".", func(r *os.Root, _ string) error {
		d.mu.Lock()
		defer d.mu.Unlock()
		now := map[string]Info{}
		if err := d.walk(r, ".", now); err != nil {
			return err
		}
		if d.seen == nil {
			d.seen = now
			return nil
		}
		var changes []Info
		for _, p := range slices.Backward(slices.Sorted(maps.Keys(d.seen))) {
			if _, ok := now[p]; !ok {
				changes = append(changes, Info{Path: p, Removed: true})
			}
		}
		for _, p := range slices.Sorted(maps.Keys(now)) {
			if old, ok := d.seen[p]; !ok || old.Version != now[p].Version {
				changes = append(changes, now[p])
			}
		}
		d.seen = now
		d.log.add(changes...)
		return nil
	})
}

// walk adds the entries under the directory p to into.
func (d *dir) walk(r *os.Root, p string, into map[string]Info) error {
	entries, err := readDir(r, p)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if len(into) >= d.maxEntries {
			return fmt.Errorf("%w: more than %d", ErrTooManyEntries, d.maxEntries)
		}
		into[e.Path] = e
		if e.Mode.IsDir() {
			if err := d.walk(r, e.Path, into); err != nil {
				return err
			}
		}
	}
	return nil
}
