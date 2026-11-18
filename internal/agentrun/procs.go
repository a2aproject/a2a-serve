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

package agentrun

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"
)

// procs are the agent processes of a pool, by key. a2aacp has no way to
// stop the process of one key, so Stop closes the process the runner
// started, and keeps a new one from starting until it is released.
// They also keep the count of processes at max: a2aacp checks the count
// before a process starts, so concurrent starts could exceed it.
type procs struct {
	// max bounds the processes that run or start; zero doesn't.
	max int

	mu   sync.Mutex
	keys map[string]*proc
	// starting counts the processes that start now.
	starting int
}

type proc struct {
	// startMu is held while the process starts and while it is stopped.
	startMu sync.Mutex
	// childrenMu is held for reading while a process whose parent this is
	// starts, and for writing while this one is stopped.
	childrenMu sync.RWMutex
	// parent is the key of the process whose stop also stops this one.
	parent string
	// proc, starting and turns are guarded by userAgents.mu. turns counts
	// the turns that use the process, including those waiting for an answer.
	proc     io.Closer
	starting bool
	turns    int
	// idle is closed when turns drops to zero, for waitIdle.
	idle chan struct{}
	// grant is what the running process got when it started.
	grant Grant
}

// AgentsBusyError refuses a turn while the processes of its pool are
// busy.
type AgentsBusyError struct{ Max int }

func (e *AgentsBusyError) Error() string {
	return fmt.Sprintf("all %d agents are busy, send the message again later", e.Max)
}

// get returns the entry of key, creating it if needed.
func (u *procs) get(key string) *proc {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.keys == nil {
		u.keys = map[string]*proc{}
	}
	a := u.keys[key]
	if a == nil {
		a = &proc{}
		u.keys[key] = a
	}
	return a
}

// setParent records parent as the key whose stop also stops a.
func (u *procs) setParent(a *proc, parent string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	a.parent = parent
}

// knows reports whether key has an entry.
func (u *procs) knows(key string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.keys[key] != nil
}

// children returns the keys of the processes whose parent is key.
func (u *procs) children(key string) []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	var out []string
	for k, a := range u.keys {
		if a.parent == key && k != key {
			out = append(out, k)
		}
	}
	return out
}

// holdChildStarts holds the starts of the processes whose parents are
// keys, until release is called.
func (u *procs) holdChildStarts(keys ...string) (release func()) {
	keys = slices.Sorted(slices.Values(keys))
	held := make([]*proc, len(keys))
	for i, key := range keys {
		held[i] = u.get(key)
		held[i].childrenMu.Lock()
	}
	return func() {
		for _, a := range held {
			a.childrenMu.Unlock()
		}
	}
}

// forget drops the entry of key if no process of it runs or starts and no
// turn uses it.
func (u *procs) forget(key string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if a := u.keys[key]; a != nil && a.proc == nil && !a.starting && a.turns == 0 {
		delete(u.keys, key)
	}
}

// reserve counts a process of a as starting until release, or returns an
// [AgentsBusyError] if max processes run or start.
func (u *procs) reserve(a *proc) (release func(), err error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.max > 0 && u.runningLocked()+u.starting >= u.max {
		return nil, &AgentsBusyError{Max: u.max}
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

func (u *procs) runningLocked() int {
	n := 0
	for _, a := range u.keys {
		if a.proc != nil {
			n++
		}
	}
	return n
}

// turn counts a turn in the process of key until end.
func (u *procs) turn(key string) (end func()) {
	a := u.get(key)
	u.mu.Lock()
	a.turns++
	u.mu.Unlock()
	return u.endTurnOnce(a)
}

// endTurnOnce returns a function that ends a turn of a once.
func (u *procs) endTurnOnce(a *proc) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			u.mu.Lock()
			defer u.mu.Unlock()
			a.turns--
			if a.turns == 0 && a.idle != nil {
				close(a.idle)
				a.idle = nil
			}
		})
	}
}

// waitIdle waits up to d for the turns in the process of key to end, and
// reports whether they did.
func (u *procs) waitIdle(ctx context.Context, key string, d time.Duration) bool {
	a := u.get(key)
	u.mu.Lock()
	if a.turns == 0 {
		u.mu.Unlock()
		return true
	}
	if a.idle == nil {
		a.idle = make(chan struct{})
	}
	idle := a.idle
	u.mu.Unlock()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-idle:
		return true
	case <-t.C:
	case <-ctx.Done():
	}
	return false
}

// hasRoom reports whether a turn in the process of key can run now: the
// process runs or starts, or it can start, possibly after an idle process
// stops.
func (u *procs) hasRoom(key string) bool {
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
	return u.max == 0 || running+u.starting < u.max || idle > 0
}

// started records proc as the running process of a, which got g.
func (u *procs) started(a *proc, proc io.Closer, g Grant) {
	u.mu.Lock()
	defer u.mu.Unlock()
	a.proc, a.grant = proc, g
}

// runningKeys returns the keys whose processes run.
func (u *procs) runningKeys() []string {
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

// grantOf returns the grant the process of key started with, if it runs.
func (u *procs) grantOf(key string) (Grant, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	a := u.keys[key]
	if a == nil || a.proc == nil {
		return Grant{}, false
	}
	return a.grant, true
}

// admit counts a turn in the process of key, which now gets want, until
// release. A process that runs with another grant stops first if no turn
// uses it. If one does, only an answer to a waiting task may use it; other
// turns get ErrGrantChanged.
func (u *procs) admit(key string, want Grant, answer bool) (release func(), err error) {
	a := u.get(key)
	u.mu.Lock()
	if a.proc == nil {
		// A closer unsets the process before it closes it, holding startMu
		// until it did; a turn admitted before that would run in the
		// process that exits.
		u.mu.Unlock()
		a.startMu.Lock()
		a.startMu.Unlock() //nolint:staticcheck // waits for a close in progress
		u.mu.Lock()
	}
	for a.proc != nil && !a.grant.Equal(want) && !answer {
		if a.turns > 0 {
			u.mu.Unlock()
			return nil, ErrGrantChanged
		}
		if !a.startMu.TryLock() {
			// A closer or a start holds the process; look again once it
			// is done.
			u.mu.Unlock()
			a.startMu.Lock()
			a.startMu.Unlock() //nolint:staticcheck // waits for a close or start in progress
			u.mu.Lock()
			continue
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
	return u.endTurnOnce(a), nil
}

// closeStale closes the process of key if it runs with another grant than
// want, no turn uses it and it doesn't start or stop now. It holds the
// process from starting until it closed, so that a new process doesn't
// start under the same container name. It reports whether it closed one.
func (u *procs) closeStale(key string, want Grant) (bool, error) {
	u.mu.Lock()
	a := u.keys[key]
	u.mu.Unlock()
	if a == nil || !a.startMu.TryLock() {
		return false, nil
	}
	defer a.startMu.Unlock()
	u.mu.Lock()
	if a.proc == nil || a.turns > 0 || a.grant.Equal(want) {
		u.mu.Unlock()
		return false, nil
	}
	proc := a.proc
	a.proc = nil
	u.mu.Unlock()
	return true, proc.Close()
}

// closeNarrowed closes the process of key if it runs with a grant that
// want doesn't cover, whatever its turns, while holding it from starting.
// It keeps the entry of key, which counts the turns. It reports whether it
// closed one.
func (u *procs) closeNarrowed(key string, want Grant) (bool, error) {
	a := u.get(key)
	a.startMu.Lock()
	defer a.startMu.Unlock()
	u.mu.Lock()
	if a.proc == nil || want.Covers(a.grant) {
		u.mu.Unlock()
		return false, nil
	}
	proc := a.proc
	a.proc = nil
	u.mu.Unlock()
	return true, proc.Close()
}

// holdIdle holds the process of key from starting until release is called,
// unless the process runs or starts: then release is nil.
func (u *procs) holdIdle(key string) (release func()) {
	a := u.get(key)
	if !a.startMu.TryLock() {
		return nil
	}
	u.mu.Lock()
	busy := a.proc != nil || a.starting
	u.mu.Unlock()
	if busy {
		a.startMu.Unlock()
		return nil
	}
	return a.startMu.Unlock
}

// exited forgets proc once it is closed.
func (u *procs) exited(a *proc, proc io.Closer) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if a.proc == proc {
		a.proc = nil
	}
}

// stop closes the processes of keys, and holds them from starting until
// release is called, which also forgets the keys.
func (u *procs) stop(keys ...string) (release func(), err error) {
	keys = slices.Sorted(slices.Values(keys))
	held := make([]*proc, 0, len(keys))
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

// stop stops the processes of keys and of the keys under them, holding
// their starts until release, which also forgets them.
func (r *Runner) stop(ctx context.Context, keys []string) (release func(), err error) {
	byPool := map[*procs][]string{}
	for _, key := range keys {
		p, err := r.poolOf(ctx, key)
		if err != nil {
			return nil, fmt.Errorf("looking up the agent process %s: %w", key, err)
		}
		if p != nil {
			byPool[p] = append(byPool[p], key)
		}
	}
	var releases []func()
	releaseAll := func() {
		for _, rel := range slices.Backward(releases) {
			rel()
		}
	}
	for p, keys := range byPool {
		// Holding the starts of the processes under keys before they are
		// listed keeps a new one from starting unseen.
		releaseStarts := p.holdChildStarts(keys...)
		var children []string
		for _, key := range keys {
			children = append(children, p.children(key)...)
		}
		releaseStop, err := p.stop(append(slices.Clone(keys), children...)...)
		if err != nil {
			releaseStarts()
			releaseAll()
			return nil, err
		}
		releases = append(releases, releaseStarts, releaseStop)
	}
	return releaseAll, nil
}
