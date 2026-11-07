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
	"log/slog"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/integrations/executor/a2aacp"

	"github.com/a2aproject/a2a-serve/internal/cardinfo"
)

// SessionStore implements the a2aacp session store: A2A context → ACP session.
type SessionStore struct {
	s *Store
	// workspace, if set, returns the current workspace of the agent.
	workspace func() (string, error)
}

// Sessions returns the ACP session store.
func (s *Store) Sessions() SessionStore { return SessionStore{s: s} }

// In returns the session store that records the workspace of new sessions,
// and that doesn't return a session created in another workspace. Agents
// such as opencode load such a session but fail its prompts.
func (ss SessionStore) In(workspace func() (string, error)) SessionStore {
	ss.workspace = workspace
	return ss
}

var _ a2aacp.SessionStore = SessionStore{}

// Get returns the ACP session of an A2A context.
func (ss SessionStore) Get(ctx context.Context, contextID string) (a2aacp.Session, bool, error) {
	sess, moved, ok, err := ss.get(ctx, contextID)
	if err != nil || !ok || moved {
		return a2aacp.Session{}, false, err
	}
	return sess, true, nil
}

// Moved reports whether the A2A context has an ACP session that was
// created in another workspace. A session recorded without a workspace
// counts as created in the current one.
func (ss SessionStore) Moved(ctx context.Context, contextID string) (bool, error) {
	_, moved, _, err := ss.get(ctx, contextID)
	return moved, err
}

func (ss SessionStore) get(ctx context.Context, contextID string) (sess a2aacp.Session, moved, ok bool, err error) {
	var workspace string
	err = ss.s.reads.QueryRowContext(ctx, `SELECT session_id, agent_key, workspace FROM sessions WHERE context_id = ?`, contextID).
		Scan(&sess.ID, &sess.AgentKey, &workspace)
	if errors.Is(err, sql.ErrNoRows) {
		return a2aacp.Session{}, false, false, nil
	}
	if err != nil {
		return a2aacp.Session{}, false, false, err
	}
	if ss.workspace == nil || workspace == "" {
		return sess, false, true, nil
	}
	current, err := ss.workspace()
	if err != nil {
		return a2aacp.Session{}, false, false, err
	}
	return sess, workspace != current, true, nil
}

// Put records the ACP session of an A2A context.
func (ss SessionStore) Put(ctx context.Context, contextID string, sess a2aacp.Session) error {
	workspace := ""
	if ss.workspace != nil {
		var err error
		if workspace, err = ss.workspace(); err != nil {
			return err
		}
	}
	_, err := ss.s.exec(ctx, `INSERT INTO sessions (context_id, session_id, agent_key, workspace) VALUES (?, ?, ?, ?)
		ON CONFLICT (context_id) DO UPDATE SET session_id = excluded.session_id, agent_key = excluded.agent_key, workspace = excluded.workspace`,
		contextID, sess.ID, sess.AgentKey, workspace)
	return err
}

// ContextOf returns the A2A context of a messenger conversation.
func (s *Store) ContextOf(ctx context.Context, messenger, conv string) (string, error) {
	var id string
	err := s.reads.QueryRowContext(ctx, `SELECT context_id FROM convs WHERE messenger = ? AND conv = ?`, messenger, conv).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return id, err
}

// SetContext binds a messenger conversation to an A2A context.
func (s *Store) SetContext(ctx context.Context, messenger, conv, contextID string) error {
	_, err := s.exec(ctx, `INSERT INTO convs (messenger, conv, context_id, updated) VALUES (?, ?, ?, ?)
		ON CONFLICT (messenger, conv) DO UPDATE SET context_id = excluded.context_id, updated = excluded.updated`,
		messenger, conv, contextID, s.now().UnixNano())
	return err
}

// ConvOf returns the messenger conversation bound to an A2A context.
func (s *Store) ConvOf(ctx context.Context, contextID string) (messenger, conv string, err error) {
	err = s.reads.QueryRowContext(ctx, `SELECT messenger, conv FROM convs WHERE context_id = ? ORDER BY updated DESC LIMIT 1`,
		contextID).Scan(&messenger, &conv)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrNotFound
	}
	return messenger, conv, err
}

// Contact is another agent this agent may call.
type Contact struct {
	Name string
	URL  string
	// Token is the outbound a2p_ token, empty for contacts reached over the
	// private network, which recognize the caller by its network identity.
	Token string
	// Via is private (the network provider's HTTP client) or direct.
	Via string
	// Card is what the contact's agent card said when it was last fetched.
	Card    cardinfo.Summary
	Created time.Time
}

// PutContact creates or replaces a contact.
func (s *Store) PutContact(ctx context.Context, c Contact) error {
	card, err := marshalCard(c.Card)
	if err != nil {
		return err
	}
	_, err = s.exec(ctx, `INSERT INTO contacts (name, url, token, via, card, created) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (name) DO UPDATE SET url = excluded.url, token = excluded.token, via = excluded.via, card = excluded.card`,
		c.Name, c.URL, c.Token, c.Via, card, s.now().UnixNano())
	return err
}

// SetContactCard replaces the card of the contact name while it is still
// reached at url. It returns [ErrNotFound] once the contact was removed or
// moved to another URL.
func (s *Store) SetContactCard(ctx context.Context, name, url string, card cardinfo.Summary) error {
	data, err := marshalCard(card)
	if err != nil {
		return err
	}
	res, err := s.exec(ctx, `UPDATE contacts SET card = ? WHERE name = ? AND url = ?`, data, name, url)
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

func marshalCard(card cardinfo.Summary) (string, error) {
	if card.IsZero() {
		return "", nil
	}
	data, err := json.Marshal(card)
	return string(data), err
}

func unmarshalCard(data string) cardinfo.Summary {
	var card cardinfo.Summary
	if data == "" {
		return card
	}
	if err := json.Unmarshal([]byte(data), &card); err != nil {
		slog.Warn("reading the stored card of a contact", "error", err)
	}
	return card
}

// Contact returns a contact by name.
func (s *Store) Contact(ctx context.Context, name string) (Contact, error) {
	c := Contact{Name: name}
	var (
		created int64
		card    string
	)
	err := s.reads.QueryRowContext(ctx, `SELECT url, token, via, card, created FROM contacts WHERE name = ?`, name).
		Scan(&c.URL, &c.Token, &c.Via, &card, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Contact{}, ErrNotFound
	}
	c.Card = unmarshalCard(card)
	c.Created = unixTime(created)
	return c, err
}

// Contacts lists contacts.
func (s *Store) Contacts(ctx context.Context) ([]Contact, error) {
	rows, err := s.reads.QueryContext(ctx, `SELECT name, url, token, via, card, created FROM contacts ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)
	var out []Contact
	for rows.Next() {
		var (
			c       Contact
			created int64
			card    string
		)
		if err := rows.Scan(&c.Name, &c.URL, &c.Token, &c.Via, &card, &created); err != nil {
			return nil, err
		}
		c.Card = unmarshalCard(card)
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

// Question is a pending agent question relayed to a messenger or a web page.
// Contact is set for questions of a remote agent's task.
type Question struct {
	// ID is short, for messenger button payloads.
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

// OpenQuestionOf returns a question record of this agent's task taskID for
// its question hitlID that isn't closed: an answered one if there is one,
// since a question can have a record for each place it was shown, and else
// the newest.
func (s *Store) OpenQuestionOf(ctx context.Context, taskID, hitlID string) (Question, error) {
	return scanQuestion(s.reads.QueryRowContext(ctx, `SELECT `+questionColumns+` FROM questions
		WHERE task_id = ? AND hitl_id = ? AND contact = '' AND NOT closed ORDER BY answered_by != '' DESC, created DESC LIMIT 1`, taskID, hitlID))
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

// UnansweredRemoteQuestions counts the questions of remote agents nobody
// answered yet.
func (s *Store) UnansweredRemoteQuestions(ctx context.Context) (int, error) {
	var n int
	err := s.reads.QueryRowContext(ctx, `SELECT COUNT(*) FROM questions WHERE answered_by = '' AND contact != ''`).Scan(&n)
	return n, err
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

// QuestionMessage is a messenger message which shows a question, kept so that
// it can be updated once the question is answered.
type QuestionMessage struct {
	QuestionID string
	Messenger  string
	Conv       string
	MessageID  string
	Text       string
}

// AddQuestionMessage records a message which shows a question.
func (s *Store) AddQuestionMessage(ctx context.Context, m QuestionMessage) error {
	_, err := s.exec(ctx, `INSERT INTO question_messages (question_id, messenger, conv, message_id, text) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT DO UPDATE SET text = excluded.text`, m.QuestionID, m.Messenger, m.Conv, m.MessageID, m.Text)
	return err
}

// QuestionOfMessage returns the ID of the question a message in a
// conversation shows.
func (s *Store) QuestionOfMessage(ctx context.Context, messenger, conv, messageID string) (string, error) {
	var id string
	err := s.reads.QueryRowContext(ctx, `SELECT question_id FROM question_messages WHERE messenger = ? AND conv = ? AND message_id = ?`,
		messenger, conv, messageID).Scan(&id)
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
		RETURNING question_id, messenger, conv, message_id, text`, questionID)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)
	var out []QuestionMessage
	for rows.Next() {
		var m QuestionMessage
		if err := rows.Scan(&m.QuestionID, &m.Messenger, &m.Conv, &m.MessageID, &m.Text); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// AddPendingQuestion makes a question one that typed replies in a
// conversation may answer.
func (s *Store) AddPendingQuestion(ctx context.Context, messenger, conv, questionID string) error {
	_, err := s.exec(ctx, `INSERT INTO pending_questions (messenger, conv, question_id) VALUES (?, ?, ?)
		ON CONFLICT DO NOTHING`, messenger, conv, questionID)
	return err
}

// PendingQuestions returns the IDs of the questions pending in a
// conversation, oldest first.
func (s *Store) PendingQuestions(ctx context.Context, messenger, conv string) ([]string, error) {
	return s.questionIDs(ctx, `SELECT question_id FROM pending_questions WHERE messenger = ? AND conv = ? ORDER BY rowid`, messenger, conv)
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
	// OriginTaskID is the task of the conversation whose turn started it.
	OriginTaskID string
}

// PutCallTask records a call task; recording it again does nothing.
func (s *Store) PutCallTask(ctx context.Context, t CallTask) error {
	_, err := s.exec(ctx, `INSERT INTO call_tasks (context_id, agent, task_id, remote_context_id, origin_task_id) VALUES (?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`,
		t.ContextID, t.Agent, t.TaskID, t.RemoteContextID, t.OriginTaskID)
	return err
}

// CallTasksOf returns the call tasks that the turn of the task originTaskID
// of a conversation started.
func (s *Store) CallTasksOf(ctx context.Context, contextID, originTaskID string) ([]CallTask, error) {
	rows, err := s.reads.QueryContext(ctx, `SELECT agent, task_id, remote_context_id FROM call_tasks WHERE context_id = ? AND origin_task_id = ?
		ORDER BY rowid`, contextID, originTaskID)
	if err != nil {
		return nil, err
	}
	var out []CallTask
	for rows.Next() {
		t := CallTask{ContextID: contextID, OriginTaskID: originTaskID}
		if err := rows.Scan(&t.Agent, &t.TaskID, &t.RemoteContextID); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		out = append(out, t)
	}
	return out, errors.Join(rows.Err(), rows.Close())
}

// HasCallTask reports whether a conversation started a task at an agent.
func (s *Store) HasCallTask(ctx context.Context, contextID, agent, taskID string) (bool, error) {
	return s.exists(ctx, `SELECT EXISTS (SELECT 1 FROM call_tasks WHERE context_id = ? AND agent = ? AND task_id = ?)`, contextID, agent, taskID)
}

// CallTasks returns the IDs of the tasks a conversation started at an
// agent, newest first. A remoteContextID other than "" keeps the tasks in
// that context of the agent.
func (s *Store) CallTasks(ctx context.Context, contextID, agent, remoteContextID string) ([]string, error) {
	rows, err := s.reads.QueryContext(ctx, `SELECT task_id FROM call_tasks WHERE context_id = ? AND agent = ? AND (? = '' OR remote_context_id = ?)
		ORDER BY rowid DESC`, contextID, agent, remoteContextID, remoteContextID)
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
	return ids, errors.Join(rows.Err(), rows.Close())
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
	// Generation counts the calls that watched the task again, such as a
	// follow-up message, after the first.
	Generation int64
	// State is the last state seen of the task, and StateChanged when it
	// was first seen.
	State        string
	StateChanged time.Time
}

const watchColumns = `contact, task_id, remote_context_id, origin_context_id, on_behalf_of, identity, created, failing_since,
	generation, state, state_changed`

// PutWatch stores a watch. Watching a task again starts a new generation
// of its watch.
func (s *Store) PutWatch(ctx context.Context, w Watch) error {
	_, err := s.exec(ctx, `INSERT INTO watches (contact, task_id, remote_context_id, origin_context_id, on_behalf_of, identity, created)
		VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT (contact, task_id) DO UPDATE SET generation = generation + 1`,
		w.Contact, w.TaskID, w.RemoteContextID, w.OriginContextID, w.OnBehalfOf, w.Identity, s.now().UnixNano())
	return err
}

func scanWatch(row interface{ Scan(...any) error }) (Watch, error) {
	var (
		w                                   Watch
		created, failingSince, stateChanged int64
	)
	err := row.Scan(&w.Contact, &w.TaskID, &w.RemoteContextID, &w.OriginContextID, &w.OnBehalfOf, &w.Identity, &created, &failingSince,
		&w.Generation, &w.State, &stateChanged)
	if errors.Is(err, sql.ErrNoRows) {
		return Watch{}, ErrNotFound
	}
	w.Created, w.FailingSince, w.StateChanged = unixTime(created), unixTime(failingSince), unixTime(stateChanged)
	return w, err
}

// Watch returns the watch of a task, or [ErrNotFound].
func (s *Store) Watch(ctx context.Context, contact, taskID string) (Watch, error) {
	return scanWatch(s.reads.QueryRowContext(ctx, `SELECT `+watchColumns+` FROM watches WHERE contact = ? AND task_id = ?`, contact, taskID))
}

// Watches lists watches.
func (s *Store) Watches(ctx context.Context) ([]Watch, error) {
	rows, err := s.reads.QueryContext(ctx, `SELECT `+watchColumns+` FROM watches`)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)
	var out []Watch
	for rows.Next() {
		w, err := scanWatch(rows)
		if err != nil {
			return nil, err
		}
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

// SetWatchState records the state a watched task changed to, and when.
func (s *Store) SetWatchState(ctx context.Context, contact, taskID, state string, at time.Time) error {
	_, err := s.exec(ctx, `UPDATE watches SET state = ?, state_changed = ? WHERE contact = ? AND task_id = ?`, state, nanos(at), contact, taskID)
	return err
}

// DeleteWatch removes the watch of a task if it is still in generation,
// or returns [ErrNotFound].
func (s *Store) DeleteWatch(ctx context.Context, contact, taskID string, generation int64) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		return deleteOneTx(ctx, tx, `DELETE FROM watches WHERE contact = ? AND task_id = ? AND generation = ?`, contact, taskID, generation)
	})
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
