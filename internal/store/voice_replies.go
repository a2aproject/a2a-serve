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

// VoiceRepliesRetention is how long the voice replies of an email sender
// stay on after their last message. Other users lose theirs with their
// pairing.
const VoiceRepliesRetention = 90 * 24 * time.Hour

// voiceRepliesTouch is how often a message of a user refreshes the time
// their voice replies were last used.
const voiceRepliesTouch = 24 * time.Hour

// SetVoiceReplies turns the voice replies of user on or off in all their
// chats.
func (s *Store) SetVoiceReplies(ctx context.Context, user string, on bool) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if !on {
			_, err := tx.ExecContext(ctx, `DELETE FROM voice_replies WHERE user_id = ?`, user)
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO voice_replies (user_id, created) VALUES (?, ?)
			ON CONFLICT (user_id) DO UPDATE SET created = excluded.created`, user, s.now().UnixNano())
		return err
	})
}

// TouchVoiceReplies records that user, who has voice replies on, sent a
// message. The column created holds the time of the last use.
func (s *Store) TouchVoiceReplies(ctx context.Context, user string) error {
	now := s.now()
	_, err := s.exec(ctx, `UPDATE voice_replies SET created = ? WHERE user_id = ? AND created < ?`,
		now.UnixNano(), user, now.Add(-voiceRepliesTouch).UnixNano())
	return err
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
