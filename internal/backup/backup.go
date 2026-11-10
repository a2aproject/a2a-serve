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

// Package backup takes encrypted snapshots of an agent's state to a
// directory, Google Cloud Storage or S3, and restores an agent from them.
package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
	"github.com/a2aproject/a2a-serve/internal/shellquote"
)

// Ext is the file extension of snapshots.
const Ext = ".a2abak"

const stampLayout = "20060102T150405Z"

// Snapshot is a stored snapshot.
type Snapshot struct {
	Name  string    `json:"name"`
	Agent string    `json:"agent"`
	Taken time.Time `json:"taken"`
}

// snapshotName is <agent>-<UTC time>.a2abak, which sorts by time.
func snapshotName(agent string, t time.Time) string {
	return agent + "-" + t.UTC().Format(stampLayout) + Ext
}

func parseName(name string) (Snapshot, bool) {
	base, ok := strings.CutSuffix(name, Ext)
	if !ok {
		return Snapshot{}, false
	}
	i := strings.LastIndex(base, "-")
	if i <= 0 {
		return Snapshot{}, false
	}
	t, err := time.Parse(stampLayout, base[i+1:])
	if err != nil {
		return Snapshot{}, false
	}
	return Snapshot{Name: name, Agent: base[:i], Taken: t}, true
}

// List returns the snapshots in dest, oldest first. A non-empty agent keeps
// only the snapshots of that agent.
func List(ctx context.Context, dest Dest, agent string) ([]Snapshot, error) {
	names, err := dest.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []Snapshot
	for _, n := range names {
		s, ok := parseName(n)
		if ok && (agent == "" || s.Agent == agent) {
			out = append(out, s)
		}
	}
	slices.SortFunc(out, func(a, b Snapshot) int { return a.Taken.Compare(b.Taken) })
	return out, nil
}

// Config is what Take needs to know about an agent's backups.
type Config struct {
	Agent     string
	Key       string
	Retention time.Duration
}

// RetentionError reports that Take stored Snapshot but couldn't remove the
// old snapshots, so storage grows until the destination allows deleting.
type RetentionError struct {
	Snapshot Snapshot
	Err      error
}

func (e *RetentionError) Error() string {
	return fmt.Sprintf("took snapshot %s, but removing old snapshots failed: %v", e.Snapshot.Name, e.Err)
}

func (e *RetentionError) Unwrap() error { return e.Err }

// Take stores a snapshot of the agent in d to dest, then removes snapshots
// older than the retention, always keeping the latest one. When removing
// fails, Take returns the stored snapshot with a *RetentionError, which
// carries a hint at the missing permission.
func Take(ctx context.Context, d agentdir.Dir, dest Dest, cfg Config, now time.Time) (Snapshot, error) {
	snap := Snapshot{Name: snapshotName(cfg.Agent, now), Agent: cfg.Agent, Taken: now.UTC().Truncate(time.Second)}
	pr, pw := io.Pipe()
	var writer sync.WaitGroup
	writer.Go(func() {
		enc, err := Encrypt(pw, cfg.Key)
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		err = writeArchive(ctx, d, enc)
		pw.CloseWithError(errors.Join(err, enc.Close()))
	})
	err := dest.Put(ctx, snap.Name, pr)
	err = errors.Join(err, pr.Close())
	// The writer holds a plaintext copy of the database until it returns.
	writer.Wait()
	if err != nil {
		// A cloud CLI may have stored what it read before the failure.
		if derr := dest.Delete(context.WithoutCancel(ctx), snap.Name); derr != nil && !errors.Is(derr, fs.ErrNotExist) {
			slog.Warn("removing a failed snapshot", "error", derr)
		}
		return Snapshot{}, fmt.Errorf("storing %s: %w", snap.Name, err)
	}
	if err := Prune(ctx, dest, cfg.Agent, now, cfg.Retention); err != nil {
		return snap, agenterr.WithHint(&RetentionError{Snapshot: snap, Err: err}, agenterr.HintBackupDelete, "")
	}
	return snap, nil
}

// Prune removes the agent's snapshots older than retention, except the
// latest.
func Prune(ctx context.Context, dest Dest, agent string, now time.Time, retention time.Duration) error {
	snaps, err := List(ctx, dest, agent)
	if err != nil || len(snaps) == 0 {
		return err
	}
	var errs []error
	for _, s := range snaps[:len(snaps)-1] {
		if now.Sub(s.Taken) > retention {
			errs = append(errs, dest.Delete(ctx, s.Name))
		}
	}
	errs = slices.DeleteFunc(errs, func(err error) bool { return err == nil })
	switch len(errs) {
	case 0:
		return nil
	case 1:
		return errs[0]
	}
	return fmt.Errorf("%w (and %d more)", errs[0], len(errs)-1)
}

// Latest returns the newest snapshot in dest, of any agent.
func Latest(ctx context.Context, dest Dest) (Snapshot, error) {
	snaps, err := List(ctx, dest, "")
	if err != nil {
		return Snapshot{}, err
	}
	if len(snaps) == 0 {
		return Snapshot{}, errors.New("no snapshots there")
	}
	return snaps[len(snaps)-1], nil
}

// checkSnapshot returns a not-found error, with a hint naming the latest
// snapshot, if dest has no snapshot name.
func checkSnapshot(ctx context.Context, dest Dest, name string) error {
	snaps, err := List(ctx, dest, "")
	if err != nil {
		return err
	}
	if slices.ContainsFunc(snaps, func(s Snapshot) bool { return s.Name == name }) {
		return nil
	}
	nf := &agenterr.NotFoundError{Kind: "snapshot", Name: name}
	if len(snaps) == 0 {
		return nf
	}
	return agenterr.WithHint(nf, agenterr.HintRestoreLatest, snaps[len(snaps)-1].Name)
}

// committedFiles are the files a repository can hold in .a2a without the
// agent's state. Restore keeps recipe.lock and .gitignore, since they belong
// to the files of the repository's commit. It replaces config.yaml with the
// snapshot's, which usually holds the settings changed after the commit.
var committedFiles = []string{pathConfig, pathRecipeLock, ".gitignore"}

// Restore recreates an agent's .a2a directory in root from a snapshot. It
// extracts the snapshot into a directory next to .a2a and renames that into
// place, so that a damaged snapshot or an interruption leaves no partial
// agent behind. The network identity is restored only with takeOver, so
// that the old and the new host don't run as the same machine. A .a2a that
// holds only committed files keeps them, except for config.yaml, and the
// returned notes say when the snapshot holds another version of one.
func Restore(ctx context.Context, dest Dest, name, key, root string, takeOver bool) (notes []string, err error) {
	if err := checkSnapshot(ctx, dest, name); err != nil {
		return nil, err
	}
	d, err := agentdir.Open(root)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(d.Path())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if slices.ContainsFunc(entries, func(e fs.DirEntry) bool { return !e.Type().IsRegular() || !slices.Contains(committedFiles, e.Name()) }) {
		if d.Exists() {
			return nil, agenterr.Conflictf("%s already has an agent; restore into another directory", root)
		}
		last := len(committedFiles) - 1
		return nil, agenterr.Conflictf("%s holds files other than %s and %s; restore into another directory", d.Path(), strings.Join(committedFiles[:last], ", "), committedFiles[last])
	}
	committed, err := readCommitted(d.Path(), entries)
	if err != nil {
		return nil, err
	}
	rc, err := dest.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	defer closeLogged(rc)
	plain, err := Decrypt(rc, key)
	if err != nil {
		return nil, err
	}
	created, err := fsutil.MkdirAll(d.Root)
	if created != "" {
		defer func() {
			if err != nil {
				removeCreated(d.Root, created)
			}
		}()
	}
	if err != nil {
		return nil, err
	}
	tmp, release, err := restoreDir(d)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := os.RemoveAll(tmp); err != nil {
			slog.Warn("removing a partial restore", "error", err)
		}
	}()
	defer func() { err = errors.Join(err, release()) }()
	if err := extractArchive(ctx, plain, tmp, takeOver); err != nil {
		return nil, err
	}
	for _, f := range committed {
		restored, err := os.ReadFile(filepath.Join(tmp, f.name))
		inSnapshot := err == nil
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		if f.name == pathConfig && inSnapshot {
			if !bytes.Equal(restored, f.data) {
				notes = append(notes, configNote(ctx, root))
			}
			continue
		}
		if f.name == pathRecipeLock && !bytes.Equal(restored, f.data) {
			notes = append(notes, "kept "+filepath.Join(agentdir.Name, f.name)+", which matches the workspace and differs from the snapshot's; the next `a2a-serve recipe` may keep settings of config.yaml that the recipe changed, so check its output")
		}
		if err := fsutil.WriteAtomic(filepath.Join(tmp, f.name), f.data, f.perm); err != nil {
			return nil, err
		}
	}
	if err := agentdir.WriteGitignore(tmp); err != nil {
		return nil, err
	}
	return notes, moveIntoPlace(tmp, d.Path(), committed)
}

// configNote says that a restore replaced the committed config.yaml in
// root, and names the command that shows the change when git tracks it.
func configNote(ctx context.Context, root string) string {
	path := filepath.Join(agentdir.Name, pathConfig)
	note := "replaced " + path + " with the snapshot's, which differs from it"
	if err := ExecRunner(ctx, nil, io.Discard, "git", "-C", root, "ls-files", "--error-unmatch", "--", path); err != nil {
		return note
	}
	return note + "; review the change with `git -C " + shellquote.Quote(root) + " diff " + path + "`"
}

// committedFile is a file of committedFiles in .a2a before a restore.
type committedFile struct {
	name string
	data []byte
	perm fs.FileMode
}

func readCommitted(dir string, entries []fs.DirEntry) ([]committedFile, error) {
	var out []committedFile
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		fi, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		out = append(out, committedFile{name: e.Name(), data: data, perm: fi.Mode().Perm()})
	}
	return out, nil
}

// moveIntoPlace renames the restored directory tmp to dir, which holds only
// the committed files. When that fails, it writes the committed files back.
func moveIntoPlace(tmp, dir string, committed []committedFile) (err error) {
	defer func() {
		if err == nil || len(committed) == 0 {
			return
		}
		if merr := os.MkdirAll(dir, 0o755); merr != nil {
			err = errors.Join(err, merr)
			return
		}
		for _, f := range committed {
			err = errors.Join(err, writeIfMissing(filepath.Join(dir, f.name), f.data, f.perm))
		}
	}()
	for _, f := range committed {
		if err := os.Remove(filepath.Join(dir, f.name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	// os.Rename doesn't replace a directory, even an empty one.
	if err := os.Remove(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Rename(tmp, dir); err != nil {
		return fmt.Errorf("moving the restored agent into place: %w", err)
	}
	return nil
}

func writeIfMissing(path string, data []byte, perm fs.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	return errors.Join(err, f.Close())
}

// removeCreated removes the empty directories from dir up to created, the
// topmost one a failed restore created. A directory that isn't empty, such
// as one another process wrote to, is kept.
func removeCreated(dir, created string) {
	for p := dir; ; p = filepath.Dir(p) {
		if err := os.Remove(p); err != nil {
			slog.Warn("removing a directory of a failed restore", "dir", p, "error", err)
			return
		}
		if p == created || filepath.Dir(p) == p {
			return
		}
	}
}

// restoreDir creates the directory next to the .a2a directory of d to
// restore into, and returns it locked. It first removes the directories of
// interrupted restores, which can hold plaintext secrets, keeping the ones
// that another restore holds locked. The lock on the root keeps a
// concurrent restore from removing a directory before it is locked.
func restoreDir(d agentdir.Dir) (dir string, release func() error, err error) {
	err = fsutil.WithDirLock(d.Root, func() error {
		removeLeftovers(filepath.Join(d.Root, agentdir.Name+".restore-*"), func(dir string) (func() error, error) {
			return lockRestored(d, dir)
		})
		if dir, err = os.MkdirTemp(d.Root, agentdir.Name+".restore-"); err != nil {
			return err
		}
		if release, err = lockRestored(d, dir); err != nil {
			return errors.Join(err, os.RemoveAll(dir))
		}
		return nil
	})
	return dir, release, err
}

// lockRestored takes the lock of the agent directory of d in tmp, where it
// is restored. The lock stays held once tmp is renamed into place.
func lockRestored(d agentdir.Dir, tmp string) (release func() error, err error) {
	rel, err := filepath.Rel(d.Path(), d.LockPath())
	if err != nil {
		return nil, err
	}
	lock := filepath.Join(tmp, rel)
	if err := os.MkdirAll(filepath.Dir(lock), 0o700); err != nil {
		return nil, err
	}
	return fsutil.TryLock(lock)
}

// ForAgent returns the destination and settings of the backups of the agent
// with cfg and secrets.
func ForAgent(cfg *agentdir.Config, secrets agentdir.Secrets, run Runner) (Dest, Config, error) {
	dest, err := Open(cfg.Backup.To, run)
	if err != nil {
		return nil, Config{}, err
	}
	if cfg.Backup.Key == "" {
		return nil, Config{}, agenterr.Invalidf("no backup key: set one with `a2a-serve config set --backup-key env:VAR`")
	}
	key, err := secrets.Resolve(cfg.Backup.Key)
	if err != nil {
		return nil, Config{}, fmt.Errorf("--backup-key: %w", err)
	}
	return dest, Config{Agent: cfg.Name, Key: key, Retention: cfg.Backup.BackupRetention()}, nil
}
