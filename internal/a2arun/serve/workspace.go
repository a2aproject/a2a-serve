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
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/a2aproject/a2a-serve/core/a2aserve"
	"github.com/a2aproject/a2a-serve/core/fsys"
	"github.com/a2aproject/a2a-serve/internal/projectfiles"
)

// Workspace kinds of --workspace, as the ready line names them.
const (
	WorkspaceDir    = "dir"
	WorkspaceMemory = "memory"
)

// Syncs of --workspace-sync, as the ready line names them.
const (
	// SyncTurn gives each context a private directory, kept in step with
	// the workspace at the turn boundaries.
	SyncTurn = "turn"
	// SyncLive lets every context change the workspace in place.
	SyncLive = "live"
)

// workspaceLimits bound a workspace with turn sync, which
// docs/reference/limits.md lists.
type workspaceLimits struct {
	bytes   int64
	entries int
}

var defaultWorkspaceLimits = workspaceLimits{bytes: 1 << 30, entries: 200_000}

// workspace is what --workspace and --workspace-sync name.
type workspace struct {
	kind string
	// path is the directory of dir:<path>.
	path string
	sync string
}

// parseWorkspace parses dir:<path> or memory, and the sync of
// --workspace-sync.
func parseWorkspace(v, sync, sandbox string) (workspace, error) {
	switch sync {
	case "":
		sync = SyncTurn
	case SyncTurn, SyncLive:
	default:
		return workspace{}, usagef("invalid --workspace-sync %q: want %s or %s", sync, SyncTurn, SyncLive)
	}
	if v == "" {
		return workspace{}, usagef("no workspace: pass --workspace dir:<path> or --workspace memory")
	}
	if v == WorkspaceMemory {
		if sync == SyncLive {
			return workspace{}, usagef("--workspace-sync live needs --workspace dir:<path>; a live memory workspace is planned, use --workspace-sync turn")
		}
		return workspace{kind: WorkspaceMemory, sync: sync}, nil
	}
	if strings.HasPrefix(v, "http://") || strings.HasPrefix(v, "https://") || strings.HasPrefix(v, "unix://") {
		return workspace{}, usagef("invalid --workspace %q: a workspace that the host serves is planned; want dir:<path> or memory", v)
	}
	kind, path, ok := strings.Cut(v, ":")
	if !ok || kind != WorkspaceDir || path == "" {
		return workspace{}, usagef("invalid --workspace %q: want dir:<path> or memory", v)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return workspace{}, usagef("invalid --workspace %q: %w", v, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return workspace{}, usagef("invalid --workspace %q: %w", v, err)
	}
	if !info.IsDir() {
		return workspace{}, usagef("invalid --workspace %q: %s isn't a directory", v, abs)
	}
	if sandbox == SandboxDocker {
		broad, err := projectfiles.BroadMount(abs)
		if err != nil {
			return workspace{}, &UsageError{Err: err}
		}
		if broad {
			return workspace{}, usagef("invalid --workspace %q: %s holds your home directory, so the agent could read your credentials; pass a project directory", v, abs)
		}
	}
	if sync == SyncTurn {
		if err := checkWorkspaceSize(abs, defaultWorkspaceLimits); err != nil {
			return workspace{}, err
		}
	}
	return workspace{kind: WorkspaceDir, path: abs, sync: sync}, nil
}

// checkWorkspaceSize fails with a usage error if the directory dir holds
// more than turn sync takes, counting the entries as the sync does.
func checkWorkspaceSize(dir string, limits workspaceLimits) error {
	var size int64
	entries := 0
	errTooLarge := errors.New("too large")
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case p == dir:
			return nil
		case d.IsDir() && d.Name() == projectfiles.AgentDir:
			return filepath.SkipDir
		}
		entries++
		if d.Type().IsRegular() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			size += info.Size()
		}
		if entries > limits.entries || size > limits.bytes {
			return errTooLarge
		}
		return nil
	})
	if errors.Is(err, errTooLarge) {
		return usagef("--workspace dir:%s holds more than %d entries or %d bytes, more than --workspace-sync turn copies for each context; pass --workspace-sync live to let the agent edit it in place", dir, limits.entries, limits.bytes)
	}
	if err != nil {
		return usagef("invalid --workspace dir:%s: %w", dir, err)
	}
	return nil
}

// tree returns the file tree of a turn-synced workspace.
func (w workspace) tree() fsys.FS {
	if w.kind == WorkspaceMemory {
		return fsys.MemoryWithLimits(fsys.MemoryLimits{MaxBytes: defaultWorkspaceLimits.bytes, MaxEntries: defaultWorkspaceLimits.entries})
	}
	return fsys.Dir(w.path)
}

// build returns the workspace of the processes of the contexts, and that
// of the main process, which only checks that the agent starts and gets an
// empty directory with turn sync. synced gets what each turn wrote to
// tree.
//
// Each process sees its private directory at its path on the host: the
// agent profiles expect the workspace at the same path on both sides.
func (w workspace) build(tree fsys.FS, stateDir string, synced func(context.Context, string, []fsys.Info)) (contexts, main a2aserve.Workspace, err error) {
	if w.sync == SyncLive {
		ws := a2aserve.SharedDir(w.path)
		return ws, ws, nil
	}
	empty := filepath.Join(stateDir, "main-workspace")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		return nil, nil, fmt.Errorf("creating the workspace of the main agent process: %w", err)
	}
	opts := a2aserve.TurnSyncOptions{MaxBytes: defaultWorkspaceLimits.bytes, MaxEntries: defaultWorkspaceLimits.entries, Synced: synced}
	return a2aserve.TurnSynced(tree, opts), a2aserve.SharedDir(empty), nil
}

// explain words the failure of a turn whose changes the workspace didn't
// take for the host, or returns nil for the error's text.
func (w workspace) explain(_ context.Context, _ *a2asrv.ExecutorContext, err error) []*a2a.Part {
	if errors.Is(err, a2aserve.ErrWorkspaceTooLarge) || errors.Is(err, fsys.ErrFull) {
		text := fmt.Sprintf("The workspace is past a2a-run's limits of %d entries and %d bytes (%v). Ask the agent to remove files it doesn't need, or remove files yourself, then send the message again.", defaultWorkspaceLimits.entries, defaultWorkspaceLimits.bytes, err)
		if w.kind == WorkspaceDir {
			text += " To work on a larger directory, restart a2a-run with --workspace-sync live."
		}
		return []*a2a.Part{a2a.NewTextPart(text)}
	}
	conflict, ok := errors.AsType[*a2aserve.SyncConflictError](err)
	if !ok {
		return nil
	}
	paths := strings.Join(conflict.Paths, ", ")
	if conflict.Omitted > 0 {
		paths += fmt.Sprintf(" and %d more", conflict.Omitted)
	}
	return []*a2a.Part{a2a.NewTextPart(fmt.Sprintf(
		"The workspace changed these files while the agent worked on them, so it kept its own version and dropped the agent's: %s. "+
			"The status metadata lists them under %s. Send the message again to redo the work on the current files.",
		paths, a2aserve.ConflictsKey))}
}
