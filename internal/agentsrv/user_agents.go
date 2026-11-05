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

	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// userAgents are the agent processes the server started for users, by
// agent key. a2aacp has no way to stop the process of one key, so removing
// a user closes the process the server started, and keeps a new one from
// starting until the removal ends.
type userAgents struct {
	mu   sync.Mutex
	keys map[string]*userAgent
}

type userAgent struct {
	// startMu is held while the process starts and while its user is
	// removed.
	startMu sync.Mutex
	// proc is guarded by userAgents.mu.
	proc io.Closer
}

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
func (u *userAgents) started(a *userAgent, proc io.Closer) {
	u.mu.Lock()
	defer u.mu.Unlock()
	a.proc = proc
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

// stopUsers cancels the unfinished tasks of principals and stops their
// agent processes, which don't start again until release is called. It
// implements [pairings.Users].
type stopUsers struct {
	tasks  *store.TaskStore
	sender *handlerSender
	users  *userAgents
	posted *postedTurns
}

func (u stopUsers) Stop(ctx context.Context, principals ...string) (func(), error) {
	// Tasks are canceled before the starts are held: a task that waits for
	// its agent process to start would keep its cancellation waiting.
	u.posted.dropUsers(principals)
	as := auth.InProcess(ctx, auth.Owner())
	for _, p := range principals {
		ids, err := u.tasks.UnfinishedOf(ctx, p)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			if _, err := u.sender.CancelTask(as, &a2a.CancelTaskRequest{ID: id}); err != nil && !errors.Is(err, a2a.ErrTaskNotCancelable) {
				return nil, fmt.Errorf("canceling task %s: %w", id, err)
			}
		}
	}
	keys := make([]string, len(principals))
	for i, p := range principals {
		keys[i] = userAgentKeyPrefix + p
	}
	return u.users.stop(keys...)
}

// userHomes deletes the homes of users' agent processes and forgets their
// pins. It implements [pairings.Homes].
type userHomes struct{ s *Server }

func (h userHomes) RemoveUserHome(id string) error {
	h.s.pins.Unpin(h.s.dir.UserHomeDir(id))
	return h.s.dir.RemoveUserHome(id)
}

func (h userHomes) UserHomeDir(id string) string { return h.s.dir.UserHomeDir(id) }

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
