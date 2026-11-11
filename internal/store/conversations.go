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

// MaxConversations is how many contexts the history of a person in a chat
// keeps.
const MaxConversations = 10

// Conversation is a context a person used in a chat.
type Conversation struct {
	ContextID string
	// Used is when the person last wrote in it, started it or switched to
	// it.
	Used time.Time
}

// UseConversation records that user wrote in the context contextID of a
// messenger conversation.
func (s *Store) UseConversation(ctx context.Context, messenger, conv, user, contextID string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		return s.useConversationTx(ctx, tx, messenger, conv, user, contextID)
	})
}

// NewConversation binds a messenger conversation to the new context
// contextID that user started.
func (s *Store) NewConversation(ctx context.Context, messenger, conv, user, contextID string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if err := s.keepUnrecordedTx(ctx, tx, messenger, conv, user); err != nil {
			return err
		}
		if err := s.setContextTx(ctx, tx, messenger, conv, contextID); err != nil {
			return err
		}
		return s.useConversationTx(ctx, tx, messenger, conv, user, contextID)
	})
}

// keepUnrecordedTx records the context a messenger conversation is bound to
// for user when nobody's history has it, as with a context bound before
// histories were kept, so that a new conversation doesn't lose it.
func (s *Store) keepUnrecordedTx(ctx context.Context, tx *sql.Tx, messenger, conv, user string) error {
	var current string
	err := tx.QueryRowContext(ctx, `SELECT context_id FROM convs WHERE messenger = ? AND conv = ?
		AND context_id NOT IN (SELECT context_id FROM conv_history WHERE messenger = ? AND conv = ?)`,
		messenger, conv, messenger, conv).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	// It is older than the new conversation, also when the clock is coarse.
	return s.recordConversationTx(ctx, tx, messenger, conv, user, current, s.now().Add(-time.Nanosecond))
}

// SwitchConversation binds a messenger conversation to contextID, which
// must be in the history of user in it, or it returns [ErrNotFound].
func (s *Store) SwitchConversation(ctx context.Context, messenger, conv, user, contextID string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var one int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM conv_history WHERE messenger = ? AND conv = ? AND user_id = ? AND context_id = ?`,
			messenger, conv, user, contextID).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if err := s.setContextTx(ctx, tx, messenger, conv, contextID); err != nil {
			return err
		}
		return s.useConversationTx(ctx, tx, messenger, conv, user, contextID)
	})
}

// Conversations returns the history of user in a messenger conversation,
// most recently used first.
func (s *Store) Conversations(ctx context.Context, messenger, conv, user string) ([]Conversation, error) {
	rows, err := s.reads.QueryContext(ctx, `SELECT context_id, used FROM conv_history WHERE messenger = ? AND conv = ? AND user_id = ?
		ORDER BY used DESC, context_id LIMIT ?`, messenger, conv, user, MaxConversations)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)
	var out []Conversation
	for rows.Next() {
		var (
			c    Conversation
			used int64
		)
		if err := rows.Scan(&c.ContextID, &used); err != nil {
			return nil, err
		}
		c.Used = unixTime(used)
		out = append(out, c)
	}
	return out, rows.Err()
}

// ForgetMessenger deletes the histories of the conversations of a
// messenger.
func (s *Store) ForgetMessenger(ctx context.Context, messenger string) error {
	_, err := s.exec(ctx, `DELETE FROM conv_history WHERE messenger = ?`, messenger)
	return err
}

func (s *Store) setContextTx(ctx context.Context, tx *sql.Tx, messenger, conv, contextID string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO convs (messenger, conv, context_id, updated) VALUES (?, ?, ?, ?)
		ON CONFLICT (messenger, conv) DO UPDATE SET context_id = excluded.context_id, updated = excluded.updated`,
		messenger, conv, contextID, s.now().UnixNano())
	return err
}

// useConversationTx records the use now.
func (s *Store) useConversationTx(ctx context.Context, tx *sql.Tx, messenger, conv, user, contextID string) error {
	return s.recordConversationTx(ctx, tx, messenger, conv, user, contextID, s.now())
}

// recordConversationTx records the use at used and drops the oldest
// contexts past MaxConversations.
func (s *Store) recordConversationTx(ctx context.Context, tx *sql.Tx, messenger, conv, user, contextID string, used time.Time) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO conv_history (messenger, conv, user_id, context_id, used) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT DO UPDATE SET used = excluded.used`, messenger, conv, user, contextID, used.UnixNano()); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM conv_history WHERE messenger = ? AND conv = ? AND user_id = ? AND context_id NOT IN (
		SELECT context_id FROM conv_history WHERE messenger = ? AND conv = ? AND user_id = ? ORDER BY used DESC, context_id LIMIT ?)`,
		messenger, conv, user, messenger, conv, user, MaxConversations)
	return err
}
