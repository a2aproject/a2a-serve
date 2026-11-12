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
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2aclient"
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

type provider struct {
	host *netprovider.Host
	out  *outbound

	mu      sync.Mutex
	joining bool
	closed  bool
	set     settings
	self    name
	id      identity
	sess    *session
	server  *slim_rpc.Server
}

func newProvider(h *netprovider.Host) *provider {
	p := &provider{host: h}
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
	case p.joining || p.sess != nil:
		p.mu.Unlock()
		return netprovider.JoinResult{}, errors.New("join was already called")
	}
	p.joining = true
	p.mu.Unlock()

	s, err := openSession(ctx, set, id, self)

	p.mu.Lock()
	defer p.mu.Unlock()
	p.joining = false
	if err != nil {
		return netprovider.JoinResult{}, err
	}
	if p.closed {
		return netprovider.JoinResult{}, errors.Join(errClosed, s.close())
	}
	p.set, p.self, p.id, p.sess = set, self, id, s
	return netprovider.JoinResult{Node: self.String()}, nil
}

func (p *provider) Listen(_ context.Context, in *netprovider.Inbound) (netprovider.ListenResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sess == nil {
		return netprovider.ListenResult{}, errors.New("listen before join")
	}
	if !in.Private() {
		return netprovider.ListenResult{}, errors.New("SLIM gives agents only a private URL")
	}
	srv := slim_rpc.ServerNewWithConnection(p.sess.app, p.sess.name, &p.sess.conn)
	a2aslimrpc.NewHandler(newInbound(in, p.self, p.id)).RegisterWith(srv)
	p.server = srv
	go p.serve(srv)
	return netprovider.ListenResult{PrivateURL: p.self.privateURL(), PrivateNote: "works only from other agents on the SLIM node"}, nil
}

func (p *provider) serve(srv *slim_rpc.Server) {
	err := srv.ServeBlocking()
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return
	}
	problem := "the agent stopped serving SLIM calls"
	if err != nil {
		problem += ": " + err.Error()
	}
	slog.Error(problem)
	p.host.Health(netprovider.Health{State: netprovider.HealthDisconnected, Problem: problem, Fix: "restart the agent"})
}

func (p *provider) Member(context.Context, string) (netprovider.Caller, error) {
	return netprovider.Caller{}, netprovider.ErrUnsupported
}

// Peers lists the agents of the peers setting: SLIM can't list the agents
// on a node.
func (p *provider) Peers(context.Context) ([]netprovider.PeerInfo, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sess == nil {
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
	s := p.sess
	p.mu.Unlock()
	if s == nil {
		return nil, errors.New("the agent hasn't joined the SLIM network")
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

func (p *provider) Close() error {
	p.mu.Lock()
	p.closed = true
	srv, s := p.server, p.sess
	p.server, p.sess = nil, nil
	p.mu.Unlock()
	if srv != nil {
		shutdown(srv)
	}
	p.out.peers.close()
	if s == nil {
		return nil
	}
	return s.close()
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

// session is the agent's app on the node.
type session struct {
	svc  *slim_bindings.Service
	app  *slim_bindings.App
	name *slim_bindings.Name
	conn uint64
}

var initSLIM = sync.OnceValue(func() error {
	tracing := slim_bindings.NewTracingConfig()
	tracing.LogLevel = "error"
	return slim_bindings.InitializeWithConfigs(slim_bindings.NewRuntimeConfig(), tracing, []slim_bindings.ServiceConfig{slim_bindings.NewServiceConfig()})
})

func openSession(ctx context.Context, set settings, id identity, self name) (*session, error) {
	if err := initSLIM(); err != nil {
		return nil, fmt.Errorf("starting SLIM: %w", err)
	}
	svc := slim_bindings.GetGlobalService()
	n := slim_bindings.NewName(self.org, self.ns, self.agent)
	app, err := id.createApp(svc, n)
	if err != nil {
		return nil, fmt.Errorf("creating the SLIM app %s: %w", self, err)
	}
	cfg := slim_bindings.NewInsecureClientConfig(set.endpoint)
	if set.tls {
		cfg = slim_bindings.NewSecureClientConfig(set.endpoint)
	}
	conn, err := connect(ctx, svc, cfg)
	if err != nil {
		app.Destroy()
		return nil, fmt.Errorf("connecting to the SLIM node at %s: %w", set.endpoint, err)
	}
	s := &session{svc: svc, app: app, name: n, conn: conn}
	if err := app.Subscribe(n, &conn); err != nil {
		return nil, errors.Join(fmt.Errorf("subscribing to %s on the SLIM node: %w", self, err), s.close())
	}
	return s, nil
}

// connect connects to the node. The bindings retry an unreachable node
// without end, so connect gives up after connectTimeout or once ctx is done,
// and disconnects if the connection comes up later.
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
	var err error
	select {
	case r := <-done:
		return r.conn, r.err
	case <-timer.C:
		err = fmt.Errorf("the node didn't answer in %v; check that it runs and that the URL is right", connectTimeout)
	case <-ctx.Done():
		err = ctx.Err()
	}
	go func() {
		r := <-done
		if r.err != nil {
			return
		}
		if err := svc.Disconnect(r.conn); err != nil {
			slog.Warn("disconnecting from the SLIM node after giving up", "error", err)
		}
	}()
	return 0, err
}

// close leaves the node. Disconnecting drops the app's subscriptions.
func (s *session) close() error {
	err := s.svc.Disconnect(s.conn)
	s.app.Destroy()
	if err != nil {
		return fmt.Errorf("disconnecting from the SLIM node: %w", err)
	}
	return nil
}
