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
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/integrations/executor/a2aacp"

	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/cryptoutil"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// CallClaims records who calls through the agents URL of each
// conversation: the harness in the conversation's running turn, for the
// user who started it. Calls to other agents act for that user, so this is
// what keeps one conversation's calls from acting for another's user.
type CallClaims struct {
	store    *store.Store
	bridge   harnessBridge
	chain    *auth.Chain
	agentKey func(ctx context.Context, contextID string) (string, error)
	sessions a2aacp.SessionStore

	handleMu sync.Mutex

	turnsMu sync.Mutex
	// turns has the claims of the running turn of each conversation; a2aacp
	// runs one turn of a conversation at a time.
	turns map[string]*callerClaims

	linedMu sync.Mutex
	// lined has the ACP session of each conversation whose prompt had the
	// agents line, which the agent keeps in the session's history.
	lined map[string]*linedSession
	// lineEvery is how many tokens a session may see before the agents
	// line is added again, in case the agent compacted it away.
	lineEvery int
}

// CallClaimsDeps are the dependencies of [CallClaims].
type CallClaimsDeps struct {
	Store *store.Store
	// Bridge is how the harness reaches the server, which agents URLs use.
	Bridge harnessBridge
	// Chain resolves the user of an agent process.
	Chain *auth.Chain
	// AgentKey returns the agent key of the process a conversation runs in.
	// If nil, calls aren't capped to the user of the process.
	AgentKey func(ctx context.Context, contextID string) (string, error)
	// Sessions are the ACP sessions of conversations, which tell when the
	// agents line is due again.
	Sessions a2aacp.SessionStore
}

func newCallClaims(deps CallClaimsDeps) *CallClaims {
	return &CallClaims{
		store:     deps.Store,
		bridge:    deps.Bridge,
		chain:     deps.Chain,
		agentKey:  deps.AgentKey,
		sessions:  deps.Sessions,
		turns:     map[string]*callerClaims{},
		lined:     map[string]*linedSession{},
		lineEvery: agentsLineEvery,
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
// when no turn runs.
func (cl *CallClaims) claimsOf(ctx context.Context, handle string) (callerClaims, error) {
	h, err := cl.store.HandleByName(ctx, handle)
	if errors.Is(err, store.ErrNotFound) {
		return callerClaims{}, errUnknownHandle
	}
	if err != nil {
		return callerClaims{}, fmt.Errorf("loading agents URL handle: %w", err)
	}
	cl.turnsMu.Lock()
	defer cl.turnsMu.Unlock()
	c := cl.turns[h.ContextID]
	if c == nil {
		return callerClaims{}, errNoTurn
	}
	return *c, nil
}

// beginTurn opens the agents URL of c's conversation to calls with c, until
// end is called.
func (cl *CallClaims) beginTurn(c callerClaims) (end func()) {
	turn := &c
	cl.turnsMu.Lock()
	cl.turns[c.ContextID] = turn
	cl.turnsMu.Unlock()
	return func() {
		cl.turnsMu.Lock()
		defer cl.turnsMu.Unlock()
		if cl.turns[c.ContextID] == turn {
			delete(cl.turns, c.ContextID)
		}
	}
}

// turn opens the agents URL of the conversation while the harness works on
// a message, until it stops, also while the task waits for an answer. It is
// the [a2aacp.Config.Turn] of the harness, so a turn which stops for a
// question goes on for its caller, whoever answers.
func (cl *CallClaims) turn(ctx context.Context, execCtx *a2asrv.ExecutorContext) (end func()) {
	c := turnClaims(ctx, execCtx)
	user, err := cl.callerIn(ctx, c.ContextID, c.User)
	if err != nil {
		slog.Error("finding the agent process of a conversation", "context", c.ContextID, "error", err)
		user = auth.Principal{ID: c.User.ID}
	}
	c.User = user
	// a2aacp stores the session of a new conversation before the turn
	// begins. The line is recorded now, since the next message may come
	// before the turn's end is called.
	if hadAgentsLine(ctx) {
		cl.markLined(ctx, c.ContextID)
	}
	return cl.beginTurn(c)
}

// callerIn returns who user acts as through the agents URL of a
// conversation, capped to the user of the agent process it runs in.
func (cl *CallClaims) callerIn(ctx context.Context, contextID string, user auth.Principal) (auth.Principal, error) {
	if cl.agentKey == nil {
		return user, nil
	}
	key, err := cl.agentKey(ctx, contextID)
	if err != nil || key == "" {
		return user, err
	}
	process := auth.Principal{ID: auth.AnonymousID, Role: auth.RoleAnonymous}
	if id, ok := userOfKey(key); ok {
		if process, err = cl.chain.Refresh(ctx, id, auth.Identity{}); err != nil {
			slog.Warn("resolving the user of an agent process", "user", id, "error", err)
			process = auth.Principal{ID: id}
		}
	}
	return capToProcessUser(user, process), nil
}

// agentsLineEvery is the default of [CallClaims.lineEvery]. ACP doesn't
// tell when an agent compacts its context, so the line comes again after
// about as many tokens as fill a context window.
const agentsLineEvery = 200_000

// linedSession is the ACP session that had the agents line, and an estimate
// of the tokens it saw since.
type linedSession struct {
	id     string
	tokens int
}

// estimateTokens roughly estimates the tokens of the text of parts.
func estimateTokens(parts []*a2a.Part) int {
	n := 0
	for _, p := range parts {
		n += len(p.Text())
	}
	return n / 4
}

// addTokens counts tokens seen by the session of a conversation since its
// agents line.
func (cl *CallClaims) addTokens(contextID string, tokens int) {
	if tokens == 0 {
		return
	}
	cl.linedMu.Lock()
	defer cl.linedMu.Unlock()
	if l := cl.lined[contextID]; l != nil {
		l.tokens += tokens
	}
}

// countTokens counts the text of events, which the session of a
// conversation sees, as they pass.
func (cl *CallClaims) countTokens(contextID string, events iter.Seq2[a2a.Event, error]) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		for ev, err := range events {
			switch ev := ev.(type) {
			case *a2a.TaskArtifactUpdateEvent:
				cl.addTokens(contextID, estimateTokens(ev.Artifact.Parts))
			case *a2a.TaskStatusUpdateEvent:
				if ev.Status.Message != nil {
					cl.addTokens(contextID, estimateTokens(ev.Status.Message.Parts))
				}
			case *a2a.Message:
				cl.addTokens(contextID, estimateTokens(ev.Parts))
			}
			if !yield(ev, err) {
				return
			}
		}
	}
}

type agentsLineKey struct{}

// hadAgentsLine reports whether the prompt of the turn of ctx had the
// agents line.
func hadAgentsLine(ctx context.Context) bool {
	v, _ := ctx.Value(agentsLineKey{}).(bool)
	return v
}

// needsAgentsLine reports whether the ACP session of a conversation is new,
// hasn't had the agents line since the server started, or saw
// [CallClaims.lineEvery] tokens since.
func (cl *CallClaims) needsAgentsLine(ctx context.Context, contextID string) bool {
	sess, ok, err := cl.sessions.Get(ctx, contextID)
	if err != nil {
		slog.Warn("looking up the session of a conversation", "context", contextID, "error", err)
		return true
	}
	if !ok {
		return true
	}
	cl.linedMu.Lock()
	defer cl.linedMu.Unlock()
	l := cl.lined[contextID]
	return l == nil || l.id != sess.ID || l.tokens >= cl.lineEvery
}

// markLined records that the ACP session of a conversation had the agents
// line.
func (cl *CallClaims) markLined(ctx context.Context, contextID string) {
	sess, ok, err := cl.sessions.Get(ctx, contextID)
	if err != nil || !ok {
		slog.Warn("looking up the session of a conversation", "context", contextID, "error", err)
		return
	}
	cl.linedMu.Lock()
	defer cl.linedMu.Unlock()
	cl.lined[contextID] = &linedSession{id: sess.ID}
}

// forgetPruned drops what is kept in memory of the conversations whose
// handle the store pruned with their tasks. A conversation that continues
// anyway gets the agents line again.
func (cl *CallClaims) forgetPruned(ctx context.Context) {
	cl.linedMu.Lock()
	ids := slices.Collect(maps.Keys(cl.lined))
	cl.linedMu.Unlock()
	for _, id := range ids {
		_, err := cl.store.HandleOf(ctx, id)
		if !errors.Is(err, store.ErrNotFound) {
			continue
		}
		cl.linedMu.Lock()
		delete(cl.lined, id)
		cl.linedMu.Unlock()
	}
}

// turnClaims returns the claims of the caller of execCtx.
func turnClaims(ctx context.Context, execCtx *a2asrv.ExecutorContext) callerClaims {
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
// caller's message, for the first prompt of an ACP session in which the
// caller may call agents, and ctx marked for [CallClaims.turn]. Answers to
// the harness's own questions are left alone, because the harness parses
// them.
func (cl *CallClaims) withAgentsLine(ctx context.Context, execCtx *a2asrv.ExecutorContext) (context.Context, *a2asrv.ExecutorContext) {
	if execCtx.Message == nil || (execCtx.StoredTask != nil && execCtx.StoredTask.Status.State == a2a.TaskStateInputRequired) {
		return ctx, execCtx
	}
	caller, ok := auth.Caller(ctx)
	if !ok {
		return ctx, execCtx
	}
	user, err := cl.callerIn(ctx, execCtx.ContextID, caller)
	if err != nil {
		slog.Warn("finding the agent process of a conversation", "context", execCtx.ContextID, "error", err)
		return ctx, execCtx
	}
	if !mayCallAgents(user) || !cl.needsAgentsLine(ctx, execCtx.ContextID) {
		return ctx, execCtx
	}
	agents, err := callableAgents(ctx, cl.store)
	if err != nil {
		slog.Warn("listing callable agents", "error", err)
		return ctx, execCtx
	}
	if len(agents) == 0 {
		return ctx, execCtx
	}
	u, err := cl.agentsURL(ctx, execCtx.ContextID)
	if err != nil {
		slog.Warn("creating the agents URL", "error", err)
		return ctx, execCtx
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
	return context.WithValue(ctx, agentsLineKey{}, true), &ec
}
