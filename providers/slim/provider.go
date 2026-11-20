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

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	a2aslimrpc "github.com/agntcy/slim-a2a-go/a2aslimrpc/v1"
	slim_bindings "github.com/agntcy/slim-bindings-go/v3"
	slim_rpc "github.com/agntcy/slim-bindings-go/v3/slim_rpc"

	"github.com/a2aproject/a2a-serve/netprovider"
)

// Version is the provider's version.
const Version = "v0.1.0"

const (
	// connectTimeout bounds connecting to the node.
	connectTimeout = 10 * time.Second
	// closeTimeout bounds waiting for SLIM sessions to end on close.
	closeTimeout = 2 * time.Second
)

// notifier is where the provider's notifications go: the server.
type notifier interface {
	Health(netprovider.Health)
}

// opener opens the agent's app on the node.
type opener func(ctx context.Context, set settings, id identity, self name) (node, error)

// node is the agent's app on a SLIM node: its connection, and its
// subscription and server on it.
type node interface {
	// connected reports whether the connection to the node is up.
	connected() bool
	// listen serves the agent's SLIMRPC calls with h until close.
	listen(h a2asrv.RequestHandler)
	// transport returns a transport to the agent n on the node.
	transport(n name) (a2aclient.Transport, error)
	// close leaves the node.
	close() error
}

type provider struct {
	host notifier
	open opener
	out  *outbound

	mu      sync.Mutex
	joining bool
	closed  bool
	set     settings
	self    name
	id      identity
	node    node
	handler a2asrv.RequestHandler
	stop    context.CancelFunc
	watched chan struct{}

	// lost is whether the provider reported the node lost. Only the watcher
	// uses it.
	lost bool
}

func newProvider(h notifier, open opener) *provider {
	p := &provider{host: h, open: open}
	p.out = &outbound{peers: newPeers(maxPeers, p.peerTransport)}
	return p
}

func (p *provider) Describe() netprovider.Description {
	return netprovider.Description{Name: "slim", Version: Version, Hosts: []string{Host}}
}

func (p *provider) Join(ctx context.Context, req netprovider.JoinRequest) (netprovider.JoinResult, error) {
	if req.Public {
		return netprovider.JoinResult{}, errors.New("SLIM gives agents no public URL; use --public tailscale or a relay")
	}
	if !agentName.MatchString(req.Agent) {
		return netprovider.JoinResult{}, fmt.Errorf("agent name %q isn't a SLIM name part: use lower-case letters, digits and dashes", req.Agent)
	}
	set, err := parseSettings(req.URL)
	if err != nil {
		return netprovider.JoinResult{}, err
	}
	id, err := newIdentity(req.Token)
	if err != nil {
		return netprovider.JoinResult{}, err
	}
	self := name{org: set.org, ns: set.ns, agent: req.Agent}

	p.mu.Lock()
	switch {
	case p.closed:
		p.mu.Unlock()
		return netprovider.JoinResult{}, errClosed
	case p.joining || p.node != nil:
		p.mu.Unlock()
		return netprovider.JoinResult{}, errors.New("join was already called")
	}
	p.joining = true
	p.mu.Unlock()

	n, err := p.open(ctx, set, id, self)

	p.mu.Lock()
	defer p.mu.Unlock()
	p.joining = false
	if err != nil {
		return netprovider.JoinResult{}, err
	}
	if p.closed {
		return netprovider.JoinResult{}, errors.Join(errClosed, n.close())
	}
	p.set, p.self, p.id, p.node = set, self, id, n
	wctx, stop := context.WithCancel(context.Background())
	p.stop, p.watched = stop, make(chan struct{})
	go p.watch(wctx, p.watched)
	return netprovider.JoinResult{Node: self.String()}, nil
}

func (p *provider) Listen(_ context.Context, in *netprovider.Inbound) (netprovider.ListenResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.node == nil {
		return netprovider.ListenResult{}, errors.New("listen before join")
	}
	if !in.Private() {
		return netprovider.ListenResult{}, errors.New("SLIM gives agents only a private URL")
	}
	p.handler = newInbound(in, p.self, p.id)
	p.node.listen(p.handler)
	return netprovider.ListenResult{PrivateURL: p.self.privateURL(), PrivateNote: "works only from other agents on the SLIM node"}, nil
}

func (p *provider) Member(context.Context, string) (netprovider.Caller, error) {
	return netprovider.Caller{}, netprovider.ErrUnsupported
}

// Peers lists the agents of the peers setting: SLIM can't list the agents
// on a node.
func (p *provider) Peers(context.Context) ([]netprovider.PeerInfo, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.node == nil {
		return nil, nil
	}
	var peers []netprovider.PeerInfo
	for _, agent := range p.set.peers {
		if agent == p.self.agent {
			continue
		}
		n := name{org: p.set.org, ns: p.set.ns, agent: agent}
		peers = append(peers, netprovider.PeerInfo{Name: agent, URL: n.privateURL(), Node: n.String()})
	}
	return peers, nil
}

func (p *provider) Outbound() http.Handler { return p.out }

func (p *provider) peerTransport(n name) (a2aclient.Transport, error) {
	p.mu.Lock()
	cur := p.node
	p.mu.Unlock()
	if cur == nil {
		return nil, errors.New("the agent hasn't joined the SLIM network")
	}
	return cur.transport(n)
}

func (p *provider) Close() error {
	p.mu.Lock()
	p.closed = true
	n, stop, watched := p.node, p.stop, p.watched
	p.node = nil
	p.mu.Unlock()
	if stop != nil {
		stop()
		select {
		case <-watched:
		case <-time.After(closeTimeout):
			slog.Warn("still joining the SLIM node again; leaving it anyway")
		}
	}
	p.out.peers.close()
	if n == nil {
		return nil
	}
	return n.close()
}

// slimNode is the agent's app on the node, with a SLIM service of its own:
// a service keeps a connection the bindings lost in its table, and refuses
// to connect to the same node again.
type slimNode struct {
	svc  *slim_bindings.Service
	app  *slim_bindings.App
	name *slim_bindings.Name
	conn uint64

	// mu guards the app against use after close, which the bindings answer
	// with a panic.
	mu      sync.Mutex
	server  *slim_rpc.Server
	closing bool
	stopped bool
}

var initSLIM = sync.OnceValue(func() error {
	tracing := slim_bindings.NewTracingConfig()
	tracing.LogLevel = "error"
	return slim_bindings.InitializeWithConfigs(slim_bindings.NewRuntimeConfig(), tracing, []slim_bindings.ServiceConfig{slim_bindings.NewServiceConfig()})
})

// connectAttempts is how many times the bindings try to reach the node when
// they connect or reconnect, a second apart. The watcher takes over after
// that, so that the agent shows as disconnected while the node is down.
const connectAttempts = 3

func openNode(ctx context.Context, set settings, id identity, self name) (node, error) {
	if err := initSLIM(); err != nil {
		return nil, fmt.Errorf("starting SLIM: %w", err)
	}
	svc, err := slim_bindings.CreateServiceWithConfig("netprovider-slim", slim_bindings.NewServiceConfig())
	if err != nil {
		return nil, fmt.Errorf("starting SLIM: %w", err)
	}
	n := slim_bindings.NewName(self.org, self.ns, self.agent)
	app, err := id.createApp(svc, n)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("creating the SLIM app %s: %w", self, err), stopService(svc))
	}
	cfg := slim_bindings.NewInsecureClientConfig(set.endpoint)
	if set.tls {
		cfg = slim_bindings.NewSecureClientConfig(set.endpoint)
	}
	var backoff slim_bindings.BackoffConfig = slim_bindings.BackoffConfigFixedInterval{Config: slim_bindings.FixedIntervalBackoff{Interval: time.Second, MaxAttempts: connectAttempts - 1}}
	cfg.Backoff = &backoff
	conn, err := connect(ctx, svc, cfg)
	if err != nil {
		app.Destroy()
		return nil, errors.Join(fmt.Errorf("connecting to the SLIM node at %s: %w", set.endpoint, err), stopService(svc))
	}
	s := &slimNode{svc: svc, app: app, name: n, conn: conn}
	if err := app.Subscribe(n, &conn); err != nil {
		return nil, errors.Join(fmt.Errorf("subscribing to %s on the SLIM node: %w", self, err), s.close())
	}
	return s, nil
}

// connect connects to the node, and gives up after connectTimeout or once
// ctx is done. Stopping svc ends an attempt it gave up on.
func connect(ctx context.Context, svc *slim_bindings.Service, cfg slim_bindings.ClientConfig) (uint64, error) {
	type result struct {
		conn uint64
		err  error
	}
	done := make(chan result, 1)
	go func() {
		conn, err := svc.Connect(cfg)
		done <- result{conn, err}
	}()
	timer := time.NewTimer(connectTimeout)
	defer timer.Stop()
	select {
	case r := <-done:
		return r.conn, r.err
	case <-timer.C:
		return 0, fmt.Errorf("the node didn't answer in %v; check that it runs and that the URL is right", connectTimeout)
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func stopService(svc *slim_bindings.Service) error {
	defer svc.Destroy()
	if err := svc.Shutdown(); err != nil {
		return fmt.Errorf("stopping SLIM: %w", err)
	}
	return nil
}

// connected reports whether the bindings still have the connection to the
// node, and the agent's server still runs. The bindings drop a connection
// they fail to reconnect, and never connect it again.
func (s *slimNode) connected() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.closing && !s.stopped && s.hasConnLocked()
}

// hasConnLocked reports whether the connection is in the bindings' table.
// Pinning it as the default gateway looks it up there, and the bindings
// export no other lookup; another error, such as a connection of another
// type, means the connection is there.
func (s *slimNode) hasConnLocked() bool {
	if err := s.app.SetDefaultGateway(s.conn); err != nil {
		return !strings.Contains(err.Error(), "connection not found")
	}
	if err := s.app.ClearDefaultGateway(); err != nil {
		slog.Debug("clearing the SLIM default gateway", "error", err)
	}
	return true
}

func (s *slimNode) listen(h a2asrv.RequestHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return
	}
	srv := slim_rpc.ServerNewWithConnection(s.app, s.name, &s.conn)
	a2aslimrpc.NewHandler(h).RegisterWith(srv)
	s.server = srv
	go s.serve(srv)
}

func (s *slimNode) serve(srv *slim_rpc.Server) {
	err := srv.ServeBlocking()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return
	}
	s.stopped = true
	slog.Error("the agent stopped serving SLIM calls", "error", err)
}

func (s *slimNode) transport(n name) (a2aclient.Transport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return nil, errors.New("the agent left the SLIM node")
	}
	ch := slim_rpc.ChannelNewWithConnection(s.app, slim_bindings.NewName(n.org, n.ns, n.agent), &s.conn)
	return &channelTransport{Transport: a2aslimrpc.NewTransport(ch), ch: ch}, nil
}

// channelTransport ends its channel's sessions before destroying it, so
// that the agent it calls doesn't wait for them when it shuts down.
type channelTransport struct {
	*a2aslimrpc.Transport
	ch *slim_rpc.Channel
}

func (t *channelTransport) Destroy() error {
	timeout := closeTimeout
	err := t.ch.CloseBlocking(&timeout)
	return errors.Join(err, t.Transport.Destroy())
}

// close leaves the node. Disconnecting drops the app's subscriptions; a
// connection the bindings dropped is gone already.
func (s *slimNode) close() error {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return nil
	}
	s.closing = true
	srv := s.server
	s.mu.Unlock()
	if srv != nil {
		shutdown(srv)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var err error
	if s.hasConnLocked() {
		if derr := s.svc.Disconnect(s.conn); derr != nil {
			err = fmt.Errorf("disconnecting from the SLIM node: %w", derr)
		}
	}
	s.app.Destroy()
	return errors.Join(err, stopService(s.svc))
}

// shutdown stops srv, and gives up waiting for its calls after closeTimeout.
func shutdown(srv *slim_rpc.Server) {
	done := make(chan struct{})
	go func() {
		srv.ShutdownBlocking()
		close(done)
	}()
	select {
	case <-done:
		srv.Destroy()
	case <-time.After(closeTimeout):
		slog.Warn("SLIM calls still run; leaving the node anyway")
	}
}
