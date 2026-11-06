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
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/approvals"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/jsonhttp"
	"github.com/a2aproject/a2a-serve/internal/pairings"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// HarnessProxy serves the agents URLs on the harness listener: the agents
// the harness may call, proxied by this server for the user of the running
// turn of the conversation.
type HarnessProxy struct {
	store     *store.Store
	claims    ClaimsSource
	clients   *ContactClients
	watches   *Watches
	schedules SchedulesAgent
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
	// whose turn user runs.
	Agent(user auth.Principal, contextID string) a2asrv.AgentExecutor
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
	return &HarnessProxy{store: deps.Store, claims: deps.Claims, clients: deps.Clients, watches: deps.Watches, schedules: deps.Schedules}
}

// Claims are who calls through the agents URL of a conversation: the
// harness in the conversation's running turn, for the user who started it.
type Claims struct {
	ContextID string
	User      auth.Principal
}

// Errors of [ClaimsSource.ClaimsOf].
var (
	ErrUnknownHandle = errors.New("unknown agents URL: use the one from the [a2a agents] line of this conversation")
	ErrNoTurn        = errors.New("this conversation has no running turn, and its agents URL works only while one runs")
)

// Caller returns the agent key of the process a request on the harness
// listener came from, or false when the request proves none.
type Caller func(r *http.Request) (agentKey string, ok bool)

// Routes serves the agents URLs on the harness listener.
func (hp *HarnessProxy) Routes(caller Caller) http.Handler {
	mux := http.NewServeMux()
	agents := hp.agentsHandler(caller)
	mux.Handle("/{handle}/{agent}", agents)
	mux.Handle("/{handle}/{agent}/{rest...}", agents)
	return jsonhttp.LimitBody(mux)
}

// agentsHandler serves the agents the harness may call, at
// /{handle}/{agent}/..., as A2A agents proxied by this server: contacts are
// called with this agent's identity, nodes in-process. caller tells the
// agent process a request comes from, which reaches only the handles of
// its own conversations.
func (hp *HarnessProxy) agentsHandler(caller Caller) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		process, ok := caller(r)
		if !ok {
			jsonhttp.Error(w, http.StatusUnauthorized, "agent key required")
			return
		}
		handle, name := r.PathValue("handle"), r.PathValue("agent")
		claims, err := hp.claims.ClaimsOf(r.Context(), handle, process)
		switch {
		case errors.Is(err, ErrUnknownHandle):
			jsonhttp.Error(w, http.StatusNotFound, err.Error())
			return
		case errors.Is(err, ErrNoTurn):
			jsonhttp.Error(w, http.StatusForbidden, err.Error())
			return
		case err != nil:
			slog.Error("resolving an agents URL", "error", err)
			jsonhttp.Error(w, http.StatusInternalServerError, "resolving the agents URL failed")
			return
		}
		if !claims.User.MayCallAgents() {
			jsonhttp.Error(w, http.StatusForbidden, "the user of this conversation can't call other agents; answer without calling one")
			return
		}
		if claims.User.Hops+1 > auth.MaxHops {
			jsonhttp.Error(w, http.StatusForbidden, fmt.Sprintf("too many agents in this chain (%d); answer without calling another agent", claims.User.Hops))
			return
		}
		rest := "/" + r.PathValue("rest")
		base := "http://" + r.Host + "/" + handle + "/" + name
		if name == pairings.SchedulesAgent {
			hp.serveSchedules(w, r, claims, base, rest)
			return
		}
		ok, err = hp.clients.callable(r.Context(), name)
		if err != nil {
			slog.Error("looking up a callable agent", "agent", name, "error", err)
			jsonhttp.Error(w, http.StatusInternalServerError, "looking up "+name+" failed")
			return
		}
		if !ok {
			jsonhttp.Error(w, http.StatusNotFound, fmt.Sprintf("%q is not an agent you can call; the [a2a agents] line lists them", name))
			return
		}
		if r.Method == http.MethodGet && (rest == "/" || rest == "/.well-known/agent-card.json") {
			a2asrv.NewStaticAgentCardHandler(proxyCard(name, base)).ServeHTTP(w, r)
			return
		}
		h, principal, err := hp.clients.agentHandler(r.Context(), claims, name)
		if err != nil {
			jsonhttp.Error(w, http.StatusBadGateway, err.Error())
			return
		}
		guarded := &guardedHandler{RequestHandler: h, proxy: hp, claims: claims, agent: name}
		r2 := r.Clone(r.Context())
		r2.URL.Path, r2.URL.RawPath = rest, ""
		if principal != nil {
			r2 = r2.WithContext(auth.WithPrincipal(r2.Context(), *principal))
		}
		inner := a2asrv.NewRESTHandler(guarded, a2asrv.WithTransportKeepAlive(15*time.Second))
		if r.Method == http.MethodPost && rest == "/" {
			inner = a2asrv.NewJSONRPCHandler(guarded, a2asrv.WithTransportKeepAlive(15*time.Second))
		}
		inner.ServeHTTP(w, r2)
	})
}

// serveSchedules serves the built-in schedules agent for the conversation
// of claims.
func (hp *HarnessProxy) serveSchedules(w http.ResponseWriter, r *http.Request, claims Claims, base, rest string) {
	if r.Method == http.MethodGet && (rest == "/" || rest == "/.well-known/agent-card.json") {
		card := proxyCard(pairings.SchedulesAgent, base)
		card.Description = "Creates, lists and removes schedules which run a prompt in this conversation. Send help for the commands."
		a2asrv.NewStaticAgentCardHandler(card).ServeHTTP(w, r)
		return
	}
	h := a2asrv.NewHandler(hp.schedules.Agent(claims.User, claims.ContextID))
	r2 := r.Clone(r.Context())
	r2.URL.Path, r2.URL.RawPath = rest, ""
	inner := a2asrv.NewRESTHandler(h)
	if r.Method == http.MethodPost && rest == "/" {
		inner = a2asrv.NewJSONRPCHandler(h)
	}
	inner.ServeHTTP(w, r2)
}

func proxyCard(name, base string) *a2a.AgentCard {
	return &a2a.AgentCard{
		Name:        name,
		Description: name + ", called through the server hosting you.",
		Version:     "1.0.0",
		SupportedInterfaces: []*a2a.AgentInterface{
			a2a.NewAgentInterface(base, a2a.TransportProtocolJSONRPC),
			a2a.NewAgentInterface(base, a2a.TransportProtocolHTTPJSON),
		},
		Capabilities:       a2a.AgentCapabilities{Streaming: true},
		DefaultInputModes:  []string{"text/plain", "application/json"},
		DefaultOutputModes: []string{"text/plain", "application/json"},
	}
}

// guardedHandler keeps a conversation to the tasks it started, and
// approvals with the user: the harness can't answer another agent's
// permission question, which is relayed to the user instead. Tasks
// the harness doesn't wait for are watched, and their result is posted into
// the conversation.
type guardedHandler struct {
	a2asrv.RequestHandler
	proxy  *HarnessProxy
	claims Claims
	agent  string
}

func (g *guardedHandler) SendMessage(ctx context.Context, req *a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
	if err := g.checkMessage(ctx, req); err != nil {
		return nil, err
	}
	res, err := g.RequestHandler.SendMessage(ctx, req)
	if task, ok := res.(*a2a.Task); ok && err == nil {
		g.record(ctx, task.ID, task.ContextID)
		g.after(ctx, task)
	}
	return res, err
}

func (g *guardedHandler) SendStreamingMessage(ctx context.Context, req *a2a.SendMessageRequest) iter.Seq2[a2a.Event, error] {
	if err := g.checkMessage(ctx, req); err != nil {
		return func(yield func(a2a.Event, error) bool) { yield(nil, err) }
	}
	return func(yield func(a2a.Event, error) bool) {
		var last *a2a.Task
		recorded := false
		for ev, err := range g.RequestHandler.SendStreamingMessage(ctx, req) {
			switch e := ev.(type) {
			case *a2a.Task:
				last = &a2a.Task{ID: e.ID, ContextID: e.ContextID, Status: e.Status}
			case *a2a.TaskStatusUpdateEvent:
				last = &a2a.Task{ID: e.TaskID, ContextID: e.ContextID, Status: e.Status}
			}
			if last != nil && !recorded {
				g.record(ctx, last.ID, last.ContextID)
				recorded = true
			}
			if !yield(ev, err) {
				break
			}
		}
		if last != nil {
			// The harness may have dropped the stream, which cancels ctx;
			// the task still has to be watched.
			g.after(context.WithoutCancel(ctx), last)
		}
	}
}

func (g *guardedHandler) GetTask(ctx context.Context, req *a2a.GetTaskRequest) (*a2a.Task, error) {
	if err := g.checkTask(ctx, req.ID); err != nil {
		return nil, err
	}
	return g.RequestHandler.GetTask(ctx, req)
}

// Paging limits of ListTasks, which match the ones of the task stores.
const (
	defaultListPageSize = 50
	maxListPageSize     = 100
)

// ListTasks lists the tasks which this conversation started, newest first,
// from the call tasks the server recorded. It gets the tasks on the page
// from the agent, or every task when req filters by status, and leaves out
// a task the agent no longer has. A page token is the index of the first
// task of the page.
func (g *guardedHandler) ListTasks(ctx context.Context, req *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	pageSize := cmp.Or(req.PageSize, defaultListPageSize)
	if pageSize < 1 || pageSize > maxListPageSize {
		return nil, fmt.Errorf("page size must be between 1 and %d inclusive, got %d: %w", maxListPageSize, pageSize, a2a.ErrInvalidRequest)
	}
	offset := 0
	if req.PageToken != "" {
		var err error
		if offset, err = strconv.Atoi(req.PageToken); err != nil || offset < 0 {
			return nil, fmt.Errorf("invalid page token: %w", a2a.ErrInvalidRequest)
		}
	}
	ids, err := g.proxy.store.CallTasks(ctx, g.claims.ContextID, g.agent, req.ContextID)
	if err != nil {
		return nil, err
	}
	page := func(total int) (start, end int) {
		return min(offset, total), min(offset+pageSize, total)
	}
	out := &a2a.ListTasksResponse{PageSize: pageSize}
	var start, end int
	if req.Status == "" && req.StatusTimestampAfter == nil {
		out.TotalSize = len(ids)
		start, end = page(len(ids))
		if out.Tasks, err = g.getTasks(ctx, ids[start:end], req); err != nil {
			return nil, err
		}
	} else {
		all, err := g.getTasks(ctx, ids, req)
		if err != nil {
			return nil, err
		}
		all = slices.DeleteFunc(all, func(t *a2a.Task) bool { return !statusMatches(t, req) })
		out.TotalSize = len(all)
		start, end = page(len(all))
		out.Tasks = all[start:end]
	}
	if end < out.TotalSize {
		out.NextPageToken = strconv.Itoa(end)
	}
	return out, nil
}

// getTasks gets the tasks with ids from the agent, with the history length
// of req or no history, and leaves out the ones the agent no longer has.
func (g *guardedHandler) getTasks(ctx context.Context, ids []string, req *a2a.ListTasksRequest) ([]*a2a.Task, error) {
	history := cmp.Or(req.HistoryLength, new(int))
	tasks := []*a2a.Task{}
	for _, id := range ids {
		t, err := g.RequestHandler.GetTask(ctx, &a2a.GetTaskRequest{ID: a2a.TaskID(id), HistoryLength: history})
		if errors.Is(err, a2a.ErrTaskNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !req.IncludeArtifacts {
			listed := *t
			listed.Artifacts = nil
			t = &listed
		}
		tasks = append(tasks, t)
	}
	return tasks, nil
}

func statusMatches(t *a2a.Task, req *a2a.ListTasksRequest) bool {
	if req.Status != "" && t.Status.State != req.Status {
		return false
	}
	after := req.StatusTimestampAfter
	return after == nil || (t.Status.Timestamp != nil && t.Status.Timestamp.After(*after))
}

func (g *guardedHandler) CancelTask(ctx context.Context, req *a2a.CancelTaskRequest) (*a2a.Task, error) {
	if err := g.checkTask(ctx, req.ID); err != nil {
		return nil, err
	}
	return g.RequestHandler.CancelTask(ctx, req)
}

func (g *guardedHandler) SubscribeToTask(ctx context.Context, req *a2a.SubscribeToTaskRequest) iter.Seq2[a2a.Event, error] {
	if err := g.checkTask(ctx, req.ID); err != nil {
		return func(yield func(a2a.Event, error) bool) { yield(nil, err) }
	}
	return g.RequestHandler.SubscribeToTask(ctx, req)
}

func (g *guardedHandler) GetTaskPushConfig(ctx context.Context, req *a2a.GetTaskPushConfigRequest) (*a2a.PushConfig, error) {
	if err := g.checkTask(ctx, req.TaskID); err != nil {
		return nil, err
	}
	return g.RequestHandler.GetTaskPushConfig(ctx, req)
}

func (g *guardedHandler) ListTaskPushConfigs(ctx context.Context, req *a2a.ListTaskPushConfigRequest) (*a2a.ListTaskPushConfigResponse, error) {
	if err := g.checkTask(ctx, req.TaskID); err != nil {
		return nil, err
	}
	return g.RequestHandler.ListTaskPushConfigs(ctx, req)
}

func (g *guardedHandler) CreateTaskPushConfig(ctx context.Context, req *a2a.PushConfig) (*a2a.PushConfig, error) {
	if err := g.checkTask(ctx, req.TaskID); err != nil {
		return nil, err
	}
	return g.RequestHandler.CreateTaskPushConfig(ctx, req)
}

func (g *guardedHandler) DeleteTaskPushConfig(ctx context.Context, req *a2a.DeleteTaskPushConfigRequest) error {
	if err := g.checkTask(ctx, req.TaskID); err != nil {
		return err
	}
	return g.RequestHandler.DeleteTaskPushConfig(ctx, req)
}

// record notes that this conversation started a task.
func (g *guardedHandler) record(ctx context.Context, id a2a.TaskID, contextID string) {
	t := store.CallTask{ContextID: g.claims.ContextID, Agent: g.agent, TaskID: string(id), RemoteContextID: contextID}
	if err := g.proxy.store.PutCallTask(context.WithoutCancel(ctx), t); err != nil {
		slog.Error("recording a call task", "agent", g.agent, "task", id, "error", err)
	}
}

// checkTask refuses a task this conversation didn't start.
func (g *guardedHandler) checkTask(ctx context.Context, id a2a.TaskID) error {
	ok, err := g.proxy.store.HasCallTask(ctx, g.claims.ContextID, g.agent, string(id))
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: this conversation didn't start task %s at %s", a2a.ErrTaskNotFound, id, g.agent)
	}
	return nil
}

// checkMessage refuses a message to, a reference to or a push config for a
// task, or a message to a context, this conversation didn't start, and a reply to a pending permission question.
func (g *guardedHandler) checkMessage(ctx context.Context, req *a2a.SendMessageRequest) error {
	if req.Message == nil {
		return nil
	}
	for _, id := range req.Message.ReferenceTasks {
		if err := g.checkTask(ctx, id); err != nil {
			return err
		}
	}
	if pc := configPush(req); pc != nil && pc.TaskID != "" {
		if err := g.checkTask(ctx, pc.TaskID); err != nil {
			return err
		}
	}
	switch m := req.Message; {
	case m.TaskID != "":
		if err := g.checkTask(ctx, m.TaskID); err != nil {
			return err
		}
	case m.ContextID != "":
		ok, err := g.proxy.store.HasCallContext(ctx, g.claims.ContextID, g.agent, m.ContextID)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: this conversation has no tasks in context %s of %s", a2a.ErrUnauthorized, m.ContextID, g.agent)
		}
	}
	return g.checkAnswer(ctx, req)
}

func configPush(req *a2a.SendMessageRequest) *a2a.PushConfig {
	if req.Config == nil {
		return nil
	}
	return req.Config.PushConfig
}

// checkAnswer rejects a reply to a pending permission question.
func (g *guardedHandler) checkAnswer(ctx context.Context, req *a2a.SendMessageRequest) error {
	if req.Message.TaskID == "" {
		return nil
	}
	task, err := g.RequestHandler.GetTask(ctx, &a2a.GetTaskRequest{ID: req.Message.TaskID})
	if errors.Is(err, a2a.ErrTaskNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("checking task %s at %s: %w", req.Message.TaskID, g.agent, err)
	}
	if task.Status.State != a2a.TaskStateInputRequired {
		return nil
	}
	if q := approvals.FromStatus(task.Status); q != nil && q.Type == approvals.TypePermission {
		g.after(ctx, task)
		return fmt.Errorf("%w: this is an approval for the user, and the server asks them for it; don't answer it. End your turn: the result comes back into this conversation", a2a.ErrUnauthorized)
	}
	return nil
}

// after relays a permission question to the user and watches a task the
// harness won't see finish: one waiting for an approval, or one it didn't wait for.
func (g *guardedHandler) after(ctx context.Context, task *a2a.Task) {
	state := task.Status.State
	if state.Terminal() || state == a2a.TaskStateAuthRequired {
		return
	}
	if state == a2a.TaskStateInputRequired {
		q := approvals.FromStatus(task.Status)
		if q == nil || q.Type != approvals.TypePermission {
			return
		}
		g.proxy.watches.relay(g.proxy.watches.ctx, g.claims, g.agent, task, q)
	}
	identity, err := json.Marshal(g.claims.User.Identity)
	if err != nil {
		slog.Error("storing watch", "agent", g.agent, "task", task.ID, "error", err)
		return
	}
	w := store.Watch{
		Contact: g.agent, TaskID: string(task.ID), RemoteContextID: task.ContextID, OriginContextID: g.claims.ContextID,
		OnBehalfOf: g.claims.User.ID, Identity: string(identity),
	}
	if err := g.proxy.store.PutWatch(ctx, w); err != nil {
		slog.Error("storing watch", "agent", g.agent, "task", task.ID, "error", err)
		return
	}
	g.proxy.watches.watch(w)
}
