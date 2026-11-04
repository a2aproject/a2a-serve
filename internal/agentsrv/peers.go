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
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/a2aproject/a2a-serve/internal/agentsrv/approvals"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/nodes"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/channel"
	"github.com/a2aproject/a2a-serve/internal/cryptoutil"
	"github.com/a2aproject/a2a-serve/internal/gossip"
	"github.com/a2aproject/a2a-serve/internal/manage"
	"github.com/a2aproject/a2a-serve/internal/network"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// Contact routes.
const (
	ViaPrivate = manage.ViaPrivate
	ViaHTTPS   = manage.ViaHTTPS
)

// Peers handles calls from this agent to other agents: contacts, agents on
// the private network and webhook nodes hosted here. The harness reaches
// them with the a2a CLI through the agents URL of its conversation.
type Peers struct {
	store     *store.Store
	private   network.Provider
	nodes     *nodes.Host
	approvals *approvals.Service
	chat      chat
	schedules *schedules
	// chain gives principals stored to act later their role.
	chain     *auth.Chain
	ads       *gossip.Table
	agentName func() string
	bridge    harnessBridge
	timings   Timings
	// ctx ends when the server stops; watches run in it.
	ctx context.Context

	handleMu sync.Mutex

	turnsMu sync.Mutex
	// turns has the claims of the running turn of each conversation; a2aacp
	// runs one turn of a conversation at a time.
	turns map[string]*callClaims
	// starting counts the turns of each conversation which run but haven't
	// begun yet.
	starting map[string]int
	// turnsChanged is closed when turns or starting change.
	turnsChanged chan struct{}
	// paused has the claims of the turn of each conversation which waits
	// for an answer, so that the answer resumes it for the same user.
	paused map[string]pausedTurn

	// buildMu serializes building contact clients, so that two calls don't
	// build one each and destroy the other's.
	buildMu sync.Mutex

	mu       sync.Mutex
	watching map[string]bool
	cache    []network.PeerInfo
	cachedAt time.Time
	clients  map[string]*contactClient
}

// chat is where peers show relayed questions and post late results: the
// router.
type chat interface {
	approvals.Notifier
	injectTurn(ctx context.Context, contextID string, p auth.Principal, text string) error
}

// harnessBridge is how the harness reaches the server: through the Unix
// socket, or over TCP at url with key, which the credential helper of
// helperEnv gives the harness's a2a CLI.
type harnessBridge struct {
	socket    string
	url       string
	key       string
	helperEnv string
}

// contactClient is the A2A client of a contact, built for the contact as
// stored when it was made.
type contactClient struct {
	contact store.Contact
	client  *a2aclient.Client
}

// callClaims is who calls through the agents URL of a conversation: the
// harness in the conversation's running turn, for the user who started it.
type callClaims struct {
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
func (p *Peers) handle(ctx context.Context, contextID string) (string, error) {
	p.handleMu.Lock()
	defer p.handleMu.Unlock()
	for {
		h, err := p.store.HandleOf(ctx, contextID)
		if err == nil {
			return h.Handle, nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return "", err
		}
		h = store.Handle{Handle: cryptoutil.RandomString(handleLen, handleAlphabet), ContextID: contextID}
		err = p.store.PutHandle(ctx, h)
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
func (p *Peers) claimsOf(ctx context.Context, handle string) (callClaims, error) {
	h, err := p.store.HandleByName(ctx, handle)
	if errors.Is(err, store.ErrNotFound) {
		return callClaims{}, errUnknownHandle
	}
	if err != nil {
		return callClaims{}, fmt.Errorf("loading agents URL handle: %w", err)
	}
	for {
		p.turnsMu.Lock()
		c, starting, changed := p.turns[h.ContextID], p.starting[h.ContextID], p.turnsChanged
		p.turnsMu.Unlock()
		if c != nil {
			return *c, nil
		}
		if starting == 0 {
			return callClaims{}, errNoTurn
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return callClaims{}, ctx.Err()
		}
	}
}

// startingTurn marks a turn of a conversation as starting, until done is
// called: a2aacp prompts the harness before it yields the working update
// which begins the turn, so the harness can call its agents URL first.
func (p *Peers) startingTurn(contextID string) (done func()) {
	p.turnsMu.Lock()
	p.starting[contextID]++
	p.turnsMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			p.turnsMu.Lock()
			defer p.turnsMu.Unlock()
			if p.starting[contextID]--; p.starting[contextID] == 0 {
				delete(p.starting, contextID)
			}
			p.notifyTurnsLocked()
		})
	}
}

// notifyTurnsLocked wakes the calls waiting for a turn to begin.
func (p *Peers) notifyTurnsLocked() {
	close(p.turnsChanged)
	p.turnsChanged = make(chan struct{})
}

// pausedTurn is a turn which waits for an answer to a question of its task.
type pausedTurn struct {
	task   a2a.TaskID
	claims callClaims
}

// beginTurn opens the agents URL of c's conversation to calls with c, until
// end is called.
func (p *Peers) beginTurn(c callClaims) (end func()) {
	turn := &c
	p.turnsMu.Lock()
	p.turns[c.ContextID] = turn
	p.notifyTurnsLocked()
	p.turnsMu.Unlock()
	return func() {
		p.turnsMu.Lock()
		defer p.turnsMu.Unlock()
		if p.turns[c.ContextID] == turn {
			delete(p.turns, c.ContextID)
		}
	}
}

// duringTurn opens the agents URL of the conversation while the harness
// runs a turn of it: from the turn's working update, which a2aacp sends once
// the turn holds the conversation, until its events end. A turn which stops
// for a question resumes for its user, whoever answers.
func (p *Peers) duringTurn(ctx context.Context, execCtx *a2asrv.ExecutorContext, events iter.Seq2[a2a.Event, error]) iter.Seq2[a2a.Event, error] {
	c := p.turnClaims(ctx, execCtx)
	return func(yield func(a2a.Event, error) bool) {
		var (
			end   func()
			state a2a.TaskState
		)
		started := p.startingTurn(c.ContextID)
		defer func() {
			started()
			if end != nil {
				end()
			}
			if state == a2a.TaskStateInputRequired {
				p.turnsMu.Lock()
				p.paused[c.ContextID] = pausedTurn{task: execCtx.TaskID, claims: c}
				p.turnsMu.Unlock()
			}
		}()
		for ev, err := range events {
			switch e := ev.(type) {
			case *a2a.Task:
				state = e.Status.State
			case *a2a.TaskStatusUpdateEvent:
				state = e.Status.State
				if end == nil && state == a2a.TaskStateWorking {
					end = p.beginTurn(c)
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
func (p *Peers) turnClaims(ctx context.Context, execCtx *a2asrv.ExecutorContext) callClaims {
	p.turnsMu.Lock()
	paused, ok := p.paused[execCtx.ContextID]
	ok = ok && paused.task == execCtx.TaskID
	if ok {
		delete(p.paused, execCtx.ContextID)
	}
	p.turnsMu.Unlock()
	if ok && execCtx.StoredTask != nil && execCtx.StoredTask.Status.State == a2a.TaskStateInputRequired {
		return paused.claims
	}
	c := callClaims{ContextID: execCtx.ContextID}
	if caller, ok := auth.Caller(ctx); ok {
		c.User = caller
	} else if execCtx.User != nil {
		c.User = auth.Principal{ID: execCtx.User.Name}
	}
	return c
}

// discover lists agents on the private network, cached briefly.
func (p *Peers) discover(ctx context.Context) ([]network.PeerInfo, error) {
	if p.private == nil {
		return nil, errors.New("this agent has no private network; add one with `a2a-serve config set --private tailscale` (or a relay URL) and `a2a-serve restart`")
	}
	p.mu.Lock()
	if time.Since(p.cachedAt) < 30*time.Second {
		out := p.cache
		p.mu.Unlock()
		return out, nil
	}
	p.mu.Unlock()
	peers, err := p.private.Peers(ctx)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.cache, p.cachedAt = peers, time.Now()
	p.mu.Unlock()
	return peers, nil
}

// contacts manages the contacts, found by name on the private network.
func (p *Peers) contacts() manage.Contacts {
	return manage.Contacts{Store: p.store, Locate: p.locate, HTTPClient: p.httpClient}
}

// locate returns the URL of the contact name, an advertised pairing or an
// agent found by name on the private network, when u is empty, and the
// route to it.
func (p *Peers) locate(ctx context.Context, name, u string) (string, string, error) {
	if u != "" {
		if p.private != nil && p.onPrivateNetwork(ctx, u) {
			return u, ViaPrivate, nil
		}
		return u, ViaHTTPS, nil
	}
	if node, host, ok := gossip.ParseKey(name); ok {
		u, err := p.advertisedURL(ctx, name, node, host)
		return u, ViaPrivate, err
	}
	peers, err := p.discover(ctx)
	if err != nil {
		return "", "", err
	}
	for _, pi := range peers {
		if strings.EqualFold(pi.Name, name) {
			return pi.URL, ViaPrivate, nil
		}
	}
	return "", "", fmt.Errorf("no agent named %q on the private network; pass its URL with --url", name)
}

// advertisedURL returns the URL of a pairing advertised as key: the node
// name at the agent host, found by name on the private network. The
// advertisement only says it exists, so it can't send calls elsewhere.
func (p *Peers) advertisedURL(ctx context.Context, key, name, host string) (string, error) {
	if _, ok := p.ads.Lookup(key); !ok {
		return "", fmt.Errorf("no agent advertises %s on the private network; `a2a-serve peers` lists the advertised pairings", key)
	}
	peers, err := p.discover(ctx)
	if err != nil {
		return "", err
	}
	for _, pi := range peers {
		if strings.EqualFold(pi.Name, host) {
			return trimSlash(pi.URL) + "/n/" + name, nil
		}
	}
	return "", fmt.Errorf("%s advertises %s, but isn't on the private network now", host, key)
}

func (p *Peers) onPrivateNetwork(ctx context.Context, u string) bool {
	peers, err := p.discover(ctx)
	if err != nil {
		return false
	}
	for _, pi := range peers {
		if trimSlash(pi.URL) == trimSlash(u) {
			return true
		}
	}
	return false
}

// httpClient returns the HTTP client for a contact, which sends its token
// to the contact's host only. It has no overall timeout, because streams to
// the contact last as long as its task; callers bound requests with their
// context.
func (p *Peers) httpClient(c store.Contact) *http.Client {
	base := http.DefaultClient
	if c.Via == ViaPrivate && p.private != nil {
		base = p.private.HTTPClient()
	}
	return manage.TokenClient(base, c)
}

// viaAgent returns ctx for calling another agent from a conversation hops
// agents deep. The agent called caps what the call may do by the hop count;
// the user stays with this server, which relays the approvals.
func viaAgent(ctx context.Context, hops int) context.Context {
	return a2aclient.AttachServiceParams(ctx, a2aclient.ServiceParams{auth.HeaderHops: {strconv.Itoa(hops + 1)}})
}

// target is an agent the server calls for a user.
type target interface {
	send(ctx context.Context, msg *a2a.Message) (a2a.SendMessageResult, error)
	get(ctx context.Context, id a2a.TaskID) (*a2a.Task, error)
}

type remoteTarget struct {
	c    *a2aclient.Client
	hops int
}

func (t remoteTarget) send(ctx context.Context, msg *a2a.Message) (a2a.SendMessageResult, error) {
	return t.c.SendMessage(viaAgent(ctx, t.hops), &a2a.SendMessageRequest{Message: msg, Config: &a2a.SendMessageConfig{ReturnImmediately: true}})
}

func (t remoteTarget) get(ctx context.Context, id a2a.TaskID) (*a2a.Task, error) {
	return t.c.GetTask(viaAgent(ctx, t.hops), &a2a.GetTaskRequest{ID: id})
}

type localTarget struct {
	h a2asrv.RequestHandler
	p auth.Principal
}

func (t localTarget) send(ctx context.Context, msg *a2a.Message) (a2a.SendMessageResult, error) {
	return t.h.SendMessage(auth.InProcess(ctx, t.p), &a2a.SendMessageRequest{Message: msg, Config: &a2a.SendMessageConfig{ReturnImmediately: true}})
}

func (t localTarget) get(ctx context.Context, id a2a.TaskID) (*a2a.Task, error) {
	return t.h.GetTask(auth.InProcess(ctx, t.p), &a2a.GetTaskRequest{ID: id})
}

// ErrOffline is returned when a contact can't be reached.
var ErrOffline = errors.New("offline")

// target resolves an agent name to a contact or a hosted node, called for
// user hops agents deep.
func (p *Peers) target(ctx context.Context, name string, user auth.Principal, hops int) (target, error) {
	node, err := p.isNode(ctx, name)
	if err != nil {
		return nil, err
	}
	if node {
		return localTarget{h: p.nodes.Handler(name), p: p.nodeCaller(user, hops)}, nil
	}
	client, err := p.contactClient(ctx, name)
	if err != nil {
		return nil, err
	}
	return remoteTarget{c: client, hops: hops}, nil
}

// isNode reports whether name is a webhook node hosted here.
func (p *Peers) isNode(ctx context.Context, name string) (bool, error) {
	_, err := p.store.Node(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// nodeCaller is the principal this agent calls its nodes as, for user hops
// agents deep. Its harness is another agent to the node, so it is capped
// at authenticated; the node admits the agent hosting it explicitly.
func (p *Peers) nodeCaller(user auth.Principal, hops int) auth.Principal {
	return auth.Principal{
		ID: "agent:" + p.agentName(), Role: auth.RoleAuthenticated, OnBehalfOf: user.ID,
		Identity: auth.Identity{Network: network.ListenerHarness, Hops: hops + 1},
	}
}

// contactClient returns the client of a contact, one per contact: the user
// and hop count go with each request. A client is rebuilt when the contact
// changed, and dropped with the contact. It returns [store.ErrNotFound] for
// a name which isn't a contact.
func (p *Peers) contactClient(ctx context.Context, name string) (*a2aclient.Client, error) {
	c, err := p.store.Contact(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		p.forgetClient(name)
		return nil, err
	}
	if err != nil {
		return nil, err
	}
	if client := p.cachedClient(name, c); client != nil {
		return client, nil
	}
	p.buildMu.Lock()
	defer p.buildMu.Unlock()
	if client := p.cachedClient(name, c); client != nil {
		return client, nil
	}
	hc := p.httpClient(c)
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	card, err := agentcard.NewResolver(hc).Resolve(cctx, c.URL)
	if err != nil {
		return nil, fmt.Errorf("%s is %w (%v)", name, ErrOffline, err)
	}
	client, err := a2aclient.NewFromCard(ctx, card, a2aclient.WithJSONRPCTransport(hc), a2aclient.WithRESTTransport(hc))
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", name, err)
	}
	p.mu.Lock()
	old := p.clients[name]
	p.clients[name] = &contactClient{contact: c, client: client}
	p.mu.Unlock()
	if old != nil {
		destroyClient(old.client)
	}
	return client, nil
}

func (p *Peers) cachedClient(name string, c store.Contact) *a2aclient.Client {
	p.mu.Lock()
	defer p.mu.Unlock()
	if cached := p.clients[name]; cached != nil && sameContact(cached.contact, c) {
		return cached.client
	}
	return nil
}

func sameContact(a, b store.Contact) bool {
	return a.URL == b.URL && a.Token == b.Token && a.Via == b.Via
}

// forgetClient drops the client of a contact, so that the next call
// resolves its card again.
func (p *Peers) forgetClient(name string) {
	p.mu.Lock()
	old := p.clients[name]
	delete(p.clients, name)
	p.mu.Unlock()
	if old != nil {
		destroyClient(old.client)
	}
}

// close drops the clients of all contacts.
func (p *Peers) close(context.Context) error {
	p.mu.Lock()
	clients := p.clients
	p.clients = map[string]*contactClient{}
	p.mu.Unlock()
	for _, c := range clients {
		destroyClient(c.client)
	}
	return nil
}

func destroyClient(c *a2aclient.Client) {
	if err := c.Destroy(); err != nil {
		slog.Debug("closing a2a client", "error", err)
	}
}

// relay asks the user on whose behalf a remote task runs to answer its
// permission question, once per question, also across restarts: in the
// conversation the call came from, or else in the user's direct chat, or
// else in the owner chat.
func (p *Peers) relay(ctx context.Context, claims callClaims, agent string, task *a2a.Task, q *approvals.Question) {
	user := claims.User.ID
	if user == "" {
		user = auth.OwnerID
	}
	rec, err := p.approvals.NewRecord(ctx, task.ID, task.ContextID, agent, user, q)
	if errors.Is(err, store.ErrExists) {
		return
	}
	if err != nil {
		slog.Error("storing remote question", "error", err)
		return
	}
	chName, conv, err := p.store.ConvOf(ctx, claims.ContextID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		slog.Warn("finding the conversation of a relayed question", "context", claims.ContextID, "error", err)
	}
	header := fmt.Sprintf("%s wants to", agent)
	if q.Title != "" {
		header = fmt.Sprintf("%s wants to run %s", agent, q.Title)
	}
	if chName != "" {
		p.chat.Ask(ctx, approvals.Chat{Channel: chName, Conv: channel.ConvKey(conv)}, header, rec)
		return
	}
	if to, ok := p.directChat(ctx, user); ok {
		p.chat.Ask(ctx, to, header, rec)
		return
	}
	to, ok := p.chat.OwnerChat(ctx)
	if !ok {
		slog.Info("remote approval waits in the inspector: no chat", "agent", agent, "user", user, "question", rec.ID)
		return
	}
	p.chat.Ask(ctx, to, header+fmt.Sprintf(" for %s", auth.DisplayPrincipal(user)), rec)
}

// directChat returns the first direct chat of user in a messenger.
func (p *Peers) directChat(ctx context.Context, user string) (approvals.Chat, bool) {
	name, ok := auth.PrincipalPairing(user)
	if !ok {
		return approvals.Chat{}, false
	}
	links, err := p.store.LinksOf(ctx, name)
	if err != nil {
		slog.Warn("listing links", "user", user, "error", err)
		return approvals.Chat{}, false
	}
	for _, l := range links {
		if l.Conv != "" {
			return approvals.Chat{Channel: l.Channel, Conv: channel.ConvKey(l.Conv)}, true
		}
	}
	return approvals.Chat{}, false
}

// AnswerRemote forwards an answer to a remote agent's question.
func (p *Peers) AnswerRemote(ctx context.Context, rec store.Question, q *approvals.Question, opt approvals.Option, by auth.Principal) error {
	t, err := p.target(ctx, rec.Contact, by, 0)
	if err != nil {
		return err
	}
	msg, err := approvals.AnswerMessage(a2a.TaskID(rec.TaskID), rec.ContextID, q.ID, opt.ID)
	if err != nil {
		return err
	}
	if _, err = t.send(ctx, msg); err != nil {
		p.forgetClient(rec.Contact)
	}
	return err
}

// resumeWatches restarts watches persisted by a previous run.
func (p *Peers) resumeWatches(ctx context.Context) {
	ws, err := p.store.Watches(ctx)
	if err != nil {
		slog.Error("loading watches", "error", err)
		return
	}
	for _, w := range ws {
		p.watch(w)
	}
}

// watch polls a remote task until it finishes or the server stops, and posts
// the result into the originating context as a new turn.
func (p *Peers) watch(w store.Watch) {
	ctx := p.ctx
	key := w.Contact + "|" + w.TaskID
	p.mu.Lock()
	if p.watching[key] {
		p.mu.Unlock()
		return
	}
	p.watching[key] = true
	p.mu.Unlock()
	go func() {
		defer func() {
			p.mu.Lock()
			delete(p.watching, key)
			p.mu.Unlock()
		}()
		claims := callClaims{ContextID: w.OriginContextID, User: p.watchPrincipal(ctx, w)}
		interval := p.timings.WatchInterval
		failingSince := w.FailingSince
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(interval):
			}
			if interval < p.timings.WatchMaxInterval {
				interval += p.timings.WatchInterval
			}
			task, err := p.poll(ctx, w, claims.User)
			if errors.Is(err, store.ErrNotFound) {
				slog.Info("stopped watching a task of a removed agent", "agent", w.Contact, "task", w.TaskID)
				if err := p.store.DeleteWatch(ctx, w.Contact, w.TaskID); err != nil {
					slog.Error("deleting watch", "error", err)
				}
				return
			}
			if err != nil {
				if failingSince.IsZero() {
					failingSince = time.Now()
					p.setWatchFailing(ctx, w, failingSince)
				}
				if time.Since(failingSince) < watchGiveUp {
					continue
				}
				p.finishWatch(ctx, w, fmt.Sprintf("%s has been unreachable for a day; giving up on task %s: %v", w.Contact, w.TaskID, err))
				return
			}
			if !failingSince.IsZero() {
				failingSince = time.Time{}
				p.setWatchFailing(ctx, w, failingSince)
			}
			if task.Status.State == a2a.TaskStateInputRequired {
				if q := approvals.FromStatus(task.Status); q != nil && q.Type == approvals.TypePermission {
					p.relay(ctx, claims, w.Contact, task, q)
					continue
				}
			}
			if !task.Status.State.Terminal() && task.Status.State != a2a.TaskStateInputRequired {
				continue
			}
			text := fmt.Sprintf("[%s finished task %s: %s]\n%s", w.Contact, w.TaskID, stateName(task.Status.State), resultText(task))
			if task.Status.State == a2a.TaskStateInputRequired {
				text = fmt.Sprintf("[%s asks, on task %s]\n%s\nAnswer with: a2a send -a <agents URL>/%s --task-id %s \"...\"",
					w.Contact, w.TaskID, messageText(task.Status.Message), w.Contact, w.TaskID)
			}
			p.finishWatch(ctx, w, text)
			return
		}
	}()
}

// watchGiveUp is how long a watched task's agent may be unreachable.
const watchGiveUp = 24 * time.Hour

func (p *Peers) setWatchFailing(ctx context.Context, w store.Watch, since time.Time) {
	if err := p.store.SetWatchFailing(ctx, w.Contact, w.TaskID, since); err != nil {
		slog.Warn("recording an unreachable watch", "agent", w.Contact, "task", w.TaskID, "error", err)
	}
}

func (p *Peers) poll(ctx context.Context, w store.Watch, user auth.Principal) (*a2a.Task, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	t, err := p.target(ctx, w.Contact, user, 0)
	if err != nil {
		return nil, err
	}
	task, err := t.get(ctx, a2a.TaskID(w.TaskID))
	if err != nil {
		p.forgetClient(w.Contact)
	}
	return task, err
}

// finishWatch posts the result of a watched task as its user, if they may
// still call this agent.
func (p *Peers) finishWatch(ctx context.Context, w store.Watch, text string) {
	if err := p.store.DeleteWatch(ctx, w.Contact, w.TaskID); err != nil {
		slog.Error("deleting watch", "error", err)
	}
	user := p.watchPrincipal(ctx, w)
	if user.Role == "" {
		slog.Warn("dropped a watched result: its user can no longer call this agent", "contact", w.Contact, "task", w.TaskID, "user", w.OnBehalfOf)
		return
	}
	if err := p.chat.injectTurn(ctx, w.OriginContextID, user, text); err != nil {
		slog.Error("posting watched result", "contact", w.Contact, "task", w.TaskID, "error", err)
	}
}

// watchPrincipal is the user a watched task's result is posted as, with
// the role they have now; no role if they can no longer call this agent.
func (p *Peers) watchPrincipal(ctx context.Context, w store.Watch) auth.Principal {
	user, err := storedPrincipal(ctx, p.chain, w.OnBehalfOf, w.Identity)
	if err != nil {
		slog.Warn("resolving the user of a watch", "contact", w.Contact, "task", w.TaskID, "user", w.OnBehalfOf, "error", err)
		return auth.Principal{ID: w.OnBehalfOf}
	}
	return user
}

// resultText is the text of a finished task: its artifacts, or its status message.
func resultText(t *a2a.Task) string {
	var parts []string
	for _, a := range t.Artifacts {
		var sb strings.Builder
		for _, p := range a.Parts {
			sb.WriteString(p.Text())
		}
		if sb.Len() > 0 {
			parts = append(parts, sb.String())
		}
	}
	if len(parts) == 0 {
		return messageText(t.Status.Message)
	}
	return strings.Join(parts, "\n\n")
}
