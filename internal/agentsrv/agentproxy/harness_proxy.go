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

package agentproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"net/http"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/a2aproject/a2a-serve/core/contact"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/network"
	"github.com/a2aproject/a2a-serve/internal/pairings"
	"github.com/a2aproject/a2a-serve/internal/question"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// HarnessProxy serves the agents URLs on the harness listener: the agents
// the harness may call, proxied by this server for the user of the running
// turn of the conversation. The calls go through [contact.Proxy]; this
// server resolves the names, records the calls and watches their tasks.
type HarnessProxy struct {
	store     *store.Store
	claims    ClaimsSource
	clients   *ContactClients
	watches   *Watches
	schedules SchedulesAgent
	// calls cancels the calls of turns; it serves no requests.
	calls *contact.Proxy
}

// ClaimsSource tells who calls through an agents URL.
type ClaimsSource interface {
	// ClaimsOf returns the claims of the running turn of a handle's
	// conversation, for a call from the agent process of process:
	// [ErrUnknownHandle] for an unknown handle or one of a conversation of
	// another process, and [ErrNoTurn] when no turn runs.
	ClaimsOf(ctx context.Context, handle, process string) (Claims, error)
}

// SchedulesAgent is the built-in agent that manages schedules.
type SchedulesAgent interface {
	// Agent returns the schedules agent of the conversation contextID,
	// whose turn user runs, from a group chat if groupChat is set.
	Agent(user auth.Principal, contextID string, groupChat bool) a2asrv.AgentExecutor
}

// HarnessProxyDeps are the dependencies of [HarnessProxy].
type HarnessProxyDeps struct {
	Store     *store.Store
	Claims    ClaimsSource
	Clients   *ContactClients
	Watches   *Watches
	Schedules SchedulesAgent
}

// NewHarnessProxy returns the proxy of deps.
func NewHarnessProxy(deps HarnessProxyDeps) *HarnessProxy {
	hp := &HarnessProxy{store: deps.Store, claims: deps.Claims, clients: deps.Clients, watches: deps.Watches, schedules: deps.Schedules}
	hp.calls = hp.proxy(func(*http.Request) (string, bool) { return "", false })
	return hp
}

// Claims are who calls through the agents URL of a conversation: the
// harness in the conversation's running turn, for the user who started it.
type Claims struct {
	ContextID string
	// TaskID is the task of the running turn, which the calls it starts
	// are recorded for.
	TaskID string
	User   auth.Principal
	// GroupChat is whether the turn came from a group chat.
	GroupChat bool
}

// turn is the turn of the calls of c.
func (c Claims) turn() contact.Turn {
	return contact.Turn{ContextID: c.ContextID, TaskID: c.TaskID, Hops: c.User.Hops, Caller: c}
}

func claimsOf(t contact.Turn) Claims {
	c, ok := t.Caller.(Claims)
	if !ok {
		return Claims{ContextID: t.ContextID, TaskID: t.TaskID}
	}
	return c
}

// Errors of [ClaimsSource.ClaimsOf].
var (
	ErrUnknownHandle error = &turnError{msg: "unknown agents URL: use the one from the [a2a agents] line of this conversation", kind: contact.ErrNotFound}
	ErrNoTurn        error = &turnError{msg: "this conversation has no running turn, and its agents URL works only while one runs", kind: contact.ErrNoTurn}
)

// turnError words an error of [contact.Turns] for the agents URL.
type turnError struct {
	msg  string
	kind error
}

func (e *turnError) Error() string { return e.msg }

func (e *turnError) Is(target error) bool { return target == e.kind }

// Caller returns the agent key of the process a request on the harness
// listener came from, or false when the request proves none.
type Caller func(r *http.Request) (agentKey string, ok bool)

// Routes serves the agents URLs on the harness listener, at
// /{handle}/{agent}/.... caller tells the agent process a request comes
// from, which reaches only the handles of its own conversations.
func (hp *HarnessProxy) Routes(caller Caller) http.Handler {
	return hp.proxy(caller)
}

func (hp *HarnessProxy) proxy(caller Caller) *contact.Proxy {
	p, err := contact.New(contact.Config{
		Turns:     harnessTurns{claims: hp.claims, caller: caller},
		Directory: hp,
		Calls:     storeCalls{store: hp.store},
		Followup:  hp.watches,
		Scoped:    true,
	})
	if err != nil {
		panic(err)
	}
	return p
}

// harnessTurns are the turns of the conversations that agents URLs stand
// for: the handle in the path stands for the conversation.
type harnessTurns struct {
	claims ClaimsSource
	caller Caller
}

func (ht harnessTurns) Turn(r *http.Request, handle string) (contact.Turn, error) {
	process, ok := ht.caller(r)
	if !ok {
		return contact.Turn{}, contact.ErrUnauthenticated
	}
	c, err := ht.claims.ClaimsOf(r.Context(), handle, process)
	if err != nil {
		return contact.Turn{}, err
	}
	if !c.User.MayCallAgents() {
		return c.turn(), contact.Refusef("the user of this conversation can't call other agents; answer without calling one")
	}
	return c.turn(), nil
}

// Card implements [contact.Directory]: contacts, hosted nodes, the agents
// of the private network the agent may call, and the schedules agent.
func (hp *HarnessProxy) Card(ctx context.Context, _ contact.Turn, name string) (*a2a.AgentCard, error) {
	if name == pairings.SchedulesAgent {
		return &a2a.AgentCard{Description: "Creates, lists and removes schedules which run a prompt in this conversation. Send help for the commands."}, nil
	}
	ok, err := hp.clients.callable(ctx, name)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, contact.Refusef("%q is not an agent you can call; the [a2a agents] line lists them", name)
	}
	return nil, nil
}

// Target implements [contact.Directory].
func (hp *HarnessProxy) Target(ctx context.Context, t contact.Turn, name string) (contact.Target, error) {
	c := claimsOf(t)
	if name == pairings.SchedulesAgent {
		return contact.Local(a2asrv.NewHandler(hp.schedules.Agent(c.User, c.ContextID, c.GroupChat))), nil
	}
	return hp.clients.agentTarget(ctx, c, name)
}

// CancelCalls cancels the tasks that the turn of the task of claims started
// at other agents, and stops watching them, so that their results aren't
// posted into the conversation. A task that it can't cancel stays watched,
// and its result still comes back.
func (hp *HarnessProxy) CancelCalls(ctx context.Context, claims Claims) error {
	canceled, err := hp.calls.CancelCalls(ctx, claims.turn())
	for _, c := range canceled {
		hp.watches.dropCanceled(ctx, c.Contact, string(c.TaskID))
	}
	if err != nil {
		return fmt.Errorf("canceling the calls of task %s: %w", claims.TaskID, err)
	}
	return nil
}

// storeCalls keeps the calls of conversations in the store, so that they
// outlive a restart.
type storeCalls struct {
	store *store.Store
}

var _ contact.Calls = storeCalls{}

func (sc storeCalls) Record(ctx context.Context, t contact.Turn, c contact.Call) error {
	return sc.store.PutCallTask(ctx, store.CallTask{ContextID: t.ContextID, Agent: c.Contact, TaskID: string(c.TaskID), RemoteContextID: c.ContextID, OriginTaskID: t.TaskID})
}

func (sc storeCalls) Started(ctx context.Context, t contact.Turn, agent string, id a2a.TaskID) (bool, error) {
	return sc.store.HasCallTask(ctx, t.ContextID, agent, string(id))
}

func (sc storeCalls) InContext(ctx context.Context, t contact.Turn, agent, contextID string) (bool, error) {
	return sc.store.HasCallContext(ctx, t.ContextID, agent, contextID)
}

func (sc storeCalls) List(ctx context.Context, t contact.Turn, agent, contextID string) ([]a2a.TaskID, error) {
	ids, err := sc.store.CallTasks(ctx, t.ContextID, agent, contextID)
	if err != nil {
		return nil, err
	}
	out := make([]a2a.TaskID, len(ids))
	for i, id := range ids {
		out[i] = a2a.TaskID(id)
	}
	return out, nil
}

func (sc storeCalls) OfTurn(ctx context.Context, t contact.Turn) ([]contact.Call, error) {
	calls, err := sc.store.CallTasksOf(ctx, t.ContextID, t.TaskID)
	if err != nil {
		return nil, err
	}
	out := make([]contact.Call, len(calls))
	for i, c := range calls {
		out[i] = contact.Call{Contact: c.Agent, TaskID: a2a.TaskID(c.TaskID), ContextID: c.RemoteContextID}
	}
	return out, nil
}

var _ contact.Followup = (*Watches)(nil)

// Follow relays a permission question to the user and watches a task the
// harness won't see finish: one waiting for an approval, or one it didn't
// wait for. It implements [contact.Followup].
func (ws *Watches) Follow(ctx context.Context, t contact.Turn, o contact.Outstanding) {
	claims, agent, task := claimsOf(t), o.Contact, o.Task
	var q *question.Question
	if o.Approval != nil {
		q = question.FromStatus(task.Status)
	}
	identity, err := json.Marshal(claims.User.Identity)
	if err != nil {
		slog.Error("storing watch", "agent", agent, "task", task.ID, "error", err)
		if q != nil {
			ws.relay(ws.ctx, claims, agent, task, q)
		}
		return
	}
	w := store.Watch{
		Contact: agent, TaskID: string(task.ID), RemoteContextID: task.ContextID, OriginContextID: claims.ContextID,
		OnBehalfOf: claims.User.ID, Identity: string(identity), GroupChat: claims.GroupChat,
	}
	// The watch is stored before the question is relayed, so that a
	// question task never exists without it.
	putErr := ws.store.PutWatch(ctx, w)
	if putErr != nil {
		slog.Error("storing watch", "agent", agent, "task", task.ID, "error", putErr)
	}
	if q != nil {
		ws.relay(ws.ctx, claims, agent, task, q)
	}
	if putErr != nil {
		return
	}
	ws.watch(w)
}

// networkNamed names the agent in an error of a network that doesn't reach
// it, so that the agent process can tell which call failed.
type networkNamed struct {
	a2asrv.RequestHandler
	agent string
}

func (h networkNamed) unreachable(err error) error {
	for _, target := range []error{network.ErrPeerUnavailable, network.ErrProviderStalled} {
		if errors.Is(err, target) {
			return fmt.Errorf("%s: %w", h.agent, target)
		}
	}
	return err
}

func (h networkNamed) SendMessage(ctx context.Context, req *a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
	res, err := h.RequestHandler.SendMessage(ctx, req)
	return res, h.unreachable(err)
}

func (h networkNamed) SendStreamingMessage(ctx context.Context, req *a2a.SendMessageRequest) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		for ev, err := range h.RequestHandler.SendStreamingMessage(ctx, req) {
			if !yield(ev, h.unreachable(err)) {
				return
			}
		}
	}
}
