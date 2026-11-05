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
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
)

// Viewer identifies the caller of a task store operation. all is true for
// callers who see every task, such as owners.
type Viewer func(ctx context.Context) (user string, all bool, err error)

// TaskStore is a [taskstore.Store] for one scope: the agent itself, or one
// webhook node. Callers see the tasks they created, unless the [Viewer]
// reports that they see all tasks.
type TaskStore struct {
	s      *Store
	scope  string
	viewer Viewer
}

var _ taskstore.Store = (*TaskStore)(nil)

// Tasks returns the task store of scope.
func (s *Store) Tasks(scope string, viewer Viewer) *TaskStore {
	return &TaskStore{s: s, scope: scope, viewer: viewer}
}

// UpdateMaterializer returns the [taskstore.UpdateMaterializer] for handlers
// using ts. The store derives tasks from [taskstore.UpdateRequest.Event], so
// executions don't need to keep full tasks in memory.
func (ts *TaskStore) UpdateMaterializer() taskstore.UpdateMaterializer {
	return taskstore.NewNoOpUpdateMaterializer()
}

func statusTime(t *a2a.Task) int64 {
	if t.Status.Timestamp != nil {
		return t.Status.Timestamp.UnixNano()
	}
	return 0
}

// taskHeader is the row of a task. Its task has no history and artifacts.
type taskHeader struct {
	owner   string
	version int64
	updated int64
	task    *a2a.Task
}

const headerColumns = "owner, version, updated, data"

type scanner interface{ Scan(dest ...any) error }

func scanHeader(row scanner) (*taskHeader, error) {
	var (
		h    taskHeader
		data string
	)
	if err := row.Scan(&h.owner, &h.version, &h.updated, &data); err != nil {
		return nil, err
	}
	h.task = &a2a.Task{}
	if err := json.Unmarshal([]byte(data), h.task); err != nil {
		return nil, fmt.Errorf("decoding task: %w", err)
	}
	return &h, nil
}

func (ts *TaskStore) insertEvent(ctx context.Context, tx *sql.Tx, taskID a2a.TaskID, version int64, ev *storedEvent) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO task_events (scope, task_id, version, kind, created, data) VALUES (?, ?, ?, ?, ?, ?)`,
		ts.scope, string(taskID), version, ev.kind, ts.s.now().UnixNano(), ev.data); err != nil {
		return err
	}
	return addBlobs(ctx, tx, ts.scope, taskID, ev.blobs)
}

// Create implements [taskstore.Store]. The task becomes the first event of
// its log.
func (ts *TaskStore) Create(ctx context.Context, task *a2a.Task) (taskstore.TaskVersion, error) {
	user, _, err := ts.viewer(ctx)
	if err != nil {
		return taskstore.TaskVersionMissing, err
	}
	ev, err := encodeEvent(task)
	if err != nil {
		return taskstore.TaskVersionMissing, err
	}
	data, err := json.Marshal(header(ev.event.(*a2a.Task)))
	if err != nil {
		return taskstore.TaskVersionMissing, err
	}
	err = ts.s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO tasks (id, scope, context_id, owner, version, state, status_time, updated, data)
			VALUES (?, ?, ?, ?, 1, ?, ?, ?, ?) ON CONFLICT DO NOTHING`,
			string(task.ID), ts.scope, task.ContextID, user, string(task.Status.State), statusTime(task), ts.s.now().UnixNano(), string(data))
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return errors.Join(taskstore.ErrTaskAlreadyExists, err)
		}
		return ts.insertEvent(ctx, tx, task.ID, 1, ev)
	})
	if err != nil {
		return taskstore.TaskVersionMissing, err
	}
	return 1, nil
}

// Update implements [taskstore.Store]. It appends [taskstore.UpdateRequest.Event],
// or the task when there is no event, to the log of the task.
func (ts *TaskStore) Update(ctx context.Context, req *taskstore.UpdateRequest) (taskstore.TaskVersion, error) {
	user, all, err := ts.viewer(ctx)
	if err != nil {
		return taskstore.TaskVersionMissing, err
	}
	event := req.Event
	if event == nil {
		event = req.Task
	}
	ev, err := encodeEvent(event)
	if err != nil {
		return taskstore.TaskVersionMissing, err
	}
	var next taskstore.TaskVersion
	err = ts.s.inTx(ctx, func(tx *sql.Tx) error {
		h, err := scanHeader(tx.QueryRowContext(ctx, `SELECT `+headerColumns+` FROM tasks WHERE scope = ? AND id = ?`,
			ts.scope, string(req.Task.ID)))
		if errors.Is(err, sql.ErrNoRows) || (err == nil && !all && h.owner != user) {
			return a2a.ErrTaskNotFound
		}
		if err != nil {
			return err
		}
		if req.PrevVersion != taskstore.TaskVersionMissing && taskstore.TaskVersion(h.version) != req.PrevVersion {
			return taskstore.ErrConcurrentModification
		}
		v, err := ts.append(ctx, tx, h, ev)
		next = taskstore.TaskVersion(v)
		return err
	})
	if err != nil {
		return taskstore.TaskVersionMissing, err
	}
	return next, nil
}

// append adds ev to the log of the task of h and returns the new version.
func (ts *TaskStore) append(ctx context.Context, tx *sql.Tx, h *taskHeader, ev *storedEvent) (int64, error) {
	if state := h.task.Status.State; state.Terminal() {
		return 0, fmt.Errorf("task in a terminal state %q: %w", state, a2a.ErrUnsupportedOperation)
	}
	next, err := applyToHeader(h.task, ev.event)
	if err != nil {
		return 0, fmt.Errorf("applying %s event: %w", ev.kind, err)
	}
	data, err := json.Marshal(next)
	if err != nil {
		return 0, err
	}
	version := h.version + 1
	res, err := tx.ExecContext(ctx, `UPDATE tasks SET version = ?, state = ?, status_time = ?, updated = ?, data = ?
		WHERE scope = ? AND id = ? AND version = ?`,
		version, string(next.Status.State), statusTime(next), ts.s.now().UnixNano(), string(data),
		ts.scope, string(next.ID), h.version)
	if err != nil {
		return 0, err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return 0, errors.Join(taskstore.ErrConcurrentModification, err)
	}
	return version, ts.insertEvent(ctx, tx, next.ID, version, ev)
}

// materialize returns the full task of h, with raw parts still referring to
// blobs. Terminal tasks are folded from their events once, then read from
// their snapshot.
func (ts *TaskStore) materialize(ctx context.Context, h *taskHeader) (*a2a.Task, error) {
	terminal := h.task.Status.State.Terminal()
	if terminal {
		var data string
		err := ts.s.reads.QueryRowContext(ctx, `SELECT data FROM task_snapshots WHERE scope = ? AND task_id = ? AND version = ?`,
			ts.scope, string(h.task.ID), h.version).Scan(&data)
		if err == nil {
			task := &a2a.Task{}
			if err := json.Unmarshal([]byte(data), task); err != nil {
				return nil, fmt.Errorf("decoding snapshot of task %s: %w", h.task.ID, err)
			}
			return task, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	events, err := ts.events(ctx, h.task.ID, h.version)
	if err != nil {
		return nil, err
	}
	task, err := fold(events)
	if err != nil {
		return nil, fmt.Errorf("materializing task %s: %w", h.task.ID, err)
	}
	if terminal {
		ts.snapshot(ctx, task, h.version)
	}
	return task, nil
}

// events returns the events of a task up to version, oldest first.
func (ts *TaskStore) events(ctx context.Context, taskID a2a.TaskID, version int64) ([]a2a.Event, error) {
	rows, err := ts.s.reads.QueryContext(ctx, `SELECT kind, data FROM task_events WHERE scope = ? AND task_id = ? AND version <= ? ORDER BY version`,
		ts.scope, string(taskID), version)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)
	var events []a2a.Event
	for rows.Next() {
		var kind, data string
		if err := rows.Scan(&kind, &data); err != nil {
			return nil, err
		}
		ev, err := decodeEvent(kind, data)
		if err != nil {
			return nil, fmt.Errorf("task %s: %w", taskID, err)
		}
		events = append(events, ev)
	}
	return events, rows.Err()
}

// snapshot caches the materialized task at version. A failure only costs
// folding the events again on the next read.
func (ts *TaskStore) snapshot(ctx context.Context, task *a2a.Task, version int64) {
	data, err := json.Marshal(task)
	if err == nil {
		_, err = ts.s.exec(ctx, `INSERT INTO task_snapshots (scope, task_id, version, data) VALUES (?, ?, ?, ?)
			ON CONFLICT (scope, task_id) DO UPDATE SET version = excluded.version, data = excluded.data`,
			ts.scope, string(task.ID), version, string(data))
	}
	if err != nil {
		slog.Warn("saving a task snapshot", "task", task.ID, "error", err)
	}
}

// Get implements [taskstore.Store].
func (ts *TaskStore) Get(ctx context.Context, taskID a2a.TaskID) (*taskstore.StoredTask, error) {
	user, all, err := ts.viewer(ctx)
	if err != nil {
		return nil, err
	}
	h, err := scanHeader(ts.s.reads.QueryRowContext(ctx, `SELECT `+headerColumns+` FROM tasks WHERE scope = ? AND id = ?`,
		ts.scope, string(taskID)))
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !all && h.owner != user) {
		return nil, a2a.ErrTaskNotFound
	}
	if err != nil {
		return nil, err
	}
	task, err := ts.materialize(ctx, h)
	if err != nil {
		return nil, err
	}
	if err := ts.s.loadBlobs(ctx, task); err != nil {
		return nil, err
	}
	return &taskstore.StoredTask{Task: task, Version: taskstore.TaskVersion(h.version), User: h.owner}, nil
}

const (
	defaultPageSize   = 50
	defaultMaxHistory = 100
)

// List implements [taskstore.Store].
func (ts *TaskStore) List(ctx context.Context, req *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	user, all, err := ts.viewer(ctx)
	if err != nil {
		return nil, err
	}
	pageSize := req.PageSize
	if pageSize == 0 {
		pageSize = defaultPageSize
	}
	if pageSize < 1 || pageSize > 100 {
		return nil, fmt.Errorf("page size must be between 1 and 100 inclusive, got %d: %w", pageSize, a2a.ErrInvalidRequest)
	}

	where := []string{"scope = ?"}
	args := []any{ts.scope}
	if !all {
		where = append(where, "owner = ?")
		args = append(args, user)
	}
	if req.ContextID != "" {
		where = append(where, "context_id = ?")
		args = append(args, req.ContextID)
	}
	if req.Status != a2a.TaskStateUnspecified {
		where = append(where, "state = ?")
		args = append(args, string(req.Status))
	}
	if req.StatusTimestampAfter != nil {
		where = append(where, "status_time >= ?")
		args = append(args, req.StatusTimestampAfter.UnixNano())
	}
	filter := strings.Join(where, " AND ")

	var total int
	if err := ts.s.reads.QueryRowContext(ctx, "SELECT COUNT(*) FROM tasks WHERE "+filter, args...).Scan(&total); err != nil {
		return nil, err
	}

	pageWhere, pageArgs := filter, args
	if req.PageToken != "" {
		updated, id, err := decodePageToken(req.PageToken)
		if err != nil {
			return nil, err
		}
		pageWhere += " AND (updated < ? OR (updated = ? AND id > ?))"
		pageArgs = append(append([]any{}, args...), updated, updated, id)
	}
	rows, err := ts.s.reads.QueryContext(ctx, "SELECT "+headerColumns+" FROM tasks WHERE "+pageWhere+
		" ORDER BY updated DESC, id LIMIT ?", append(pageArgs, pageSize+1)...)
	if err != nil {
		return nil, err
	}
	var headers []*taskHeader
	for rows.Next() {
		h, err := scanHeader(rows)
		if err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		headers = append(headers, h)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}

	history := defaultMaxHistory
	if req.HistoryLength != nil {
		history = *req.HistoryLength
	}
	resp := &a2a.ListTasksResponse{Tasks: []*a2a.Task{}, TotalSize: total, PageSize: pageSize}
	if len(headers) > pageSize {
		headers = headers[:pageSize]
		last := headers[pageSize-1]
		resp.NextPageToken = encodePageToken(last.updated, string(last.task.ID))
	}
	for _, h := range headers {
		task := h.task
		if history > 0 || req.IncludeArtifacts {
			if task, err = ts.materialize(ctx, h); err != nil {
				return nil, err
			}
		}
		switch {
		case history <= 0:
			task.History = []*a2a.Message{}
		case len(task.History) > history:
			task.History = task.History[len(task.History)-history:]
		}
		if !req.IncludeArtifacts {
			task.Artifacts = nil
		}
		resp.Tasks = append(resp.Tasks, task)
	}
	if err := ts.s.loadBlobs(ctx, resp.Tasks...); err != nil {
		return nil, err
	}
	return resp, nil
}

func encodePageToken(updated int64, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(updated, 10) + "|" + id))
}

func decodePageToken(tok string) (int64, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil {
		return 0, "", fmt.Errorf("invalid page token: %w", a2a.ErrInvalidRequest)
	}
	ts, id, ok := strings.Cut(string(raw), "|")
	updated, err := strconv.ParseInt(ts, 10, 64)
	if !ok || err != nil {
		return 0, "", fmt.Errorf("invalid page token: %w", a2a.ErrInvalidRequest)
	}
	return updated, id, nil
}

// FailUnfinished marks every task of the scope which isn't in a terminal
// state as failed with reason. Executions don't survive a restart, so such
// tasks would otherwise never finish.
func (ts *TaskStore) FailUnfinished(ctx context.Context, reason string) (int, error) {
	rows, err := ts.s.reads.QueryContext(ctx, `SELECT `+headerColumns+` FROM tasks WHERE scope = ? AND state NOT IN (?, ?, ?, ?)`,
		ts.scope, string(a2a.TaskStateCompleted), string(a2a.TaskStateFailed), string(a2a.TaskStateCanceled), string(a2a.TaskStateRejected))
	if err != nil {
		return 0, err
	}
	var pending []*taskHeader
	for rows.Next() {
		h, err := scanHeader(rows)
		if err != nil {
			return 0, errors.Join(err, rows.Close())
		}
		pending = append(pending, h)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return 0, err
	}
	now := ts.s.now()
	for _, h := range pending {
		msg := a2a.NewMessageForTask(a2a.MessageRoleAgent, h.task, a2a.NewTextPart(reason))
		ev, err := encodeEvent(&a2a.TaskStatusUpdateEvent{
			TaskID:    h.task.ID,
			ContextID: h.task.ContextID,
			Status:    a2a.TaskStatus{State: a2a.TaskStateFailed, Message: msg, Timestamp: &now},
		})
		if err != nil {
			return 0, err
		}
		if err := ts.s.inTx(ctx, func(tx *sql.Tx) error {
			_, err := ts.append(ctx, tx, h, ev)
			return err
		}); err != nil {
			return 0, err
		}
	}
	return len(pending), nil
}

// LatestInContext returns the most recently updated task of a context.
func (ts *TaskStore) LatestInContext(ctx context.Context, contextID string) (*a2a.Task, error) {
	h, err := scanHeader(ts.s.reads.QueryRowContext(ctx, `SELECT `+headerColumns+` FROM tasks WHERE scope = ? AND context_id = ? ORDER BY updated DESC LIMIT 1`,
		ts.scope, contextID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	task, err := ts.materialize(ctx, h)
	if err != nil {
		return nil, err
	}
	if err := ts.s.loadBlobs(ctx, task); err != nil {
		return nil, err
	}
	return task, nil
}

// LatestStateInContext returns the ID and state of the most recently
// updated task of a context, without loading the task.
func (ts *TaskStore) LatestStateInContext(ctx context.Context, contextID string) (a2a.TaskID, a2a.TaskState, error) {
	var id, state string
	err := ts.s.reads.QueryRowContext(ctx, `SELECT id, state FROM tasks WHERE scope = ? AND context_id = ? ORDER BY updated DESC LIMIT 1`,
		ts.scope, contextID).Scan(&id, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrNotFound
	}
	if err != nil {
		return "", "", err
	}
	return a2a.TaskID(id), a2a.TaskState(state), nil
}

// HasOthersTasks reports whether a context has tasks of another owner than user.
func (ts *TaskStore) HasOthersTasks(ctx context.Context, contextID, user string) (bool, error) {
	return ts.s.exists(ctx, `SELECT EXISTS (SELECT 1 FROM tasks WHERE scope = ? AND context_id = ? AND owner != ?)`, ts.scope, contextID, user)
}

// TaskCounts returns the number of tasks of the scope per state.
func (ts *TaskStore) TaskCounts(ctx context.Context) (map[a2a.TaskState]int, error) {
	rows, err := ts.s.reads.QueryContext(ctx, `SELECT state, COUNT(*) FROM tasks WHERE scope = ? GROUP BY state`, ts.scope)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)
	out := map[a2a.TaskState]int{}
	for rows.Next() {
		var (
			state string
			n     int
		)
		if err := rows.Scan(&state, &n); err != nil {
			return nil, err
		}
		out[a2a.TaskState(state)] = n
	}
	return out, rows.Err()
}

func closeRows(rows *sql.Rows) {
	if err := rows.Close(); err != nil {
		slog.Warn("closing rows", "error", err)
	}
}

func unixTime(ns int64) time.Time {
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}
