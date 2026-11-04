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
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/a2aproject/a2a-serve/internal/agentsrv/approvals"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
	"github.com/a2aproject/a2a-serve/internal/harness"
	"github.com/a2aproject/a2a-serve/internal/jsonhttp"
	"github.com/a2aproject/a2a-serve/internal/localsrv"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// harnessHost is the host of agents URLs when the harness reaches the server
// through a Unix socket; the CLI sends every request through the socket.
const (
	harnessHost = "a2a"
	socketHost  = "http://" + harnessHost
)

// harnessHelperPath is where the harness's credential helper is written,
// relative to its home.
const harnessHelperPath = ".local/share/a2a-serve/credential-helper"

// writeHarnessHelper writes a credential helper into the harness home that
// gives the harness's a2a CLI the key for the TCP bridge, and returns the
// environment variable that has the CLI use it for requests to host only.
func writeHarnessHelper(home, host, key string) (string, error) {
	p := filepath.Join(home, filepath.FromSlash(harnessHelperPath))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", err
	}
	script := "#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' '{\"headers\":{\"Authorization\":[\"Bearer " + key + "\"]}}'\n"
	if err := fsutil.WriteAtomic(p, []byte(script), 0o700); err != nil {
		return "", err
	}
	return harness.EnvCredentialHelper + "=" + host + "=" + path.Join(harness.ContainerHome, harnessHelperPath), nil
}

// agentsURL is where the harness calls other agents in one conversation:
// <agents URL>/<name> is an A2A agent. The handle in the path stands for the
// conversation; the user and the hop count are those of its running turn.
func (p *Peers) agentsURL(ctx context.Context, contextID string) (string, error) {
	base := p.bridge.url
	if p.bridge.socket != "" {
		base = socketHost
	}
	if base == "" {
		return "", errors.New("the harness listener is not running")
	}
	h, err := p.handle(ctx, contextID)
	if err != nil {
		return "", err
	}
	return base + "/" + h, nil
}

// harnessRoutes serves the agents URLs on the harness listener.
func (p *Peers) harnessRoutes(key string) http.Handler {
	mux := http.NewServeMux()
	agents := p.agentsHandler(key)
	mux.Handle("/{handle}/{agent}", agents)
	mux.Handle("/{handle}/{agent}/{rest...}", agents)
	return mux
}

// agentsHandler serves the agents the harness may call, at
// /{handle}/{agent}/..., as A2A agents proxied by this server: contacts are
// called with this agent's identity, nodes in-process. Over a Unix socket
// only the harness can connect; over TCP it sends key.
func (p *Peers) agentsHandler(key string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if key != "" {
			tok, ok := auth.BearerToken(r.Header.Get("Authorization"))
			if !ok || subtle.ConstantTimeCompare([]byte(tok), []byte(key)) != 1 {
				jsonhttp.Error(w, http.StatusUnauthorized, "harness key required")
				return
			}
		}
		handle, name := r.PathValue("handle"), r.PathValue("agent")
		claims, err := p.claimsOf(r.Context(), handle)
		switch {
		case errors.Is(err, errUnknownHandle):
			jsonhttp.Error(w, http.StatusNotFound, err.Error())
			return
		case errors.Is(err, errNoTurn):
			jsonhttp.Error(w, http.StatusForbidden, err.Error())
			return
		case err != nil:
			slog.Error("resolving an agents URL", "error", err)
			jsonhttp.Error(w, http.StatusInternalServerError, "resolving the agents URL failed")
			return
		}
		if !mayCallAgents(claims.User) {
			jsonhttp.Error(w, http.StatusForbidden, "the user of this conversation can't call other agents; answer without calling one")
			return
		}
		if claims.User.Hops+1 > auth.MaxHops {
			jsonhttp.Error(w, http.StatusForbidden, fmt.Sprintf("too many agents in this chain (%d); answer without calling another agent", claims.User.Hops))
			return
		}
		rest := "/" + r.PathValue("rest")
		base := "http://" + r.Host + "/" + handle + "/" + name
		if name == schedulesAgent {
			p.serveSchedules(w, r, claims, base, rest)
			return
		}
		ok, err := p.callable(r.Context(), name)
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
		h, principal, err := p.agentHandler(r.Context(), claims, name)
		if err != nil {
			jsonhttp.Error(w, http.StatusBadGateway, err.Error())
			return
		}
		guarded := &guardedHandler{RequestHandler: h, p: p, claims: claims, agent: name}
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

// mayCallAgents reports whether the conversations of user may call other
// agents. Anonymous callers can't use this agent's contacts and nodes.
func mayCallAgents(user auth.Principal) bool {
	return user.Role == auth.RoleOwner || user.Role == auth.RoleAuthenticated
}

// serveSchedules serves the built-in schedules agent for the conversation
// of claims.
func (p *Peers) serveSchedules(w http.ResponseWriter, r *http.Request, claims callClaims, base, rest string) {
	if r.Method == http.MethodGet && (rest == "/" || rest == "/.well-known/agent-card.json") {
		card := proxyCard(schedulesAgent, base)
		card.Description = "Creates, lists and removes schedules which run a prompt in this conversation. Send help for the commands."
		a2asrv.NewStaticAgentCardHandler(card).ServeHTTP(w, r)
		return
	}
	h := a2asrv.NewHandler(&schedulesExecutor{sc: p.schedules, claims: claims})
	r2 := r.Clone(r.Context())
	r2.URL.Path, r2.URL.RawPath = rest, ""
	inner := a2asrv.NewRESTHandler(h)
	if r.Method == http.MethodPost && rest == "/" {
		inner = a2asrv.NewJSONRPCHandler(h)
	}
	inner.ServeHTTP(w, r2)
}

// callable reports whether name is a contact or a hosted node.
func (p *Peers) callable(ctx context.Context, name string) (bool, error) {
	if node, err := p.isNode(ctx, name); err != nil || node {
		return node, err
	}
	_, err := p.store.Contact(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

func proxyCard(name, base string) *a2a.AgentCard {
	return &a2a.AgentCard{
		Name:        name,
		Description: name + ", called through the agent server hosting you.",
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

// agentHandler returns the A2A handler of a node or a contact. A node is
// served in-process, as principal.
func (p *Peers) agentHandler(ctx context.Context, c callClaims, name string) (a2asrv.RequestHandler, *auth.Principal, error) {
	node, err := p.isNode(ctx, name)
	if err != nil {
		return nil, nil, err
	}
	if node {
		pr := p.nodeCaller(c.User, c.User.Hops)
		return p.nodes.Handler(name), &pr, nil
	}
	client, err := p.contactClient(ctx, name)
	if err != nil {
		return nil, nil, err
	}
	return &localsrv.ClientHandler{Client: client, Context: func(ctx context.Context) context.Context {
		return viaAgent(ctx, c.User.Hops)
	}}, nil, nil
}

// guardedHandler keeps a conversation to the tasks it started, and
// approvals with the user: the harness can't answer another agent's
// permission question, which is relayed to the user's chat instead. Tasks
// the harness doesn't wait for are watched, and their result is posted into
// the conversation.
type guardedHandler struct {
	a2asrv.RequestHandler
	p      *Peers
	claims callClaims
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

// ListTasks lists the tasks of the page which this conversation started.
func (g *guardedHandler) ListTasks(ctx context.Context, req *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	res, err := g.RequestHandler.ListTasks(ctx, req)
	if err != nil {
		return nil, err
	}
	out := *res
	out.Tasks = nil
	for _, t := range res.Tasks {
		ok, err := g.p.store.HasCallTask(ctx, g.claims.ContextID, g.agent, string(t.ID))
		if err != nil {
			return nil, err
		}
		if ok {
			out.Tasks = append(out.Tasks, t)
		}
	}
	return &out, nil
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
	if err := g.p.store.PutCallTask(context.WithoutCancel(ctx), t); err != nil {
		slog.Error("recording a call task", "agent", g.agent, "task", id, "error", err)
	}
}

// checkTask refuses a task this conversation didn't start.
func (g *guardedHandler) checkTask(ctx context.Context, id a2a.TaskID) error {
	ok, err := g.p.store.HasCallTask(ctx, g.claims.ContextID, g.agent, string(id))
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: this conversation didn't start task %s at %s", a2a.ErrTaskNotFound, id, g.agent)
	}
	return nil
}

// checkMessage refuses a message to a task or a context this conversation
// didn't start, and a reply to a pending permission question.
func (g *guardedHandler) checkMessage(ctx context.Context, req *a2a.SendMessageRequest) error {
	if req.Message == nil {
		return nil
	}
	switch m := req.Message; {
	case m.TaskID != "":
		if err := g.checkTask(ctx, m.TaskID); err != nil {
			return err
		}
	case m.ContextID != "":
		ok, err := g.p.store.HasCallContext(ctx, g.claims.ContextID, g.agent, m.ContextID)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: this conversation has no tasks in context %s of %s", a2a.ErrUnauthorized, m.ContextID, g.agent)
		}
	}
	return g.checkAnswer(ctx, req)
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
		return fmt.Errorf("%w: this is an approval for the user, who was asked in their chat; don't answer it, the result will be posted into this conversation", a2a.ErrUnauthorized)
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
		g.p.relay(g.p.ctx, g.claims, g.agent, task, q)
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
	if err := g.p.store.PutWatch(ctx, w); err != nil {
		slog.Error("storing watch", "agent", g.agent, "task", task.ID, "error", err)
		return
	}
	g.p.watch(w)
}
