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

// Command netprovider-agentmesh is the a2a-serve network provider of Agent
// Mesh relays. It joins a relay in-process with meshnet: private streams
// between agents on the relay, verified caller identity, discovery, and a
// public URL on the relay that forwards requests unchanged, so pairing
// codes and Google sign-in keep working.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
	"time"

	"github.com/google/agentmesh/api"
	"github.com/google/agentmesh/sdk/meshnet"

	"github.com/a2aproject/a2a-serve/internal/agentmesh"
	"github.com/a2aproject/a2a-serve/netprovider"
)

// agentMeshHost is the virtual host of private URLs on a relay, which only
// agents on the relay reach, through their in-process mesh node.
const agentMeshHost = "agentmesh.local"

// privateNote says where the private URL works.
const privateNote = "works only from other a2a-serve agents on the relay; give people the public URL"

func main() {
	netprovider.Main(func(h *netprovider.Host) netprovider.Provider {
		return newAgentMesh(h, func(srv *meshnet.Server) meshServer { return srv })
	})
}

// notifier is where the provider's notifications go: the server.
type notifier interface {
	Notice(message string)
	Health(netprovider.Health)
}

// meshSession is what the provider uses of meshnet, so that its connection
// handling, WhoIs and discovery are tested without a relay.
type meshSession interface {
	WhoIs(remoteAddr string) (*meshnet.CallerInfo, bool)
	Discover(ctx context.Context, serviceType api.ServiceType, serviceName string) ([]*api.DiscoveredProvider, error)
	HTTPClient() *http.Client
	Status() meshnet.Status
	Refresh(ctx context.Context) error
	Close() error
}

// meshServer is what the provider uses of a [meshnet.Server] to join the
// relay.
type meshServer interface {
	meshSession
	Up(ctx context.Context) (*meshnet.Status, error)
	ListenA2A(ctx context.Context, opts meshnet.A2AOptions) (*meshnet.Endpoints, error)
}

// inbound is what the provider uses of [netprovider.Inbound].
type inbound interface {
	Private() bool
	Public() bool
	Serve(kind netprovider.Kind, ln net.Listener, callerOf func(net.Conn) netprovider.Caller)
}

type agentMesh struct {
	host notifier
	// serve returns the server that joins the relay with the settings of
	// srv.
	serve func(srv *meshnet.Server) meshServer

	// joinMu serializes join and listen.
	joinMu sync.Mutex

	mu         sync.Mutex
	closed     bool
	req        netprovider.JoinRequest
	srv        meshServer
	peerID     string
	epClose    func() error
	listeners  []net.Listener
	health     netprovider.Health
	probedAt   time.Time
	stopHealth func()
}

func newAgentMesh(host notifier, serve func(*meshnet.Server) meshServer) *agentMesh {
	return &agentMesh{host: host, serve: serve}
}

func (a *agentMesh) Describe() netprovider.Description {
	return netprovider.Description{Name: "agentmesh", Version: agentmesh.Version, Hosts: []string{agentMeshHost}}
}

func (a *agentMesh) Join(ctx context.Context, req netprovider.JoinRequest) (netprovider.JoinResult, error) {
	if err := agentmesh.CheckRelayURL(req.URL); err != nil {
		return netprovider.JoinResult{}, fmt.Errorf("invalid relay URL %q: %w", req.URL, err)
	}
	a.joinMu.Lock()
	defer a.joinMu.Unlock()
	a.mu.Lock()
	joined := a.srv != nil
	a.mu.Unlock()
	if joined {
		return netprovider.JoinResult{}, errors.New("join was already called")
	}
	j := &join{req: req, notice: a.host.Notice, serve: a.serve}
	srv, st, err := j.up(ctx)
	if err != nil {
		return netprovider.JoinResult{}, err
	}
	res := netprovider.JoinResult{Node: st.PeerID}
	// Agents that joined with a token don't say who owns them.
	if req.Token == "" {
		res.Owner = savedOwner(req.StateDir)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return netprovider.JoinResult{}, errors.Join(errors.New("the network provider is closing"), srv.Close())
	}
	a.req, a.srv, a.peerID = req, srv, st.PeerID
	watchCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go a.watchHealth(watchCtx, done)
	a.stopHealth = func() { cancel(); <-done }
	slog.Info("joined the relay", "relay", req.URL, "peer_id", st.PeerID, "routers", st.AttachedRouters)
	return res, nil
}

func (a *agentMesh) Listen(ctx context.Context, in *netprovider.Inbound) (netprovider.ListenResult, error) {
	return a.listen(ctx, in)
}

func (a *agentMesh) listen(ctx context.Context, in inbound) (netprovider.ListenResult, error) {
	a.joinMu.Lock()
	defer a.joinMu.Unlock()
	a.mu.Lock()
	srv, req, listened := a.srv, a.req, a.epClose != nil
	a.mu.Unlock()
	if srv == nil {
		return netprovider.ListenResult{}, errors.New("listen before join")
	}
	if listened {
		return netprovider.ListenResult{}, errors.New("listen was already called")
	}
	opts := meshnet.A2AOptions{Name: req.Agent, Description: req.Description, Public: req.Public}
	if req.Public {
		opts.IngressAuth = api.IngressAuthMode_INGRESS_AUTH_MODE_PASSTHROUGH
	}
	ep, err := srv.ListenA2A(ctx, opts)
	if err != nil {
		return netprovider.ListenResult{}, fmt.Errorf("registering the agent on the relay: %w", err)
	}
	switch {
	case req.Public && ep.PublicURL == nil:
		return netprovider.ListenResult{}, errors.Join(errors.New("the relay didn't give this agent a public URL: its policy doesn't allow it, or it has no ingress"), ep.Close())
	case in.Public() && ep.PublicListener == nil:
		return netprovider.ListenResult{}, errors.Join(errors.New("the relay gives this agent no public URL"), ep.Close())
	case in.Private() && ep.PrivateListener == nil:
		return netprovider.ListenResult{}, errors.Join(errors.New("the relay gives this agent no private URL"), ep.Close())
	}
	var res netprovider.ListenResult
	var serving []net.Listener
	if keepListener(in.Private(), ep.PrivateListener) {
		serving = append(serving, ep.PrivateListener)
		go in.Serve(netprovider.Private, ep.PrivateListener, privateCaller(srv))
		res.PrivateURL, res.PrivateNote = ep.PrivateURL.String(), privateNote
	}
	if keepListener(in.Public(), ep.PublicListener) {
		serving = append(serving, ep.PublicListener)
		go in.Serve(netprovider.Public, ep.PublicListener, publicCaller(srv))
		res.PublicURL = ep.PublicURL.String()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return netprovider.ListenResult{}, errors.Join(errors.New("the network provider is closing"), closeListeners(serving), ep.Close())
	}
	a.epClose, a.listeners = ep.Close, serving
	return res, nil
}

// keepListener reports whether the agent wants ln, and closes it when it
// doesn't.
func keepListener(want bool, ln net.Listener) bool {
	if ln == nil {
		return false
	}
	if want {
		return true
	}
	if err := ln.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		slog.Debug("closing an unused relay listener", "error", err)
	}
	return false
}

// privateCaller identifies an agent on the relay by its peer ID. A
// principal shaped like an email is its email, as Agent Mesh keeps an email
// unless the identity provider marked it unverified.
func privateCaller(srv meshSession) func(net.Conn) netprovider.Caller {
	return func(c net.Conn) netprovider.Caller {
		info, ok := srv.WhoIs(c.RemoteAddr().String())
		if !ok || info == nil || info.PeerID == "" {
			return netprovider.Caller{}
		}
		caller := netprovider.Caller{Node: info.PeerID, Machine: info.PeerID, ClientKey: "peer:" + info.PeerID}
		if isEmail(info.Principal) {
			caller.Email = info.Principal
		}
		return caller
	}
}

func publicCaller(srv meshSession) func(net.Conn) netprovider.Caller {
	return func(c net.Conn) netprovider.Caller {
		info, ok := srv.WhoIs(c.RemoteAddr().String())
		if !ok || info == nil || info.PeerID == "" {
			return netprovider.Caller{}
		}
		return netprovider.Caller{ClientKey: "peer:" + info.PeerID}
	}
}

// Member implements [netprovider.Provider]. The relay looks peers up only
// by their connections.
func (a *agentMesh) Member(context.Context, string) (netprovider.Caller, error) {
	return netprovider.Caller{}, netprovider.ErrUnsupported
}

func (a *agentMesh) session() meshSession {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.srv == nil {
		return nil
	}
	return a.srv
}

func (a *agentMesh) Peers(ctx context.Context) ([]netprovider.PeerInfo, error) {
	a.mu.Lock()
	srv, self := a.srv, a.peerID
	a.mu.Unlock()
	if srv == nil {
		return nil, netprovider.ErrUnsupported
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	found, err := srv.Discover(ctx, api.ServiceType_SERVICE_TYPE_A2A, "")
	if err != nil {
		return nil, fmt.Errorf("discovering agents on the relay: %w", err)
	}
	var peers []netprovider.PeerInfo
	for _, p := range found {
		// The service name is chosen by the member and reaches the agent's
		// prompt as the peer's name, so it has to be a valid agent name.
		if p.GetPeerId() == "" || !validName(p.GetSrvName()) || p.GetPeerId() == self {
			continue
		}
		peers = append(peers, netprovider.PeerInfo{
			Name:        p.GetSrvName(),
			URL:         fmt.Sprintf("http://%s/mesh/%s/a2a/%s", agentMeshHost, p.GetPeerId(), p.GetSrvName()),
			Description: p.GetSrvDescription(),
			Node:        p.GetPeerId(),
		})
	}
	return peers, nil
}

// Outbound sends the server's requests to http://agentmesh.local/... over
// the relay's authenticated streams, unchanged.
func (a *agentMesh) Outbound() http.Handler {
	proxy := &httputil.ReverseProxy{
		FlushInterval: -1,
		Rewrite: func(r *httputil.ProxyRequest) {
			r.Out.URL.Scheme, r.Out.URL.Host, r.Out.Host = "http", agentMeshHost, agentMeshHost
		},
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			srv := a.session()
			if srv == nil {
				return nil, errNotJoined
			}
			return srv.HTTPClient().Transport.RoundTrip(r)
		}),
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(hostname(r.Host), agentMeshHost) {
			http.Error(w, "this network provider reaches only "+agentMeshHost, http.StatusBadGateway)
			return
		}
		if a.session() == nil {
			http.Error(w, errNotJoined.Error(), http.StatusServiceUnavailable)
			return
		}
		proxy.ServeHTTP(w, r)
	})
}

var errNotJoined = errors.New("not connected to the relay")

func hostname(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

func (a *agentMesh) Close() error {
	a.mu.Lock()
	a.closed = true
	stopHealth := a.stopHealth
	a.stopHealth = nil
	a.mu.Unlock()
	// The health check takes the lock, so it stops before Close takes it.
	if stopHealth != nil {
		stopHealth()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	errs := []error{closeListeners(a.listeners)}
	a.listeners = nil
	if a.epClose != nil {
		errs = append(errs, a.epClose())
		a.epClose = nil
	}
	if a.srv != nil {
		errs = append(errs, a.srv.Close())
		a.srv = nil
	}
	return errors.Join(errs...)
}

func closeListeners(lns []net.Listener) error {
	var errs []error
	for _, ln := range lns {
		if err := ln.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
