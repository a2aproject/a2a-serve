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
	"fmt"
	"io"
	"slices"
	"sync"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/a2aproject/a2a-serve/internal/access"
	"github.com/a2aproject/a2a-serve/internal/agentkey"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/chat"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// userAgents are the agent processes the server started for users, by
// agent key. a2aacp has no way to stop the process of one key, so removing
// a user closes the process the server started, and keeps a new one from
// starting until the removal ends.
// They also keep the count of processes at maxUserAgents: a2aacp checks
// the count before a process starts, so concurrent starts could exceed it.
type userAgents struct {
	mu   sync.Mutex
	keys map[string]*userAgent
	// starting counts the processes that start now.
	starting int
}

type userAgent struct {
	// startMu is held while the process starts and while its user is
	// removed.
	startMu sync.Mutex
	// proc, starting and turns are guarded by userAgents.mu. turns counts
	// the turns that use the process, including those waiting for an answer.
	proc     io.Closer
	starting bool
	turns    int
	// sandbox is what the running process got when it started.
	sandbox access.Sandbox
}

var errAgentsBusy = fmt.Errorf("all %d agents are busy, send the message again later", maxUserAgents)

// get returns the entry of key, creating it if needed.
func (u *userAgents) get(key string) *userAgent {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.keys == nil {
		u.keys = map[string]*userAgent{}
	}
	a := u.keys[key]
	if a == nil {
		a = &userAgent{}
		u.keys[key] = a
	}
	return a
}

// started records proc as the running process of a.
// reserve counts a process of a as starting until release, or returns
// errAgentsBusy if maxUserAgents processes run or start.
func (u *userAgents) reserve(a *userAgent) (release func(), err error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.runningLocked()+u.starting >= maxUserAgents {
		return nil, errAgentsBusy
	}
	u.starting++
	a.starting = true
	var once sync.Once
	return func() {
		once.Do(func() {
			u.mu.Lock()
			defer u.mu.Unlock()
			u.starting--
			a.starting = false
		})
	}, nil
}

func (u *userAgents) runningLocked() int {
	n := 0
	for _, a := range u.keys {
		if a.proc != nil {
			n++
		}
	}
	return n
}

// turn counts a turn in the process of key until end.
func (u *userAgents) turn(key string) (end func()) {
	a := u.get(key)
	u.mu.Lock()
	a.turns++
	u.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			u.mu.Lock()
			defer u.mu.Unlock()
			a.turns--
		})
	}
}

// hasRoom reports whether a turn in the process of key can run now: the
// process runs or starts, or it can start, possibly after an idle process
// stops.
func (u *userAgents) hasRoom(key string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if a := u.keys[key]; a != nil && (a.proc != nil || a.starting) {
		return true
	}
	running, idle := 0, 0
	for _, a := range u.keys {
		if a.proc == nil {
			continue
		}
		running++
		if a.turns == 0 {
			idle++
		}
	}
	return running+u.starting < maxUserAgents || idle > 0
}

func (u *userAgents) started(a *userAgent, proc io.Closer, sb access.Sandbox) {
	u.mu.Lock()
	defer u.mu.Unlock()
	a.proc, a.sandbox = proc, sb
}

// runningKeys returns the keys whose processes run.
func (u *userAgents) runningKeys() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	var out []string
	for k, a := range u.keys {
		if a.proc != nil {
			out = append(out, k)
		}
	}
	return out
}

// sandboxOf returns the sandbox the process of key started with, if it
// runs.
func (u *userAgents) sandboxOf(key string) (access.Sandbox, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	a := u.keys[key]
	if a == nil || a.proc == nil {
		return access.Sandbox{}, false
	}
	return a.sandbox, true
}

// admit counts a turn in the process of key, which the rules give want,
// until release. A process that runs with another sandbox stops first if
// no turn uses it. If one does, only an answer to a waiting task may use
// it; other turns get errSandboxRestarting.
func (u *userAgents) admit(key string, want access.Sandbox, answer bool) (release func(), err error) {
	a := u.get(key)
	u.mu.Lock()
	if a.proc != nil && !a.sandbox.Equal(want) && !answer {
		if a.turns > 0 || !a.startMu.TryLock() {
			u.mu.Unlock()
			return nil, errSandboxRestarting
		}
		proc := a.proc
		a.proc = nil
		u.mu.Unlock()
		err := proc.Close()
		a.startMu.Unlock()
		if err != nil {
			return nil, fmt.Errorf("stopping the agent process %s, whose sandbox changed: %w", key, err)
		}
		u.mu.Lock()
	}
	a.turns++
	u.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			u.mu.Lock()
			defer u.mu.Unlock()
			a.turns--
		})
	}, nil
}

// closeStale closes the process of key if it runs with another sandbox than
// want, no turn uses it and it doesn't start or stop now. It holds the
// process from starting until it closed, so that a new process doesn't
// start under the same container name. It reports whether it closed one.
func (u *userAgents) closeStale(key string, want access.Sandbox) (bool, error) {
	u.mu.Lock()
	a := u.keys[key]
	u.mu.Unlock()
	if a == nil || !a.startMu.TryLock() {
		return false, nil
	}
	defer a.startMu.Unlock()
	u.mu.Lock()
	if a.proc == nil || a.turns > 0 || a.sandbox.Equal(want) {
		u.mu.Unlock()
		return false, nil
	}
	proc := a.proc
	a.proc = nil
	u.mu.Unlock()
	return true, proc.Close()
}

// closeNarrowed closes the process of key if it runs with a sandbox that
// want doesn't cover, whatever its turns, while holding it from starting.
// It keeps the entry of key, which counts the turns. It reports whether it
// closed one.
func (u *userAgents) closeNarrowed(key string, want access.Sandbox) (bool, error) {
	a := u.get(key)
	a.startMu.Lock()
	defer a.startMu.Unlock()
	u.mu.Lock()
	if a.proc == nil || want.Covers(a.sandbox) {
		u.mu.Unlock()
		return false, nil
	}
	proc := a.proc
	a.proc = nil
	u.mu.Unlock()
	return true, proc.Close()
}

// stopIdle holds the process of key from starting until release is called,
// unless the process runs or starts: then release is nil.
func (u *userAgents) stopIdle(key string) (release func(), err error) {
	a := u.get(key)
	if !a.startMu.TryLock() {
		return nil, nil
	}
	u.mu.Lock()
	busy := a.proc != nil || a.starting
	u.mu.Unlock()
	if busy {
		a.startMu.Unlock()
		return nil, nil
	}
	return a.startMu.Unlock, nil
}

// exited forgets proc once it is closed.
func (u *userAgents) exited(a *userAgent, proc io.Closer) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if a.proc == proc {
		a.proc = nil
	}
}

// stop closes the processes of keys, and holds them from starting until
// release is called, which also forgets the keys.
func (u *userAgents) stop(keys ...string) (release func(), err error) {
	keys = slices.Sorted(slices.Values(keys))
	held := make([]*userAgent, 0, len(keys))
	release = func() {
		u.mu.Lock()
		for i, a := range held {
			if u.keys[keys[i]] == a {
				delete(u.keys, keys[i])
			}
		}
		u.mu.Unlock()
		for _, a := range held {
			a.startMu.Unlock()
		}
	}
	var errs []error
	for _, key := range keys {
		a := u.get(key)
		a.startMu.Lock()
		held = append(held, a)
		u.mu.Lock()
		proc := a.proc
		u.mu.Unlock()
		if proc == nil {
			continue
		}
		if err := proc.Close(); err != nil {
			errs = append(errs, fmt.Errorf("stopping the agent process %s: %w", key, err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

// stopUsers cancels the unfinished tasks that removing principals deletes
// and stops their agent processes, which don't start again until release
// is called. It implements [pairings.Users].
type stopUsers struct {
	tasks     *store.TaskStore
	sender    *handlerSender
	users     *userAgents
	posted    *chat.PostedTurns
	sandboxes *sandboxes
}

func (u stopUsers) Stop(ctx context.Context, principals ...string) (func(), error) {
	// Tasks are canceled before the starts are held: a task that waits for
	// its agent process to start would keep its cancellation waiting.
	u.posted.DropUsers(principals)
	as := auth.InProcess(ctx, auth.Owner())
	keys := make([]string, len(principals))
	for i, p := range principals {
		keys[i] = agentkey.User(p)
		ids, err := u.tasks.UnfinishedRemoved(ctx, store.PairingUser{Principal: p, AgentKey: keys[i]})
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			if _, err := u.sender.CancelTask(as, &a2a.CancelTaskRequest{ID: id}); err != nil && !errors.Is(err, a2a.ErrTaskNotCancelable) {
				return nil, fmt.Errorf("canceling task %s: %w", id, err)
			}
		}
	}
	release, err := u.users.stop(keys...)
	if err != nil {
		return nil, err
	}
	u.sandboxes.forget(keys...)
	return release, nil
}

// userHomes deletes the homes of users' agent processes and forgets their
// pins. It implements [pairings.Homes].
type userHomes struct{ s *Server }

func (h userHomes) RemoveUserHome(id string) error {
	h.s.pins.Unpin(h.s.dir.UserHomeDir(id))
	h.s.forgetStorageNote(h.s.dir.UserHomeDir(id))
	return h.s.dir.RemoveUserHome(id)
}

func (h userHomes) UserHomeDir(id string) string { return h.s.dir.UserHomeDir(id) }

func (h userHomes) UserWorkspacesDir(id string) string { return h.s.dir.UserWorkspacesDir(id) }

// errRemovedUser is returned when the agent process of a user whose
// pairing or webhook node was removed would start.
var errRemovedUser = errors.New("this user's pairing was removed")

// checkUser returns errRemovedUser if the principal id names a pairing or
// webhook node that no longer exists.
func (s *Server) checkUser(ctx context.Context, id string) error {
	identity := auth.IdentityOf(id)
	var err error
	switch {
	case identity.Pairing != "":
		_, err = s.store.Pairing(ctx, identity.Pairing)
	case identity.WebhookNode != "":
		_, err = s.store.Node(ctx, identity.WebhookNode)
	}
	if errors.Is(err, store.ErrNotFound) {
		return errRemovedUser
	}
	return err
}
