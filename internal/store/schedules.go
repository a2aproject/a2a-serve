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

// Schedule runs a prompt as a user on a cron expression, or once at a time.
type Schedule struct {
	Name   string
	Cron   string
	At     time.Time
	TZ     string
	Prompt string
	// UserID is the principal the runs are for, and UserIdentity its
	// identity as JSON, or empty when the ID says it all.
	UserID       string
	UserIdentity string
	// DeliverChannel and DeliverUser pick the chat the runs are shown in:
	// DeliverUser's direct chat on DeliverChannel.
	DeliverChannel string
	DeliverUser    string
	// Conversation is new or same.
	Conversation string
	// ContextID is the context every run continues, for a schedule made in
	// a conversation.
	ContextID string
	// CreatedBy is the principal who made it.
	CreatedBy string
	Created   time.Time
	NextRun   time.Time
	LastRun   time.Time
	LastError string
}

const scheduleColumns = `name, cron, at, tz, prompt, user_id, user_identity, deliver_channel, deliver_user, conversation, context_id,
	created_by, created, next_run, last_run, last_error`

func nanos(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

// ErrLimit is returned for a schedule past the limit of its creator.
var ErrLimit = errors.New("too many schedules")

// AddSchedule stores a new schedule, or returns [ErrExists].
func (s *Store) AddSchedule(ctx context.Context, sc Schedule) error {
	return s.AddScheduleCapped(ctx, sc, 0)
}

// AddScheduleCapped stores a new schedule, or returns [ErrExists], or
// [ErrLimit] when its creator already made limit schedules. Zero is no
// limit.
func (s *Store) AddScheduleCapped(ctx context.Context, sc Schedule, limit int) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if limit > 0 {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM schedules WHERE created_by = ?`, sc.CreatedBy).Scan(&n); err != nil {
				return err
			}
			if n >= limit {
				return ErrLimit
			}
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO schedules (`+scheduleColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (name) DO NOTHING`,
			sc.Name, sc.Cron, nanos(sc.At), sc.TZ, sc.Prompt, sc.UserID, sc.UserIdentity, sc.DeliverChannel, sc.DeliverUser, sc.Conversation, sc.ContextID,
			sc.CreatedBy, s.now().UnixNano(), nanos(sc.NextRun), nanos(sc.LastRun), sc.LastError)
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
	})
}

func scanSchedule(row interface{ Scan(...any) error }) (Schedule, error) {
	var (
		sc                            Schedule
		at, created, nextRun, lastRun int64
	)
	err := row.Scan(&sc.Name, &sc.Cron, &at, &sc.TZ, &sc.Prompt, &sc.UserID, &sc.UserIdentity, &sc.DeliverChannel, &sc.DeliverUser, &sc.Conversation, &sc.ContextID,
		&sc.CreatedBy, &created, &nextRun, &lastRun, &sc.LastError)
	if errors.Is(err, sql.ErrNoRows) {
		return Schedule{}, ErrNotFound
	}
	sc.At, sc.Created, sc.NextRun, sc.LastRun = unixTime(at), unixTime(created), unixTime(nextRun), unixTime(lastRun)
	return sc, err
}

// Schedule returns a schedule by name.
func (s *Store) Schedule(ctx context.Context, name string) (Schedule, error) {
	return scanSchedule(s.reads.QueryRowContext(ctx, `SELECT `+scheduleColumns+` FROM schedules WHERE name = ?`, name))
}

// Schedules lists the schedules by name.
func (s *Store) Schedules(ctx context.Context) ([]Schedule, error) {
	rows, err := s.reads.QueryContext(ctx, `SELECT `+scheduleColumns+` FROM schedules ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)
	var out []Schedule
	for rows.Next() {
		sc, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

// SetScheduleNextRun sets when a schedule runs next, or never when next is
// zero.
func (s *Store) SetScheduleNextRun(ctx context.Context, name string, next time.Time) error {
	_, err := s.exec(ctx, `UPDATE schedules SET next_run = ? WHERE name = ?`, nanos(next), name)
	return err
}

// RecordScheduleResult records when a schedule ran and its error, or "",
// and leaves when it runs next as it is.
func (s *Store) RecordScheduleResult(ctx context.Context, name string, ran time.Time, runErr string) error {
	_, err := s.exec(ctx, `UPDATE schedules SET last_run = ?, last_error = ? WHERE name = ?`, nanos(ran), runErr, name)
	return err
}

// DeleteSchedule removes a schedule, or returns [ErrNotFound].
func (s *Store) DeleteSchedule(ctx context.Context, name string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		return deleteOneTx(ctx, tx, `DELETE FROM schedules WHERE name = ?`, name)
	})
}
