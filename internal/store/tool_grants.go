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
)

// MaxToolGrants is how many grants a conversation keeps; a new grant
// replaces the oldest.
const MaxToolGrants = 50

// AddToolGrant records that user allowed the tool calls that rule matches
// in a conversation, for the tasks that user starts there. The grants go
// with the conversation's last task.
func (s *Store) AddToolGrant(ctx context.Context, contextID, user, rule string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO tool_grants (context_id, user_id, rule, created) VALUES (?, ?, ?, ?)
			ON CONFLICT DO UPDATE SET created = excluded.created`, contextID, user, rule, s.now().UnixNano()); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM tool_grants WHERE context_id = ? AND rowid NOT IN
			(SELECT rowid FROM tool_grants WHERE context_id = ? ORDER BY created DESC LIMIT ?)`, contextID, contextID, MaxToolGrants)
		return err
	})
}

// ToolGrants returns the rules that user allowed in a conversation, the
// latest first.
func (s *Store) ToolGrants(ctx context.Context, contextID, user string) ([]string, error) {
	return queryStrings(ctx, s.reads, `SELECT rule FROM tool_grants WHERE context_id = ? AND user_id = ? ORDER BY created DESC`, contextID, user)
}

// queryStrings returns the single string column of the rows of a query.
func queryStrings(ctx context.Context, db *sql.DB, query string, args ...any) ([]string, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		out = append(out, v)
	}
	return out, errors.Join(rows.Err(), rows.Close())
}
