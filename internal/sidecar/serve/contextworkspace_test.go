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
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/core/a2aserve"
	"github.com/a2aproject/a2a-serve/core/fsys"
)

func header(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}

func TestContextHeader(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		ctx  context.Context
		want http.Header
	}{
		{name: "no context", ctx: context.Background()},
		{name: "context", ctx: withWorkspaceContext(context.Background(), "ctx-1"), want: header("A2A-Sidecar-Context", "ctx-1")},
		{
			name: "context and task",
			ctx:  withWorkspaceContext(context.WithValue(context.Background(), taskKey{}, a2a.TaskID("task-1")), "ctx-1"),
			want: header("A2A-Sidecar-Context", "ctx-1", "A2A-Sidecar-Task", "task-1"),
		},
		{name: "task without context", ctx: context.WithValue(context.Background(), taskKey{}, a2a.TaskID("task-1"))},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(tc.want, contextHeader(tc.ctx)); diff != "" {
				t.Fatalf("contextHeader() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

// contextsSeen is a tree that records the A2A contexts of its Stat calls.
type contextsSeen struct {
	fsys.FS
	mu   sync.Mutex
	seen []string
}

func (c *contextsSeen) Stat(ctx context.Context, p string) (fsys.Info, error) {
	c.mu.Lock()
	c.seen = append(c.seen, contextHeader(ctx).Get("A2A-Sidecar-Context"))
	c.mu.Unlock()
	return c.FS.Stat(ctx, p)
}

func TestContextWorkspacesSyncsEachContextWithItsTree(t *testing.T) {
	t.Parallel()
	tree := &contextsSeen{FS: fsys.Memory()}
	c := newContextWorkspaces(tree, a2aserve.TurnSyncOptions{Dir: t.TempDir()})

	a, err := c.Attach(t.Context(), "ctx-1")
	if err != nil {
		t.Fatalf("contextWorkspaces.Attach() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(a.Dir, "hello.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.Release(); err != nil {
		t.Fatalf("Attachment.Release() error = %v", err)
	}

	if _, err := tree.FS.Stat(t.Context(), "hello.txt"); err != nil {
		t.Fatalf("Stat(hello.txt) of the tree after the release error = %v, want the agent's file", err)
	}
	tree.mu.Lock()
	defer tree.mu.Unlock()
	if len(tree.seen) == 0 || slices.ContainsFunc(tree.seen, func(id string) bool { return id != "ctx-1" }) {
		t.Fatalf("contexts of the requests to the tree = %v, want only ctx-1", tree.seen)
	}
}

func TestContextWorkspacesAttachesAContextAgainAfterARelease(t *testing.T) {
	t.Parallel()
	c := newContextWorkspaces(fsys.Memory(), a2aserve.TurnSyncOptions{Dir: t.TempDir()})
	held, err := c.Attach(t.Context(), "ctx-1")
	if err != nil {
		t.Fatalf("contextWorkspaces.Attach() error = %v", err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := c.Attach(canceled, "ctx-1"); err == nil {
		t.Fatal("contextWorkspaces.Attach() with a canceled context while another attachment holds the directory error = nil, want an error")
	}
	if err := held.Release(); err != nil {
		t.Fatalf("Attachment.Release() error = %v", err)
	}

	ctx, stop := context.WithTimeout(t.Context(), 10*time.Second)
	defer stop()
	again, err := c.Attach(ctx, "ctx-1")
	if err != nil {
		t.Fatalf("contextWorkspaces.Attach() after the release error = %v, want an attachment", err)
	}
	if err := again.Release(); err != nil {
		t.Fatalf("Attachment.Release() error = %v", err)
	}
}
