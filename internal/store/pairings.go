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
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/a2aproject/a2a-serve/internal/cryptoutil"
	"github.com/a2aproject/a2a-serve/internal/pairapi"
)

// LocalPairing is the built-in pairing of the user running the agent: the
// token of this machine, and the chat accounts linked with the code printed
// at start. It is created with the database and never deleted.
const LocalPairing = "local"

// Pairing is someone allowed to call the agent. Tokens, chat links and codes
// point to it and carry no role of their own.
type Pairing struct {
	Name string
	// Kind is one of the pairapi.Pairing* kinds, or empty until the first
	// code is redeemed.
	Kind    string
	Role    string
	Created time.Time
}

// EnsurePairing creates the pairing if it doesn't exist and returns it.
// Pruning deletes a pairing with no kind and no codes, so a new pairing
// that waits for its first code gets it with [Store.InviteCode].
func (s *Store) EnsurePairing(ctx context.Context, p Pairing) (Pairing, error) {
	if _, err := s.exec(ctx, `INSERT INTO pairings (name, kind, role, created) VALUES (?, ?, ?, ?) ON CONFLICT DO NOTHING`,
		p.Name, p.Kind, p.Role, s.now().UnixNano()); err != nil {
		return Pairing{}, err
	}
	return s.Pairing(ctx, p.Name)
}

// Pairing returns a pairing by name.
func (s *Store) Pairing(ctx context.Context, name string) (Pairing, error) {
	return scanPairing(s.reads.QueryRowContext(ctx, `SELECT name, kind, role, created FROM pairings WHERE name = ?`, name))
}

// Pairings lists the pairings by name.
func (s *Store) Pairings(ctx context.Context) ([]Pairing, error) {
	rows, err := s.reads.QueryContext(ctx, `SELECT name, kind, role, created FROM pairings ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)
	var out []Pairing
	for rows.Next() {
		p, err := scanPairing(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func scanPairing(row interface{ Scan(...any) error }) (Pairing, error) {
	var (
		p       Pairing
		created int64
	)
	err := row.Scan(&p.Name, &p.Kind, &p.Role, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Pairing{}, ErrNotFound
	}
	p.Created = unixTime(created)
	return p, err
}

// UpdatePairing changes the kind and role of a pairing. They apply to the
// next request of each of its tokens and chat links.
func (s *Store) UpdatePairing(ctx context.Context, p Pairing) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		return deleteOneTx(ctx, tx, `UPDATE pairings SET kind = ?, role = ? WHERE name = ?`, p.Kind, p.Role, p.Name)
	})
}

// DeletePairing deletes a pairing with its tokens, chat links, codes, and
// its webhook node and the node's watches. principals are the users the
// pairing acts as. Their schedules, watches, tasks and questions go with
// it, and so do the channel conversations, agent sessions and handles of
// the contexts left without tasks, so a new pairing with the same name
// starts with none of them.
func (s *Store) DeletePairing(ctx context.Context, name string, principals ...string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM watches WHERE contact IN (SELECT id FROM nodes WHERE id = ?)`, name); err != nil {
			return err
		}
		for _, p := range principals {
			for _, del := range []string{
				`DELETE FROM schedules WHERE user_id = ?`,
				`DELETE FROM watches WHERE on_behalf_of = ?`,
				`DELETE FROM questions WHERE user_id = ?`,
			} {
				if _, err := tx.ExecContext(ctx, del, p); err != nil {
					return err
				}
			}
			deleted, err := deleteTasks(ctx, tx, `owner = ?`, p)
			if err != nil {
				return err
			}
			if err := forgetTasks(ctx, tx, deleted, "convs", "sessions", "handles", "call_tasks"); err != nil {
				return err
			}
		}
		if err := deleteOrphans(ctx, tx); err != nil {
			return err
		}
		return deleteOneTx(ctx, tx, `DELETE FROM pairings WHERE name = ?`, name)
	})
}

// PairingRemoval counts what [Store.DeletePairing] deletes for a pairing and
// the users it acts as.
type PairingRemoval struct {
	Tokens    int
	Links     int
	Codes     int
	Schedules int
	Watches   int
	// Questions are the approvals of the users.
	Questions int
	Tasks     int
	// Unfinished is the number of Tasks that are canceled first.
	Unfinished int
	// Conversations are the channel conversations of contexts left without
	// tasks.
	Conversations int
}

// PairingRemoval counts what DeletePairing(name, principals...) would
// delete, and changes nothing.
func (s *Store) PairingRemoval(ctx context.Context, name string, principals ...string) (PairingRemoval, error) {
	in := "(" + strings.TrimSuffix(strings.Repeat("?,", len(principals)), ",") + ")"
	var users []any
	for _, p := range principals {
		users = append(users, p)
	}
	var r PairingRemoval
	for _, c := range []struct {
		dst   *int
		query string
		args  []any
	}{
		{&r.Tokens, `SELECT COUNT(*) FROM tokens WHERE pairing = ?`, []any{name}},
		{&r.Links, `SELECT COUNT(*) FROM links WHERE pairing = ?`, []any{name}},
		{&r.Codes, `SELECT COUNT(*) FROM codes WHERE pairing = ?`, []any{name}},
		{&r.Schedules, `SELECT COUNT(*) FROM schedules WHERE user_id IN ` + in, users},
		{&r.Watches, `SELECT COUNT(*) FROM watches WHERE contact IN (SELECT id FROM nodes WHERE id = ?) OR on_behalf_of IN ` + in, append([]any{name}, users...)},
		{&r.Questions, `SELECT COUNT(*) FROM questions WHERE user_id IN ` + in + ` OR (contact = '' AND task_id IN (SELECT id FROM tasks WHERE owner IN ` + in + `))`, slices.Concat(users, users)},
		{&r.Tasks, `SELECT COUNT(*) FROM tasks WHERE owner IN ` + in, users},
		{&r.Unfinished, `SELECT COUNT(*) FROM tasks WHERE owner IN ` + in + ` AND state NOT IN (?, ?, ?, ?)`, append(slices.Clone(users),
			string(a2a.TaskStateCompleted), string(a2a.TaskStateFailed), string(a2a.TaskStateCanceled), string(a2a.TaskStateRejected))},
		{&r.Conversations, `SELECT COUNT(*) FROM convs WHERE context_id IN (SELECT context_id FROM tasks WHERE owner IN ` + in + `)
			AND NOT EXISTS (SELECT 1 FROM tasks WHERE tasks.context_id = convs.context_id AND tasks.owner NOT IN ` + in + `)`, slices.Concat(users, users)},
	} {
		if err := s.reads.QueryRowContext(ctx, c.query, c.args...).Scan(c.dst); err != nil {
			return PairingRemoval{}, err
		}
	}
	return r, nil
}

// RevokeCredentials deletes the tokens, chat links and codes of a pairing,
// and keeps the pairing.
func (s *Store) RevokeCredentials(ctx context.Context, name string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		for _, table := range []string{"tokens", "links", "codes"} {
			if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE pairing = ?`, name); err != nil {
				return err
			}
		}
		return nil
	})
}

// setKindIfUnset gives a pairing its kind when its first code is redeemed.
func setKindIfUnset(ctx context.Context, tx *sql.Tx, name, kind string) error {
	_, err := tx.ExecContext(ctx, `UPDATE pairings SET kind = ? WHERE name = ? AND kind = ''`, kind, name)
	return err
}

// Token is a bearer token, stored by its hash: the token of a pairing, or
// of a person who signed in with Google.
type Token struct {
	// Pairing is empty for Google sign-in tokens.
	Pairing string
	// Email is the email of a Google sign-in token, and Domain its Google
	// Workspace domain when it is the email's domain.
	Email  string
	Domain string
	// URL is the agent URL the token was issued at.
	URL      string
	Created  time.Time
	LastUsed time.Time
}

// PutToken stores a token by its hash. A pairing whose kind is unset gets
// kind.
func (s *Store) PutToken(ctx context.Context, token string, t Token, kind string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error { return s.putTokenTx(ctx, tx, token, t, kind) })
}

func (s *Store) putTokenTx(ctx context.Context, tx *sql.Tx, token string, t Token, kind string) error {
	var pairing any
	if t.Pairing != "" {
		pairing = t.Pairing
		if err := setKindIfUnset(ctx, tx, t.Pairing, kind); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO tokens (hash, pairing, email, domain, url, created) VALUES (?, ?, ?, ?, ?, ?)`,
		cryptoutil.Hash(token), pairing, t.Email, t.Domain, t.URL, s.now().UnixNano())
	return err
}

const tokenColumns = `coalesce(pairing, ''), email, domain, url, created, last_used`

func scanToken(row interface{ Scan(...any) error }) (Token, error) {
	var (
		t                 Token
		created, lastUsed int64
	)
	err := row.Scan(&t.Pairing, &t.Email, &t.Domain, &t.URL, &created, &lastUsed)
	if errors.Is(err, sql.ErrNoRows) {
		return Token{}, ErrNotFound
	}
	t.Created, t.LastUsed = unixTime(created), unixTime(lastUsed)
	return t, err
}

// lastUsedGranularity is how stale a token's last use gets before a lookup
// records a new one, which keeps lookups from writing on every request.
const lastUsedGranularity = time.Minute

// LookupToken returns the token record and records its use. A failure to
// record the use is logged and doesn't fail the lookup.
func (s *Store) LookupToken(ctx context.Context, token string) (Token, error) {
	h := cryptoutil.Hash(token)
	t, err := scanToken(s.reads.QueryRowContext(ctx, `SELECT `+tokenColumns+` FROM tokens WHERE hash = ?`, h))
	if err != nil {
		return Token{}, err
	}
	now := s.now()
	if now.Sub(t.LastUsed) <= lastUsedGranularity {
		return t, nil
	}
	if _, err := s.exec(ctx, `UPDATE tokens SET last_used = ? WHERE hash = ?`, now.UnixNano(), h); err != nil {
		slog.Warn("recording the use of a token", "pairing", t.Pairing, "error", err)
	}
	return t, nil
}

// Tokens lists issued tokens.
func (s *Store) Tokens(ctx context.Context) ([]Token, error) {
	rows, err := s.reads.QueryContext(ctx, `SELECT `+tokenColumns+` FROM tokens ORDER BY created`)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)
	var out []Token
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DeleteTokens deletes the tokens of a pairing, keeping its chat links.
func (s *Store) DeleteTokens(ctx context.Context, pairing string) error {
	_, err := s.exec(ctx, `DELETE FROM tokens WHERE pairing = ?`, pairing)
	return err
}

// DeleteSignIns deletes the Google sign-in tokens of email at every URL,
// and returns how many it deleted.
func (s *Store) DeleteSignIns(ctx context.Context, email string) (int64, error) {
	res, err := s.exec(ctx, `DELETE FROM tokens WHERE pairing IS NULL AND email = ?`, strings.ToLower(strings.TrimSpace(email)))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PruneSignIns deletes the Google sign-in tokens whose email and domain
// allowed refuses.
func (s *Store) PruneSignIns(ctx context.Context, allowed func(email, domain string) bool) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT DISTINCT email, domain FROM tokens WHERE pairing IS NULL`)
		if err != nil {
			return err
		}
		var refused [][2]string
		for rows.Next() {
			var email, domain string
			if err := rows.Scan(&email, &domain); err != nil {
				return errors.Join(err, rows.Close())
			}
			if !allowed(email, domain) {
				refused = append(refused, [2]string{email, domain})
			}
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		for _, r := range refused {
			if _, err := tx.ExecContext(ctx, `DELETE FROM tokens WHERE pairing IS NULL AND email = ? AND domain = ?`, r[0], r[1]); err != nil {
				return err
			}
		}
		return nil
	})
}

// Link ties a chat account to a pairing. Conv is the conversation of the
// account's direct chat, where the agent can reach the pairing unprompted.
type Link struct {
	Channel  string
	Identity string
	Pairing  string
	Conv     string
}

// PutLink links a chat account to a pairing, replacing an existing link. A
// pairing whose kind is unset gets kind.
func (s *Store) PutLink(ctx context.Context, l Link, kind string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error { return s.putLinkTx(ctx, tx, l, kind) })
}

func (s *Store) putLinkTx(ctx context.Context, tx *sql.Tx, l Link, kind string) error {
	if err := setKindIfUnset(ctx, tx, l.Pairing, kind); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO links (channel, identity, pairing, conv, created) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (channel, identity) DO UPDATE SET pairing = excluded.pairing, conv = excluded.conv`,
		l.Channel, l.Identity, l.Pairing, l.Conv, s.now().UnixNano())
	return err
}

// LinkOf returns the link of a chat account.
func (s *Store) LinkOf(ctx context.Context, channel, identity string) (Link, error) {
	l := Link{Channel: channel, Identity: identity}
	err := s.reads.QueryRowContext(ctx, `SELECT pairing, conv FROM links WHERE channel = ? AND identity = ?`,
		channel, identity).Scan(&l.Pairing, &l.Conv)
	if errors.Is(err, sql.ErrNoRows) {
		return Link{}, ErrNotFound
	}
	return l, err
}

// LinksOf returns the links of a pairing.
func (s *Store) LinksOf(ctx context.Context, pairing string) ([]Link, error) {
	all, err := s.links(ctx, `WHERE pairing = ?`, pairing)
	return all[pairing], err
}

// Links returns every link, by pairing.
func (s *Store) Links(ctx context.Context) (map[string][]Link, error) {
	return s.links(ctx, "")
}

func (s *Store) links(ctx context.Context, where string, args ...any) (map[string][]Link, error) {
	rows, err := s.reads.QueryContext(ctx, `SELECT channel, identity, pairing, conv FROM links `+where+` ORDER BY created`, args...)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)
	out := map[string][]Link{}
	for rows.Next() {
		var l Link
		if err := rows.Scan(&l.Channel, &l.Identity, &l.Pairing, &l.Conv); err != nil {
			return nil, err
		}
		out[l.Pairing] = append(out[l.Pairing], l)
	}
	return out, rows.Err()
}

// Code is a pending one-time code of a pairing, exchanged for a token or a
// chat link, or for the node token of a webhook pairing.
type Code struct {
	Pairing string
	// Kind is the kind of the pairing when the code was read.
	Kind    string
	Expires time.Time
}

// PutCode stores a code by its hash.
func (s *Store) PutCode(ctx context.Context, code string, c Code) error {
	_, err := s.exec(ctx, `INSERT INTO codes (hash, pairing, expires) VALUES (?, ?, ?)`,
		cryptoutil.Hash(code), c.Pairing, c.Expires.UnixNano())
	return err
}

// InviteCode creates the pairing if it doesn't exist and stores a code of
// it, in one transaction, so that pruning never finds a new pairing without
// its code.
func (s *Store) InviteCode(ctx context.Context, p Pairing, code string, expires time.Time) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO pairings (name, kind, role, created) VALUES (?, ?, ?, ?) ON CONFLICT DO NOTHING`,
			p.Name, p.Kind, p.Role, s.now().UnixNano()); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO codes (hash, pairing, expires) VALUES (?, ?, ?)`,
			cryptoutil.Hash(code), p.Name, expires.UnixNano())
		return err
	})
}

// ReplaceCode stores a code by its hash and deletes the other codes of its
// pairing, so that at most one of them is pending.
func (s *Store) ReplaceCode(ctx context.Context, code string, c Code) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM codes WHERE pairing = ?`, c.Pairing); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO codes (hash, pairing, expires) VALUES (?, ?, ?)`,
			cryptoutil.Hash(code), c.Pairing, c.Expires.UnixNano())
		return err
	})
}

// ErrCodeExpired is returned when a code was found but has expired.
var ErrCodeExpired = errors.New("the code has expired")

// Grant is what a code is exchanged for: a bearer token, a chat link, or
// the node token of a webhook node. It sets at most one of them.
type Grant struct {
	// Token is a bearer token of the code's pairing, issued at TokenURL.
	Token    string
	TokenURL string
	// Link links a chat account to the code's pairing, whatever its Pairing.
	Link *Link
	// Kind is given to the code's pairing if it has none yet, with a Token
	// or a Link.
	Kind string
	// NodeToken replaces the token of the code's webhook node, and
	// NodeSecret the key its wakes are signed with.
	NodeToken  string
	NodeSecret string
}

// RedeemCode consumes a code and stores what grant returns for it, in one
// transaction. A code grant refuses, or whose grant fails to store, stays
// pending. A code can be used once. Codes past their expiry are deleted
// and reported as [ErrCodeExpired].
func (s *Store) RedeemCode(ctx context.Context, code string, grant func(Code) (Grant, error)) (Code, error) {
	var (
		c       Code
		expired bool
	)
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var expires int64
		h := cryptoutil.Hash(code)
		err := tx.QueryRowContext(ctx, `SELECT c.pairing, p.kind, c.expires FROM codes c JOIN pairings p ON p.name = c.pairing WHERE c.hash = ?`, h).
			Scan(&c.Pairing, &c.Kind, &expires)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		c.Expires = unixTime(expires)
		g, err := grant(c)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM codes WHERE hash = ?`, h); err != nil {
			return err
		}
		if expired = s.now().After(c.Expires); expired {
			return nil
		}
		return s.storeGrant(ctx, tx, c.Pairing, g)
	})
	if err != nil {
		return Code{}, err
	}
	if expired {
		return Code{}, ErrCodeExpired
	}
	return c, nil
}

func (s *Store) storeGrant(ctx context.Context, tx *sql.Tx, pairing string, g Grant) error {
	switch {
	case g.Token != "":
		return s.putTokenTx(ctx, tx, g.Token, Token{Pairing: pairing, URL: g.TokenURL}, g.Kind)
	case g.Link != nil:
		l := *g.Link
		l.Pairing = pairing
		return s.putLinkTx(ctx, tx, l, g.Kind)
	case g.NodeToken != "":
		return deleteOneTx(ctx, tx, `UPDATE nodes SET token_hash = ?, secret = ? WHERE id = ?`, cryptoutil.Hash(g.NodeToken), g.NodeSecret, pairing)
	}
	return nil
}

// TakeCode consumes a code which accept accepts. A code it refuses stays
// pending, and is reported as not found. A code can be used once.
func (s *Store) TakeCode(ctx context.Context, code string, accept func(Code) bool) (Code, error) {
	return s.RedeemCode(ctx, code, func(c Code) (Grant, error) {
		if !accept(c) {
			return Grant{}, ErrNotFound
		}
		return Grant{}, nil
	})
}

// AnyCode accepts every code, for [Store.TakeCode].
func AnyCode(Code) bool { return true }

// PendingCodes lists the codes not used or expired yet, by pairing.
func (s *Store) PendingCodes(ctx context.Context) (map[string][]Code, error) {
	rows, err := s.reads.QueryContext(ctx, `SELECT c.pairing, p.kind, c.expires FROM codes c JOIN pairings p ON p.name = c.pairing
		WHERE c.expires > ? ORDER BY c.expires`, s.now().UnixNano())
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
		if err := rows.Scan(&c.Pairing, &c.Kind, &expires); err != nil {
			return nil, err
		}
		c.Expires = unixTime(expires)
		out[c.Pairing] = append(out[c.Pairing], c)
	}
	return out, rows.Err()
}

// putWebhookPairing creates the webhook pairing of a node, and fails with
// [ErrExists] if a pairing of another kind has its name.
func (s *Store) putWebhookPairing(ctx context.Context, tx *sql.Tx, name string) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO pairings (name, kind, role, created) VALUES (?, ?, 'authenticated', ?) ON CONFLICT DO NOTHING`,
		name, pairapi.PairingWebhook, s.now().UnixNano()); err != nil {
		return err
	}
	var kind string
	if err := tx.QueryRowContext(ctx, `SELECT kind FROM pairings WHERE name = ?`, name).Scan(&kind); err != nil {
		return err
	}
	if kind != pairapi.PairingWebhook {
		return ErrExists
	}
	return nil
}
