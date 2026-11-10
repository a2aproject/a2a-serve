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
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/a2aproject/a2a-serve/internal/a2alimit"
)

// blobKey is the part metadata key under which a stored raw part refers to
// its bytes in the blobs table.
const blobKey = "a2a-serve.blob"

// missingBlobText stands in for an attachment whose bytes are no longer stored.
const missingBlobText = "[attachment unavailable]"

// GuestID is the principal that the tasks of guests who proved nothing
// store, and the agent key that the sessions of all guest conversations
// store.
const GuestID = "anonymous"

// maxGuestAttachmentBytes is how many bytes of attachments the tasks of
// guests keep in all. Over it, the oldest finished guest tasks lose theirs.
const maxGuestAttachmentBytes = 1 << 30

type blob struct {
	hash string
	data []byte
}

// externalizer replaces the raw parts of messages, artifacts and tasks with
// references to blobs, collecting the blobs. It doesn't change its inputs.
type externalizer struct{ blobs []blob }

func (x *externalizer) part(p *a2a.Part) *a2a.Part {
	raw := p.Raw()
	if len(raw) == 0 {
		if _, ok := p.Metadata[blobKey]; !ok {
			return p
		}
		ref := *p
		ref.Metadata = maps.Clone(p.Metadata)
		delete(ref.Metadata, blobKey)
		return &ref
	}
	sum := sha256.Sum256(raw)
	h := hex.EncodeToString(sum[:])
	x.blobs = append(x.blobs, blob{hash: h, data: raw})
	ref := *p
	ref.Content = a2a.Raw{}
	ref.Metadata = maps.Clone(p.Metadata)
	if ref.Metadata == nil {
		ref.Metadata = map[string]any{}
	}
	ref.Metadata[blobKey] = h
	return &ref
}

func (x *externalizer) message(m *a2a.Message) *a2a.Message {
	if m == nil {
		return nil
	}
	parts, changed := replaceEach(m.Parts, x.part)
	if !changed {
		return m
	}
	ref := *m
	ref.Parts = parts
	return &ref
}

func (x *externalizer) artifact(a *a2a.Artifact) *a2a.Artifact {
	if a == nil {
		return nil
	}
	parts, changed := replaceEach(a.Parts, x.part)
	if !changed {
		return a
	}
	ref := *a
	ref.Parts = parts
	return &ref
}

func (x *externalizer) task(task *a2a.Task) *a2a.Task {
	out := *task
	out.Status.Message = x.message(task.Status.Message)
	out.History, _ = replaceEach(task.History, x.message)
	out.Artifacts, _ = replaceEach(task.Artifacts, x.artifact)
	return &out
}

// replaceEach maps the elements of in with f, cloning in on the first
// element f changes.
func replaceEach[T comparable](in []T, f func(T) T) ([]T, bool) {
	out, changed := in, false
	for i, v := range in {
		nv := f(v)
		if nv == v {
			continue
		}
		if !changed {
			out, changed = slices.Clone(in), true
		}
		out[i] = nv
	}
	return out, changed
}

// addBlobs stores the blobs of a task. Blobs stay referenced by the task
// until it is pruned, because its events keep referring to them.
func addBlobs(ctx context.Context, tx *sql.Tx, scope string, taskID a2a.TaskID, blobs []blob) error {
	saved := map[string]bool{}
	for _, b := range blobs {
		if saved[b.hash] {
			continue
		}
		saved[b.hash] = true
		if _, err := tx.ExecContext(ctx, `INSERT INTO blobs (hash, data) VALUES (?, ?) ON CONFLICT DO NOTHING`, b.hash, b.data); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO task_blobs (scope, task_id, hash) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`,
			scope, string(taskID), b.hash); err != nil {
			return err
		}
	}
	return nil
}

// loadBlobs puts the bytes of the blobs the raw parts of tasks refer to back
// into the parts. It must not run while rows of the store are open: an
// in-memory store has a single connection.
func (s *Store) loadBlobs(ctx context.Context, tasks ...*a2a.Task) error {
	return s.loadBlobsUpTo(ctx, 0, tasks...)
}

// loadBlobsUpTo is [Store.loadBlobs] that leaves out the blobs longer than
// limit bytes, unless limit is 0, with a note in place of each.
func (s *Store) loadBlobsUpTo(ctx context.Context, limit int, tasks ...*a2a.Task) error {
	type ref struct {
		part *a2a.Part
		task a2a.TaskID
	}
	refs := map[string][]ref{}
	for _, t := range tasks {
		collect := func(parts []*a2a.Part) {
			for _, p := range parts {
				raw, isRaw := p.Content.(a2a.Raw)
				if !isRaw || len(raw) > 0 {
					continue
				}
				if h, ok := p.Metadata[blobKey].(string); ok {
					refs[h] = append(refs[h], ref{part: p, task: t.ID})
				}
			}
		}
		if t.Status.Message != nil {
			collect(t.Status.Message.Parts)
		}
		for _, m := range t.History {
			if m != nil {
				collect(m.Parts)
			}
		}
		for _, a := range t.Artifacts {
			if a != nil {
				collect(a.Parts)
			}
		}
	}
	for h, parts := range refs {
		var (
			size int
			data []byte
		)
		err := s.reads.QueryRowContext(ctx, `SELECT length(data), CASE WHEN ? = 0 OR length(data) <= ? THEN data END FROM blobs WHERE hash = ?`,
			limit, limit, h).Scan(&size, &data)
		missing := errors.Is(err, sql.ErrNoRows)
		if err != nil && !missing {
			return fmt.Errorf("loading blob %s: %w", h, err)
		}
		for _, r := range parts {
			p := r.part
			switch {
			case missing:
				p.Content = a2a.Text(missingBlobText)
			case limit > 0 && size > limit:
				*p = *a2alimit.OmitFile(r.task, p, size)
			default:
				p.Content = a2a.Raw(data)
			}
			delete(p.Metadata, blobKey)
			if len(p.Metadata) == 0 {
				p.Metadata = nil
			}
		}
	}
	return nil
}

// guestTask is true for the tasks of guests, aliased t: the ones that
// guests who proved nothing created, and the ones of conversations that run
// in the agent process of guests.
const guestTask = `(t.owner = '` + GuestID + `' OR t.context_id IN (SELECT context_id FROM sessions WHERE agent_key = '` + GuestID + `'))`

// finishedGuestTask reports whether the task is a guest task with
// attachments that is in a terminal state.
func finishedGuestTask(ctx context.Context, tx *sql.Tx, scope string, taskID a2a.TaskID) (bool, error) {
	var ok bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM tasks t WHERE t.scope = ? AND t.id = ? AND t.state IN (?, ?, ?, ?) AND `+guestTask+`
		AND EXISTS (SELECT 1 FROM task_blobs WHERE scope = t.scope AND task_id = t.id))`,
		append([]any{scope, string(taskID)}, terminalStates()...)...).Scan(&ok)
	return ok, err
}

// trimGuestAttachments is [trimGuestBlobs] in a transaction of its own, for
// after a guest task finished. A failure leaves it to [Store.Prune].
func (s *Store) trimGuestAttachments(ctx context.Context) {
	err := s.inTx(ctx, func(tx *sql.Tx) error { return trimGuestBlobs(ctx, tx, s.guestAttachmentCap) })
	if err != nil {
		slog.Warn("trimming the attachments of guest tasks", "error", err)
	}
}

// trimGuestBlobs deletes the attachments of the oldest finished guest tasks
// until the blobs that only guest tasks refer to take up to limit bytes.
func trimGuestBlobs(ctx context.Context, tx *sql.Tx, limit int64) error {
	var size int64
	if err := tx.QueryRowContext(ctx, `WITH guest AS (SELECT t.scope, t.id FROM tasks t WHERE `+guestTask+`)
		SELECT coalesce(sum(length(b.data)), 0) FROM blobs b
		WHERE b.hash IN (SELECT hash FROM task_blobs WHERE (scope, task_id) IN (SELECT scope, id FROM guest))
		AND NOT EXISTS (SELECT 1 FROM task_blobs o WHERE o.hash = b.hash AND (o.scope, o.task_id) NOT IN (SELECT scope, id FROM guest))`).Scan(&size); err != nil {
		return err
	}
	if size <= limit {
		return nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT t.scope, t.id FROM tasks t WHERE `+guestTask+` AND t.state IN (?, ?, ?, ?)
		AND EXISTS (SELECT 1 FROM task_blobs WHERE scope = t.scope AND task_id = t.id)
		ORDER BY t.updated, t.rowid`, terminalStates()...)
	if err != nil {
		return err
	}
	type task struct{ scope, id string }
	var tasks []task
	for rows.Next() {
		var t task
		if err := rows.Scan(&t.scope, &t.id); err != nil {
			return errors.Join(err, rows.Close())
		}
		tasks = append(tasks, t)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, t := range tasks {
		if size <= limit {
			return nil
		}
		freed, err := dropTaskBlobs(ctx, tx, t.scope, t.id)
		if err != nil {
			return err
		}
		size -= freed
	}
	return nil
}

// dropTaskBlobs deletes the references of a task to its blobs, and the blobs
// no other task refers to, and returns their size. The task shows
// missingBlobText for each blob that is gone.
func dropTaskBlobs(ctx context.Context, tx *sql.Tx, scope, taskID string) (int64, error) {
	rows, err := tx.QueryContext(ctx, `DELETE FROM task_blobs WHERE scope = ? AND task_id = ? RETURNING hash`, scope, taskID)
	if err != nil {
		return 0, err
	}
	var hashes []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return 0, errors.Join(err, rows.Close())
		}
		hashes = append(hashes, h)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return 0, err
	}
	var freed int64
	for _, h := range hashes {
		var size int64
		err := tx.QueryRowContext(ctx, `DELETE FROM blobs WHERE hash = ? AND NOT EXISTS (SELECT 1 FROM task_blobs WHERE hash = ?)
			RETURNING length(data)`, h, h).Scan(&size)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return 0, err
		}
		freed += size
	}
	return freed, nil
}
