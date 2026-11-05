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
	"encoding/json"
	"errors"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/integrations/executor/a2aacp"
)

// SessionStore implements the a2aacp session store: A2A context → ACP session.
type SessionStore struct{ s *Store }

// Sessions returns the ACP session store.
func (s *Store) Sessions() SessionStore { return SessionStore{s: s} }

var _ a2aacp.SessionStore = SessionStore{}

// Get returns the ACP session of an A2A context.
func (ss SessionStore) Get(ctx context.Context, contextID string) (a2aacp.Session, bool, error) {
	var sess a2aacp.Session
	err := ss.s.reads.QueryRowContext(ctx, `SELECT session_id, agent_key FROM sessions WHERE context_id = ?`, contextID).
		Scan(&sess.ID, &sess.AgentKey)
	if errors.Is(err, sql.ErrNoRows) {
		return a2aacp.Session{}, false, nil
	}
	return sess, err == nil, err
}

// Put records the ACP session of an A2A context.
func (ss SessionStore) Put(ctx context.Context, contextID string, sess a2aacp.Session) error {
	_, err := ss.s.exec(ctx, `INSERT INTO sessions (context_id, session_id, agent_key) VALUES (?, ?, ?)
		ON CONFLICT (context_id) DO UPDATE SET session_id = excluded.session_id, agent_key = excluded.agent_key`,
		contextID, sess.ID, sess.AgentKey)
	return err
}

// ContextOf returns the A2A context of a channel conversation.
func (s *Store) ContextOf(ctx context.Context, channel, conv string) (string, error) {
	var id string
	err := s.reads.QueryRowContext(ctx, `SELECT context_id FROM convs WHERE channel = ? AND conv = ?`, channel, conv).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return id, err
}

// SetContext binds a channel conversation to an A2A context.
func (s *Store) SetContext(ctx context.Context, channel, conv, contextID string) error {
	_, err := s.exec(ctx, `INSERT INTO convs (channel, conv, context_id, updated) VALUES (?, ?, ?, ?)
		ON CONFLICT (channel, conv) DO UPDATE SET context_id = excluded.context_id, updated = excluded.updated`,
		channel, conv, contextID, s.now().UnixNano())
	return err
}

// ConvOf returns the channel conversation bound to an A2A context.
func (s *Store) ConvOf(ctx context.Context, contextID string) (channel, conv string, err error) {
	err = s.reads.QueryRowContext(ctx, `SELECT channel, conv FROM convs WHERE context_id = ? ORDER BY updated DESC LIMIT 1`,
		contextID).Scan(&channel, &conv)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrNotFound
	}
	return channel, conv, err
}

// Contact is another agent this agent may call.
type Contact struct {
	Name string
	URL  string
	// Token is the outbound a2p_ token, empty for contacts reached over the
	// private network, which recognize the caller by its network identity.
	Token string
	// Via is private (the network provider's HTTP client) or https.
	Via     string
	Created time.Time
}

// PutContact creates or replaces a contact.
func (s *Store) PutContact(ctx context.Context, c Contact) error {
	_, err := s.exec(ctx, `INSERT INTO contacts (name, url, token, via, created) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (name) DO UPDATE SET url = excluded.url, token = excluded.token, via = excluded.via`,
		c.Name, c.URL, c.Token, c.Via, s.now().UnixNano())
	return err
}

// Contact returns a contact by name.
func (s *Store) Contact(ctx context.Context, name string) (Contact, error) {
	c := Contact{Name: name}
	var created int64
	err := s.reads.QueryRowContext(ctx, `SELECT url, token, via, created FROM contacts WHERE name = ?`, name).
		Scan(&c.URL, &c.Token, &c.Via, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Contact{}, ErrNotFound
	}
	c.Created = unixTime(created)
	return c, err
}

// Contacts lists contacts.
func (s *Store) Contacts(ctx context.Context) ([]Contact, error) {
	rows, err := s.reads.QueryContext(ctx, `SELECT name, url, token, via, created FROM contacts ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)
	var out []Contact
	for rows.Next() {
		var (
			c       Contact
			created int64
		)
		if err := rows.Scan(&c.Name, &c.URL, &c.Token, &c.Via, &created); err != nil {
			return nil, err
		}
		c.Created = unixTime(created)
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeleteContact removes a contact and its watches.
func (s *Store) DeleteContact(ctx context.Context, name string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM watches WHERE contact = ?`, name); err != nil {
			return err
		}
		return deleteOneTx(ctx, tx, `DELETE FROM contacts WHERE name = ?`, name)
	})
}

// Node is a webhook node hosted by the agent.
type Node struct {
	ID          string
	Name        string
	Description string
	Webhook     string
	Headers     map[string]string
	// Secret signs wakes with HMAC-SHA256: the key derived from the node
	// token, or a random one until the node pairs.
	Secret    string
	TokenHash string
	Created   time.Time
}

// PutNode creates or replaces a node, with its webhook pairing. It returns
// [ErrExists] if a pairing of another kind has its ID. Use [Store.UpdateNode]
// to change a node that was read.
func (s *Store) PutNode(ctx context.Context, n Node) error {
	headers, err := json.Marshal(n.Headers)
	if err != nil {
		return err
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if err := s.putWebhookPairing(ctx, tx, n.ID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO nodes (id, name, description, webhook, headers, secret, token_hash, created)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (id) DO UPDATE SET name = excluded.name, description = excluded.description,
			webhook = excluded.webhook, headers = excluded.headers, secret = excluded.secret, token_hash = excluded.token_hash`,
			n.ID, n.Name, n.Description, n.Webhook, string(headers), n.Secret, n.TokenHash, s.now().UnixNano())
		return err
	})
}

// ErrChanged is returned when a record changed since the caller read it.
var ErrChanged = errors.New("the record changed since it was read")

// UpdateNode replaces the node prev with next, as long as prev is still what
// is stored. It returns [ErrNotFound] if the node is gone and [ErrChanged]
// if another update was faster.
func (s *Store) UpdateNode(ctx context.Context, prev, next Node) error {
	oldHeaders, err := json.Marshal(prev.Headers)
	if err != nil {
		return err
	}
	headers, err := json.Marshal(next.Headers)
	if err != nil {
		return err
	}
	res, err := s.exec(ctx, `UPDATE nodes SET name = ?, description = ?, webhook = ?, headers = ?, secret = ?, token_hash = ?
		WHERE id = ? AND name = ? AND description = ? AND webhook = ? AND headers = ? AND secret = ? AND token_hash = ?`,
		next.Name, next.Description, next.Webhook, string(headers), next.Secret, next.TokenHash,
		prev.ID, prev.Name, prev.Description, prev.Webhook, string(oldHeaders), prev.Secret, prev.TokenHash)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	if _, err := s.Node(ctx, prev.ID); err != nil {
		return err
	}
	return ErrChanged
}

// Node returns a node by ID.
func (s *Store) Node(ctx context.Context, id string) (Node, error) {
	n := Node{ID: id}
	var (
		headers string
		created int64
	)
	err := s.reads.QueryRowContext(ctx, `SELECT name, description, webhook, headers, secret, token_hash, created FROM nodes WHERE id = ?`, id).
		Scan(&n.Name, &n.Description, &n.Webhook, &headers, &n.Secret, &n.TokenHash, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Node{}, ErrNotFound
	}
	if err != nil {
		return Node{}, err
	}
	n.Created = unixTime(created)
	return n, json.Unmarshal([]byte(headers), &n.Headers)
}

// Nodes lists nodes.
func (s *Store) Nodes(ctx context.Context) ([]Node, error) {
	rows, err := s.reads.QueryContext(ctx, `SELECT id FROM nodes ORDER BY id`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		ids = append(ids, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	out := make([]Node, 0, len(ids))
	for _, id := range ids {
		n, err := s.Node(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

// Question is a pending agent question relayed to a channel or a web page.
// Contact is set for questions of a remote agent's task.
type Question struct {
	// ID is short, for channel button payloads.
	ID string
	// Token is long and unguessable, for /hitl/<token> pages.
	Token     string
	TaskID    string
	ContextID string
	Contact   string
	// HitlID is the ID the agent gave the question; with Contact and
	// TaskID it identifies a remote agent's question.
	HitlID string
	// User may answer the question besides owners: whoever started the task.
	User string
	// Data describes the question: its ID, type, title, options and text.
	Data       map[string]any
	Created    time.Time
	AnsweredBy string
	Answer     string
	// Closed is set once the task no longer waits for an answer.
	Closed bool
}

// PutQuestion stores a question. It returns [ErrExists] if the remote
// agent's question was stored before.
func (s *Store) PutQuestion(ctx context.Context, q Question) error {
	data, err := json.Marshal(q.Data)
	if err != nil {
		return err
	}
	res, err := s.exec(ctx, `INSERT INTO questions (id, token, task_id, context_id, contact, hitl_id, user_id, data, created)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (contact, task_id, hitl_id) WHERE contact != '' DO NOTHING`,
		q.ID, q.Token, q.TaskID, q.ContextID, q.Contact, q.HitlID, q.User, string(data), s.now().UnixNano())
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrExists
	}
	return nil
}

// QuestionByID returns a question by its short ID.
func (s *Store) QuestionByID(ctx context.Context, id string) (Question, error) {
	return s.question(ctx, `id = ?`, id)
}

// QuestionByToken returns a question by its page token.
func (s *Store) QuestionByToken(ctx context.Context, token string) (Question, error) {
	return s.question(ctx, `token = ?`, token)
}

const questionColumns = `id, token, task_id, context_id, contact, hitl_id, user_id, data, created, answered_by, answer, closed`

func scanQuestion(row interface{ Scan(...any) error }) (Question, error) {
	var (
		q       Question
		data    string
		created int64
	)
	err := row.Scan(&q.ID, &q.Token, &q.TaskID, &q.ContextID, &q.Contact, &q.HitlID, &q.User, &data, &created, &q.AnsweredBy, &q.Answer, &q.Closed)
	if errors.Is(err, sql.ErrNoRows) {
		return Question{}, ErrNotFound
	}
	if err != nil {
		return Question{}, err
	}
	q.Created = unixTime(created)
	return q, json.Unmarshal([]byte(data), &q.Data)
}

func (s *Store) question(ctx context.Context, where string, arg string) (Question, error) {
	return scanQuestion(s.reads.QueryRowContext(ctx, `SELECT `+questionColumns+` FROM questions WHERE `+where, arg))
}

// UnansweredQuestions lists the questions nobody answered yet, newest
// first. Questions of tasks which ended without an answer are among them
// until [Store.CloseQuestion] marks them closed.
func (s *Store) UnansweredQuestions(ctx context.Context) ([]Question, error) {
	rows, err := s.reads.QueryContext(ctx, `SELECT `+questionColumns+` FROM questions WHERE answered_by = '' AND NOT closed ORDER BY created DESC`)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)
	var out []Question
	for rows.Next() {
		q, err := scanQuestion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// ErrAnswered is returned when a question was already answered.
var ErrAnswered = errors.New("the question was already answered")

// AnswerQuestion records the answer once.
func (s *Store) AnswerQuestion(ctx context.Context, id, by, answer string) error {
	res, err := s.exec(ctx, `UPDATE questions SET answered_by = ?, answer = ? WHERE id = ? AND answered_by = ''`, by, answer, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	if _, err := s.QuestionByID(ctx, id); err != nil {
		return err
	}
	return ErrAnswered
}

// CloseQuestion marks a question nobody answered closed: its task no longer
// waits for an answer.
func (s *Store) CloseQuestion(ctx context.Context, id string) error {
	_, err := s.exec(ctx, `UPDATE questions SET closed = 1 WHERE id = ? AND answered_by = ''`, id)
	return err
}

// UnanswerQuestion clears an answer by, so that a question whose answer
// couldn't be delivered can be answered again.
func (s *Store) UnanswerQuestion(ctx context.Context, id, by string) error {
	_, err := s.exec(ctx, `UPDATE questions SET answered_by = '', answer = '' WHERE id = ? AND answered_by = ?`, id, by)
	return err
}

// QuestionMessage is a channel message which shows a question, kept so that
// it can be updated once the question is answered.
type QuestionMessage struct {
	QuestionID string
	Channel    string
	Conv       string
	MessageID  string
	Text       string
}

// AddQuestionMessage records a message which shows a question.
func (s *Store) AddQuestionMessage(ctx context.Context, m QuestionMessage) error {
	_, err := s.exec(ctx, `INSERT INTO question_messages (question_id, channel, conv, message_id, text) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT DO UPDATE SET text = excluded.text`, m.QuestionID, m.Channel, m.Conv, m.MessageID, m.Text)
	return err
}

// QuestionOfMessage returns the ID of the question a message in a
// conversation shows.
func (s *Store) QuestionOfMessage(ctx context.Context, channel, conv, messageID string) (string, error) {
	var id string
	err := s.reads.QueryRowContext(ctx, `SELECT question_id FROM question_messages WHERE channel = ? AND conv = ? AND message_id = ?`,
		channel, conv, messageID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return id, err
}

// TakeQuestionMessages returns and forgets the messages which show a question.
func (s *Store) TakeQuestionMessages(ctx context.Context, questionID string) ([]QuestionMessage, error) {
	done, err := s.beginWrite()
	if err != nil {
		return nil, err
	}
	defer done()
	rows, err := s.db.QueryContext(ctx, `DELETE FROM question_messages WHERE question_id = ?
		RETURNING question_id, channel, conv, message_id, text`, questionID)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)
	var out []QuestionMessage
	for rows.Next() {
		var m QuestionMessage
		if err := rows.Scan(&m.QuestionID, &m.Channel, &m.Conv, &m.MessageID, &m.Text); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// AddPendingQuestion makes a question one that typed replies in a
// conversation may answer.
func (s *Store) AddPendingQuestion(ctx context.Context, channel, conv, questionID string) error {
	_, err := s.exec(ctx, `INSERT INTO pending_questions (channel, conv, question_id) VALUES (?, ?, ?)
		ON CONFLICT DO NOTHING`, channel, conv, questionID)
	return err
}

// PendingQuestions returns the IDs of the questions pending in a
// conversation, oldest first.
func (s *Store) PendingQuestions(ctx context.Context, channel, conv string) ([]string, error) {
	return s.questionIDs(ctx, `SELECT question_id FROM pending_questions WHERE channel = ? AND conv = ? ORDER BY rowid`, channel, conv)
}

// ForgetPendingQuestion stops a question from being pending in any
// conversation.
func (s *Store) ForgetPendingQuestion(ctx context.Context, questionID string) error {
	_, err := s.exec(ctx, `DELETE FROM pending_questions WHERE question_id = ?`, questionID)
	return err
}

// UnansweredQuestionsOf returns the IDs of the open, unanswered questions
// of a task of this agent.
func (s *Store) UnansweredQuestionsOf(ctx context.Context, taskID string) ([]string, error) {
	return s.questionIDs(ctx, `SELECT id FROM questions WHERE task_id = ? AND contact = '' AND answered_by = '' AND NOT closed`, taskID)
}

// StaleQuestions returns the IDs of the unanswered questions of this
// agent's tasks which no longer wait for input, and which are still pending
// in a conversation or shown in a message.
func (s *Store) StaleQuestions(ctx context.Context) ([]string, error) {
	return s.questionIDs(ctx, `SELECT q.id FROM questions q WHERE q.contact = '' AND q.answered_by = ''
		AND (EXISTS (SELECT 1 FROM pending_questions p WHERE p.question_id = q.id)
			OR EXISTS (SELECT 1 FROM question_messages m WHERE m.question_id = q.id))
		AND NOT EXISTS (SELECT 1 FROM tasks t WHERE t.scope = '' AND t.id = q.task_id AND t.state = ?)`, string(a2a.TaskStateInputRequired))
}

func (s *Store) questionIDs(ctx context.Context, query string, args ...any) ([]string, error) {
	rows, err := s.reads.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// Handle is the short name of the agents URL of one conversation.
type Handle struct {
	Handle    string
	ContextID string
}

// PutHandle stores a handle. It returns [ErrExists] if the handle is taken
// or its context already has one.
func (s *Store) PutHandle(ctx context.Context, h Handle) error {
	res, err := s.exec(ctx, `INSERT INTO handles (handle, context_id) VALUES (?, ?) ON CONFLICT DO NOTHING`, h.Handle, h.ContextID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrExists
	}
	return nil
}

// HandleByName returns a handle.
func (s *Store) HandleByName(ctx context.Context, handle string) (Handle, error) {
	return s.handle(ctx, `handle = ?`, handle)
}

// HandleOf returns the handle of a context.
func (s *Store) HandleOf(ctx context.Context, contextID string) (Handle, error) {
	return s.handle(ctx, `context_id = ?`, contextID)
}

func (s *Store) handle(ctx context.Context, where string, args ...any) (Handle, error) {
	var h Handle
	err := s.reads.QueryRowContext(ctx, `SELECT handle, context_id FROM handles WHERE `+where, args...).Scan(&h.Handle, &h.ContextID)
	if errors.Is(err, sql.ErrNoRows) {
		return Handle{}, ErrNotFound
	}
	return h, err
}

// CallTask is a task a conversation started at another agent through its
// agents URL.
type CallTask struct {
	ContextID       string
	Agent           string
	TaskID          string
	RemoteContextID string
}

// PutCallTask records a call task; recording it again does nothing.
func (s *Store) PutCallTask(ctx context.Context, t CallTask) error {
	_, err := s.exec(ctx, `INSERT INTO call_tasks (context_id, agent, task_id, remote_context_id) VALUES (?, ?, ?, ?) ON CONFLICT DO NOTHING`,
		t.ContextID, t.Agent, t.TaskID, t.RemoteContextID)
	return err
}

// HasCallTask reports whether a conversation started a task at an agent.
func (s *Store) HasCallTask(ctx context.Context, contextID, agent, taskID string) (bool, error) {
	return s.exists(ctx, `SELECT EXISTS (SELECT 1 FROM call_tasks WHERE context_id = ? AND agent = ? AND task_id = ?)`, contextID, agent, taskID)
}

// HasCallContext reports whether a conversation started a task in a context
// of an agent.
func (s *Store) HasCallContext(ctx context.Context, contextID, agent, remoteContextID string) (bool, error) {
	return s.exists(ctx, `SELECT EXISTS (SELECT 1 FROM call_tasks WHERE context_id = ? AND agent = ? AND remote_context_id = ?)`, contextID, agent, remoteContextID)
}

func (s *Store) exists(ctx context.Context, query string, args ...any) (bool, error) {
	var found bool
	err := s.reads.QueryRowContext(ctx, query, args...).Scan(&found)
	return found, err
}

// Watch is a remote task started by a2a.send that outlived the call. When
// it finishes, its result is posted into the originating context.
type Watch struct {
	Contact         string
	TaskID          string
	RemoteContextID string
	OriginContextID string
	// OnBehalfOf is the principal the result is posted as, and Identity
	// its identity as JSON.
	OnBehalfOf string
	Identity   string
	Created    time.Time
	// FailingSince is when polling the task started failing, or zero.
	FailingSince time.Time
}

// PutWatch stores a watch.
func (s *Store) PutWatch(ctx context.Context, w Watch) error {
	_, err := s.exec(ctx, `INSERT INTO watches (contact, task_id, remote_context_id, origin_context_id, on_behalf_of, identity, created)
		VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`,
		w.Contact, w.TaskID, w.RemoteContextID, w.OriginContextID, w.OnBehalfOf, w.Identity, s.now().UnixNano())
	return err
}

// Watches lists watches.
func (s *Store) Watches(ctx context.Context) ([]Watch, error) {
	rows, err := s.reads.QueryContext(ctx, `SELECT contact, task_id, remote_context_id, origin_context_id, on_behalf_of, identity, created, failing_since FROM watches`)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)
	var out []Watch
	for rows.Next() {
		var (
			w                     Watch
			created, failingSince int64
		)
		if err := rows.Scan(&w.Contact, &w.TaskID, &w.RemoteContextID, &w.OriginContextID, &w.OnBehalfOf, &w.Identity, &created, &failingSince); err != nil {
			return nil, err
		}
		w.Created, w.FailingSince = unixTime(created), unixTime(failingSince)
		out = append(out, w)
	}
	return out, rows.Err()
}

// SetWatchFailing records since when polling a watched task fails; zero
// clears it.
func (s *Store) SetWatchFailing(ctx context.Context, contact, taskID string, since time.Time) error {
	var n int64
	if !since.IsZero() {
		n = since.UnixNano()
	}
	_, err := s.exec(ctx, `UPDATE watches SET failing_since = ? WHERE contact = ? AND task_id = ?`, n, contact, taskID)
	return err
}

// DeleteWatch removes a watch.
func (s *Store) DeleteWatch(ctx context.Context, contact, taskID string) error {
	_, err := s.exec(ctx, `DELETE FROM watches WHERE contact = ? AND task_id = ?`, contact, taskID)
	return err
}

// deleteOneTx runs a delete and returns [ErrNotFound] if it deleted nothing.
func deleteOneTx(ctx context.Context, db interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}, query string, args ...any) error {
	res, err := db.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
