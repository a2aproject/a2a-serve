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

// SetVoiceReplies turns the voice replies of user on or off in all their
// chats.
func (s *Store) SetVoiceReplies(ctx context.Context, user string, on bool) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if !on {
			_, err := tx.ExecContext(ctx, `DELETE FROM voice_replies WHERE user_id = ?`, user)
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO voice_replies (user_id, created) VALUES (?, ?) ON CONFLICT DO NOTHING`, user, s.now().UnixNano())
		return err
	})
}

// VoiceReplies reports whether user turned voice replies on.
func (s *Store) VoiceReplies(ctx context.Context, user string) (bool, error) {
	var one int
	err := s.reads.QueryRowContext(ctx, `SELECT 1 FROM voice_replies WHERE user_id = ?`, user).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
