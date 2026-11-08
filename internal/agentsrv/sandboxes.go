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

package agentsrv

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/a2aproject/a2a-serve/internal/access"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agentkey"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/harness"
	"github.com/a2aproject/a2a-serve/internal/workspaces"
)

// maxRememberedUsers bounds the users whose principal [sandboxes] keeps; the
// least recently seen is forgotten first.
const maxRememberedUsers = 1024

// sandboxes tells what the agent container of each agent key gets under the
// sandbox rules in force. The rules match the identity of a user, which
// only a request proves, so it keeps the latest principal of each user who
// called: a user's container can start for a turn of an owner who writes
// into the user's conversation, or after a restart. Without one, it matches
// the identity that the principal ID names.
type sandboxes struct {
	config func() *agentdir.Config

	mu    sync.Mutex
	users map[string]rememberedUser
}

type rememberedUser struct {
	p    auth.Principal
	seen time.Time
}

func newSandboxes(config func() *agentdir.Config) *sandboxes {
	return &sandboxes{config: config, users: map[string]rememberedUser{}}
}

// remember keeps the principal of a user who called, for the sandbox of
// their container.
func (s *sandboxes) remember(p auth.Principal) {
	key := agentkey.Of(p, true)
	if _, ok := agentkey.UserOf(key); !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.users[key]; !ok && len(s.users) >= maxRememberedUsers {
		oldest := ""
		for k, u := range s.users {
			if oldest == "" || u.seen.Before(s.users[oldest].seen) {
				oldest = k
			}
		}
		delete(s.users, oldest)
	}
	s.users[key] = rememberedUser{p: p, seen: time.Now()}
}

// forget drops the principals of the agent keys of removed users.
func (s *sandboxes) forget(keys ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range keys {
		delete(s.users, k)
	}
}

// principalOf returns whom the sandbox rules match for the container of an
// agent key: the owner for none, any anonymous caller, or the user.
func (s *sandboxes) principalOf(key string) auth.Principal {
	if key == "" {
		return auth.Owner()
	}
	if key == agentkey.Anonymous {
		return auth.Principal{ID: auth.AnonymousID, Role: auth.RoleAnonymous}
	}
	id, _ := agentkey.UserOf(key)
	s.mu.Lock()
	u, ok := s.users[key]
	s.mu.Unlock()
	if ok {
		return u.p
	}
	return auth.Principal{ID: id, Role: auth.RoleAuthenticated, Identity: auth.IdentityOf(id)}
}

// of returns the sandbox of the container of an agent key.
func (s *sandboxes) of(key string) access.Sandbox {
	return s.resolve(s.principalOf(key))
}

// resolve returns the sandbox of the container that p's conversations run
// in.
func (s *sandboxes) resolve(p auth.Principal) access.Sandbox {
	return access.SandboxPolicyOf(s.config()).Resolve(p)
}

// Refusals of turns by the sandbox rules.
var (
	errSandboxCovers     = errors.New("this conversation runs in the agent container of the user who started it, which gets more storage or secrets than your sandbox rule gives you; start a conversation of your own")
	errSandboxPrivate    = errors.New("this conversation runs in the private workspace of the user who started it; start a conversation of your own")
	errSandboxRestarting = errors.New("the sandbox rules of this conversation changed, and its agent container restarts with them once its current tasks end; send the message again then")
)

// admitTurn lets the turn of execCtx by the caller in ctx run in the
// container of an agent key:
//
//   - a caller from another container needs a sandbox that covers what this
//     container gets now and got when it started, and only an owner may
//     write into another user's private workspace;
//   - a container whose sandbox changed stops if it is idle, so that the turn
//     starts it with the rules in force. If it is busy, only answers to its
//     waiting tasks run in it until it restarts.
func (s *Server) admitTurn(ctx context.Context, key string, execCtx *a2asrv.ExecutorContext) error {
	want := s.sandboxes.of(key)
	if caller, ok := auth.Caller(ctx); ok {
		s.sandboxes.remember(caller)
		want = s.sandboxes.of(key)
		if key != agentkey.Of(caller, ok) {
			if err := s.admitOther(caller, key, want); err != nil {
				return err
			}
		}
	}
	agents := s.agentsOf(key)
	if agents == nil {
		return nil
	}
	running, ok := agents.sandboxOf(key)
	if !ok || running.Equal(want) || answersWaiting(execCtx) {
		return nil
	}
	s.closeStale(key)
	if _, ok := agents.sandboxOf(key); ok {
		return errSandboxRestarting
	}
	return nil
}

// admitOther checks a caller's turn in the container of another agent key,
// which gets want under the rules in force.
func (s *Server) admitOther(caller auth.Principal, key string, want access.Sandbox) error {
	mine := s.sandboxes.resolve(caller)
	if !mine.Covers(want) {
		return errSandboxCovers
	}
	if agents := s.agentsOf(key); agents != nil {
		if running, ok := agents.sandboxOf(key); ok && !mine.Covers(running) {
			return errSandboxCovers
		}
	}
	if !caller.IsOwner() && (want.Storage == agentdir.StorageOwn || want.Storage == agentdir.StorageEmpty) {
		return errSandboxPrivate
	}
	return nil
}

// answersWaiting reports whether execCtx answers a task that waits for
// input, which continues in the container that asked.
func answersWaiting(execCtx *a2asrv.ExecutorContext) bool {
	return execCtx != nil && execCtx.StoredTask != nil && execCtx.StoredTask.Status.State == a2a.TaskStateInputRequired
}

// closeStale stops the container of an agent key if no turn uses it and
// the sandbox rules changed what it gets. The owner's container keeps its
// sandbox until the server restarts.
func (s *Server) closeStale(key string) {
	agents := s.agentsOf(key)
	if agents == nil {
		return
	}
	stopped, err := agents.closeStale(key, s.sandboxes.of(key))
	if err != nil {
		slog.Warn("stopping an agent process whose sandbox changed", "key", key, "error", err)
	}
	if stopped {
		slog.Info("stopped an agent process whose sandbox changed; its next turn starts it again", "key", key)
	}
}

// agentsOf returns the processes that the agent key runs in, or nil for the
// owner's.
func (s *Server) agentsOf(key string) *userAgents {
	if key == agentkey.Anonymous {
		return &s.anonymous
	}
	if _, ok := agentkey.UserOf(key); ok {
		return &s.users
	}
	return nil
}

// sandboxLoop applies changes of the sandbox rules to the idle containers,
// so that a removed secret leaves them without waiting for their next turn.
func (s *Server) sandboxLoop(ctx context.Context) {
	t := time.NewTicker(s.timings.SandboxCheckInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		for _, agents := range []*userAgents{&s.anonymous, &s.users} {
			for _, key := range agents.runningKeys() {
				s.closeStale(key)
			}
		}
	}
}

// sandboxNotes returns notes with one added when the owner's container
// runs with another sandbox than the rule of self gives now.
func (s *Server) sandboxNotes(notes []string) []string {
	if started := s.mainSandbox.Load(); started != nil && !started.Equal(s.sandboxes.of("")) {
		notes = append(slices.Clone(notes), "your agent container applies the changed sandbox rule of self after `a2a-serve restart`")
	}
	return notes
}

// sandboxMounts returns the mounts of a container with storage: as they are
// for shared, read-only for read-only and own, and only the workspace for
// empty. Own and empty mount a private workspace in place of the workspace.
func sandboxMounts(storage string, mounts []agentdir.Mount) []agentdir.Mount {
	switch storage {
	case agentdir.StorageShared:
		return mounts
	case agentdir.StorageEmpty:
		ws := mounts[0]
		ws.ReadOnly = false
		return []agentdir.Mount{ws}
	}
	out := slices.Clone(mounts)
	for i := range out {
		out[i].ReadOnly = storage != agentdir.StorageOwn || i > 0
	}
	return out
}

// workspaceName names the private workspaces of the container of an agent
// key.
func workspaceName(key string) string {
	if key == "" {
		return "owner"
	}
	if id, ok := agentkey.UserOf(key); ok {
		return agentdir.UserKey(id)
	}
	return key
}

// applySandbox gives spec the storage and secrets of sb, for the container of
// an agent key. Without the Docker sandbox, only the secrets apply.
func (s *Server) applySandbox(ctx context.Context, spec *harness.Spec, key string, sb access.Sandbox) error {
	values, missing := sb.Values(s.rt.Secrets())
	if len(missing) > 0 {
		return errors.New("secret " + missing[0] + " is not set; pass --secret " + missing[0] + " again with the variable set")
	}
	spec.Env = append(spec.Env, values...)
	if spec.Sandbox == harness.SandboxLocal {
		return nil
	}
	ws := spec.Mounts[0].Src
	spec.Mounts = sandboxMounts(sb.Storage, spec.Mounts)
	if sb.Storage != agentdir.StorageOwn && sb.Storage != agentdir.StorageEmpty {
		return nil
	}
	dir, err := s.workspaces.Prepare(ctx, workspaceName(key), s.sandboxes.principalOf(key).ID, sb.Storage, ws)
	if errors.Is(err, workspaces.ErrTooLarge) || errors.Is(err, workspaces.ErrTooMany) {
		return errors.New(err.Error() + "; the sandbox rule of this caller gives own storage, a private copy of the workspace, and the owner can give read-only storage, which needs no copy")
	}
	if err != nil {
		return err
	}
	spec.WorkspaceSrc = dir
	return nil
}

// storageNote tells the agent what its storage is and how to return work.
func storageNote(storage string) string {
	const reply = " Return your work in your reply, such as a patch or the full text of the files you changed."
	switch storage {
	case agentdir.StorageShared:
		return "Storage: shared. The workspace is the owner's project, and your changes to it are saved there."
	case agentdir.StorageReadOnly:
		return "Storage: read-only. You can't change the workspace or the mounted directories. Write scratch files in /tmp or your home directory." + reply
	case agentdir.StorageOwn:
		return "Storage: own. The workspace is your private copy of the project, made when you first ran. Your changes stay in it and don't reach the owner's project, and the mounted directories are read-only." + reply
	case agentdir.StorageEmpty:
		return "Storage: empty. The workspace started empty, is private to you, and no other directories are mounted." + reply
	}
	return ""
}

// setStorageNote records the storage note of the agent home, which the
// instructions written into it carry.
func (s *Server) setStorageNote(home, storage string) {
	s.instructionsMu.Lock()
	defer s.instructionsMu.Unlock()
	if s.storageNotes == nil {
		s.storageNotes = map[string]string{}
	}
	s.storageNotes[home] = storageNote(storage)
}

// forgetStorageNote drops the storage note of a deleted agent home.
func (s *Server) forgetStorageNote(home string) {
	s.instructionsMu.Lock()
	defer s.instructionsMu.Unlock()
	delete(s.storageNotes, home)
}

// unusedWorkspaceTTL is how long the private workspace of a user without
// a pairing is kept unused.
const unusedWorkspaceTTL = 30 * 24 * time.Hour

// pruneWorkspaces deletes the private workspaces that are no longer needed:
// those of users without a pairing unused for unusedWorkspaceTTL, and those
// of the owner's and the anonymous container whose sandbox no longer has
// their storage. `pair rm` deletes those of pairings. The sandbox of a
// user's container depends on how they call, so it isn't checked here.
func (s *Server) pruneWorkspaces(ctx context.Context) {
	entries, err := s.workspaces.List()
	if err != nil {
		slog.Warn("listing the private workspaces", "error", err)
		return
	}
	for _, e := range entries {
		if ctx.Err() != nil {
			return
		}
		if err := s.pruneWorkspace(e); err != nil {
			slog.Warn("deleting a private workspace", "name", e.Name, "error", err)
		}
	}
}

func (s *Server) pruneWorkspace(e workspaces.Entry) error {
	switch e.Name {
	case workspaceName(""):
		// The owner's container keeps its sandbox until the server restarts.
		return nil
	case workspaceName(agentkey.Anonymous):
		release, err := s.anonymous.stopIdle(agentkey.Anonymous)
		if err != nil || release == nil {
			return err
		}
		defer release()
		storage := s.sandboxes.of(agentkey.Anonymous).Storage
		var errs []error
		for _, st := range e.Storage {
			if st != storage {
				errs = append(errs, s.workspaces.RemoveStorage(e.Name, st))
			}
		}
		return errors.Join(errs...)
	}
	key := agentkey.User(e.Principal)
	pairing := strings.HasPrefix(e.Principal, auth.PairPrefix) || auth.IdentityOf(e.Principal).WebhookNode != ""
	if e.Principal == "" || workspaceName(key) != e.Name || pairing || time.Since(e.Used) < unusedWorkspaceTTL {
		return nil
	}
	release, err := s.users.stopIdle(key)
	if err != nil || release == nil {
		return err
	}
	defer release()
	return s.workspaces.Remove(e.Name)
}
