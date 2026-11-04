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
	"iter"
	"log/slog"
	"strings"
	"sync"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/cryptoutil"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// CallClaims records who calls through the agents URL of each
// conversation: the harness in the conversation's running turn, for the
// user who started it. Calls to other agents act for that user, so this is
// what keeps one conversation's calls from acting for another's user.
type CallClaims struct {
	store  *store.Store
	bridge harnessBridge

	handleMu sync.Mutex

	turnsMu sync.Mutex
	// turns has the claims of the running turn of each conversation; a2aacp
	// runs one turn of a conversation at a time.
	turns map[string]*callerClaims
	// starting counts the turns of each conversation which run but haven't
	// begun yet.
	starting map[string]int
	// turnsChanged is closed when turns or starting change.
	turnsChanged chan struct{}
	// paused has the claims of the turn of each conversation which waits
	// for an answer, so that the answer resumes it for the same user.
	paused map[string]pausedTurn
}

// CallClaimsDeps are the dependencies of [CallClaims].
type CallClaimsDeps struct {
	Store *store.Store
	// Bridge is how the harness reaches the server, which agents URLs use.
	Bridge harnessBridge
}

func newCallClaims(deps CallClaimsDeps) *CallClaims {
	return &CallClaims{
		store:        deps.Store,
		bridge:       deps.Bridge,
		turns:        map[string]*callerClaims{},
		starting:     map[string]int{},
		paused:       map[string]pausedTurn{},
		turnsChanged: make(chan struct{}),
	}
}

// callerClaims is who calls through the agents URL of a conversation: the
// harness in the conversation's running turn, for the user who started it.
type callerClaims struct {
	ContextID string
	User      auth.Principal
}

// handleAlphabet avoids characters that are easy to confuse.
const handleAlphabet = "abcdefghijkmnpqrstuvwxyz23456789"

// handleLen is the length of handles. One harness serves every
// conversation, so a handle must not be guessable from another one.
const handleLen = 16

// handle returns the handle of the agents URL of a conversation, the same
// one for its whole life, also across restarts.
func (cl *CallClaims) handle(ctx context.Context, contextID string) (string, error) {
	cl.handleMu.Lock()
	defer cl.handleMu.Unlock()
	for {
		h, err := cl.store.HandleOf(ctx, contextID)
		if err == nil {
			return h.Handle, nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return "", err
		}
		h = store.Handle{Handle: cryptoutil.RandomString(handleLen, handleAlphabet), ContextID: contextID}
		err = cl.store.PutHandle(ctx, h)
		if errors.Is(err, store.ErrExists) {
			continue
		}
		return h.Handle, err
	}
}

var (
	errUnknownHandle = errors.New("unknown agents URL: use the one from the [a2a agents] line of this conversation")
	errNoTurn        = errors.New("this conversation has no running turn, and its agents URL works only while one runs")
)

// claimsOf returns the claims of the running turn of a handle's
// conversation: [errUnknownHandle] for an unknown handle, and [errNoTurn]
// when no turn runs. It waits for a turn which is starting.
func (cl *CallClaims) claimsOf(ctx context.Context, handle string) (callerClaims, error) {
	h, err := cl.store.HandleByName(ctx, handle)
	if errors.Is(err, store.ErrNotFound) {
		return callerClaims{}, errUnknownHandle
	}
	if err != nil {
		return callerClaims{}, fmt.Errorf("loading agents URL handle: %w", err)
	}
	for {
		cl.turnsMu.Lock()
		c, starting, changed := cl.turns[h.ContextID], cl.starting[h.ContextID], cl.turnsChanged
		cl.turnsMu.Unlock()
		if c != nil {
			return *c, nil
		}
		if starting == 0 {
			return callerClaims{}, errNoTurn
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return callerClaims{}, ctx.Err()
		}
	}
}

// startingTurn marks a turn of a conversation as starting, until done is
// called: a2aacp prompts the harness before it yields the working update
// which begins the turn, so the harness can call its agents URL first.
func (cl *CallClaims) startingTurn(contextID string) (done func()) {
	cl.turnsMu.Lock()
	cl.starting[contextID]++
	cl.turnsMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			cl.turnsMu.Lock()
			defer cl.turnsMu.Unlock()
			if cl.starting[contextID]--; cl.starting[contextID] == 0 {
				delete(cl.starting, contextID)
			}
			cl.notifyTurnsLocked()
		})
	}
}

// notifyTurnsLocked wakes the calls waiting for a turn to begin.
func (cl *CallClaims) notifyTurnsLocked() {
	close(cl.turnsChanged)
	cl.turnsChanged = make(chan struct{})
}

// pausedTurn is a turn which waits for an answer to a question of its task.
type pausedTurn struct {
	task   a2a.TaskID
	claims callerClaims
}

// beginTurn opens the agents URL of c's conversation to calls with c, until
// end is called.
func (cl *CallClaims) beginTurn(c callerClaims) (end func()) {
	turn := &c
	cl.turnsMu.Lock()
	cl.turns[c.ContextID] = turn
	cl.notifyTurnsLocked()
	cl.turnsMu.Unlock()
	return func() {
		cl.turnsMu.Lock()
		defer cl.turnsMu.Unlock()
		if cl.turns[c.ContextID] == turn {
			delete(cl.turns, c.ContextID)
		}
	}
}

// duringTurn opens the agents URL of the conversation while the harness
// runs a turn of it: from the turn's working update, which a2aacp sends once
// the turn holds the conversation, until its events end. A turn which stops
// for a question resumes for its user, whoever answers.
func (cl *CallClaims) duringTurn(ctx context.Context, execCtx *a2asrv.ExecutorContext, events iter.Seq2[a2a.Event, error]) iter.Seq2[a2a.Event, error] {
	c := cl.turnClaims(ctx, execCtx)
	return func(yield func(a2a.Event, error) bool) {
		var (
			end   func()
			state a2a.TaskState
		)
		started := cl.startingTurn(c.ContextID)
		defer func() {
			started()
			if end != nil {
				end()
			}
			if state == a2a.TaskStateInputRequired {
				cl.turnsMu.Lock()
				cl.paused[c.ContextID] = pausedTurn{task: execCtx.TaskID, claims: c}
				cl.turnsMu.Unlock()
			}
		}()
		for ev, err := range events {
			switch e := ev.(type) {
			case *a2a.Task:
				state = e.Status.State
			case *a2a.TaskStatusUpdateEvent:
				state = e.Status.State
				if end == nil && state == a2a.TaskStateWorking {
					end = cl.beginTurn(c)
					started()
				}
			}
			if !yield(ev, err) {
				return
			}
		}
	}
}

// turnClaims returns the claims of the turn execCtx runs: those of the
// paused turn it answers, or else of its caller.
func (cl *CallClaims) turnClaims(ctx context.Context, execCtx *a2asrv.ExecutorContext) callerClaims {
	cl.turnsMu.Lock()
	paused, ok := cl.paused[execCtx.ContextID]
	ok = ok && paused.task == execCtx.TaskID
	if ok {
		delete(cl.paused, execCtx.ContextID)
	}
	cl.turnsMu.Unlock()
	if ok && execCtx.StoredTask != nil && execCtx.StoredTask.Status.State == a2a.TaskStateInputRequired {
		return paused.claims
	}
	c := callerClaims{ContextID: execCtx.ContextID}
	if caller, ok := auth.Caller(ctx); ok {
		c.User = caller
	} else if execCtx.User != nil {
		c.User = auth.Principal{ID: execCtx.User.Name}
	}
	return c
}

// agentsURL is where the harness calls other agents in one conversation:
// <agents URL>/<name> is an A2A agent. The handle in the path stands for the
// conversation; the user and the hop count are those of its running turn.
func (cl *CallClaims) agentsURL(ctx context.Context, contextID string) (string, error) {
	base := cl.bridge.url
	if cl.bridge.socket != "" {
		base = socketHost
	}
	if base == "" {
		return "", errors.New("the harness listener is not running")
	}
	h, err := cl.handle(ctx, contextID)
	if err != nil {
		return "", err
	}
	return base + "/" + h, nil
}

// withAgentsLine returns execCtx with the agents line added before the
// caller's message, for a new prompt of a caller who may call agents.
// Answers to the harness's own questions are left alone, because the
// harness parses them.
func (cl *CallClaims) withAgentsLine(ctx context.Context, execCtx *a2asrv.ExecutorContext) *a2asrv.ExecutorContext {
	if execCtx.Message == nil || (execCtx.StoredTask != nil && execCtx.StoredTask.Status.State == a2a.TaskStateInputRequired) {
		return execCtx
	}
	if caller, ok := auth.Caller(ctx); !ok || !mayCallAgents(caller) {
		return execCtx
	}
	agents, err := callableAgents(ctx, cl.store)
	if err != nil {
		slog.Warn("listing callable agents", "error", err)
		return execCtx
	}
	if len(agents) == 0 {
		return execCtx
	}
	u, err := cl.agentsURL(ctx, execCtx.ContextID)
	if err != nil {
		slog.Warn("creating the agents URL", "error", err)
		return execCtx
	}
	names := make([]string, len(agents))
	for i, a := range agents {
		names[i] = a.Name
	}
	line := fmt.Sprintf("%s Agents URL for this conversation: %s (agents: %s; call one with a2a send -a %s/<name>)",
		agentsLinePrefix, u, strings.Join(names, ", "), u)
	ec := *execCtx
	msg := *execCtx.Message
	msg.Parts = append([]*a2a.Part{a2a.NewTextPart(line)}, msg.Parts...)
	ec.Message = &msg
	return &ec
}
