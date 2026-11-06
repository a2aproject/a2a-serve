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

// Package store keeps the state of an agent server in SQLite: tasks, ACP
// sessions, pairings with their tokens, chat links and codes, contacts,
// webhook nodes, channel conversations, questions and the handles of agents
// URLs.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"modernc.org/sqlite" // registers the sqlite driver
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/a2aproject/a2a-serve/internal/agenterr"
)

// ErrNotFound is returned when a record doesn't exist.
var ErrNotFound = errors.New("not found")

// Store is the SQLite database of one agent server.
type Store struct {
	// db is the only connection that writes. reads serves the read-only
	// queries and is db for an in-memory database.
	db    *sql.DB
	reads *sql.DB
	now   func() time.Time
	// taskTTL is how long tasks are kept after their last update.
	taskTTL atomic.Int64

	// Close waits for writes in progress: a transaction keeps its connection
	// past sql.DB.Close and could commit after the next server opened the
	// database.
	closeMu sync.Mutex
	closed  bool
	writes  sync.WaitGroup
}

// readConns is the size of the pool that serves reads of an on-disk database.
const readConns = 4

// Open opens or creates the database at path. ":memory:" opens a private
// in-memory database, for tests.
func Open(path string) (*Store, error) {
	if path == ":memory:" {
		db, err := sql.Open("sqlite", "file::memory:?_pragma=foreign_keys(1)")
		if err != nil {
			return nil, err
		}
		// Each connection has a database of its own, so there is only one.
		db.SetMaxOpenConns(1)
		return newStore(db, db, path)
	}
	u := url.URL{Scheme: "file", OmitHost: true, Path: path}
	// WAL lets reads proceed during a write; NORMAL syncs at checkpoints
	// only, which can't corrupt the database in WAL mode.
	const common = "_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	// A deferred transaction that reads before it writes gets SQLITE_BUSY
	// without the busy timeout when a reader holds the write lock for a
	// moment; IMMEDIATE takes the lock up front, where the timeout applies.
	u.RawQuery = common + "&_txlock=immediate"
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	// SQLite allows one writer.
	db.SetMaxOpenConns(1)
	u.RawQuery = common + "&_pragma=query_only(1)"
	reads, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, errors.Join(err, db.Close())
	}
	reads.SetMaxOpenConns(readConns)
	return newStore(db, reads, path)
}

// newStore migrates db and wraps it. reads serves the read-only queries, and
// is db itself or a pool of read-only connections.
func newStore(db, reads *sql.DB, path string) (*Store, error) {
	s := &Store{db: db, reads: reads, now: time.Now}
	s.SetTaskTTL(DefaultTaskTTL)
	if err := s.migrate(context.Background()); err != nil {
		err = fmt.Errorf("migrating %s: %w", path, err)
		if damaged(err) {
			err = agenterr.WithHint(err, agenterr.HintDamagedDB, path)
		}
		return nil, errors.Join(err, s.closeDBs())
	}
	if _, err := s.db.Exec(indexes); err != nil {
		return nil, errors.Join(fmt.Errorf("indexing %s: %w", path, err), s.closeDBs())
	}
	return s, nil
}

// damaged reports whether err is SQLite's error for a file that isn't a
// database or is corrupt.
func damaged(err error) bool {
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return false
	}
	code := se.Code() & 0xff
	return code == sqlite3.SQLITE_NOTADB || code == sqlite3.SQLITE_CORRUPT
}

// indexes are created when the store opens, outside the migrations, so that
// an older a2a-serve, which doesn't know them, still opens the database.
const indexes = `
CREATE INDEX IF NOT EXISTS tasks_context_updated ON tasks (scope, context_id, updated DESC, id);
CREATE INDEX IF NOT EXISTS tasks_owner_updated ON tasks (scope, owner, updated DESC, id);
CREATE INDEX IF NOT EXISTS questions_task ON questions (task_id);
CREATE INDEX IF NOT EXISTS questions_unanswered ON questions (created) WHERE answered_by = '';
`

// Close waits for the writes in progress and closes the database. Later
// writes fail with ErrClosed.
func (s *Store) Close() error {
	s.closeMu.Lock()
	s.closed = true
	s.closeMu.Unlock()
	s.writes.Wait()
	return s.closeDBs()
}

func (s *Store) closeDBs() error {
	if s.reads == s.db {
		return s.db.Close()
	}
	return errors.Join(s.reads.Close(), s.db.Close())
}

// beginWrite registers a write for Close to wait for, and returns the
// function that ends it.
func (s *Store) beginWrite() (func(), error) {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	s.writes.Add(1)
	return s.writes.Done, nil
}

func (s *Store) exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	done, err := s.beginWrite()
	if err != nil {
		return nil, err
	}
	defer done()
	return s.db.ExecContext(ctx, query, args...)
}

// migrations bring the database from the version in PRAGMA user_version,
// which is its index here, to the next. They run in a transaction each and
// must never change once released; add a new one instead.
var migrations = []func(ctx context.Context, tx *sql.Tx) error{
	// 1: the prototype schema. Databases created before user_version was
	// kept are at 0 with some of these tables, so it is idempotent.
	func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, schemaV1); err != nil {
			return err
		}
		return addColumn(ctx, tx, "links", "name", "TEXT NOT NULL DEFAULT ''")
	},
	// 2: handles and pending questions move out of the kv table, which keeps
	// only channel state, and remote questions and the messages that show
	// questions are kept, so both survive a restart.
	func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, schemaV2)
		return err
	},
	// 3: ACP sessions record the key of the agent process which has them.
	func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `ALTER TABLE sessions ADD COLUMN agent_key TEXT NOT NULL DEFAULT ''`)
		return err
	},
	// 4: raw parts of tasks move to a blob table, so that updating a task
	// doesn't rewrite its attachments. Tasks stored before keep them inline
	// until their next update.
	func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, schemaV4)
		return err
	},
	// 5: watches record since when their agent is unreachable, so that a
	// restart doesn't reset the time until they give up.
	func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `ALTER TABLE watches ADD COLUMN failing_since INTEGER NOT NULL DEFAULT 0`)
		return err
	},
	// 6: schedules, which run a prompt on a cron expression or once.
	func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, schemaV6)
		return err
	},
	// 7: questions record the user who may answer them besides owners.
	func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `ALTER TABLE questions ADD COLUMN user_id TEXT NOT NULL DEFAULT ''`)
		return err
	},
	// 8: tasks keep an append-only log of events, and the tasks row only a
	// header without history and artifacts. Existing tasks become a log of
	// one full task event.
	func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, schemaV8)
		return err
	},
	// 9: an agents URL handle stands for a conversation only, and the tasks
	// each conversation started at other agents are recorded. Old handles
	// are dropped.
	func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, schemaV9)
		return err
	},
	// 10: one role per user and token, named as in the access model; owner
	// pairings get a principal of their own; Google sign-in tokens keep
	// their Workspace domain; watches and schedules keep the identity of
	// the user they act for.
	func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, schemaV10)
		return err
	},
	// 11: pairings are records with a name, a kind and a role, which
	// tokens, chat links and codes point to; Google sign-in tokens keep the
	// email. Users, tokens, links and codes are dropped, so callers pair
	// again; webhook nodes become webhook pairings. The built-in local
	// pairing is the user running the agent.
	func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, schemaV11)
		return err
	},
	// 12: pruning deletes expired dedup keys by time.
	func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS seen_at ON seen (at)`)
		return err
	},
	// 13: schedules made for the local pairing run as and are shown to the
	// user running the agent, whom its tokens and chat links act as.
	func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, schemaV13)
		return err
	},
	// 14: a conversation keeps every question pending in it, and a typed
	// answer finds its question by the message it replies to. Questions
	// whose tasks stopped waiting for them are marked closed.
	func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, schemaV14)
		return err
	},
	// 15: watches count the calls that watched their task again, so that
	// posting a result deletes only the watch it was read from, and record
	// the task's last state, so that a watch of a stuck task ends.
	func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, schemaV15)
		return err
	},
	// 16: ACP sessions record the workspace they were created in, so that a
	// session isn't continued after the workspace moved.
	func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `ALTER TABLE sessions ADD COLUMN workspace TEXT NOT NULL DEFAULT ''`)
		return err
	},
}

const schemaV1 = `
CREATE TABLE IF NOT EXISTS kv (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS sessions (context_id TEXT PRIMARY KEY, session_id TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS tasks (
	id TEXT NOT NULL, scope TEXT NOT NULL, context_id TEXT NOT NULL, owner TEXT NOT NULL,
	version INTEGER NOT NULL, state TEXT NOT NULL, status_time INTEGER NOT NULL,
	updated INTEGER NOT NULL, data TEXT NOT NULL,
	PRIMARY KEY (scope, id));
CREATE INDEX IF NOT EXISTS tasks_context ON tasks (scope, context_id);
CREATE INDEX IF NOT EXISTS tasks_updated ON tasks (scope, updated DESC, id);
CREATE TABLE IF NOT EXISTS users (id TEXT PRIMARY KEY, roles TEXT NOT NULL, created INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS links (
	channel TEXT NOT NULL, identity TEXT NOT NULL, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	conv TEXT NOT NULL DEFAULT '', created INTEGER NOT NULL,
	PRIMARY KEY (channel, identity));
CREATE TABLE IF NOT EXISTS convs (
	channel TEXT NOT NULL, conv TEXT NOT NULL, context_id TEXT NOT NULL, updated INTEGER NOT NULL,
	PRIMARY KEY (channel, conv));
CREATE INDEX IF NOT EXISTS convs_context ON convs (context_id);
CREATE TABLE IF NOT EXISTS codes (
	hash TEXT PRIMARY KEY, purpose TEXT NOT NULL, name TEXT NOT NULL, role TEXT NOT NULL,
	expires INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS tokens (
	hash TEXT PRIMARY KEY, name TEXT NOT NULL, principal TEXT NOT NULL, roles TEXT NOT NULL,
	url TEXT NOT NULL, created INTEGER NOT NULL, last_used INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS tokens_name ON tokens (name);
CREATE TABLE IF NOT EXISTS contacts (
	name TEXT PRIMARY KEY, url TEXT NOT NULL, token TEXT NOT NULL DEFAULT '', via TEXT NOT NULL,
	created INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS nodes (
	id TEXT PRIMARY KEY, name TEXT NOT NULL, description TEXT NOT NULL DEFAULT '',
	webhook TEXT NOT NULL, headers TEXT NOT NULL DEFAULT '{}', secret TEXT NOT NULL,
	token_hash TEXT NOT NULL DEFAULT '', created INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS seen (key TEXT PRIMARY KEY, at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS questions (
	id TEXT PRIMARY KEY, token TEXT NOT NULL UNIQUE, task_id TEXT NOT NULL, context_id TEXT NOT NULL,
	contact TEXT NOT NULL DEFAULT '', data TEXT NOT NULL, created INTEGER NOT NULL,
	answered_by TEXT NOT NULL DEFAULT '', answer TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS watches (
	contact TEXT NOT NULL, task_id TEXT NOT NULL, remote_context_id TEXT NOT NULL,
	origin_context_id TEXT NOT NULL, on_behalf_of TEXT NOT NULL, created INTEGER NOT NULL,
	PRIMARY KEY (contact, task_id));
`

const schemaV2 = `
CREATE TABLE handles (
	handle TEXT PRIMARY KEY, context_id TEXT NOT NULL, on_behalf_of TEXT NOT NULL, hops INTEGER NOT NULL,
	UNIQUE (context_id, on_behalf_of, hops));
INSERT OR IGNORE INTO handles
	SELECT substr(key, 8), json_extract(value, '$.c'), coalesce(json_extract(value, '$.u'), ''), coalesce(json_extract(value, '$.h'), 0)
	FROM kv WHERE key LIKE 'handle:%';
CREATE TABLE pending_questions (
	channel TEXT NOT NULL, conv TEXT NOT NULL, question_id TEXT NOT NULL,
	PRIMARY KEY (channel, conv));
INSERT OR IGNORE INTO pending_questions
	SELECT substr(rest, 1, instr(rest, ':') - 1), substr(rest, instr(rest, ':') + 1), value
	FROM (SELECT substr(key, 9) AS rest, value FROM kv WHERE key LIKE 'pending:%' AND value != '');
DELETE FROM kv WHERE key LIKE 'handle:%' OR key LIKE 'handle-of:%' OR key LIKE 'pending:%';
ALTER TABLE kv RENAME TO channel_state;
ALTER TABLE questions ADD COLUMN hitl_id TEXT NOT NULL DEFAULT '';
UPDATE questions SET hitl_id = coalesce(json_extract(data, '$.id'), '');
DELETE FROM questions WHERE contact != '' AND rowid NOT IN (
	SELECT min(rowid) FROM questions WHERE contact != '' GROUP BY contact, task_id, hitl_id);
CREATE UNIQUE INDEX questions_remote ON questions (contact, task_id, hitl_id) WHERE contact != '';
CREATE TABLE question_messages (
	question_id TEXT NOT NULL REFERENCES questions(id) ON DELETE CASCADE,
	channel TEXT NOT NULL, conv TEXT NOT NULL, message_id TEXT NOT NULL, text TEXT NOT NULL,
	PRIMARY KEY (question_id, channel, message_id));
`

const schemaV4 = `
CREATE TABLE blobs (hash TEXT PRIMARY KEY, data BLOB NOT NULL);
CREATE TABLE task_blobs (
	scope TEXT NOT NULL, task_id TEXT NOT NULL, hash TEXT NOT NULL,
	PRIMARY KEY (scope, task_id, hash));
CREATE INDEX task_blobs_hash ON task_blobs (hash);
CREATE INDEX tasks_state_updated ON tasks (state, updated);
CREATE INDEX tasks_context_any_scope ON tasks (context_id);
`

const schemaV6 = `
CREATE TABLE schedules (
	name TEXT PRIMARY KEY, cron TEXT NOT NULL DEFAULT '', at INTEGER NOT NULL DEFAULT 0, tz TEXT NOT NULL DEFAULT '',
	prompt TEXT NOT NULL, user_id TEXT NOT NULL, deliver_channel TEXT NOT NULL DEFAULT '',
	deliver_user TEXT NOT NULL DEFAULT '', conversation TEXT NOT NULL, context_id TEXT NOT NULL DEFAULT '',
	created_by TEXT NOT NULL, created INTEGER NOT NULL, next_run INTEGER NOT NULL DEFAULT 0,
	last_run INTEGER NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT '');
`

const schemaV8 = `
CREATE TABLE task_events (
	scope TEXT NOT NULL, task_id TEXT NOT NULL, version INTEGER NOT NULL, kind TEXT NOT NULL,
	created INTEGER NOT NULL, data TEXT NOT NULL,
	PRIMARY KEY (scope, task_id, version),
	FOREIGN KEY (scope, task_id) REFERENCES tasks (scope, id) ON DELETE CASCADE);
CREATE TABLE task_snapshots (
	scope TEXT NOT NULL, task_id TEXT NOT NULL, version INTEGER NOT NULL, data TEXT NOT NULL,
	PRIMARY KEY (scope, task_id),
	FOREIGN KEY (scope, task_id) REFERENCES tasks (scope, id) ON DELETE CASCADE);
INSERT INTO task_events (scope, task_id, version, kind, created, data)
	SELECT scope, id, version, 'task', updated, data FROM tasks;
UPDATE tasks SET data = json_remove(data, '$.history', '$.artifacts');
DROP INDEX tasks_state_updated;
CREATE INDEX tasks_updated_any_scope ON tasks (updated);
`

const schemaV10 = `
ALTER TABLE tokens ADD COLUMN domain TEXT NOT NULL DEFAULT '';
ALTER TABLE watches ADD COLUMN identity TEXT NOT NULL DEFAULT '';
ALTER TABLE schedules ADD COLUMN user_identity TEXT NOT NULL DEFAULT '';
UPDATE users SET roles = CASE WHEN ',' || roles || ',' LIKE '%,owner,%' THEN 'owner' ELSE 'authenticated' END;
UPDATE tokens SET roles = CASE WHEN ',' || roles || ',' LIKE '%,owner,%' THEN 'owner' ELSE 'authenticated' END;
UPDATE codes SET role = 'authenticated' WHERE purpose = 'pair' AND role != 'owner';
INSERT INTO users (id, roles, created)
	SELECT DISTINCT 'pair:' || name, 'owner', created FROM tokens WHERE principal = 'user:owner' AND name != 'local'
	UNION SELECT DISTINCT 'pair:' || name, 'owner', created FROM links WHERE user_id = 'user:owner' AND name NOT IN ('', 'owner')
	ON CONFLICT (id) DO UPDATE SET roles = 'owner';
UPDATE tokens SET principal = 'pair:' || name WHERE principal = 'user:owner' AND name != 'local';
UPDATE links SET user_id = 'pair:' || name WHERE user_id = 'user:owner' AND name NOT IN ('', 'owner');
`

const schemaV9 = `
DROP TABLE handles;
CREATE TABLE handles (handle TEXT PRIMARY KEY, context_id TEXT NOT NULL UNIQUE);
CREATE TABLE call_tasks (
	context_id TEXT NOT NULL, agent TEXT NOT NULL, task_id TEXT NOT NULL, remote_context_id TEXT NOT NULL,
	PRIMARY KEY (context_id, agent, task_id));
CREATE INDEX call_tasks_remote_context ON call_tasks (context_id, agent, remote_context_id);
`

const schemaV11 = `
DROP TABLE links;
DROP TABLE tokens;
DROP TABLE codes;
DROP TABLE users;
CREATE TABLE pairings (name TEXT PRIMARY KEY, kind TEXT NOT NULL, role TEXT NOT NULL, created INTEGER NOT NULL);
INSERT INTO pairings (name, kind, role, created) VALUES ('local', 'person', 'owner', CAST(strftime('%s', 'now') AS INTEGER) * 1000000000);
INSERT INTO pairings (name, kind, role, created) SELECT id, 'webhook', 'authenticated', created FROM nodes WHERE id != 'local';
DELETE FROM nodes WHERE id = 'local';
CREATE TABLE nodes_v11 (
	id TEXT PRIMARY KEY REFERENCES pairings(name) ON DELETE CASCADE, name TEXT NOT NULL, description TEXT NOT NULL DEFAULT '',
	webhook TEXT NOT NULL, headers TEXT NOT NULL DEFAULT '{}', secret TEXT NOT NULL,
	token_hash TEXT NOT NULL DEFAULT '', created INTEGER NOT NULL);
INSERT INTO nodes_v11 SELECT id, name, description, webhook, headers, secret, token_hash, created FROM nodes;
DROP TABLE nodes;
ALTER TABLE nodes_v11 RENAME TO nodes;
CREATE TABLE tokens (
	hash TEXT PRIMARY KEY, pairing TEXT REFERENCES pairings(name) ON DELETE CASCADE,
	email TEXT NOT NULL DEFAULT '', domain TEXT NOT NULL DEFAULT '',
	url TEXT NOT NULL, created INTEGER NOT NULL, last_used INTEGER NOT NULL DEFAULT 0);
CREATE INDEX tokens_pairing ON tokens (pairing);
CREATE TABLE links (
	channel TEXT NOT NULL, identity TEXT NOT NULL, pairing TEXT NOT NULL REFERENCES pairings(name) ON DELETE CASCADE,
	conv TEXT NOT NULL DEFAULT '', created INTEGER NOT NULL,
	PRIMARY KEY (channel, identity));
CREATE INDEX links_pairing ON links (pairing);
CREATE TABLE codes (
	hash TEXT PRIMARY KEY, pairing TEXT NOT NULL REFERENCES pairings(name) ON DELETE CASCADE, expires INTEGER NOT NULL);
CREATE INDEX codes_pairing ON codes (pairing);
`

const schemaV13 = `
UPDATE schedules SET user_id = 'user:owner', user_identity = '' WHERE user_id = 'pair:local';
UPDATE schedules SET deliver_user = 'user:owner' WHERE deliver_user = 'pair:local';
`

const schemaV14 = `
CREATE TABLE pending_questions_v14 (
	channel TEXT NOT NULL, conv TEXT NOT NULL, question_id TEXT NOT NULL,
	PRIMARY KEY (channel, conv, question_id));
INSERT INTO pending_questions_v14 SELECT channel, conv, question_id FROM pending_questions;
DROP TABLE pending_questions;
ALTER TABLE pending_questions_v14 RENAME TO pending_questions;
CREATE INDEX pending_questions_question ON pending_questions (question_id);
CREATE INDEX question_messages_message ON question_messages (channel, conv, message_id);
ALTER TABLE questions ADD COLUMN closed INTEGER NOT NULL DEFAULT 0;
`

const schemaV15 = `
ALTER TABLE watches ADD COLUMN generation INTEGER NOT NULL DEFAULT 0;
ALTER TABLE watches ADD COLUMN state TEXT NOT NULL DEFAULT '';
ALTER TABLE watches ADD COLUMN state_changed INTEGER NOT NULL DEFAULT 0;
`

func (s *Store) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version > len(migrations) {
		return fmt.Errorf("the database is at version %d, newer than this a2a-serve supports (%d); upgrade a2a-serve", version, len(migrations))
	}
	for v := version; v < len(migrations); v++ {
		if err := s.runMigration(ctx, v); err != nil {
			return fmt.Errorf("to version %d: %w", v+1, err)
		}
	}
	return nil
}

func (s *Store) runMigration(ctx context.Context, from int) (err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, tx.Rollback())
		}
	}()
	if err := migrations[from](ctx, tx); err != nil {
		return err
	}
	// PRAGMA takes no bound parameters; the version is an int.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", from+1)); err != nil {
		return err
	}
	return tx.Commit()
}

// inTx runs fn in a transaction, committing it if fn succeeds.
func (s *Store) inTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	done, err := s.beginWrite()
	if err != nil {
		return err
	}
	defer done()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return errors.Join(err, tx.Rollback())
	}
	return tx.Commit()
}

// readTx runs fn in a read transaction, whose queries all see the same state
// of the database.
func (s *Store) readTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.reads.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return errors.Join(err, tx.Rollback())
	}
	return tx.Commit()
}

// addColumn adds a column to a table created before the column existed.
func addColumn(ctx context.Context, tx *sql.Tx, table, column, decl string) error {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err := tx.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, decl))
	return err
}

// ErrClosed is returned by writes to a closed store.
var ErrClosed = errors.New("the database is closed")

// ErrExists is returned when a record to create already exists.
var ErrExists = errors.New("already exists")

// ChannelState keeps small state of the channels, such as the last message
// seen, across restarts. Keys start with the channel name.
type ChannelState struct{ s *Store }

// ChannelState returns the state of the channels.
func (s *Store) ChannelState() ChannelState { return ChannelState{s: s} }

// Get returns the value of key, or false if it is not set.
func (cs ChannelState) Get(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := cs.s.reads.QueryRowContext(ctx, `SELECT value FROM channel_state WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return v, err == nil, err
}

// Set stores the value of key.
func (cs ChannelState) Set(ctx context.Context, key, value string) error {
	_, err := cs.s.exec(ctx, `INSERT INTO channel_state (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// Delete removes key.
func (cs ChannelState) Delete(ctx context.Context, key string) error {
	_, err := cs.s.exec(ctx, `DELETE FROM channel_state WHERE key = ?`, key)
	return err
}

// seenRetention is how long dedup keys are kept.
const seenRetention = 7 * 24 * time.Hour

// FirstSeen records key and reports whether it was new. Channels use it to
// drop redelivered messages. [Store.Prune] forgets old keys.
func (s *Store) FirstSeen(ctx context.Context, key string) (bool, error) {
	res, err := s.exec(ctx, `INSERT INTO seen (key, at) VALUES (?, ?) ON CONFLICT DO NOTHING`, key, s.now().UnixNano())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}
