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
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/a2aproject/a2a-serve/internal/store"
	"github.com/a2aproject/a2a-serve/internal/workspaces"
)

// maxRememberedUsers bounds the users whose principal [sandboxes] keeps in
// memory; the least recently seen is forgotten first.
const maxRememberedUsers = 1024

// sandboxes tells what the agent container of each agent key gets under the
// sandbox rules in force. The rules match the identity of a user, which
// only a request proves, so it keeps the last identity the server verified
// for each user in the store: a user's container can start for a turn of an
// owner who writes into the user's conversation, or a schedule, after a
// restart. Without one, it matches the identity that the principal ID
// names.
type sandboxes struct {
	config func() *agentdir.Config
	store  *store.Store

	mu    sync.Mutex
	users map[string]rememberedUser
}

type rememberedUser struct {
	p    auth.Principal
	seen time.Time
	// saved is when the identity was last saved; zero for one read from
	// the store, whose time is unknown.
	saved time.Time
}

// identityRefresh is how often the identity of a user who keeps calling is
// saved again, so that the store keeps it while they call.
const identityRefresh = 24 * time.Hour

func newSandboxes(config func() *agentdir.Config, st *store.Store) *sandboxes {
	return &sandboxes{config: config, store: st, users: map[string]rememberedUser{}}
}

// remember keeps the principal of a user who called, for the sandbox of
// their container, and saves its identity when it changed.
func (s *sandboxes) remember(ctx context.Context, p auth.Principal) {
	key := agentkey.Of(p, true)
	if _, ok := agentkey.UserOf(key); !ok {
		return
	}
	s.mu.Lock()
	prev, known := s.users[key]
	save := !known || prev.p.Identity != p.Identity || time.Since(prev.saved) > identityRefresh
	saved := prev.saved
	if save {
		saved = time.Now()
	}
	s.keepLocked(key, rememberedUser{p: p, saved: saved})
	s.mu.Unlock()
	if !save {
		return
	}
	data, err := json.Marshal(p.Identity)
	if err == nil {
		err = s.store.SaveUserIdentity(ctx, p.ID, string(data))
	}
	if err != nil {
		slog.Warn("saving the identity of a user", "user", p.ID, "error", err)
	}
}

// keepLocked keeps u as the user of key in memory. The caller holds s.mu.
func (s *sandboxes) keepLocked(key string, u rememberedUser) {
	if _, ok := s.users[key]; !ok && len(s.users) >= maxRememberedUsers {
		oldest := ""
		for k, u := range s.users {
			if oldest == "" || u.seen.Before(s.users[oldest].seen) {
				oldest = k
			}
		}
		delete(s.users, oldest)
	}
	u.seen = time.Now()
	s.users[key] = u
}

// forget drops the principals of the agent keys of removed users from
// memory. The store deletes their identities with the pairing.
func (s *sandboxes) forget(keys ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range keys {
		delete(s.users, k)
	}
}

// principalOf returns whom the sandbox rules match for the container of an
// agent key: the owner for none, any anonymous caller, or the user. It
// reports whether the server verified the user's identity; one rebuilt from
// the principal ID alone lacks what only a request proves.
func (s *sandboxes) principalOf(ctx context.Context, key string) (auth.Principal, bool) {
	if key == "" {
		return auth.Owner(), true
	}
	if key == agentkey.Anonymous {
		return auth.Principal{ID: auth.AnonymousID, Role: auth.RoleAnonymous}, true
	}
	id, _ := agentkey.UserOf(key)
	s.mu.Lock()
	u, ok := s.users[key]
	s.mu.Unlock()
	if ok {
		return u.p, true
	}
	rebuilt := auth.Principal{ID: id, Role: auth.RoleAuthenticated, Identity: auth.IdentityOf(id)}
	data, err := s.store.UserIdentity(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return rebuilt, false
	}
	p := rebuilt
	if err == nil {
		err = json.Unmarshal([]byte(data), &p.Identity)
	}
	if err != nil {
		slog.Warn("reading the identity of a user", "user", id, "error", err)
		return rebuilt, false
	}
	s.mu.Lock()
	s.keepLocked(key, rememberedUser{p: p})
	s.mu.Unlock()
	return p, true
}

// of returns the sandbox of the container of an agent key. A user whose
// identity the server hasn't verified gets the least that it could match.
func (s *sandboxes) of(ctx context.Context, key string) access.Sandbox {
	p, verified := s.principalOf(ctx, key)
	if !verified {
		return access.SandboxPolicyOf(s.config()).ResolveUnverified(p)
	}
	return s.resolve(p)
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
	errSandboxNarrowed   = errors.New("the sandbox rules of this conversation changed and give less, so its waiting tasks ended; send the message again")
)

// admitTurn lets the turn of execCtx by the caller in ctx run in the
// container of an agent key, and counts it there until release:
//
//   - a caller from another container needs a sandbox that covers what this
//     container gets now and got when it started, and only an owner may
//     write into another user's private workspace;
//   - a container whose rules now give less ends its tasks and stops, so
//     that the turn starts it with the rules in force;
//   - a container whose rules changed otherwise stops if it is idle. If it
//     is busy, only answers to its waiting tasks run in it until it
//     restarts.
func (s *Server) admitTurn(ctx context.Context, key string, execCtx *a2asrv.ExecutorContext) (func(), error) {
	caller, ok := auth.Caller(ctx)
	if ok {
		s.sandboxes.remember(ctx, caller)
	}
	want := s.sandboxes.of(ctx, key)
	if ok && key != agentkey.Of(caller, ok) {
		if err := s.admitOther(ctx, caller, key, want); err != nil {
			return nil, err
		}
	}
	agents := s.agentsOf(key)
	if agents == nil {
		return func() {}, nil
	}
	answer := answersWaiting(execCtx)
	if running, ok := agents.sandboxOf(key); ok && !want.Covers(running) {
		// An answer fails its own task here; sandboxLoop then ends the
		// others, since stopping the container now would fail the task
		// without saying why.
		if answer {
			return nil, errSandboxNarrowed
		}
		var asking a2a.TaskID
		if execCtx != nil {
			asking = execCtx.TaskID
		}
		if err := s.endNarrowed(ctx, key, asking); err != nil {
			return nil, err
		}
	}
	counted, err := agents.admit(key, want, answer)
	if err != nil {
		return nil, err
	}
	return func() {
		counted()
		s.closeStale(ctx, key)
	}, nil
}

// admitOther checks a caller's turn in the container of another agent key,
// which gets want under the rules in force.
func (s *Server) admitOther(ctx context.Context, caller auth.Principal, key string, want access.Sandbox) error {
	mine := s.sandboxes.resolve(caller)
	if !mine.Covers(want) {
		return errSandboxCovers
	}
	running, ok := access.Sandbox{}, false
	if agents := s.agentsOf(key); agents != nil {
		running, ok = agents.sandboxOf(key)
	} else if started := s.mainSandbox.Load(); started != nil {
		running, ok = *started, true
	}
	if ok && !mine.Covers(running) {
		return errSandboxCovers
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

// endNarrowed cancels the unfinished tasks of the container of an agent
// key, but the task of the turn that asks, and stops it, if its rules now
// give less than it got: its tasks would keep the access they got.
func (s *Server) endNarrowed(ctx context.Context, key string, asking a2a.TaskID) error {
	agents := s.agentsOf(key)
	want := s.sandboxes.of(ctx, key)
	if running, ok := agents.sandboxOf(key); !ok || want.Covers(running) {
		return nil
	}
	ids, err := s.tasks.UnfinishedIn(ctx, key)
	if err != nil {
		return err
	}
	as := auth.InProcess(context.WithoutCancel(ctx), auth.Owner())
	for _, t := range ids {
		if t == asking {
			continue
		}
		if _, err := s.sender.CancelTask(as, &a2a.CancelTaskRequest{ID: t}); err != nil && !errors.Is(err, a2a.ErrTaskNotCancelable) {
			return fmt.Errorf("canceling task %s: %w", t, err)
		}
	}
	stopped, err := agents.closeNarrowed(key, want)
	if stopped {
		slog.Info("stopped an agent process whose sandbox rules now give less", "key", key, "canceled", len(ids))
	}
	return err
}

// closeStale stops the container of an agent key if no turn uses it and
// the sandbox rules changed what it gets. The owner's container keeps its
// sandbox until the server restarts.
func (s *Server) closeStale(ctx context.Context, key string) {
	agents := s.agentsOf(key)
	if agents == nil {
		return
	}
	stopped, err := agents.closeStale(key, s.sandboxes.of(ctx, key))
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

// sandboxLoop applies changes of the sandbox rules to the containers that
// no turn starts: an idle one stops, and a busy one whose rules now give
// less ends its tasks, so that a removed secret doesn't wait for them.
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
				s.applySandboxChange(ctx, agents, key)
			}
		}
	}
}

func (s *Server) applySandboxChange(ctx context.Context, agents *userAgents, key string) {
	want := s.sandboxes.of(ctx, key)
	if running, ok := agents.sandboxOf(key); ok && !want.Covers(running) {
		if err := s.endNarrowed(ctx, key, ""); err != nil {
			slog.Warn("stopping an agent process whose sandbox rules now give less", "key", key, "error", err)
		}
		return
	}
	s.closeStale(ctx, key)
}

// sandboxNotes returns notes with one added when the owner's container
// runs with another sandbox than the rule of self gives now.
func (s *Server) sandboxNotes(notes []string) []string {
	if started := s.mainSandbox.Load(); started != nil && !started.Equal(s.sandboxes.of(context.Background(), "")) {
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
		hint := fmt.Sprintf("secret %s is not set; pass --secret %s again with the variable set", missing[0], missing[0])
		if key == "" {
			return errors.New(hint)
		}
		slog.Warn("an agent process of a caller can't start: "+hint, "key", key)
		return fmt.Errorf("the owner of the agent hasn't stored the secret %s that this conversation's agent gets; ask them to store it", missing[0])
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
	p, _ := s.sandboxes.principalOf(ctx, key)
	dir, err := s.workspaces.Prepare(ctx, workspaceName(key), p.ID, sb.Storage, ws)
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
		if err := s.pruneWorkspace(ctx, e); err != nil {
			slog.Warn("deleting a private workspace", "name", e.Name, "error", err)
		}
	}
}

func (s *Server) pruneWorkspace(ctx context.Context, e workspaces.Entry) error {
	switch e.Name {
	case workspaceName(""):
		if started := s.mainSandbox.Load(); started != nil {
			return s.removeStorageBut(e, started.Storage)
		}
		return nil
	case workspaceName(agentkey.Anonymous):
		release, err := s.anonymous.stopIdle(agentkey.Anonymous)
		if err != nil || release == nil {
			return err
		}
		defer release()
		return s.removeStorageBut(e, s.sandboxes.of(ctx, agentkey.Anonymous).Storage)
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
	// The user may have started a turn since the list was read.
	if used, err := s.workspaces.Used(e.Name); err != nil || time.Since(used) < unusedWorkspaceTTL {
		return err
	}
	return s.workspaces.Remove(e.Name)
}

// removeStorageBut deletes the workspaces of e but the one of storage.
func (s *Server) removeStorageBut(e workspaces.Entry, storage string) error {
	var errs []error
	for _, st := range e.Storage {
		if st != storage {
			errs = append(errs, s.workspaces.RemoveStorage(e.Name, st))
		}
	}
	return errors.Join(errs...)
}
