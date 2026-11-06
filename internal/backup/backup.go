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

// Take stores a snapshot of the agent in d to dest, then removes snapshots
// older than the retention, always keeping the latest one.
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
		slog.Warn("removing old backup snapshots", "error", err)
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
	return errors.Join(errs...)
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

// Restore recreates an agent's .a2a directory in root from a snapshot. It
// extracts the snapshot into a directory next to .a2a and renames that into
// place, so that a damaged snapshot or an interruption leaves no partial
// agent behind. The network identity is restored only with takeOver, so
// that the old and the new host don't run as the same machine.
func Restore(ctx context.Context, dest Dest, name, key, root string, takeOver bool) (err error) {
	if err := checkSnapshot(ctx, dest, name); err != nil {
		return err
	}
	d, err := agentdir.Open(root)
	if err != nil {
		return err
	}
	if d.Exists() {
		return agenterr.Conflictf("%s already has an agent; restore into another directory", root)
	}
	entries, err := os.ReadDir(d.Path())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if len(entries) > 0 {
		return agenterr.Conflictf("%s is not empty; restore into another directory", d.Path())
	}
	rc, err := dest.Get(ctx, name)
	if err != nil {
		return err
	}
	defer closeLogged(rc)
	plain, err := Decrypt(rc, key)
	if err != nil {
		return err
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
		return err
	}
	tmp, release, err := restoreDir(d)
	if err != nil {
		return err
	}
	defer func() {
		if err := os.RemoveAll(tmp); err != nil {
			slog.Warn("removing a partial restore", "error", err)
		}
	}()
	defer func() { err = errors.Join(err, release()) }()
	if err := extractArchive(ctx, plain, tmp, takeOver); err != nil {
		return err
	}
	if err := agentdir.WriteGitignore(tmp); err != nil {
		return err
	}
	// os.Rename doesn't replace a directory, even an empty one.
	if err := os.Remove(d.Path()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Rename(tmp, d.Path()); err != nil {
		return fmt.Errorf("moving the restored agent into place: %w", err)
	}
	return nil
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
