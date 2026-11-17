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

package agentsrv

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/backup"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
)

// backups takes the agent's snapshots at its backup interval and when the
// owner asks, and remembers how the last one went, for status.
type backups struct {
	// take serializes snapshots.
	take sync.Mutex
	// mu guards the fields below. dest is nil while the server takes no
	// backups.
	mu       sync.Mutex
	status   controlapi.BackupStatus
	dest     backup.Dest
	cfg      backup.Config
	interval time.Duration
	// moved wakes the schedule when a snapshot moved the next one.
	moved chan struct{}
}

func (b *backups) get() *controlapi.BackupStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.status.To == "" {
		return nil
	}
	st := b.status
	return &st
}

func (b *backups) update(fn func(*controlapi.BackupStatus)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	fn(&b.status)
}

// record remembers the outcome of Take. A snapshot stored with a failed
// retention still counts as the last one.
func (b *backups) record(snap backup.Snapshot, err error) {
	var retention *backup.RetentionError
	stored := err == nil || errors.As(err, &retention)
	b.update(func(st *controlapi.BackupStatus) {
		st.LastError = agenterr.Text(err)
		if stored {
			st.Last = snap.Taken
		}
	})
}

// errNotDue is returned for a scheduled snapshot that another snapshot
// moved to later.
var errNotDue = errors.New("the next snapshot is not due yet")

// startBackups reports the backup destination in status right away, and
// takes snapshots in the background until ctx ends.
func (s *Server) startBackups(ctx context.Context) {
	cfg := s.rt.Config()
	if cfg.Backup.To == "" {
		return
	}
	dest, bcfg, err := backup.ForAgent(cfg, s.rt.Secrets(), nil)
	if err != nil {
		s.logf("! backups are off: %v", err)
		return
	}
	moved := make(chan struct{}, 1)
	b := &s.backups
	b.mu.Lock()
	b.status.To, b.status.Next = cfg.Backup.To, time.Now().Add(s.timings.FirstBackupDelay)
	b.dest, b.cfg, b.interval, b.moved = dest, bcfg, cfg.Backup.BackupInterval(), moved
	b.mu.Unlock()
	s.background.Go(func() { s.runBackups(ctx, moved) })
}

// runBackups takes snapshots when they are due until ctx ends. The first
// one is due an interval after the latest stored snapshot.
func (s *Server) runBackups(ctx context.Context, moved <-chan struct{}) {
	b := &s.backups
	b.mu.Lock()
	dest, agent, to, interval := b.dest, b.cfg.Agent, b.status.To, b.interval
	b.mu.Unlock()
	snaps, err := backup.List(ctx, dest, agent)
	switch {
	case err != nil:
		s.logf("! listing backup snapshots in %s: %v", to, err)
	case len(snaps) > 0:
		last := snaps[len(snaps)-1].Taken
		b.update(func(st *controlapi.BackupStatus) {
			st.Last = last
			st.Next = maxTime(st.Next, last.Add(interval))
		})
	}
	for {
		b.mu.Lock()
		next := b.status.Next
		b.mu.Unlock()
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-moved:
			timer.Stop()
			continue
		case <-timer.C:
		}
		_, err := s.takeBackup(ctx, true)
		if ctx.Err() != nil {
			return
		}
		if err != nil && !errors.Is(err, errNotDue) {
			slog.Error("taking a backup snapshot", "to", to, "error", err)
		}
	}
}

// takeBackup takes a snapshot, records it for status and moves the next
// scheduled one an interval later. A scheduled snapshot that another one
// moved returns errNotDue.
func (s *Server) takeBackup(ctx context.Context, scheduled bool) (backup.Snapshot, error) {
	b := &s.backups
	b.take.Lock()
	defer b.take.Unlock()
	b.mu.Lock()
	dest, cfg, interval, next, moved := b.dest, b.cfg, b.interval, b.status.Next, b.moved
	b.mu.Unlock()
	if dest == nil {
		return backup.Snapshot{}, agenterr.Conflictf("the running agent takes no backups; apply the backup settings with `a2a-serve restart`")
	}
	if scheduled && time.Now().Before(next) {
		return backup.Snapshot{}, errNotDue
	}
	snap, err := backup.Take(ctx, s.dir, dest, cfg, time.Now())
	if ctx.Err() != nil {
		return snap, err
	}
	b.record(snap, err)
	b.update(func(st *controlapi.BackupStatus) { st.Next = time.Now().Add(interval) })
	select {
	case moved <- struct{}{}:
	default:
	}
	return snap, err
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
