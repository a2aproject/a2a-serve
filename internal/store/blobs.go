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
