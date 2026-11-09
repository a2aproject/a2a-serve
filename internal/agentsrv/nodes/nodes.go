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

// Package nodes hosts webhook nodes: agents which only run when woken, such
// as cloud agents. The agent server plays the A2A server role for them,
// wakes them with signed webhooks and turns their replies into task updates.
package nodes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/a2aproject/a2a-serve/internal/a2aerr"
	"github.com/a2aproject/a2a-serve/internal/a2alimit"
	"github.com/a2aproject/a2a-serve/internal/a2atext"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/cryptoutil"
	"github.com/a2aproject/a2a-serve/internal/egress"
	"github.com/a2aproject/a2a-serve/internal/httpserver"
	"github.com/a2aproject/a2a-serve/internal/jsonhttp"
	"github.com/a2aproject/a2a-serve/internal/mailboxapi"
	"github.com/a2aproject/a2a-serve/internal/network"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// nodeTaskTimeout fails a node task which posts no event for this long.
const nodeTaskTimeout = 10 * time.Minute

// defaultWebhookRetries are the delays before each wake delivery attempt.
func defaultWebhookRetries() []time.Duration {
	return []time.Duration{0, 2 * time.Second, 10 * time.Second, 30 * time.Second}
}

const inboxSize = 200

// Config configures a [Host].
type Config struct {
	Store *store.Store
	// Viewer decides which node tasks a caller sees.
	Viewer store.Viewer
	// Runtime has the config of the hosting agent.
	Runtime *agentdir.Runtime
	// BaseURL returns the URL the hosting agent is reached at.
	BaseURL func() string
	// Authenticate puts the principal of a request on a listener into its
	// context, or rejects it.
	Authenticate func(listener string, next http.Handler) http.Handler
	// WebhookRetries are the delays before each wake delivery attempt;
	// nil uses the default retries.
	WebhookRetries []time.Duration
	// Advertised reports whether the node is advertised to the private
	// network, which lets members call it too. Nil
	// advertises none.
	Advertised func(id string) bool
}

// Host hosts the webhook nodes of an agent.
//
// Open tasks, idempotency keys and the inbox are kept in memory only. Tasks
// still open when the server stops are failed on the next start, so after a
// restart no event could be delivered to them and no key or wake of theirs
// would be acted on.
type Host struct {
	cfg Config
	// ctx ends when the server stops; wake deliveries run in it.
	ctx context.Context

	handlerMu sync.Mutex

	// failMu serializes failing the interrupted tasks of nodes.
	failMu sync.Mutex
	failed map[string]bool

	mu       sync.Mutex
	handlers map[string]a2asrv.RequestHandler
	open     map[a2a.TaskID]*nodeWaiter
	keys     map[a2a.TaskID]*turnKeys
	inbox    map[string][]*mailboxapi.InboxEntry
	// stopped is set by Stop, after which wakes aren't delivered.
	stopped    bool
	deliveries sync.WaitGroup
}

type nodeWaiter struct {
	node   string
	caller string
	events chan nodeDelivery
	done   chan struct{}
	once   sync.Once
}

func (w *nodeWaiter) close() { w.once.Do(func() { close(w.done) }) }

// turnKeys are the idempotency keys of one waiter, so that a node asking the
// same question in a later turn isn't taken for a retry.
type turnKeys struct {
	w    *nodeWaiter
	seen map[string]*keyedEvent
}

// keyedEvent is an event posted with an idempotency key. done is closed
// once the event is applied or fails; a failed event loses its key.
type keyedEvent struct {
	done chan struct{}
	res  mailboxapi.EventResult
	err  error
}

type nodeDelivery struct {
	ev     mailboxapi.Event
	result chan error
}

// New returns a host whose background work runs until ctx ends.
func New(ctx context.Context, cfg Config) *Host {
	if cfg.WebhookRetries == nil {
		cfg.WebhookRetries = defaultWebhookRetries()
	}
	return &Host{
		cfg:      cfg,
		ctx:      ctx,
		handlers: map[string]a2asrv.RequestHandler{},
		open:     map[a2a.TaskID]*nodeWaiter{},
		keys:     map[a2a.TaskID]*turnKeys{},
		inbox:    map[string][]*mailboxapi.InboxEntry{},
		failed:   map[string]bool{},
	}
}

func nodeScope(id string) string { return "n/" + id }

// tasks returns the task store of a node. Tasks that were open when the
// server stopped are failed on its first use, which is retried on later
// uses until it succeeds.
func (n *Host) tasks(ctx context.Context, id string) *store.TaskStore {
	ts := n.cfg.Store.Tasks(nodeScope(id), n.cfg.Viewer)
	n.failMu.Lock()
	defer n.failMu.Unlock()
	if n.failed[id] {
		return ts
	}
	if _, err := ts.FailUnfinished(context.WithoutCancel(ctx), "The server restarted before the node answered.", nil); err != nil {
		slog.Error("failing interrupted node tasks", "node", id, "error", err)
		return ts
	}
	n.failed[id] = true
	return ts
}

// Handler returns the A2A handler of a node.
func (n *Host) Handler(id string) a2asrv.RequestHandler {
	ts := n.tasks(context.Background(), id)
	// Two handlers for one node would keep separate queues of its tasks.
	n.handlerMu.Lock()
	defer n.handlerMu.Unlock()
	n.mu.Lock()
	h, ok := n.handlers[id]
	n.mu.Unlock()
	if ok {
		return h
	}
	h = a2alimit.NewHandler(a2asrv.NewHandler(&nodeExecutor{n: n, node: id},
		a2asrv.WithTaskStore(ts),
		a2asrv.WithUpdateMaterializer(ts.UpdateMaterializer()),
		a2asrv.WithCallInterceptors(a2aerr.Sanitizer{}, a2alimit.NewMessageCheck(a2alimit.Default()), &nodeCallerInterceptor{node: id, advertised: n.cfg.Advertised, tasks: ts}),
		a2asrv.WithCapabilityChecks(&a2a.AgentCapabilities{Streaming: true}),
		a2asrv.WithLogger(slog.Default()),
	), a2alimit.Default())
	n.mu.Lock()
	n.handlers[id] = h
	n.mu.Unlock()
	return h
}

// Forget drops what the host keeps in memory for the node id: its handler,
// open tasks, idempotency keys and inbox. Call it when the node is removed,
// so a node paired later under the same name starts with none of them.
func (n *Host) Forget(id string) {
	n.failMu.Lock()
	delete(n.failed, id)
	n.failMu.Unlock()
	n.handlerMu.Lock()
	defer n.handlerMu.Unlock()
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.handlers, id)
	delete(n.inbox, id)
	for taskID, w := range n.open {
		if w.node == id {
			w.close()
			delete(n.open, taskID)
		}
	}
	for taskID, k := range n.keys {
		if k.w.node == id {
			delete(n.keys, taskID)
		}
	}
}

// nodeCallerInterceptor lets owners and the hosting agent call nodes; the
// agent calls them in-process from its harness. An advertised node also
// takes members, such as the agents of other people on the
// private network.
type nodeCallerInterceptor struct {
	a2asrv.PassthroughCallInterceptor
	node       string
	advertised func(id string) bool
	tasks      *store.TaskStore
}

const errNodeCaller = "webhook nodes are reachable by the hosting agent and its owner"

// mayCallNode reports whether p may call node.
func mayCallNode(p auth.Principal, advertised func(id string) bool, node string) bool {
	hosting := p.Network == network.ListenerHarness
	return p.IsOwner() || hosting || (p.Role == auth.RoleMember && advertised != nil && advertised(node))
}

func (c *nodeCallerInterceptor) Before(ctx context.Context, callCtx *a2asrv.CallContext, req *a2asrv.Request) (context.Context, any, error) {
	p, ok := auth.Caller(ctx)
	if !ok {
		return ctx, nil, a2a.ErrUnauthenticated
	}
	if !mayCallNode(p, c.advertised, c.node) {
		return ctx, nil, fmt.Errorf("%w: %s", a2a.ErrUnauthorized, errNodeCaller)
	}
	auth.SetCallUser(callCtx, p)
	// a2a-go streams a running task to any subscriber, so subscribers get
	// the same scoping as GetTask.
	if sub, ok := req.Payload.(*a2a.SubscribeToTaskRequest); ok {
		_, err := c.tasks.Header(ctx, sub.ID)
		return ctx, nil, err
	}
	if msg, ok := req.Payload.(*a2a.SendMessageRequest); ok && msg.Message != nil && msg.Message.TaskID != "" {
		return ctx, nil, c.tasks.CheckNotInterrupted(ctx, msg.Message.TaskID)
	}
	return ctx, nil, nil
}

// Register adds the node routes to a listener's mux.
func (n *Host) Register(mux *http.ServeMux, kind string, base func(*http.Request) string) {
	serveNode := func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("node")
		rest := "/" + r.PathValue("rest")
		if (r.Method == http.MethodGet || r.Method == http.MethodOptions) && (rest == "/" || rest == a2asrv.WellKnownAgentCardPath || rest == "/.well-known/agent.json") {
			httpserver.PublicCORS(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { n.serveCard(w, r, kind, base, id) })).ServeHTTP(w, r)
			return
		}
		if _, err := n.cfg.Store.Node(r.Context(), id); err != nil {
			jsonhttp.Error(w, http.StatusNotFound, "no such agent")
			return
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path, r2.URL.RawPath = rest, ""
		h := n.Handler(id)
		inner := a2asrv.NewRESTHandler(h, a2asrv.WithTransportKeepAlive(15*time.Second))
		if r.Method == http.MethodPost && rest == "/" {
			inner = a2asrv.NewJSONRPCHandler(h, a2asrv.WithTransportKeepAlive(15*time.Second))
		}
		n.cfg.Authenticate(kind, inner).ServeHTTP(w, r2)
	}
	node := jsonhttp.RequireJSON(jsonhttp.LimitBody(http.HandlerFunc(serveNode)))
	mux.Handle("/n/{node}", node)
	mux.Handle("/n/{node}/{rest...}", node)
	mux.HandleFunc("GET "+mailboxapi.Prefix+"/api/node", n.nodeAPI(base, n.handleGetNode))
	mux.Handle("POST "+mailboxapi.Prefix+"/api/tasks/{id}/events", jsonhttp.RequireJSON(n.nodeAPI(base, n.handleEvent)))
	mux.HandleFunc("GET "+mailboxapi.Prefix+"/api/tasks/{id}", n.nodeAPI(base, n.handleGetTask))
	mux.HandleFunc("GET "+mailboxapi.Prefix+"/api/inbox", n.nodeAPI(base, n.handleInbox))
}

// serveCard serves the card of a node. The card of an advertised node is
// public; any other goes to the callers who may call the node, which also
// keeps the callers who can't from telling whether a node exists.
func (n *Host) serveCard(w http.ResponseWriter, r *http.Request, kind string, base func(*http.Request) string, id string) {
	serve := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		node, err := n.cfg.Store.Node(r.Context(), id)
		if err != nil {
			jsonhttp.Error(w, http.StatusNotFound, "no such agent")
			return
		}
		a2asrv.NewStaticAgentCardHandler(n.card(base(r), node)).ServeHTTP(w, r)
	})
	if n.cfg.Advertised != nil && n.cfg.Advertised(id) {
		serve.ServeHTTP(w, r)
		return
	}
	n.cfg.Authenticate(kind, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p, ok := auth.PrincipalFrom(r.Context()); !ok || !mayCallNode(p, n.cfg.Advertised, id) {
			jsonhttp.Error(w, http.StatusForbidden, errNodeCaller)
			return
		}
		serve.ServeHTTP(w, r)
	})).ServeHTTP(w, r)
}

func (n *Host) card(baseURL string, node store.Node) *a2a.AgentCard {
	u := mailboxapi.NodeURL(baseURL, node.ID)
	desc := node.Description
	if desc == "" {
		desc = node.Name + " is a webhook node hosted by " + n.cfg.Runtime.Config().Name + "."
	}
	return &a2a.AgentCard{
		Name:        node.Name,
		Description: desc,
		Version:     "1.0.0",
		SupportedInterfaces: []*a2a.AgentInterface{
			a2a.NewAgentInterface(u, a2a.TransportProtocolJSONRPC),
			a2a.NewAgentInterface(u, a2a.TransportProtocolHTTPJSON),
		},
		Capabilities:       a2a.AgentCapabilities{Streaming: true},
		DefaultInputModes:  []string{"text/plain", "application/json"},
		DefaultOutputModes: []string{"text/plain", "application/json"},
		Skills: []a2a.AgentSkill{{
			ID: "node", Name: node.Name, Tags: []string{"async"},
			Description: "Works asynchronously: a task stays submitted until the agent wakes up.",
		}},
	}
}

// Mailbox counts the wakes of node id not delivered yet, and its tasks
// waiting for a reply.
func (n *Host) Mailbox(id string) (waiting, open int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, e := range n.inbox[id] {
		if !e.Delivered {
			waiting++
		}
	}
	for _, w := range n.open {
		if w.node == id {
			open++
		}
	}
	return waiting, open
}

func (n *Host) wireInfo(baseURL string, node store.Node) mailboxapi.NodeInfo {
	return mailboxapi.NodeInfo{
		ID: node.ID, Name: node.Name, Description: node.Description, URL: mailboxapi.NodeURL(baseURL, node.ID),
		Webhook: mailboxapi.RedactWebhookURL(node.Webhook), Paired: node.TokenHash != "", CreatedAt: node.Created,
	}
}

// nodeAPI authenticates a node by its node token.
func (n *Host) nodeAPI(base func(*http.Request) string, h func(http.ResponseWriter, *http.Request, string, store.Node)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, _ := auth.BearerToken(r.Header.Get("Authorization"))
		if _, _, ok := mailboxapi.ParseNodeToken(tok); !ok {
			jsonhttp.Error(w, http.StatusUnauthorized, "node token required")
			return
		}
		node, ok, err := auth.NodeOfToken(r.Context(), n.cfg.Store, tok)
		if err != nil {
			jsonhttp.Error(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !ok {
			jsonhttp.Error(w, http.StatusUnauthorized, "invalid node token")
			return
		}
		h(w, r, base(r), node)
	}
}

func (n *Host) handleGetNode(w http.ResponseWriter, _ *http.Request, baseURL string, node store.Node) {
	jsonhttp.Write(w, http.StatusOK, n.wireInfo(baseURL, node))
}

func (n *Host) handleGetTask(w http.ResponseWriter, r *http.Request, _ string, node store.Node) {
	t, err := n.tasks(r.Context(), node.ID).Get(auth.WithPrincipal(r.Context(), auth.Owner()), a2a.TaskID(r.PathValue("id")))
	if err != nil {
		jsonhttp.Error(w, http.StatusNotFound, fmt.Sprintf("no task %s for node %s", r.PathValue("id"), node.ID))
		return
	}
	jsonhttp.Write(w, http.StatusOK, t.Task)
}

func (n *Host) handleInbox(w http.ResponseWriter, r *http.Request, _ string, node store.Node) {
	all := r.URL.Query().Get("all") != ""
	n.mu.Lock()
	out := []mailboxapi.InboxEntry{}
	for _, e := range n.inbox[node.ID] {
		entry := *e
		if entry.Envelope.Kind == mailboxapi.KindTask || entry.Envelope.Kind == mailboxapi.KindMessage {
			entry.Open = n.open[a2a.TaskID(entry.Envelope.TaskID)] != nil
		}
		if all || !entry.Delivered || entry.Open {
			out = append(out, entry)
		}
	}
	n.mu.Unlock()
	jsonhttp.Write(w, http.StatusOK, mailboxapi.Inbox{Entries: out})
}

func (n *Host) handleEvent(w http.ResponseWriter, r *http.Request, _ string, node store.Node) {
	taskID := a2a.TaskID(r.PathValue("id"))
	var ev mailboxapi.Event
	if !jsonhttp.Read(w, r, &ev) {
		return
	}
	state, ok := mailboxapi.ParseState(ev.State)
	if !ok {
		jsonhttp.Error(w, http.StatusBadRequest, "state must be one of working, input-required, auth-required, completed, failed, rejected, canceled")
		return
	}
	ev.State = string(state)
	parts := ev.Parts
	if ev.Text != "" {
		parts = append([]*a2a.Part{a2a.NewTextPart(ev.Text)}, parts...)
	}
	var metadata []map[string]any
	for _, a := range ev.Artifacts {
		if a == nil || len(a.Parts) == 0 {
			jsonhttp.Error(w, http.StatusBadRequest, "every artifact needs at least one part")
			return
		}
		parts = append(parts, a.Parts...)
		metadata = append(metadata, a.Metadata)
	}
	// A reply counts as one message, so that its artifacts can't add up to
	// more than a message may have.
	if err := a2alimit.Default().CheckParts("a reply", parts, metadata...); err != nil {
		jsonhttp.Error(w, http.StatusBadRequest, err.Error())
		return
	}

	res, err := n.postEvent(r.Context(), node.ID, taskID, ev)
	if err != nil {
		jsonhttp.Error(w, http.StatusConflict, err.Error())
		return
	}
	jsonhttp.Write(w, http.StatusOK, res)
}

// postEvent hands ev to the open task taskID of node. A retry of an event
// that is still being applied waits for its result, and is applied itself
// if the first attempt fails.
func (n *Host) postEvent(ctx context.Context, node string, taskID a2a.TaskID, ev mailboxapi.Event) (mailboxapi.EventResult, error) {
	for {
		n.mu.Lock()
		wt := n.open[taskID]
		if wt != nil && wt.node != node {
			wt = nil
		}
		keys := n.keys[taskID]
		if prev := keys.lookup(ev.IdempotencyKey); prev != nil {
			n.mu.Unlock()
			select {
			case <-prev.done:
			case <-ctx.Done():
				return mailboxapi.EventResult{}, fmt.Errorf("task %s: %v", taskID, ctx.Err())
			}
			if prev.err != nil {
				continue
			}
			res := prev.res
			res.Duplicate = true
			return res, nil
		}
		if wt == nil {
			n.mu.Unlock()
			current := "unknown"
			if t, err := n.tasks(ctx, node).Get(auth.WithPrincipal(ctx, auth.Owner()), taskID); err == nil {
				current = mailboxapi.StateName(t.Task.Status.State)
			}
			return mailboxapi.EventResult{}, fmt.Errorf("task %s is %s and not waiting for an event from the node", taskID, current)
		}
		state, _ := mailboxapi.ParseState(ev.State)
		res := mailboxapi.EventResult{TaskID: string(taskID), State: mailboxapi.StateName(state)}
		var claim *keyedEvent
		if ev.IdempotencyKey != "" {
			if keys == nil || keys.w != wt {
				keys = &turnKeys{w: wt, seen: map[string]*keyedEvent{}}
				n.keys[taskID] = keys
			}
			claim = &keyedEvent{done: make(chan struct{}), res: res}
			keys.seen[ev.IdempotencyKey] = claim
		}
		n.mu.Unlock()

		err := deliverEvent(ctx, wt, ev)
		if claim != nil {
			n.mu.Lock()
			if err != nil && keys.seen[ev.IdempotencyKey] == claim {
				delete(keys.seen, ev.IdempotencyKey)
			}
			claim.err = err
			close(claim.done)
			n.mu.Unlock()
		}
		if err != nil {
			return mailboxapi.EventResult{}, fmt.Errorf("task %s: %v", taskID, err)
		}
		return res, nil
	}
}

// lookup returns the event posted with key, if any.
func (k *turnKeys) lookup(key string) *keyedEvent {
	if k == nil || key == "" {
		return nil
	}
	return k.seen[key]
}

// deliverEvent hands ev to the execution waiting on w and returns the
// result of applying it.
func deliverEvent(ctx context.Context, w *nodeWaiter, ev mailboxapi.Event) error {
	d := nodeDelivery{ev: ev, result: make(chan error, 1)}
	select {
	case w.events <- d:
		return <-d.result
	case <-w.done:
		return errTaskClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

// nodeExecutor wakes a node and turns its events into A2A updates. An
// execution stays open until the node posts input-required or a terminal
// state, or posts nothing for nodeTaskTimeout.
type nodeExecutor struct {
	n    *Host
	node string
}

var _ a2asrv.AgentExecutor = (*nodeExecutor)(nil)

func (e *nodeExecutor) Execute(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		n := e.n
		w, err := n.openTask(ec.TaskID, e.node, cappedCaller(ctx), ec.StoredTask == nil)
		if err != nil {
			yield(nil, err)
			return
		}
		defer n.closeTask(ec.TaskID, w)

		kind, state := mailboxapi.KindMessage, a2a.TaskStateWorking
		if ec.StoredTask == nil {
			kind, state = mailboxapi.KindTask, a2a.TaskStateSubmitted
			if !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
				return
			}
		} else if !yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateWorking, nil), nil) {
			return
		}

		n.wake(e.node, mailboxapi.Envelope{
			Kind: kind, TaskID: string(ec.TaskID), ContextID: ec.ContextID, Sender: senderOf(ctx, n.cfg.Runtime.Config().Name),
			Message: ec.Message, Text: a2atext.Message(ec.Message), State: mailboxapi.StateName(state),
		})

		timer := time.NewTimer(nodeTaskTimeout)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-w.done:
				return
			case <-timer.C:
				msg := a2a.NewMessageForTask(a2a.MessageRoleAgent, ec,
					a2a.NewTextPart(fmt.Sprintf("The node did not respond within %s.", nodeTaskTimeout)))
				yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateFailed, msg), nil)
				return
			case d := <-w.events:
				finished, err := applyNodeEvent(ec, d.ev, yield)
				d.result <- err
				if finished || err != nil {
					return
				}
				timer.Reset(nodeTaskTimeout)
			}
		}
	}
}

var errTaskClosed = errors.New("the task is no longer waiting for events")

// applyNodeEvent emits the A2A events for a node event and reports whether
// the execution is over.
func applyNodeEvent(ec *a2asrv.ExecutorContext, ev mailboxapi.Event, yield func(a2a.Event, error) bool) (bool, error) {
	state, _ := mailboxapi.ParseState(ev.State)
	for _, art := range ev.Artifacts {
		if art.ID == "" {
			art.ID = a2a.NewArtifactID()
		}
		if !yield(&a2a.TaskArtifactUpdateEvent{TaskID: ec.TaskID, ContextID: ec.ContextID, Artifact: art, LastChunk: true}, nil) {
			return true, errTaskClosed
		}
	}
	parts := ev.Parts
	if ev.Text != "" {
		parts = append([]*a2a.Part{a2a.NewTextPart(ev.Text)}, parts...)
	}
	var msg *a2a.Message
	if len(parts) > 0 {
		if state == a2a.TaskStateCompleted {
			art := a2a.NewArtifactEvent(ec, parts...)
			art.Artifact.Name = "response"
			art.LastChunk = true
			if !yield(art, nil) {
				return true, errTaskClosed
			}
		} else {
			msg = a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, parts...)
		}
	}
	if !yield(a2a.NewStatusUpdateEvent(ec, state, msg), nil) {
		return true, errTaskClosed
	}
	return state.Terminal() || state == a2a.TaskStateInputRequired || state == a2a.TaskStateAuthRequired, nil
}

func (e *nodeExecutor) Cancel(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		n := e.n
		n.mu.Lock()
		w := n.open[ec.TaskID]
		n.mu.Unlock()
		if w != nil {
			w.close()
		}
		n.wake(e.node, mailboxapi.Envelope{Kind: mailboxapi.KindCanceled, TaskID: string(ec.TaskID), ContextID: ec.ContextID,
			Sender: senderOf(ctx, n.cfg.Runtime.Config().Name), State: mailboxapi.StateName(a2a.TaskStateCanceled)})
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCanceled, nil), nil)
	}
}

func senderOf(ctx context.Context, agent string) mailboxapi.Sender {
	p, ok := auth.Caller(ctx)
	if !ok {
		return mailboxapi.Sender{Kind: mailboxapi.SenderServer}
	}
	if strings.HasPrefix(p.ID, "agent:") {
		return mailboxapi.Sender{Kind: mailboxapi.SenderAgent, Name: agent, OnBehalfOf: p.OnBehalfOf}
	}
	return mailboxapi.Sender{Kind: mailboxapi.SenderOwner, Name: p.ID}
}

// The open tasks a node takes from callers other than its owners and the
// hosting agent, per caller and in all.
const (
	maxCallerOpenTasks = 20
	maxNodeOpenTasks   = 100
)

// openTask opens task id of node for events. caller is the capped caller
// the task is for, or empty; a new task past its caps is refused.
func (n *Host) openTask(id a2a.TaskID, node, caller string, isNew bool) (*nodeWaiter, error) {
	w := &nodeWaiter{node: node, caller: caller, events: make(chan nodeDelivery), done: make(chan struct{})}
	n.mu.Lock()
	defer n.mu.Unlock()
	if isNew && caller != "" {
		if err := n.checkCaps(node, caller); err != nil {
			return nil, err
		}
	}
	if old := n.open[id]; old != nil {
		old.close()
	}
	delete(n.keys, id)
	n.open[id] = w
	return w, nil
}

// checkCaps returns an error if caller may open no more tasks on node.
// n.mu is held.
func (n *Host) checkCaps(node, caller string) error {
	ofCaller, capped := 0, 0
	for _, w := range n.open {
		if w.node != node || w.caller == "" {
			continue
		}
		capped++
		if w.caller == caller {
			ofCaller++
		}
	}
	if ofCaller >= maxCallerOpenTasks {
		return fmt.Errorf("%w: you have %d open tasks on %s; send more when one finishes", a2a.ErrUnauthorized, ofCaller, node)
	}
	if capped >= maxNodeOpenTasks {
		return fmt.Errorf("%w: %s has %d open tasks; try again when one finishes", a2a.ErrUnauthorized, node, capped)
	}
	return nil
}

// cappedCaller returns the caller of ctx if the open task caps apply to it.
func cappedCaller(ctx context.Context) string {
	p, ok := auth.Caller(ctx)
	if !ok || p.IsOwner() || p.Network == network.ListenerHarness {
		return ""
	}
	return p.ID
}

// idempotencyRetention is how long the idempotency keys of a closed task are
// kept, so that a node retrying its last event still gets a duplicate reply.
// Reopening the task drops them.
var idempotencyRetention = 10 * time.Minute

func (n *Host) closeTask(id a2a.TaskID, w *nodeWaiter) {
	w.close()
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.open[id] != w {
		return
	}
	delete(n.open, id)
	time.AfterFunc(idempotencyRetention, func() {
		n.mu.Lock()
		defer n.mu.Unlock()
		if k := n.keys[id]; k != nil && k.w == w {
			delete(n.keys, id)
		}
	})
}

// wake records an envelope in the node's inbox and delivers it to the
// node's webhook in the background.
func (n *Host) wake(nodeID string, env mailboxapi.Envelope) *mailboxapi.InboxEntry {
	env.ID = "wake_" + cryptoutil.RandomString(12, cryptoutil.Alphanumeric)
	env.Node = nodeID
	env.CreatedAt = time.Now().UTC()
	if env.TaskID != "" && (env.Kind == mailboxapi.KindTask || env.Kind == mailboxapi.KindMessage) {
		env.ReplyURL = mailboxapi.APIBase(n.cfg.BaseURL()) + "/api/tasks/" + env.TaskID + "/events"
	}
	env.Instructions = instructions(env)
	entry := &mailboxapi.InboxEntry{Envelope: env}

	n.mu.Lock()
	list := append(n.inbox[nodeID], entry)
	if len(list) > inboxSize {
		list = list[len(list)-inboxSize:]
	}
	n.inbox[nodeID] = list
	if !n.stopped {
		n.deliveries.Go(func() { n.deliver(n.ctx, entry) })
	}
	n.mu.Unlock()
	return entry
}

// Stop waits for the wake deliveries in progress, which end with the
// context of the host, and keeps later wakes from being delivered.
func (n *Host) Stop() {
	n.mu.Lock()
	n.stopped = true
	n.mu.Unlock()
	n.deliveries.Wait()
}

func (n *Host) deliver(ctx context.Context, entry *mailboxapi.InboxEntry) {
	env := entry.Envelope
	body, err := json.Marshal(env)
	if err != nil {
		slog.Error("encoding envelope", "error", err)
		return
	}
	for i, delay := range n.cfg.WebhookRetries {
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		node, err := n.cfg.Store.Node(ctx, env.Node)
		if errors.Is(err, store.ErrNotFound) {
			return
		}
		if err != nil {
			slog.Error("loading node for webhook delivery", "node", env.Node, "error", err)
			return
		}
		err = postWebhook(ctx, node, env.ID, body)
		n.mu.Lock()
		entry.Attempts = i + 1
		if err == nil {
			entry.Delivered, entry.LastError = true, ""
		} else {
			entry.LastError = wakeError(err)
		}
		n.mu.Unlock()
		if err == nil {
			slog.Info("woke node", "node", node.ID, "kind", env.Kind, "task", env.TaskID)
			return
		}
		slog.Warn("webhook delivery failed", "node", node.ID, "webhook", mailboxapi.WebhookOrigin(node.Webhook), "attempt", i+1, "error", err)
	}
}

// postWebhook delivers a wake to the node's webhook. Its errors leave out
// the URL and the response body, which may hold secrets.
func postWebhook(ctx context.Context, node store.Node, wakeID string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, node.Webhook, bytes.NewReader(body))
	if err != nil {
		return withoutURL(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "a2a-server")
	req.Header.Set(mailboxapi.WakeHeader, wakeID)
	req.Header.Set(mailboxapi.SignatureHeader, mailboxapi.Sign(node.Secret, time.Now(), body))
	for k, v := range node.Headers {
		req.Header.Set(k, v)
	}
	client := wakeClient(node)
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return withoutURL(err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			slog.Debug("closing webhook response", "error", err)
		}
	}()
	if resp.StatusCode/100 != 2 {
		return &statusError{code: resp.StatusCode}
	}
	return nil
}

// statusError is the error of a wake the webhook answered with a status
// other than 2xx.
type statusError struct{ code int }

func (e *statusError) Error() string {
	return fmt.Sprintf("webhook returned %d %s", e.code, http.StatusText(e.code))
}

// errWakeUnreachable is what the node sees of a wake that got no response.
const errWakeUnreachable = "the webhook couldn't be reached"

// wakeError is what the node sees of a failed wake. It keeps only the
// status of a response, so that wakes can't probe the networks the server
// reaches.
func wakeError(err error) string {
	var se *statusError
	if errors.As(err, &se) {
		return se.Error()
	}
	return errWakeUnreachable
}

// wakeTimeout bounds one wake delivery, redirects included.
const wakeTimeout = 20 * time.Second

// wakeClient returns the HTTP client for the wakes of a node. It follows
// redirects, but doesn't send the node's headers and signature to another
// host than the webhook. Redirects can't lead from https to http, or from
// a public webhook into a private network.
func wakeClient(node store.Node) *http.Client {
	return &http.Client{
		Timeout:   wakeTimeout,
		Transport: egress.Transport(node.Webhook),
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if err := egress.CheckRedirect(req, via); err != nil {
				return err
			}
			if req.URL.Host == via[0].URL.Host {
				return nil
			}
			for k := range node.Headers {
				req.Header.Del(k)
			}
			req.Header.Del(mailboxapi.SignatureHeader)
			return nil
		},
	}
}

// withoutURL drops the URL of a *url.Error.
func withoutURL(err error) error {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return uerr.Err
	}
	return err
}

// instructions tell an LLM-driven node what a wake means and how to answer.
// Anyone who knows the webhook can send a wake with other instructions, so
// they point to the skill for anything that needs the node token.
func instructions(env mailboxapi.Envelope) string {
	switch env.Kind {
	case mailboxapi.KindTask, mailboxapi.KindMessage:
		what := "New A2A task"
		if env.Kind == mailboxapi.KindMessage {
			what = "The caller answered your question on A2A task"
		}
		return fmt.Sprintf("%s %s from %s. "+
			"Reply with `%s reply %s --final \"<answer>\"` (add --artifact <file> to attach files). "+
			"Use `--status working --text \"<progress>\"` to report progress, `--input-required \"<question>\"` to ask the caller, "+
			"`--fail \"<reason>\"` to give up. Without the CLI, follow \"Without the CLI\" in the skill you paired with. "+
			"The task fails if you post nothing for %s.",
			what, env.TaskID, env.Sender, mailboxapi.CLI, env.TaskID, nodeTaskTimeout)
	case mailboxapi.KindCanceled:
		return fmt.Sprintf("The caller canceled task %s. Stop working on it; replies are no longer accepted.", env.TaskID)
	case mailboxapi.KindPing:
		return "Test wake. No reply needed."
	}
	return ""
}
