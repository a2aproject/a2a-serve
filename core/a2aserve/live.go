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

package a2aserve

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"

	"github.com/a2aproject/a2a-serve/core/fsys"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
)

// LiveOptions configures [Live].
type LiveOptions struct {
	// At is where the processes see the tree, an absolute path in the
	// sandbox; "/workspace" if empty.
	At string
}

// defaultLiveAt is where the processes see a Live tree by default.
const defaultLiveAt = "/workspace"

// Live returns the workspace that shows tree to every process live, with
// [sandbox.Served]: each file operation of the agent goes to tree, so the
// caller sees the agent's changes as they happen, and the agent sees the
// caller's. Files are written whole when the agent closes them, and the
// last write wins. All processes share tree.
//
// The process sees .mcp.json with no MCP servers and the configuration
// files of its agent empty, and can't change them. The configuration
// folders of its agent, such as .opencode, start empty and keep what the
// process writes there to itself. The agent directories (.a2a) are hidden
// at any depth.
//
// The start of a process fails with a [*sandbox.UnsupportedMountError]
// where its runtime can't mount tree: the local runtime, and Docker
// without the egress boundary or under an AppArmor profile that denies
// the mount. Attach outside an Agent fails, since the sandbox mounts tree
// over an empty directory in the StateDir of the Agent.
func Live(tree fsys.FS, opts LiveOptions) Workspace {
	return &liveWorkspace{tree: tree, at: cmp.Or(opts.At, defaultLiveAt)}
}

type liveWorkspace struct {
	tree fsys.FS
	at   string
}

func (l *liveWorkspace) served() fsys.FS { return l.tree }

func (l *liveWorkspace) Attach(ctx context.Context, key string) (Attachment, error) {
	return l.attachIn(ctx, key, "")
}

func (l *liveWorkspace) attachIn(ctx context.Context, key, stateDir string) (Attachment, error) {
	if err := ctx.Err(); err != nil {
		return Attachment{}, err
	}
	switch {
	case l.tree == nil:
		return Attachment{}, errors.New("a2aserve: Live has no tree")
	case !path.IsAbs(l.at):
		return Attachment{}, fmt.Errorf("a2aserve: Live at %q, which isn't an absolute path", l.at)
	case stateDir == "":
		return Attachment{}, errors.New("a2aserve: Live attaches only in an Agent")
	}
	root := filepath.Join(stateDir, "live")
	if err := fsutil.MkdirPrivate(root); err != nil {
		return Attachment{}, fmt.Errorf("a2aserve: creating the directory of live workspaces: %w", err)
	}
	dir, err := os.MkdirTemp(root, dirName(key)+"-")
	if err != nil {
		return Attachment{}, fmt.Errorf("a2aserve: creating the directory under the live workspace of key %q: %w", key, err)
	}
	return Attachment{
		Dir:     dir,
		Path:    path.Clean(l.at),
		Release: func() error { return os.RemoveAll(dir) },
	}, nil
}
