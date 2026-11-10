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

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/backup"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
)

// backups takes the agent's snapshots at its backup interval and remembers
// how the last one went, for status.
type backups struct {
	mu     sync.Mutex
	status controlapi.BackupStatus
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
	next := time.Now().Add(s.timings.FirstBackupDelay)
	s.backups.update(func(st *controlapi.BackupStatus) {
		st.To = cfg.Backup.To
		st.Next = next
	})
	s.background.Go(func() { s.runBackups(ctx, cfg, dest, bcfg, next) })
}

// runBackups takes snapshots until ctx ends. The first one is due an
// interval after the latest stored snapshot.
func (s *Server) runBackups(ctx context.Context, cfg *agentdir.Config, dest backup.Dest, bcfg backup.Config, next time.Time) {
	interval := cfg.Backup.BackupInterval()
	snaps, err := backup.List(ctx, dest, bcfg.Agent)
	switch {
	case err != nil:
		s.logf("! listing backup snapshots in %s: %v", cfg.Backup.To, err)
	case len(snaps) > 0:
		last := snaps[len(snaps)-1].Taken
		s.backups.update(func(st *controlapi.BackupStatus) { st.Last = last })
		next = maxTime(next, last.Add(interval))
	}
	for {
		s.backups.update(func(st *controlapi.BackupStatus) { st.Next = next })
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(next)):
		}
		snap, err := backup.Take(ctx, s.dir, dest, bcfg, time.Now())
		if ctx.Err() != nil {
			return
		}
		s.backups.record(snap, err)
		if err != nil {
			slog.Error("taking a backup snapshot", "to", cfg.Backup.To, "error", err)
		}
		next = time.Now().Add(interval)
	}
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
