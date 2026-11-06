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
	"slices"
	"strings"
	"time"
)

// MaxToolPreferences is how many rules a person can allow in all their
// conversations.
const MaxToolPreferences = 50

// ToolTapRetention is how long a count of taps on "Always allow" is kept
// after the last tap.
const ToolTapRetention = 30 * 24 * time.Hour

// ErrPreferenceLimit is returned for a preference past MaxToolPreferences.
var ErrPreferenceLimit = errors.New("too many tool calls allowed in all conversations")

// AddToolPreference records that user allowed the tool calls that rule
// matches in all the tasks they start. It returns [ErrPreferenceLimit] when
// user allowed MaxToolPreferences rules already.
func (s *Store) AddToolPreference(ctx context.Context, user, rule string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM tool_preferences WHERE user_id = ? AND rule != ?`, user, rule).Scan(&n); err != nil {
			return err
		}
		if n >= MaxToolPreferences {
			return ErrPreferenceLimit
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO tool_preferences (user_id, rule, created) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`, user, rule, s.now().UnixNano())
		return err
	})
}

// ToolPreferences returns the rules user allowed in all their tasks, in
// the order they were allowed.
func (s *Store) ToolPreferences(ctx context.Context, user string) ([]string, error) {
	return queryStrings(ctx, s.reads, `SELECT rule FROM tool_preferences WHERE user_id = ? ORDER BY created, rule`, user)
}

// AllToolPreferences returns the rules each user allowed in all their
// tasks, in the order they were allowed.
func (s *Store) AllToolPreferences(ctx context.Context) (map[string][]string, error) {
	rows, err := s.reads.QueryContext(ctx, `SELECT user_id, rule FROM tool_preferences ORDER BY created, rule`)
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for rows.Next() {
		var user, rule string
		if err := rows.Scan(&user, &rule); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		out[user] = append(out[user], rule)
	}
	return out, errors.Join(rows.Err(), rows.Close())
}

// DeleteToolPreference removes a rule user allowed in all their tasks. It
// returns [ErrNotFound] if user didn't allow it.
func (s *Store) DeleteToolPreference(ctx context.Context, user, rule string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		return deleteOneTx(ctx, tx, `DELETE FROM tool_preferences WHERE user_id = ? AND rule = ?`, user, rule)
	})
}

// maxTapContexts is how many conversations a count of taps remembers.
const maxTapContexts = 10

// CountToolTap counts a tap of user on "Always allow" for rule in a
// conversation, and returns in how many conversations user tapped it since
// the count started. Taps in a conversation counted before don't count
// again.
func (s *Store) CountToolTap(ctx context.Context, user, rule, contextID string) (int, error) {
	var n int
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var joined string
		err := tx.QueryRowContext(ctx, `SELECT contexts FROM tool_taps WHERE user_id = ? AND rule = ?`, user, rule).Scan(&joined)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		contexts := strings.Fields(joined)
		if !slices.Contains(contexts, contextID) {
			contexts = append(contexts, contextID)
		}
		if len(contexts) > maxTapContexts {
			contexts = contexts[len(contexts)-maxTapContexts:]
		}
		n = len(contexts)
		_, err = tx.ExecContext(ctx, `INSERT INTO tool_taps (user_id, rule, contexts, updated) VALUES (?, ?, ?, ?)
			ON CONFLICT DO UPDATE SET contexts = excluded.contexts, updated = excluded.updated`,
			user, rule, strings.Join(contexts, "\n"), s.now().UnixNano())
		return err
	})
	return n, err
}

// ToolTaps returns in how many conversations user tapped "Always allow"
// for rule since the count started.
func (s *Store) ToolTaps(ctx context.Context, user, rule string) (int, error) {
	var joined string
	err := s.reads.QueryRowContext(ctx, `SELECT contexts FROM tool_taps WHERE user_id = ? AND rule = ?`, user, rule).Scan(&joined)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return len(strings.Fields(joined)), err
}

// ResetToolTaps starts the count of user's taps for rule over.
func (s *Store) ResetToolTaps(ctx context.Context, user, rule string) error {
	_, err := s.exec(ctx, `DELETE FROM tool_taps WHERE user_id = ? AND rule = ?`, user, rule)
	return err
}

// ToolSuggestion is an offer to a person to allow a rule in all their
// conversations.
type ToolSuggestion struct {
	ID        string
	User      string
	Rule      string
	ContextID string
}

// PutToolSuggestion stores an offer, replacing an earlier one of the same
// user and rule. It goes when it is answered, or with its conversation.
func (s *Store) PutToolSuggestion(ctx context.Context, sg ToolSuggestion) error {
	_, err := s.exec(ctx, `INSERT INTO tool_suggestions (id, user_id, rule, context_id, created) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (user_id, rule) DO UPDATE SET id = excluded.id, context_id = excluded.context_id, created = excluded.created`,
		sg.ID, sg.User, sg.Rule, sg.ContextID, s.now().UnixNano())
	return err
}

// TakeToolSuggestion returns an offer and deletes it, so that it is
// answered once. It returns [ErrNotFound] for an offer that is gone.
func (s *Store) TakeToolSuggestion(ctx context.Context, id, user string) (ToolSuggestion, error) {
	sg := ToolSuggestion{ID: id}
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `DELETE FROM tool_suggestions WHERE id = ? AND user_id = ? RETURNING user_id, rule, context_id`, id, user).
			Scan(&sg.User, &sg.Rule, &sg.ContextID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
	return sg, err
}

// ToolSuggestionUser returns the user an offer was made to.
func (s *Store) ToolSuggestionUser(ctx context.Context, id string) (string, error) {
	var user string
	err := s.reads.QueryRowContext(ctx, `SELECT user_id FROM tool_suggestions WHERE id = ?`, id).Scan(&user)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return user, err
}
