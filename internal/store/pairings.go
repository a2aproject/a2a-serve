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
)

// NamedLinks returns the channel links made with pairings, by pairing name.
func (s *Store) NamedLinks(ctx context.Context) (map[string][]Link, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT channel, identity, user_id, conv, name FROM links WHERE name != '' ORDER BY created`)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)
	out := map[string][]Link{}
	for rows.Next() {
		var l Link
		if err := rows.Scan(&l.Channel, &l.Identity, &l.UserID, &l.Conv, &l.Name); err != nil {
			return nil, err
		}
		out[l.Name] = append(out[l.Name], l)
	}
	return out, rows.Err()
}

// PendingCodes lists the codes not used or expired yet, by name, which
// identify them only by purpose, role and expiry.
func (s *Store) PendingCodes(ctx context.Context) (map[string][]Code, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT purpose, name, role, expires FROM codes WHERE expires > ? ORDER BY expires`, s.now().UnixNano())
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)
	out := map[string][]Code{}
	for rows.Next() {
		var (
			c       Code
			expires int64
		)
		if err := rows.Scan(&c.Purpose, &c.Name, &c.Role, &expires); err != nil {
			return nil, err
		}
		c.Expires = unixTime(expires)
		out[c.Name] = append(out[c.Name], c)
	}
	return out, rows.Err()
}

// DeleteCodes deletes the pending codes of purpose and name, and returns
// how many there were.
func (s *Store) DeleteCodes(ctx context.Context, purpose, name string) (int, error) {
	res, err := s.exec(ctx, `DELETE FROM codes WHERE purpose = ? AND name = ?`, purpose, name)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// SetPairingRole moves the tokens, links and pending codes of the pairing
// name to principal with role, and deletes the user previous, which the
// pairing no longer uses, unless it is principal.
func (s *Store) SetPairingRole(ctx context.Context, name, role, principal, previous string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO users (id, roles, created) VALUES (?, ?, ?) ON CONFLICT (id) DO NOTHING`,
			principal, role, s.now().UnixNano()); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tokens SET principal = ?, roles = ? WHERE name = ?`, principal, role, name); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE links SET user_id = ? WHERE name = ?`, principal, name); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE codes SET role = ? WHERE purpose = 'pair' AND name = ?`, role, name); err != nil {
			return err
		}
		if previous == principal {
			return nil
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM users WHERE id = ? AND NOT EXISTS (SELECT 1 FROM links WHERE user_id = ?)`, previous, previous)
		return err
	})
}
