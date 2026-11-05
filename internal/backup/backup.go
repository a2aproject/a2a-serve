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
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agenterr"
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

// Restore recreates an agent's .a2a directory in root from a snapshot. The
// network identity is restored only with takeOver, so that the old and the
// new host don't run as the same machine.
func Restore(ctx context.Context, dest Dest, name, key, root string, takeOver bool) error {
	d, err := agentdir.Open(root)
	if err != nil {
		return err
	}
	if d.Exists() {
		return agenterr.Conflictf("%s already has an agent; restore into another directory", root)
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
	_, statErr := os.Stat(d.Path())
	created := errors.Is(statErr, fs.ErrNotExist)
	if err := os.MkdirAll(d.StateDir(), 0o700); err != nil {
		return err
	}
	if err := extractArchive(plain, d.Path(), takeOver); err != nil {
		if created {
			err = errors.Join(err, os.RemoveAll(d.Path()))
		}
		return err
	}
	return nil
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
