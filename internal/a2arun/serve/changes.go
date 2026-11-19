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

package serve

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"iter"
	"log/slog"
	"mime"
	"net/http"
	"path"
	"slices"
	"sync"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/a2aproject/a2a-serve/core/fsys"
)

// ChangesArtifact is the name of the artifact with the files that the
// agent changed in a turn of a turn-synced workspace.
const ChangesArtifact = "a2a-run/changes"

// Reasons of the changes artifact for a file whose content it leaves out.
const (
	omittedFileTooLarge  = "file-too-large"
	omittedTotalTooLarge = "total-too-large"
	omittedUnreadable    = "unreadable"
)

// changesLimits bound a changes artifact, which docs/reference/limits.md
// lists.
type changesLimits struct {
	// FileBytes and TotalBytes bound the content of a file and of all
	// files; Listed bounds the paths the artifact names.
	FileBytes  int64
	TotalBytes int64
	Listed     int
}

var defaultChangesLimits = changesLimits{FileBytes: 1 << 20, TotalBytes: 4 << 20, Listed: 1000}

// maxPendingChanges bounds the artifacts that wait for the end of their
// task, which a task canceled before its turn ended never takes.
const maxPendingChanges = 1024

// taskKey is the context key of the ID of the task whose execution the
// context belongs to.
type taskKey struct{}

// changesets turn what the sync after a turn wrote to the workspace into
// an artifact of the turn's task, which goes out before the task's final
// event.
type changesets struct {
	tree   fsys.FS
	limits changesLimits
	log    *slog.Logger

	mu      sync.Mutex
	pending map[a2a.TaskID]*a2a.Artifact
	// order are the tasks of pending, oldest first.
	order []a2a.TaskID
}

func newChangesets(tree fsys.FS, limits changesLimits, log *slog.Logger) *changesets {
	return &changesets{tree: tree, limits: limits, log: log, pending: map[a2a.TaskID]*a2a.Artifact{}}
}

// synced keeps the artifact of changes for the task of ctx.
func (c *changesets) synced(ctx context.Context, _ string, changes []fsys.Info) {
	id, ok := ctx.Value(taskKey{}).(a2a.TaskID)
	if !ok {
		return
	}
	art := c.artifact(ctx, changes)
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.pending[id]; !ok {
		c.order = append(c.order, id)
	}
	c.pending[id] = art
	for len(c.order) > maxPendingChanges {
		delete(c.pending, c.order[0])
		c.order = c.order[1:]
	}
}

// take returns and forgets the artifact of the task id, or nil.
func (c *changesets) take(id a2a.TaskID) *a2a.Artifact {
	c.mu.Lock()
	defer c.mu.Unlock()
	art, ok := c.pending[id]
	if !ok {
		return nil
	}
	delete(c.pending, id)
	c.order = slices.DeleteFunc(c.order, func(o a2a.TaskID) bool { return o == id })
	return art
}

// artifact returns the artifact of changes: a data part that lists what
// the files parts don't carry, then a file part for each changed regular
// file within the limits.
func (c *changesets) artifact(ctx context.Context, changes []fsys.Info) *a2a.Artifact {
	removed, symlinks, omitted := []any{}, []any{}, []any{}
	var files []*a2a.Part
	var total int64
	listed, unlisted := 0, 0
	for _, ch := range changes {
		if !ch.Removed && !ch.Mode.IsRegular() && ch.Mode&fs.ModeSymlink == 0 {
			continue
		}
		if listed >= c.limits.Listed {
			unlisted++
			continue
		}
		listed++
		switch {
		case ch.Removed:
			removed = append(removed, ch.Path)
		case ch.Mode&fs.ModeSymlink != 0:
			symlinks = append(symlinks, map[string]any{"path": ch.Path, "target": ch.Target})
		case ch.Size > c.limits.FileBytes:
			omitted = append(omitted, omission(ch, omittedFileTooLarge))
		case total+ch.Size > c.limits.TotalBytes:
			omitted = append(omitted, omission(ch, omittedTotalTooLarge))
		default:
			data, err := c.read(ctx, ch.Path)
			switch {
			case err != nil:
				c.log.Warn("reading a changed file for the changes artifact", "path", ch.Path, "error", err)
				omitted = append(omitted, omission(ch, omittedUnreadable))
			case int64(len(data)) > c.limits.FileBytes:
				omitted = append(omitted, omission(ch, omittedFileTooLarge))
			case total+int64(len(data)) > c.limits.TotalBytes:
				omitted = append(omitted, omission(ch, omittedTotalTooLarge))
			default:
				total += int64(len(data))
				files = append(files, filePart(ch, data))
			}
		}
	}
	summary := map[string]any{"removed": removed}
	if len(symlinks) > 0 {
		summary["symlinks"] = symlinks
	}
	if len(omitted) > 0 {
		summary["omitted"] = omitted
	}
	if unlisted > 0 {
		summary["unlisted"] = unlisted
	}
	return &a2a.Artifact{
		ID:          a2a.NewArtifactID(),
		Name:        ChangesArtifact,
		Description: "The files the agent changed in this turn",
		Parts:       append([]*a2a.Part{a2a.NewDataPart(summary)}, files...),
	}
}

// read reads the file p of the tree, up to one byte past the file limit.
func (c *changesets) read(ctx context.Context, p string) (_ []byte, err error) {
	rc, err := c.tree.Open(ctx, p)
	if err != nil {
		return nil, err
	}
	defer func() {
		if cerr := rc.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()
	return io.ReadAll(io.LimitReader(rc, c.limits.FileBytes+1))
}

func omission(ch fsys.Info, reason string) map[string]any {
	return map[string]any{"path": ch.Path, "size": ch.Size, "reason": reason}
}

func filePart(ch fsys.Info, data []byte) *a2a.Part {
	mediaType := mime.TypeByExtension(path.Ext(ch.Path))
	if mediaType == "" {
		mediaType = http.DetectContentType(data)
	}
	return &a2a.Part{
		Content:   a2a.Raw(data),
		Filename:  ch.Path,
		MediaType: mediaType,
		Metadata:  map[string]any{"mode": fmt.Sprintf("%04o", ch.Mode.Perm())},
	}
}

// middleware returns next with the changes artifact of each turn sent
// before the final event of its task.
func (c *changesets) middleware(next a2asrv.AgentExecutor) a2asrv.AgentExecutor {
	return &changesExecutor{next: next, c: c}
}

type changesExecutor struct {
	next a2asrv.AgentExecutor
	c    *changesets
}

var _ a2asrv.AgentExecutionCleaner = (*changesExecutor)(nil)

func (e *changesExecutor) Execute(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	ctx = context.WithValue(ctx, taskKey{}, execCtx.TaskID)
	return func(yield func(a2a.Event, error) bool) {
		for ev, err := range e.next.Execute(ctx, execCtx) {
			if err == nil && final(ev) {
				if art := e.c.take(execCtx.TaskID); art != nil {
					up := &a2a.TaskArtifactUpdateEvent{TaskID: execCtx.TaskID, ContextID: execCtx.ContextID, Artifact: art, LastChunk: true}
					if !yield(up, nil) {
						return
					}
				}
			}
			if !yield(ev, err) {
				return
			}
		}
	}
}

func (e *changesExecutor) Cancel(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return e.next.Cancel(ctx, execCtx)
}

func (e *changesExecutor) Cleanup(ctx context.Context, execCtx *a2asrv.ExecutorContext, result a2a.SendMessageResult, err error) {
	if c, ok := e.next.(a2asrv.AgentExecutionCleaner); ok {
		c.Cleanup(ctx, execCtx, result, err)
	}
}

// final reports whether ev ends its task.
func final(ev a2a.Event) bool {
	up, ok := ev.(*a2a.TaskStatusUpdateEvent)
	return ok && up.Status.State.Terminal()
}
