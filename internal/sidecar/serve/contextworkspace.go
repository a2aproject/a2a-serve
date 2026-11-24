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
	"io"
	"io/fs"
	"net/http"
	"sync"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/a2aproject/a2a-serve/core/a2aserve"
	"github.com/a2aproject/a2a-serve/core/egress"
	"github.com/a2aproject/a2a-serve/core/fsys"
)

// workspaceContextKey is the context key of the A2A context that a request
// to the workspace is for.
type workspaceContextKey struct{}

// withWorkspaceContext returns ctx, whose requests to the workspace are for
// the A2A context contextID.
func withWorkspaceContext(ctx context.Context, contextID string) context.Context {
	return context.WithValue(ctx, workspaceContextKey{}, contextID)
}

// contextHeader returns the headers that tell the file server of a URL
// workspace the A2A context and the task of the request of ctx, or none for
// a request of no context, such as the check at start.
func contextHeader(ctx context.Context) http.Header {
	contextID, ok := ctx.Value(workspaceContextKey{}).(string)
	if !ok || contextID == "" {
		return nil
	}
	h := http.Header{}
	h.Set(egress.ProxyHeaderContext, contextID)
	if task, ok := ctx.Value(taskKey{}).(a2a.TaskID); ok && task != "" {
		h.Set(egress.ProxyHeaderTask, string(task))
	}
	return h
}

// contextTree is tree as the process of one A2A context sees it: each of
// its requests is for that context.
type contextTree struct {
	tree      fsys.FS
	contextID string
}

var _ fsys.FS = contextTree{}

func (t contextTree) ctx(ctx context.Context) context.Context {
	return withWorkspaceContext(ctx, t.contextID)
}

func (t contextTree) Stat(ctx context.Context, p string) (fsys.Info, error) {
	return t.tree.Stat(t.ctx(ctx), p)
}

func (t contextTree) ReadDir(ctx context.Context, p string) ([]fsys.Info, error) {
	return t.tree.ReadDir(t.ctx(ctx), p)
}

func (t contextTree) Open(ctx context.Context, p string) (io.ReadCloser, error) {
	return t.tree.Open(t.ctx(ctx), p)
}

func (t contextTree) Write(ctx context.Context, p string, r io.Reader, mode fs.FileMode, ifVersion string) (fsys.Info, error) {
	return t.tree.Write(t.ctx(ctx), p, r, mode, ifVersion)
}

func (t contextTree) Mkdir(ctx context.Context, p string, mode fs.FileMode) error {
	return t.tree.Mkdir(t.ctx(ctx), p, mode)
}

func (t contextTree) Symlink(ctx context.Context, target, p string) error {
	return t.tree.Symlink(t.ctx(ctx), target, p)
}

func (t contextTree) Rename(ctx context.Context, from, to string) error {
	return t.tree.Rename(t.ctx(ctx), from, to)
}

func (t contextTree) Remove(ctx context.Context, p string) error {
	return t.tree.Remove(t.ctx(ctx), p)
}

func (t contextTree) Changes(ctx context.Context, cursor string) ([]fsys.Info, string, error) {
	return t.tree.Changes(t.ctx(ctx), cursor)
}

// contextWorkspaces is the workspace with turn sync that gives the process
// of each A2A context a private directory, kept in step with tree as that
// context sees it, so that a file server can keep a tree for each context.
type contextWorkspaces struct {
	tree fsys.FS
	opts a2aserve.TurnSyncOptions

	mu sync.Mutex
	// attached are the workspaces of the contexts with an attachment, or
	// one that waits for the release of the attachment before it.
	attached map[string]*contextWorkspace
}

type contextWorkspace struct {
	ws   a2aserve.Workspace
	refs int
}

// newContextWorkspaces returns the workspace of tree with turn sync. opts
// must have a Dir.
func newContextWorkspaces(tree fsys.FS, opts a2aserve.TurnSyncOptions) *contextWorkspaces {
	return &contextWorkspaces{tree: tree, opts: opts, attached: map[string]*contextWorkspace{}}
}

func (c *contextWorkspaces) Attach(ctx context.Context, key string) (a2aserve.Attachment, error) {
	ws := c.acquire(key)
	a, err := ws.Attach(ctx, key)
	if err != nil {
		c.release(key)
		return a2aserve.Attachment{}, err
	}
	release := a.Release
	a.Release = func() error {
		defer c.release(key)
		return release()
	}
	return a, nil
}

// acquire returns the workspace of key, which the attachments of key share
// so that each waits for the release of the one before.
func (c *contextWorkspaces) acquire(key string) a2aserve.Workspace {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.attached[key]
	if e == nil {
		e = &contextWorkspace{ws: a2aserve.TurnSynced(contextTree{tree: c.tree, contextID: key}, c.opts)}
		c.attached[key] = e
	}
	e.refs++
	return e.ws
}

// release forgets the workspace of key once no attachment uses it.
func (c *contextWorkspaces) release(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.attached[key]
	if e == nil {
		return
	}
	e.refs--
	if e.refs <= 0 {
		delete(c.attached, key)
	}
}
