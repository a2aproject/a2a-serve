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
	"errors"
	"fmt"
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
	"github.com/a2aproject/a2a-serve/internal/a2aproxy"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/approvals"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/nodes"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/contacts"
	"github.com/a2aproject/a2a-serve/internal/egress"
	"github.com/a2aproject/a2a-serve/internal/gossip"
	"github.com/a2aproject/a2a-serve/internal/mailboxapi"
	"github.com/a2aproject/a2a-serve/internal/network"
	"github.com/a2aproject/a2a-serve/internal/store"
	"golang.org/x/sync/singleflight"
)

// ContactClients reaches other agents: contacts, through one cached client
// each, agents found on the private network and webhook nodes hosted here.
type ContactClients struct {
	store   *store.Store
	private network.Provider
	nodes   *nodes.Host
	ads     *gossip.Table
	config  func() *agentdir.Config

	// builds joins the concurrent builds of one contact's client, so that
	// two calls don't build one each and destroy the other's.
	builds singleflight.Group

	mu       sync.Mutex
	cache    []network.PeerInfo
	cachedAt time.Time
	// rediscovered is when rediscover last bypassed the cache.
	rediscovered time.Time
	clients      map[string]*contactClient
}

// ContactClientsDeps are the dependencies of [ContactClients].
type ContactClientsDeps struct {
	Store   *store.Store
	Private network.Provider
	Nodes   *nodes.Host
	Ads     *gossip.Table
	// Config returns the current config of the agent.
	Config func() *agentdir.Config
}

var _ approvals.RemoteAnswerer = (*ContactClients)(nil)

// NewContactClients returns the contact clients of deps.
func NewContactClients(deps ContactClientsDeps) *ContactClients {
	return &ContactClients{
		store:   deps.Store,
		private: deps.Private,
		nodes:   deps.Nodes,
		ads:     deps.Ads,
		config:  deps.Config,
		clients: map[string]*contactClient{},
	}
}

// Contact routes.
const (
	ViaPrivate = contacts.ViaPrivate
	ViaDirect  = contacts.ViaDirect
)

// contactClient is the A2A client of a contact, built for the contact as
// stored when it was made.
type contactClient struct {
	contact store.Contact
	client  *a2aclient.Client
}

// maxContactResponse limits the size of a response from a contact, except
// event streams.
const maxContactResponse = 32 << 20

// Discover lists agents on the private network, cached briefly.
func (cc *ContactClients) Discover(ctx context.Context) ([]network.PeerInfo, error) {
	return cc.discoverWithin(ctx, 30*time.Second)
}

// Rediscover lists agents on the private network bypassing the cache,
// unless it did so within minInterval.
func (cc *ContactClients) Rediscover(ctx context.Context, minInterval time.Duration) ([]network.PeerInfo, error) {
	cc.mu.Lock()
	if time.Since(cc.rediscovered) < minInterval {
		cc.mu.Unlock()
		return cc.Discover(ctx)
	}
	cc.rediscovered = time.Now()
	cc.mu.Unlock()
	return cc.discoverWithin(ctx, 0)
}

func (cc *ContactClients) discoverWithin(ctx context.Context, maxAge time.Duration) ([]network.PeerInfo, error) {
	if cc.private == nil {
		return nil, errors.New("this agent has no private network; add one with `a2a-serve config set --private tailscale` (or a relay URL) and `a2a-serve restart`")
	}
	if err := network.HealthOf(cc.private).Err(); err != nil {
		return nil, err
	}
	cc.mu.Lock()
	if time.Since(cc.cachedAt) < maxAge {
		out := cc.cache
		cc.mu.Unlock()
		return out, nil
	}
	cc.mu.Unlock()
	peers, err := cc.private.Peers(ctx)
	if err != nil {
		return nil, err
	}
	cc.mu.Lock()
	cc.cache, cc.cachedAt = peers, time.Now()
	cc.mu.Unlock()
	return peers, nil
}

// Locate returns the URL of the contact name, an advertised pairing or an
// agent found by name on the private network, when u is empty, and the
// route to it.
func (cc *ContactClients) Locate(ctx context.Context, name, u string) (string, string, error) {
	if u != "" {
		if cc.private != nil && cc.onPrivateNetwork(ctx, u) {
			return u, ViaPrivate, nil
		}
		return u, ViaDirect, nil
	}
	if _, _, ok := gossip.ParseKey(name); ok {
		u, err := cc.advertisedURL(ctx, name)
		return u, ViaPrivate, err
	}
	peers, err := cc.Discover(ctx)
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

// advertisedURL returns the URL of a pairing advertised as key, with the
// name `a2a-serve peers` lists it by: the node at its host found on the
// private network. The advertisement only says it exists, so it can't send
// calls outside the private network.
func (cc *ContactClients) advertisedURL(ctx context.Context, key string) (string, error) {
	peers, err := cc.Discover(ctx)
	if err != nil {
		return "", err
	}
	for _, l := range cc.ads.List(peers) {
		if !strings.EqualFold(l.Key, key) {
			continue
		}
		if l.Agent == nil {
			return "", fmt.Errorf("%s advertises %s, but isn't on the private network now", l.Host, key)
		}
		return mailboxapi.NodeURL(l.Agent.URL, l.Name), nil
	}
	return "", fmt.Errorf("no agent advertises %s on the private network; `a2a-serve peers` lists the advertised pairings", key)
}

func (cc *ContactClients) onPrivateNetwork(ctx context.Context, u string) bool {
	peers, err := cc.Discover(ctx)
	if err != nil {
		return false
	}
	for _, pi := range peers {
		if strings.TrimSuffix(pi.URL, "/") == strings.TrimSuffix(u, "/") {
			return true
		}
	}
	return false
}

// HTTPClient returns the HTTP client for a contact, which sends its token
// to the contact's host only, limits its responses, and keeps a contact on
// a public URL out of private networks. It has no overall timeout, because
// streams to the contact last as long as its task; callers bound requests
// with their context.
func (cc *ContactClients) HTTPClient(c store.Contact) *http.Client {
	base := &http.Client{Transport: egress.Transport(c.URL), CheckRedirect: egress.CheckRedirect}
	if c.Via == ViaPrivate && cc.private != nil {
		base = cc.private.HTTPClient()
	}
	rt := base.Transport
	if rt == nil {
		rt = http.DefaultTransport
	}
	limited := &http.Client{Transport: egress.LimitResponses(rt, maxContactResponse), CheckRedirect: base.CheckRedirect}
	return contacts.TokenClient(limited, c)
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
	// get returns the task with no history, which watches don't read.
	get(ctx context.Context, id a2a.TaskID) (*a2a.Task, error)
	cancel(ctx context.Context, id a2a.TaskID) error
}

type remoteTarget struct {
	c    *a2aclient.Client
	hops int
}

func (t remoteTarget) send(ctx context.Context, msg *a2a.Message) (a2a.SendMessageResult, error) {
	return t.c.SendMessage(viaAgent(ctx, t.hops), &a2a.SendMessageRequest{Message: msg, Config: &a2a.SendMessageConfig{ReturnImmediately: true}})
}

func (t remoteTarget) get(ctx context.Context, id a2a.TaskID) (*a2a.Task, error) {
	return t.c.GetTask(viaAgent(ctx, t.hops), &a2a.GetTaskRequest{ID: id, HistoryLength: new(int)})
}

func (t remoteTarget) cancel(ctx context.Context, id a2a.TaskID) error {
	_, err := t.c.CancelTask(viaAgent(ctx, t.hops), &a2a.CancelTaskRequest{ID: id})
	return err
}

type localTarget struct {
	h a2asrv.RequestHandler
	p auth.Principal
}

func (t localTarget) send(ctx context.Context, msg *a2a.Message) (a2a.SendMessageResult, error) {
	return t.h.SendMessage(auth.InProcess(ctx, t.p), &a2a.SendMessageRequest{Message: msg, Config: &a2a.SendMessageConfig{ReturnImmediately: true}})
}

func (t localTarget) get(ctx context.Context, id a2a.TaskID) (*a2a.Task, error) {
	return t.h.GetTask(auth.InProcess(ctx, t.p), &a2a.GetTaskRequest{ID: id, HistoryLength: new(int)})
}

func (t localTarget) cancel(ctx context.Context, id a2a.TaskID) error {
	_, err := t.h.CancelTask(auth.InProcess(ctx, t.p), &a2a.CancelTaskRequest{ID: id})
	return err
}

// ErrOffline is returned when a contact can't be reached.
var ErrOffline = errors.New("offline")

// target resolves an agent name to a contact or a hosted node, called for
// user hops agents deep.
func (cc *ContactClients) target(ctx context.Context, name string, user auth.Principal, hops int) (target, error) {
	node, err := cc.isNode(ctx, name)
	if err != nil {
		return nil, err
	}
	if node {
		return localTarget{h: cc.nodes.Handler(name), p: cc.nodeCaller(user, hops)}, nil
	}
	client, err := cc.contactClient(ctx, name)
	if err != nil {
		return nil, err
	}
	return remoteTarget{c: client, hops: hops}, nil
}

// isNode reports whether name is a webhook node hosted here.
func (cc *ContactClients) isNode(ctx context.Context, name string) (bool, error) {
	_, err := cc.store.Node(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// nodeCaller is the principal this agent calls its nodes as, for user hops
// agents deep. Its harness is another agent to the node, so it is capped
// at authenticated; the node admits the agent hosting it explicitly.
func (cc *ContactClients) nodeCaller(user auth.Principal, hops int) auth.Principal {
	return auth.Principal{
		ID: "agent:" + cc.config().Name, Role: auth.RoleAuthenticated, OnBehalfOf: user.ID,
		Identity: auth.Identity{Network: network.ListenerHarness, Hops: hops + 1},
	}
}

// callable reports whether name is a contact, a hosted node, or an agent
// on the private network the agent may call.
func (cc *ContactClients) callable(ctx context.Context, name string) (bool, error) {
	if node, err := cc.isNode(ctx, name); err != nil || node {
		return node, err
	}
	_, err := cc.contact(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// CallablePeers returns the names of the agents on the private network the
// agent may call by name: all of them with --private-allow-outgoing
// everyone, and none otherwise.
func (cc *ContactClients) CallablePeers(ctx context.Context) []string {
	if !cc.CallsPeers() {
		return nil
	}
	peers, err := cc.Discover(ctx)
	if err != nil {
		slog.Warn("listing the agents on the private network", "error", err)
		return nil
	}
	names := make([]string, 0, len(peers))
	for _, p := range peers {
		names = append(names, p.Name)
	}
	return names
}

// CallsPeers reports whether the agent may call everyone on its private
// network.
func (cc *ContactClients) CallsPeers() bool {
	return cc.private != nil && cc.config().Policy.PrivateAllowOutgoing == agentdir.AccessEveryone
}

// contact returns the contact name, or, when the agent may call everyone
// on its private network, the agent found there by that name, which the
// network's identity authenticates. It returns [store.ErrNotFound] for
// another name.
func (cc *ContactClients) contact(ctx context.Context, name string) (store.Contact, error) {
	c, err := cc.store.Contact(ctx, name)
	if !errors.Is(err, store.ErrNotFound) || !cc.CallsPeers() {
		return c, err
	}
	peers, derr := cc.Discover(ctx)
	if derr != nil {
		return store.Contact{}, derr
	}
	for _, p := range peers {
		if strings.EqualFold(p.Name, name) {
			return store.Contact{Name: p.Name, URL: p.URL, Via: ViaPrivate}, nil
		}
	}
	return store.Contact{}, err
}

// agentHandler returns the A2A handler of a node or a contact. A node is
// served in-process, as principal.
func (cc *ContactClients) agentHandler(ctx context.Context, c Claims, name string) (a2asrv.RequestHandler, *auth.Principal, error) {
	node, err := cc.isNode(ctx, name)
	if err != nil {
		return nil, nil, err
	}
	if node {
		pr := cc.nodeCaller(c.User, c.User.Hops)
		return cc.nodes.Handler(name), &pr, nil
	}
	client, err := cc.contactClient(ctx, name)
	if err != nil {
		return nil, nil, err
	}
	return &a2aproxy.ClientHandler{Client: client, Context: func(ctx context.Context) context.Context {
		return viaAgent(ctx, c.User.Hops)
	}}, nil, nil
}

// contactClient returns the client of a contact, or of an agent on the
// private network the agent may call, one per name: the user and hop count
// go with each request. A client is rebuilt when the contact changed, and
// dropped with the contact. It returns [store.ErrNotFound] for a name it
// can't call.
func (cc *ContactClients) contactClient(ctx context.Context, name string) (*a2aclient.Client, error) {
	c, err := cc.contact(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		cc.forgetClient(name)
		return nil, err
	}
	if err != nil {
		return nil, err
	}
	if client := cc.cachedClient(name, c); client != nil {
		return client, nil
	}
	// The build outlives the call that started it, which others may wait for.
	bctx := context.WithoutCancel(ctx)
	ch := cc.builds.DoChan(buildKey(c), func() (any, error) { return cc.buildClient(bctx, name, c) })
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-ch:
		if res.Err != nil {
			return nil, res.Err
		}
		return res.Val.(*a2aclient.Client), nil
	}
}

// buildKey identifies a contact as stored, so that a changed contact isn't
// served the client built for its previous version.
func buildKey(c store.Contact) string {
	return strings.Join([]string{c.Name, c.URL, c.Via, c.Token}, "\x00")
}

func (cc *ContactClients) buildClient(ctx context.Context, name string, c store.Contact) (*a2aclient.Client, error) {
	if client := cc.cachedClient(name, c); client != nil {
		return client, nil
	}
	hc := cc.HTTPClient(c)
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	card, err := agentcard.NewResolver(hc).Resolve(cctx, c.URL)
	if err != nil {
		return nil, fmt.Errorf("%s is %w (%w)", name, ErrOffline, err)
	}
	client, err := a2aclient.NewFromCard(ctx, card, a2aclient.WithJSONRPCTransport(hc), a2aclient.WithRESTTransport(hc))
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", name, err)
	}
	cc.mu.Lock()
	old := cc.clients[name]
	cc.clients[name] = &contactClient{contact: c, client: client}
	cc.mu.Unlock()
	if old != nil {
		destroyClient(old.client)
	}
	return client, nil
}

func (cc *ContactClients) cachedClient(name string, c store.Contact) *a2aclient.Client {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	if cached := cc.clients[name]; cached != nil && sameContact(cached.contact, c) {
		return cached.client
	}
	return nil
}

func sameContact(a, b store.Contact) bool {
	return a.URL == b.URL && a.Token == b.Token && a.Via == b.Via
}

// forgetClient drops the client of a contact, so that the next call
// resolves its card again.
func (cc *ContactClients) forgetClient(name string) {
	cc.mu.Lock()
	old := cc.clients[name]
	delete(cc.clients, name)
	cc.mu.Unlock()
	if old != nil {
		destroyClient(old.client)
	}
}

// Close drops the clients of all contacts.
func (cc *ContactClients) Close(context.Context) error {
	cc.mu.Lock()
	clients := cc.clients
	cc.clients = map[string]*contactClient{}
	cc.mu.Unlock()
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

// AnswerRemote forwards an answer to a remote agent's question.
func (cc *ContactClients) AnswerRemote(ctx context.Context, rec store.Question, q *approvals.Question, opt approvals.Option, by auth.Principal) error {
	t, err := cc.target(ctx, rec.Contact, by, 0)
	if err != nil {
		return err
	}
	msg, err := approvals.AnswerMessage(a2a.TaskID(rec.TaskID), rec.ContextID, q.ID, opt.ID)
	if err != nil {
		return err
	}
	if _, err = t.send(ctx, msg); err != nil {
		cc.forgetClient(rec.Contact)
	}
	return err
}
