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

// UserIdentityRetention is how long the identity of a user who doesn't
// call again is kept.
const UserIdentityRetention = 90 * 24 * time.Hour

// SaveUserIdentity keeps identity, as JSON, as the last identity the
// server verified for the user id.
func (s *Store) SaveUserIdentity(ctx context.Context, id, identity string) error {
	_, err := s.exec(ctx, `INSERT INTO user_identities (user_id, identity, seen) VALUES (?, ?, ?)
		ON CONFLICT (user_id) DO UPDATE SET identity = excluded.identity, seen = excluded.seen`, id, identity, s.now().UnixNano())
	return err
}

// UserIdentity returns the last identity the server verified for the user
// id, as JSON, or [ErrNotFound].
func (s *Store) UserIdentity(ctx context.Context, id string) (string, error) {
	var identity string
	err := s.reads.QueryRowContext(ctx, `SELECT identity FROM user_identities WHERE user_id = ?`, id).Scan(&identity)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return identity, err
}
