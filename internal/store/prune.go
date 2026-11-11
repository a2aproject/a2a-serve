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

// DefaultTaskTTL is how long finished tasks are kept after their last update
// unless [Store.SetTaskTTL] sets another time.
const DefaultTaskTTL = 30 * 24 * time.Hour

// SetTaskTTL sets how long finished tasks are kept after their last update.
func (s *Store) SetTaskTTL(ttl time.Duration) { s.taskTTL.Store(int64(ttl)) }

// Prune deletes expired codes, Google sign-in tokens idle for
// [SignInTokenTTL], and the pairings nobody redeemed a code of when their
// last code expires; tasks in a terminal state that weren't updated for the
// task TTL, with their events, snapshots, questions and attachments;
// questions of remote agents as old whose task nobody watches; the messenger
// conversations, chat histories, agent sessions, handles, call tasks,
// grants and suggestions of contexts left without tasks; the contexts in
// chat histories unused for the task TTL that never had a task; the call
// tasks of deleted tasks in the contexts that remain; the counts of taps on
// "Always allow" older than their retention; the voice replies of email senders unused for
// their retention; the dedup keys of messages older than the
// retention; and the attachments of the oldest finished guest tasks over
// the cap of guest attachments.
func (s *Store) Prune(ctx context.Context) error {
	now := s.now()
	cutoff := now.Add(-time.Duration(s.taskTTL.Load())).UnixNano()
	return s.inTx(ctx, func(tx *sql.Tx) error {
		// A pairing without codes may have just had its code taken, with its
		// kind set by a later transaction, so only pairings whose codes all
		// expired are deleted.
		if _, err := tx.ExecContext(ctx, `DELETE FROM pairings WHERE kind = ''
			AND name IN (SELECT pairing FROM codes WHERE expires < ?)
			AND name NOT IN (SELECT pairing FROM codes WHERE expires >= ?)`, now.UnixNano(), now.UnixNano()); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM codes WHERE expires < ?`, now.UnixNano()); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM tokens WHERE pairing IS NULL AND max(created, last_used) < ?`, now.Add(-SignInTokenTTL).UnixNano()); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM seen WHERE at < ?`, now.Add(-seenRetention).UnixNano()); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM tool_taps WHERE updated < ?`, now.Add(-ToolTapRetention).UnixNano()); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM user_identities WHERE seen < ?`, now.Add(-UserIdentityRetention).UnixNano()); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM voice_replies WHERE user_id LIKE 'email:%' AND created < ?`, now.Add(-VoiceRepliesRetention).UnixNano()); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM conv_history WHERE used < ?
			AND NOT EXISTS (SELECT 1 FROM tasks WHERE tasks.context_id = conv_history.context_id)`, cutoff); err != nil {
			return err
		}
		pruned, err := deleteTasks(ctx, tx, `updated < ? AND state IN (?, ?, ?, ?)`, append([]any{cutoff}, terminalStates()...)...)
		if err != nil {
			return err
		}
		if err := forgetTasks(ctx, tx, pruned); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM call_tasks WHERE origin_task_id != ''
			AND NOT EXISTS (SELECT 1 FROM tasks WHERE tasks.context_id = call_tasks.context_id AND tasks.id = call_tasks.origin_task_id)`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM questions WHERE contact != '' AND created < ?
			AND NOT EXISTS (SELECT 1 FROM watches WHERE watches.contact = questions.contact AND watches.task_id = questions.task_id)`, cutoff); err != nil {
			return err
		}
		if err := deleteOrphans(ctx, tx); err != nil {
			return err
		}
		return trimGuestBlobs(ctx, tx, s.guestAttachmentCap)
	})
}

// deleteOrphans deletes pending questions whose question is gone, and
// attachments no task refers to.
func deleteOrphans(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM pending_questions WHERE question_id NOT IN (SELECT id FROM questions)`); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM blobs WHERE NOT EXISTS (SELECT 1 FROM task_blobs WHERE task_blobs.hash = blobs.hash)`)
	return err
}

type deletedTask struct{ scope, id, contextID string }

// deleteTasks deletes the tasks matching where, with their events and
// snapshots, and returns them.
func deleteTasks(ctx context.Context, tx *sql.Tx, where string, args ...any) ([]deletedTask, error) {
	rows, err := tx.QueryContext(ctx, `DELETE FROM tasks WHERE `+where+` RETURNING scope, id, context_id`, args...)
	if err != nil {
		return nil, err
	}
	var out []deletedTask
	for rows.Next() {
		var t deletedTask
		if err := rows.Scan(&t.scope, &t.id, &t.contextID); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		out = append(out, t)
	}
	return out, errors.Join(rows.Err(), rows.Close())
}

// contextTables keep the state of a context, which goes with its last task.
var contextTables = []string{"convs", "conv_history", "sessions", "handles", "call_tasks", "tool_grants", "tool_suggestions"}

// forgetTasks deletes the attachment references and questions of deleted
// tasks, and the rows of contextTables for the contexts left without tasks.
func forgetTasks(ctx context.Context, tx *sql.Tx, deleted []deletedTask) error {
	contexts := map[string]bool{}
	for _, t := range deleted {
		contexts[t.contextID] = true
		if _, err := tx.ExecContext(ctx, `DELETE FROM task_blobs WHERE scope = ? AND task_id = ?`, t.scope, t.id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM questions WHERE task_id = ? AND contact = ''`, t.id); err != nil {
			return err
		}
	}
	for c := range contexts {
		for _, table := range contextTables {
			if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE context_id = ? AND NOT EXISTS (SELECT 1 FROM tasks WHERE context_id = ?)`, c, c); err != nil {
				return err
			}
		}
	}
	return nil
}
