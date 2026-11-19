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

	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/a2aproject/a2a-serve/internal/access"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agentkey"
	"github.com/a2aproject/a2a-serve/internal/auth"
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
	if _, ok := agentkey.UserOf(agentkey.Of(p, true)); !ok {
		return
	}
	// The rules match the user, whichever of its callers a call is for.
	key := agentkey.User(p.ID)
	p.Caller = ""
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
// agent key: the owner for none, any guest, or the user. It
// reports whether the server verified the user's identity; one rebuilt from
// the principal ID alone lacks what only a request proves. It fails when
// the store can't tell, so that a read error changes no container.
func (s *sandboxes) principalOf(ctx context.Context, key string) (auth.Principal, bool, error) {
	if key == "" {
		return auth.Owner(), true, nil
	}
	if key == agentkey.Guest {
		return auth.Principal{ID: auth.GuestID, Role: auth.RoleGuest}, true, nil
	}
	id, _ := agentkey.UserOf(key)
	s.mu.Lock()
	u, ok := s.users[agentkey.User(id)]
	s.mu.Unlock()
	if ok {
		return u.p, true, nil
	}
	rebuilt := auth.Principal{ID: id, Role: auth.RoleMember, Identity: auth.IdentityOf(id)}
	data, err := s.store.UserIdentity(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return rebuilt, false, nil
	}
	if err != nil {
		return auth.Principal{}, false, fmt.Errorf("reading the identity of %s: %w", id, err)
	}
	p := rebuilt
	if err := json.Unmarshal([]byte(data), &p.Identity); err != nil {
		slog.Warn("reading the identity of a user", "user", id, "error", err)
		return rebuilt, false, nil
	}
	s.mu.Lock()
	s.keepLocked(key, rememberedUser{p: p})
	s.mu.Unlock()
	return p, true, nil
}

// of returns the sandbox of the container of an agent key. A user whose
// identity the server hasn't verified gets the least that it could match.
func (s *sandboxes) of(ctx context.Context, key string) (access.Sandbox, error) {
	_, sb, err := s.grant(ctx, key)
	return sb, err
}

// grant returns whom the sandbox rules match for the container of an agent
// key, and the sandbox they give it.
func (s *sandboxes) grant(ctx context.Context, key string) (auth.Principal, access.Sandbox, error) {
	p, verified, err := s.principalOf(ctx, key)
	if err != nil {
		return auth.Principal{}, access.Sandbox{}, err
	}
	if !verified {
		return p, access.SandboxPolicyOf(s.config()).ResolveUnverified(p), nil
	}
	return p, s.resolve(p), nil
}

// resolve returns the sandbox of the container that p's conversations run
// in.
func (s *sandboxes) resolve(p auth.Principal) access.Sandbox {
	return access.SandboxPolicyOf(s.config()).Resolve(p)
}

// Refusals of turns in agent containers.
var (
	errOtherContainer = errors.New("this conversation runs in the agent container of another user, or of a role you had before; start a new conversation")
	errSandboxCovers  = errors.New("this conversation runs in the agent container of the user who started it, which gets more storage or secrets than your sandbox rule gives you; start a conversation of your own")
	errSandboxPrivate = errors.New("this conversation runs in the private workspace of the user who started it; start a conversation of your own")
)

// admitCaller lets the caller in ctx write into the container of an agent
// key, before the container itself is checked. Only an owner, or someone in a group chat, may write
// into the container of another user, such as one of a role the caller had
// before. They need a sandbox that covers what this container gets now and
// got when it started, and only an owner may write into another user's
// private workspace.
func (s *Server) admitCaller(ctx context.Context, key string, _ *a2asrv.ExecutorContext) (func(), error) {
	caller, ok := auth.Caller(ctx)
	// A posted turn, such as a schedule run, acts for a user with what the
	// server stored of them, which it didn't verify now.
	if ok && !auth.IsPosted(ctx) {
		s.sandboxes.remember(ctx, caller)
	}
	if ok && !sameProcessUser(key, agentkey.Of(caller, ok)) {
		want, err := s.sandboxes.of(ctx, key)
		if err != nil {
			return nil, err
		}
		// The people in a group chat share its conversation, which runs in
		// the container of whoever started it.
		if !caller.IsOwner() && !auth.IsGroupChat(ctx) {
			return nil, errOtherContainer
		}
		if err := s.admitOther(caller, key, want); err != nil {
			return nil, err
		}
	}
	return func() {}, nil
}

// holdBudget counts a guest's turn against the budget of guests until
// release. It implements the Hold hook of the agent.
func (s *Server) holdBudget(ctx context.Context, _ string, _ *a2asrv.ExecutorContext) (func(), error) {
	if p, ok := auth.Caller(ctx); ok && p.Role == auth.RoleGuest {
		return s.budget.run(budgetListener(p))
	}
	return func() {}, nil
}

// admitOther checks a caller's turn in the container of another agent key,
// which gets want under the rules in force.
func (s *Server) admitOther(caller auth.Principal, key string, want access.Sandbox) error {
	mine := s.sandboxes.resolve(caller)
	if !mine.Covers(want) {
		return errSandboxCovers
	}
	if running, ok := s.startedWith(key); ok && !mine.Covers(running) {
		return errSandboxCovers
	}
	if !caller.IsOwner() && (want.Storage == agentdir.StorageOwn || want.Storage == agentdir.StorageEmpty) {
		return errSandboxPrivate
	}
	return nil
}

// sameProcessUser reports whether two agent keys are the processes of one
// user, whose sandbox rules are the same: a user's own process and those
// it has for the people behind its calls.
func sameProcessUser(a, b string) bool {
	if a == b {
		return true
	}
	ida, oka := agentkey.UserOf(a)
	idb, okb := agentkey.UserOf(b)
	return oka && okb && ida == idb
}

// sandboxNotes returns notes with one added when the owner's container
// runs with another sandbox than the rule of self gives now.
func (s *Server) sandboxNotes(notes []string) []string {
	if started, ok := s.startedWith(""); ok && !started.Equal(s.sandboxes.resolve(auth.Owner())) {
		notes = append(slices.Clone(notes), "your agent container applies the changed sandbox rule of self after `a2a-serve restart`")
	}
	return notes
}

// workspaceName names the private workspaces of the container of an agent
// key.
func workspaceName(key string) string {
	if key == "" {
		return "owner"
	}
	if _, ok := agentkey.UserOf(key); ok {
		return userProcessName(key)
	}
	return key
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
// of the owner's and the guests' container whose sandbox no longer has
// their storage. `pair rm` deletes those of pairings. The sandbox of a
// user's container depends on how they call, so it isn't checked here.
func (s *Server) pruneWorkspaces(ctx context.Context) {
	entries, err := s.copies.List()
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
		if started, ok := s.startedWith(""); ok {
			return s.removeStorageBut(e, started.Storage)
		}
		return nil
	case workspaceName(agentkey.Guest):
		release, ok, err := s.engine.HoldIdle(ctx, agentkey.Guest)
		if err != nil || !ok {
			return err
		}
		defer release()
		return s.removeStorageBut(e, s.sandboxes.resolve(auth.Principal{ID: auth.GuestID, Role: auth.RoleGuest}).Storage)
	}
	key := agentkey.User(e.Principal)
	if caller, ok := strings.CutPrefix(e.Name, agentdir.CallerName(e.Principal, "")); ok {
		key = agentkey.CallerKey(e.Principal, caller)
	}
	pairing := strings.HasPrefix(e.Principal, auth.PairPrefix) || auth.IdentityOf(e.Principal).WebhookNode != ""
	if e.Principal == "" || workspaceName(key) != e.Name || pairing || time.Since(e.Used) < unusedWorkspaceTTL {
		return nil
	}
	release, ok, err := s.engine.HoldIdle(ctx, key)
	if err != nil || !ok {
		return err
	}
	defer release()
	// The user may have started a turn since the list was read.
	if used, err := s.copies.Used(e.Name); err != nil || time.Since(used) < unusedWorkspaceTTL {
		return err
	}
	return s.copies.Remove(e.Name)
}

// removeStorageBut deletes the workspaces of e but the one of storage.
func (s *Server) removeStorageBut(e workspaces.Entry, storage string) error {
	var errs []error
	for _, st := range e.Storage {
		if st != storage {
			errs = append(errs, s.copies.RemoveStorage(e.Name, st))
		}
	}
	return errors.Join(errs...)
}
