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
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/a2aproject/a2a-serve/core/a2aserve"
	"github.com/a2aproject/a2a-serve/core/egress"
	"github.com/a2aproject/a2a-serve/core/fsys"
	"github.com/a2aproject/a2a-serve/internal/projectfiles"
)

// Workspace kinds of --workspace, as the ready line names them.
const (
	WorkspaceDir    = "dir"
	WorkspaceMemory = "memory"
	// WorkspaceURL is a tree that the host serves with the workspace
	// protocol.
	WorkspaceURL = "url"
)

// Syncs of --workspace-sync, as the ready line names them.
const (
	// SyncTurn gives each context a private directory, kept in step with
	// the workspace at the turn boundaries.
	SyncTurn = "turn"
	// SyncLive lets every context change the workspace in place.
	SyncLive = "live"
)

// workspaceCheckTimeout bounds the check at start that the host serves the
// workspace of a URL.
const workspaceCheckTimeout = 10 * time.Second

// liveHint is the hint of a live workspace that the sandbox can't mount,
// and appArmorHint the hint where Docker's AppArmor profile denies it.
const (
	liveHint     = "use --workspace-sync turn, which works on every sandbox"
	appArmorHint = "load the profile with `a2a-serve-sidecar apparmor-profile | sudo apparmor_parser -r`, or " + liveHint
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
	// client is the tree of a URL.
	client fsys.FS
	// shown names a memory or URL workspace in errors, a URL without what
	// may carry a secret.
	shown string
	sync  string
	// liveOK is set where --workspace-sync live works for the workspace.
	liveOK bool
}

// served reports whether the sandbox mounts the workspace as a served
// tree: a live workspace that isn't a host directory.
func (w workspace) served() bool { return w.sync == SyncLive && w.kind != WorkspaceDir }

// parseWorkspace parses dir:<path>, memory or the URL of a host's tree,
// and the sync of --workspace-sync.
func parseWorkspace(v, sync, sandbox string) (workspace, error) {
	switch sync {
	case "":
		sync = SyncTurn
	case SyncTurn, SyncLive:
	default:
		return workspace{}, usagef("invalid --workspace-sync %q: want %s or %s", sync, SyncTurn, SyncLive)
	}
	if v == "" {
		return workspace{}, usagef("no workspace: pass --workspace dir:<path>, --workspace memory, or the URL of your file server")
	}
	if v == WorkspaceMemory || isURL(v) {
		w := workspace{kind: WorkspaceMemory, shown: WorkspaceMemory, sync: sync}
		if isURL(v) {
			var err error
			if w, err = parseWorkspaceURL(v, sync); err != nil {
				return workspace{}, err
			}
		}
		if w.served() && sandbox == SandboxLocal {
			return workspace{}, usagef("--workspace-sync live with --workspace %s needs --sandbox docker or bwrap, which mount the workspace in the sandbox; %s", w.shown, liveHint)
		}
		return w, nil
	}
	kind, path, ok := strings.Cut(v, ":")
	if !ok || kind != WorkspaceDir || path == "" {
		return workspace{}, usagef("invalid --workspace %q: want dir:<path>, memory, or an http://, https:// or unix:///path URL", v)
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
	if sandbox != SandboxLocal {
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

func isURL(v string) bool {
	return strings.HasPrefix(v, "http://") || strings.HasPrefix(v, "https://") || strings.HasPrefix(v, "unix://")
}

// parseWorkspaceURL parses the URL of a host's file server. Its errors
// leave out the URL, whose path may carry a secret.
func parseWorkspaceURL(v, sync string) (workspace, error) {
	u, err := neturl.Parse(v)
	if err != nil {
		return workspace{}, usagef("invalid --workspace: the URL doesn't parse; want http://host:port, https://host:port or unix:///path")
	}
	client, err := workspaceClient(v, "")
	if err != nil {
		return workspace{}, err
	}
	shown := u.Scheme + "://" + u.Host
	if u.Scheme == "unix" {
		shown = "unix://" + u.Path
	}
	return workspace{kind: WorkspaceURL, client: client, shown: shown, sync: sync}, nil
}

// withToken rebuilds the URL workspace client to attach token in
// [egress.ProxyHeaderToken] on every request.
func (w *workspace) withToken(rawURL, token string) error {
	client, err := workspaceClient(rawURL, token)
	if err != nil {
		return err
	}
	w.client = client
	return nil
}

// workspaceClient returns the client of the file server at rawURL, whose
// requests carry token, if any, and the A2A context and task they are for.
func workspaceClient(rawURL, token string) (fsys.FS, error) {
	opts := fsys.ClientOptions{RequestHeader: contextHeader}
	if token != "" {
		opts.Header = http.Header{egress.ProxyHeaderToken: {token}}
	}
	client, err := fsys.ClientWithOptions(rawURL, opts)
	if err != nil {
		return nil, usagef("invalid --workspace: %w", err)
	}
	return client, nil
}

// check checks that the host serves the workspace of a URL: that its root
// is a directory.
func (w workspace) check(ctx context.Context) error {
	if w.kind != WorkspaceURL {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, workspaceCheckTimeout)
	defer cancel()
	info, err := w.client.Stat(ctx, ".")
	if err != nil {
		return fmt.Errorf("the file server of --workspace %s doesn't answer: %s", w.shown, redactURLs(err.Error()))
	}
	if !info.Mode.IsDir() {
		return fmt.Errorf("the file server of --workspace %s serves a root that isn't a directory", w.shown)
	}
	return nil
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

// tree returns the file tree of the workspace.
func (w workspace) tree() fsys.FS {
	switch w.kind {
	case WorkspaceMemory:
		return fsys.MemoryWithLimits(fsys.MemoryLimits{MaxBytes: defaultWorkspaceLimits.bytes, MaxEntries: defaultWorkspaceLimits.entries})
	case WorkspaceURL:
		return w.client
	}
	return fsys.Dir(w.path)
}

// build returns the workspace of the processes of the contexts, and that
// of the main process, which only checks that the agent starts and gets an
// empty directory with turn sync. synced gets what each turn wrote to
// tree.
//
// With turn sync, each process sees its private directory at its path on
// the host: the agent profiles expect the workspace at the same path on
// both sides, and its requests to the tree of a URL are for its context.
// A live tree has no path on the host, so the processes see it at the
// default path of the core.
func (w workspace) build(tree fsys.FS, stateDir string, synced func(context.Context, string, []fsys.Info)) (contexts, main a2aserve.Workspace, err error) {
	if w.served() {
		ws := a2aserve.Live(tree, a2aserve.LiveOptions{})
		return ws, ws, nil
	}
	if w.sync == SyncLive {
		ws := a2aserve.SharedDir(w.path)
		return ws, ws, nil
	}
	empty := filepath.Join(stateDir, "main-workspace")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		return nil, nil, fmt.Errorf("creating the workspace of the main agent process: %w", err)
	}
	opts := a2aserve.TurnSyncOptions{Dir: filepath.Join(stateDir, "synced"), MaxBytes: defaultWorkspaceLimits.bytes, MaxEntries: defaultWorkspaceLimits.entries, Synced: synced}
	return newContextWorkspaces(tree, opts), a2aserve.SharedDir(empty), nil
}

// explain words the failure of a turn whose changes the workspace didn't
// take for the host, or returns nil for the error's text.
func (w workspace) explain(_ context.Context, _ *a2asrv.ExecutorContext, err error) []*a2a.Part {
	if errors.Is(err, a2aserve.ErrWorkspaceTooLarge) || errors.Is(err, fsys.ErrFull) {
		text := fmt.Sprintf("The workspace is past a2a-serve-sidecar's limits of %d entries and %d bytes (%v). Ask the agent to remove files it doesn't need, or remove files yourself, then send the message again.", defaultWorkspaceLimits.entries, defaultWorkspaceLimits.bytes, err)
		if w.kind != WorkspaceMemory && w.sync == SyncTurn && w.liveOK {
			text += " To work on a larger workspace, restart a2a-serve-sidecar with --workspace-sync live."
		}
		return []*a2a.Part{a2a.NewTextPart(text)}
	}
	if _, ok := errors.AsType[*net.OpError](err); (ok || errors.Is(err, context.DeadlineExceeded)) && w.kind == WorkspaceURL {
		return []*a2a.Part{a2a.NewTextPart(fmt.Sprintf(
			"The workspace at %s didn't answer: %s. The agent's changes stay in its copy and go back at the next turn.", w.shown, redactURLs(err.Error())))}
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
