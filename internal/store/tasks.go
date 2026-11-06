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
	if err := ts.insertEvent(ctx, tx, next.ID, version, ev); err != nil {
		return 0, err
	}
	state := next.Status.State
	if version%snapshotEvery != 0 && state != a2a.TaskStateInputRequired && !state.Terminal() {
		return version, nil
	}
	return version, ts.snapshotInTx(ctx, tx, &taskHeader{owner: h.owner, version: version, updated: h.updated, task: next})
}

// snapshotEvery is how many events a task may have after its snapshot.
const snapshotEvery = 100

// snapshotInTx stores the task of h, folded from its snapshot and the events
// after it, as its snapshot at the version of h. A task which can't be folded
// keeps its events only, so that the update which led here still succeeds.
func (ts *TaskStore) snapshotInTx(ctx context.Context, tx *sql.Tx, h *taskHeader) error {
	logs, err := ts.logs(ctx, tx, []*taskHeader{h})
	if err != nil {
		return err
	}
	task, err := logs[0].fold()
	if err != nil {
		slog.Warn("folding a task for its snapshot", "task", h.task.ID, "error", err)
		return nil
	}
	return ts.putSnapshot(ctx, tx, task, h.version)
}

// putSnapshot stores task as its snapshot at version. A terminal task never
// changes again, so its snapshot replaces its events.
func (ts *TaskStore) putSnapshot(ctx context.Context, tx *sql.Tx, task *a2a.Task, version int64) error {
	data, err := json.Marshal(task)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO task_snapshots (scope, task_id, version, data) VALUES (?, ?, ?, ?)
		ON CONFLICT (scope, task_id) DO UPDATE SET version = excluded.version, data = excluded.data`,
		ts.scope, string(task.ID), version, string(data)); err != nil {
		return err
	}
	if !task.Status.State.Terminal() {
		return nil
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM task_events WHERE scope = ? AND task_id = ? AND version <= ?`,
		ts.scope, string(task.ID), version)
	return err
}

// materialize returns the full tasks of logs, with raw parts still referring
// to blobs. A terminal task without a snapshot at its version, as older
// versions of a2a-serve left them, is folded from its events once and then
// read from its snapshot. It writes, so it must not run in a read transaction.
func (ts *TaskStore) materialize(ctx context.Context, logs []*taskLog) ([]*a2a.Task, error) {
	tasks := make([]*a2a.Task, len(logs))
	for i, l := range logs {
		var err error
		if tasks[i], err = l.fold(); err != nil {
			return nil, fmt.Errorf("materializing task %s: %w", l.header.task.ID, err)
		}
		if l.header.task.Status.State.Terminal() && l.snapshotVersion != l.header.version {
			ts.snapshot(ctx, tasks[i], l.header.version)
		}
	}
	return tasks, nil
}

// snapshot stores the snapshot of a terminal task read from its events. A
// failure only costs folding the events again on the next read.
func (ts *TaskStore) snapshot(ctx context.Context, task *a2a.Task, version int64) {
	if err := ts.s.inTx(ctx, func(tx *sql.Tx) error { return ts.putSnapshot(ctx, tx, task, version) }); err != nil {
		slog.Warn("saving a task snapshot", "task", task.ID, "error", err)
	}
}

// taskLog is what a task is folded from: its snapshot, if it has one, and
// the events after it up to the version of its header.
type taskLog struct {
	header          *taskHeader
	snapshot        *a2a.Task
	snapshotVersion int64
	events          []a2a.Event
}

func (l *taskLog) fold() (*a2a.Task, error) { return fold(l.snapshot, l.events) }

// querier runs queries on the read pool or in a transaction.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// logs reads the logs of the tasks of headers, with one query for their
// snapshots and one for their events. A terminal task loses its events, so
// readers read the headers and logs in one transaction. CROSS JOIN makes SQLite look up the
// rows of each task, which it otherwise may find by scanning all events.
func (ts *TaskStore) logs(ctx context.Context, q querier, headers []*taskHeader) ([]*taskLog, error) {
	if len(headers) == 0 {
		return nil, nil
	}
	logs := make([]*taskLog, len(headers))
	byID := make(map[string]*taskLog, len(headers))
	args := make([]any, 0, 3*len(headers)+1)
	for i, h := range headers {
		logs[i] = &taskLog{header: h}
		byID[string(h.task.ID)] = logs[i]
		args = append(args, string(h.task.ID), h.version)
	}
	err := queryEach(ctx, q, pageValues(len(headers), 2)+`SELECT s.task_id, s.version, s.data FROM page
		CROSS JOIN task_snapshots s ON s.scope = ? AND s.task_id = page.id AND s.version <= page.v1`,
		append(args, ts.scope), func(rows *sql.Rows) error {
			var (
				id, data string
				version  int64
			)
			if err := rows.Scan(&id, &version, &data); err != nil {
				return err
			}
			task := &a2a.Task{}
			if err := json.Unmarshal([]byte(data), task); err != nil {
				return fmt.Errorf("decoding snapshot of task %s: %w", id, err)
			}
			byID[id].snapshot, byID[id].snapshotVersion = task, version
			return nil
		})
	if err != nil {
		return nil, err
	}
	args = args[:0]
	for _, l := range logs {
		args = append(args, string(l.header.task.ID), l.snapshotVersion, l.header.version)
	}
	err = queryEach(ctx, q, pageValues(len(headers), 3)+`SELECT e.task_id, e.kind, e.data FROM page
		CROSS JOIN task_events e ON e.scope = ? AND e.task_id = page.id AND e.version > page.v1 AND e.version <= page.v2
		ORDER BY e.task_id, e.version`,
		append(args, ts.scope), func(rows *sql.Rows) error {
			var id, kind, data string
			if err := rows.Scan(&id, &kind, &data); err != nil {
				return err
			}
			ev, err := decodeEvent(kind, data)
			if err != nil {
				return fmt.Errorf("task %s: %w", id, err)
			}
			byID[id].events = append(byID[id].events, ev)
			return nil
		})
	if err != nil {
		return nil, err
	}
	return logs, nil
}

// pageValues starts a query with the table page of n rows of an id and
// columns-1 numbers v1, v2 and so on, bound as parameters.
func pageValues(n, columns int) string {
	names := []string{"id"}
	for i := 1; i < columns; i++ {
		names = append(names, "v"+strconv.Itoa(i))
	}
	row := "(" + strings.TrimSuffix(strings.Repeat("?, ", columns), ", ") + ")"
	return "WITH page (" + strings.Join(names, ", ") + ") AS (VALUES " + strings.TrimSuffix(strings.Repeat(row+", ", n), ", ") + ") "
}

// queryEach runs query and calls fn for each row.
func queryEach(ctx context.Context, q querier, query string, args []any, fn func(*sql.Rows) error) error {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	for rows.Next() {
		if err := fn(rows); err != nil {
			return errors.Join(err, rows.Close())
		}
	}
	return errors.Join(rows.Err(), rows.Close())
}

// Get implements [taskstore.Store].
func (ts *TaskStore) Get(ctx context.Context, taskID a2a.TaskID) (*taskstore.StoredTask, error) {
	user, all, err := ts.viewer(ctx)
	if err != nil {
		return nil, err
	}
	var logs []*taskLog
	err = ts.s.readTx(ctx, func(tx *sql.Tx) error {
		h, err := scanHeader(tx.QueryRowContext(ctx, `SELECT `+headerColumns+` FROM tasks WHERE scope = ? AND id = ?`,
			ts.scope, string(taskID)))
		if errors.Is(err, sql.ErrNoRows) || (err == nil && !all && h.owner != user) {
			return a2a.ErrTaskNotFound
		}
		if err != nil {
			return err
		}
		logs, err = ts.logs(ctx, tx, []*taskHeader{h})
		return err
	})
	if err != nil {
		return nil, err
	}
	h := logs[0].header
	tasks, err := ts.materialize(ctx, logs)
	if err != nil {
		return nil, err
	}
	task := tasks[0]
	if err := ts.s.loadBlobs(ctx, task); err != nil {
		return nil, err
	}
	return &taskstore.StoredTask{Task: task, Version: taskstore.TaskVersion(h.version), User: h.owner}, nil
}

const defaultPageSize = 50

// List implements [taskstore.Store].
func (ts *TaskStore) List(ctx context.Context, req *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	resp, _, err := ts.ListWithOwners(ctx, req)
	return resp, err
}

// ListWithOwners is [TaskStore.List] that also returns the owner of each
// listed task, in the order of the tasks.
func (ts *TaskStore) ListWithOwners(ctx context.Context, req *a2a.ListTasksRequest) (*a2a.ListTasksResponse, []string, error) {
	user, all, err := ts.viewer(ctx)
	if err != nil {
		return nil, nil, err
	}
	pageSize := req.PageSize
	if pageSize == 0 {
		pageSize = defaultPageSize
	}
	if pageSize < 1 || pageSize > 100 {
		return nil, nil, fmt.Errorf("page size must be between 1 and 100 inclusive, got %d: %w", pageSize, a2a.ErrInvalidRequest)
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

	// Lists carry history only when asked, so that large messages don't
	// make every page large.
	history := 0
	if req.HistoryLength != nil {
		history = *req.HistoryLength
	}
	var (
		total   int
		more    bool
		headers []*taskHeader
		logs    []*taskLog
	)
	err = ts.s.readTx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM tasks WHERE "+filter, args...).Scan(&total); err != nil {
			return err
		}
		pageWhere, pageArgs := filter, args
		if req.PageToken != "" {
			updated, id, err := decodePageToken(req.PageToken)
			if err != nil {
				return err
			}
			pageWhere += " AND (updated < ? OR (updated = ? AND id > ?))"
			pageArgs = append(append([]any{}, args...), updated, updated, id)
		}
		err := queryEach(ctx, tx, "SELECT "+headerColumns+" FROM tasks WHERE "+pageWhere+" ORDER BY updated DESC, id LIMIT ?",
			append(pageArgs, pageSize+1), func(rows *sql.Rows) error {
				h, err := scanHeader(rows)
				headers = append(headers, h)
				return err
			})
		if err != nil {
			return err
		}
		if more = len(headers) > pageSize; more {
			headers = headers[:pageSize]
		}
		if history > 0 || req.IncludeArtifacts {
			logs, err = ts.logs(ctx, tx, headers)
		}
		return err
	})
	if err != nil {
		return nil, nil, err
	}

	resp := &a2a.ListTasksResponse{Tasks: []*a2a.Task{}, TotalSize: total, PageSize: pageSize}
	if more {
		last := headers[pageSize-1]
		resp.NextPageToken = encodePageToken(last.updated, string(last.task.ID))
	}
	var full []*a2a.Task
	if logs != nil {
		if full, err = ts.materialize(ctx, logs); err != nil {
			return nil, nil, err
		}
	}
	owners := make([]string, 0, len(headers))
	for i, h := range headers {
		task := h.task
		if full != nil {
			task = full[i]
		}
		owners = append(owners, h.owner)
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
		return nil, nil, err
	}
	return resp, owners, nil
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
// state as failed with reason, except the ones keep keeps. Executions don't
// survive a restart, so such tasks would otherwise never finish.
func (ts *TaskStore) FailUnfinished(ctx context.Context, reason string, keep func(*a2a.Task) bool) (int, error) {
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
		if keep != nil && keep(h.task) {
			continue
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

// UnfinishedOf returns the IDs of the tasks of the scope that user owns and
// that aren't in a terminal state.
func (ts *TaskStore) UnfinishedOf(ctx context.Context, user string) ([]a2a.TaskID, error) {
	rows, err := ts.s.reads.QueryContext(ctx, `SELECT id FROM tasks WHERE scope = ? AND owner = ? AND state NOT IN (?, ?, ?, ?)`,
		ts.scope, user, string(a2a.TaskStateCompleted), string(a2a.TaskStateFailed), string(a2a.TaskStateCanceled), string(a2a.TaskStateRejected))
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)
	var out []a2a.TaskID
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, a2a.TaskID(id))
	}
	return out, rows.Err()
}

// LatestInContext returns the most recently updated task of a context.
func (ts *TaskStore) LatestInContext(ctx context.Context, contextID string) (*a2a.Task, error) {
	var logs []*taskLog
	err := ts.s.readTx(ctx, func(tx *sql.Tx) error {
		h, err := scanHeader(tx.QueryRowContext(ctx, `SELECT `+headerColumns+` FROM tasks WHERE scope = ? AND context_id = ? ORDER BY updated DESC LIMIT 1`,
			ts.scope, contextID))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		logs, err = ts.logs(ctx, tx, []*taskHeader{h})
		return err
	})
	if err != nil {
		return nil, err
	}
	tasks, err := ts.materialize(ctx, logs)
	if err != nil {
		return nil, err
	}
	if err := ts.s.loadBlobs(ctx, tasks[0]); err != nil {
		return nil, err
	}
	return tasks[0], nil
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

// ContextStarter returns the owner of the oldest stored task of a context,
// or [ErrNotFound] if the context has no tasks.
func (ts *TaskStore) ContextStarter(ctx context.Context, contextID string) (string, error) {
	var owner string
	err := ts.s.reads.QueryRowContext(ctx, `SELECT owner FROM tasks WHERE scope = ? AND context_id = ? ORDER BY rowid LIMIT 1`,
		ts.scope, contextID).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return owner, err
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
