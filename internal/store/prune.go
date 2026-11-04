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

package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// DefaultTaskTTL is how long tasks are kept after their last update unless
// [Store.SetTaskTTL] sets another time.
const DefaultTaskTTL = 30 * 24 * time.Hour

// SetTaskTTL sets how long tasks are kept after their last update.
func (s *Store) SetTaskTTL(ttl time.Duration) { s.taskTTL.Store(int64(ttl)) }

// Prune deletes expired codes, and the pairings nobody redeemed a code of
// when their last code expires; tasks not updated for the task TTL with their
// events, snapshots, questions and attachments; questions of remote agents
// as old; the handles and call tasks of contexts left without tasks;
// and the dedup keys of messages older than the retention.
func (s *Store) Prune(ctx context.Context) error {
	now := s.now()
	cutoff := now.Add(-time.Duration(s.taskTTL.Load())).UnixNano()
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM codes WHERE expires < ?`, now.UnixNano()); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM seen WHERE at < ?`, now.Add(-seenRetention).UnixNano()); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM pairings WHERE kind = '' AND name NOT IN (SELECT pairing FROM codes)`); err != nil {
			return err
		}
		pruned, err := pruneTasks(ctx, tx, cutoff)
		if err != nil {
			return err
		}
		contexts := map[string]bool{}
		for _, t := range pruned {
			contexts[t.contextID] = true
			if _, err := tx.ExecContext(ctx, `DELETE FROM task_blobs WHERE scope = ? AND task_id = ?`, t.scope, t.id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM questions WHERE task_id = ? AND contact = ''`, t.id); err != nil {
				return err
			}
		}
		for c := range contexts {
			for _, table := range []string{"handles", "call_tasks"} {
				if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE context_id = ? AND NOT EXISTS (SELECT 1 FROM tasks WHERE context_id = ?)`, c, c); err != nil {
					return err
				}
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM questions WHERE contact != '' AND created < ?`, cutoff); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM pending_questions WHERE question_id NOT IN (SELECT id FROM questions)`); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM blobs WHERE NOT EXISTS (SELECT 1 FROM task_blobs WHERE task_blobs.hash = blobs.hash)`)
		return err
	})
}

type prunedTask struct{ scope, id, contextID string }

func pruneTasks(ctx context.Context, tx *sql.Tx, cutoff int64) ([]prunedTask, error) {
	rows, err := tx.QueryContext(ctx, `DELETE FROM tasks WHERE updated < ? RETURNING scope, id, context_id`, cutoff)
	if err != nil {
		return nil, err
	}
	var out []prunedTask
	for rows.Next() {
		var t prunedTask
		if err := rows.Scan(&t.scope, &t.id, &t.contextID); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		out = append(out, t)
	}
	return out, errors.Join(rows.Err(), rows.Close())
}
