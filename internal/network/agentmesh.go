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

//go:build agentmesh

package network

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/agentmesh/api"
	"github.com/google/agentmesh/sdk/meshnet"
)

// AgentMeshBuilt reports whether this a2a-serve can join Agent Mesh relays.
const AgentMeshBuilt = true

// meshSession is what the provider uses of meshnet, so that its connection
// handling, WhoIs and discovery are tested without a relay.
type meshSession interface {
	WhoIs(remoteAddr string) (*meshnet.CallerInfo, bool)
	Discover(ctx context.Context, serviceType api.ServiceType, serviceName string) ([]*api.DiscoveredProvider, error)
	HTTPClient() *http.Client
	Close() error
}

// agentMesh is a [Provider] which joins an Agent Mesh relay in-process with
// meshnet: private streams between agents on the relay, verified caller
// identity, discovery, and a public URL on the relay which forwards
// requests unchanged, so pairing codes and Google sign-in keep working.
type agentMesh struct {
	cfg RelayConfig

	mu         sync.Mutex
	started    atomic.Bool
	srv        meshSession
	epClose    func() error
	private    *splitListener
	public     *splitListener
	peerID     string
	owner      string
	privateURL *url.URL
	publicURL  *url.URL
}

// NewRelay returns the provider of an Agent Mesh relay.
func NewRelay(cfg RelayConfig) (Provider, error) {
	if !IsRelayURL(cfg.URL) {
		return nil, fmt.Errorf("invalid relay URL %q", cfg.URL)
	}
	return &agentMesh{cfg: cfg}, nil
}

func (a *agentMesh) Name() string { return ProviderRelay }

func (a *agentMesh) ListenPrivate(ctx context.Context) (net.Listener, *url.URL, error) {
	if err := a.start(ctx); err != nil {
		return nil, nil, err
	}
	if a.private == nil {
		return nil, nil, ErrUnsupported
	}
	return a.private, a.privateURL, nil
}

func (a *agentMesh) ListenPublic(ctx context.Context) (net.Listener, *url.URL, error) {
	if err := a.start(ctx); err != nil {
		return nil, nil, err
	}
	if a.public == nil {
		return nil, nil, errors.New("the relay gives this agent no public URL")
	}
	return a.public, a.publicURL, nil
}

func (a *agentMesh) logf(format string, args ...any) {
	if a.cfg.Logf != nil {
		a.cfg.Logf(format, args...)
	}
}

func hasMeshIdentity(stateDir string) bool {
	_, cred, err := meshnet.ReadStateDir(stateDir)
	return err == nil && len(cred.GetBiscuit()) > 0
}

func (a *agentMesh) start(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.started.Load() {
		return nil
	}
	srv := &meshnet.Server{
		StateDir:        a.cfg.StateDir,
		ControlPlaneURL: a.cfg.URL,
		BootstrapToken:  a.cfg.Token,
		AllowInsecure:   strings.HasPrefix(a.cfg.URL, "http://"),
		OnCredentialLapse: func(err error) {
			slog.Warn("relay credential lapsed; retrying", "error", err)
			a.logf("! relay: credential lapsed (%v)", err)
		},
	}
	// Without a token, the agent signs in with an account; the first run
	// prints the link, later runs use the stored identity.
	if a.cfg.Token == "" {
		login := &RelayLogin{RelayURL: a.cfg.URL, StateDir: a.cfg.StateDir, HTTP: &http.Client{Timeout: 30 * time.Second}, Logf: a.cfg.Logf, Poll: time.Second}
		srv.TokenSource = login.Token
		if !hasMeshIdentity(a.cfg.StateDir) {
			a.logf("… joining the relay %s; this needs a sign-in on the first run (or --relay-token)", a.cfg.URL)
		}
	}
	st, err := srv.Up(ctx)
	if err != nil {
		return errors.Join(fmt.Errorf("joining the relay %s: %w", a.cfg.URL, err), srv.Close())
	}
	opts := meshnet.A2AOptions{Name: a.cfg.AgentName, Description: a.cfg.Description, Public: a.cfg.Public}
	if a.cfg.Public {
		opts.IngressAuth = api.IngressAuthMode_INGRESS_AUTH_MODE_PASSTHROUGH
	}
	ep, err := srv.ListenA2A(ctx, opts)
	if err != nil {
		return errors.Join(fmt.Errorf("registering the agent on the relay: %w", err), srv.Close())
	}
	if a.cfg.Public && ep.PublicURL == nil {
		return errors.Join(errors.New("the relay didn't give this agent a public URL: its policy doesn't allow it, or it has no ingress"), ep.Close(), srv.Close())
	}
	a.srv, a.epClose, a.peerID = srv, ep.Close, st.PeerID
	// Agents which joined with a token don't say who owns them.
	if a.cfg.Token == "" {
		a.owner = SavedOwner(a.cfg.StateDir)
	}
	a.attach(ep.PrivateListener, ep.PrivateURL, ep.PublicListener, ep.PublicURL)
	slog.Info("joined the relay", "relay", a.cfg.URL, "peer_id", st.PeerID, "routers", st.AttachedRouters)
	a.started.Store(true)
	return nil
}

func (a *agentMesh) attach(privLn net.Listener, privURL *url.URL, pubLn net.Listener, pubURL *url.URL) {
	attach := func(want bool, ln net.Listener, u *url.URL) (*splitListener, *url.URL) {
		if ln == nil {
			return nil, nil
		}
		if !want {
			if err := ln.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				slog.Debug("closing an unused relay listener", "error", err)
			}
			return nil, nil
		}
		sl := newSplitListener(ln.Addr(), nil)
		sl.claim()
		go pump(ln, sl)
		return sl, u
	}
	a.private, a.privateURL = attach(a.cfg.Private, privLn, privURL)
	a.public, a.publicURL = attach(a.cfg.Public, pubLn, pubURL)
}

func pump(src net.Listener, dst *splitListener) {
	for {
		c, err := src.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				slog.Warn("relay listener stopped", "error", err)
			}
			return
		}
		go dst.deliver(c)
	}
}

func (a *agentMesh) session() meshSession {
	if !a.started.Load() {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.srv
}

// WhoIs identifies an agent on the relay by its peer ID. A principal shaped
// like an email is its email, as Agent Mesh keeps an email unless the
// identity provider marked it unverified. The caller is this agent's user
// when that email is the one this agent signed in with.
func (a *agentMesh) WhoIs(_ context.Context, remoteAddr string) (*Peer, error) {
	srv := a.session()
	if srv == nil {
		return nil, ErrNoIdentity
	}
	info, ok := srv.WhoIs(remoteAddr)
	if !ok || info == nil || info.PeerID == "" {
		return nil, ErrNoIdentity
	}
	p := &Peer{Node: info.PeerID, Machine: info.PeerID}
	if !IsEmail(info.Principal) {
		return p, nil
	}
	p.Email = info.Principal
	a.mu.Lock()
	owner := a.owner
	a.mu.Unlock()
	p.Self = owner != "" && strings.EqualFold(p.Email, owner)
	return p, nil
}

// Member implements [Provider]. The relay looks peers up only by their
// connections.
func (a *agentMesh) Member(context.Context, string) (*Peer, error) {
	return nil, ErrUnsupported
}

func (a *agentMesh) Peers(ctx context.Context) ([]PeerInfo, error) {
	srv := a.session()
	if srv == nil {
		return nil, ErrUnsupported
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	found, err := srv.Discover(ctx, api.ServiceType_SERVICE_TYPE_A2A, "")
	if err != nil {
		return nil, fmt.Errorf("discovering agents on the relay: %w", err)
	}
	a.mu.Lock()
	self := a.peerID
	a.mu.Unlock()
	var candidates []PeerInfo
	for _, p := range found {
		if p.GetPeerId() == "" || p.GetSrvName() == "" || p.GetPeerId() == self {
			continue
		}
		candidates = append(candidates, PeerInfo{
			Name:        p.GetSrvName(),
			URL:         fmt.Sprintf("http://%s/mesh/%s/a2a/%s", AgentMeshHost, p.GetPeerId(), p.GetSrvName()),
			Description: p.GetSrvDescription(),
			Online:      true,
			Node:        p.GetPeerId(),
		})
	}
	return ProbeAgents(ctx, a.HTTPClient(), candidates), nil
}

// HTTPClient sends requests to http://agentmesh.local/... over the relay's
// authenticated streams, and others over the internet.
func (a *agentMesh) HTTPClient() *http.Client {
	return &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL == nil || !strings.EqualFold(r.URL.Host, AgentMeshHost) {
			return http.DefaultTransport.RoundTrip(r)
		}
		srv := a.session()
		if srv == nil {
			return nil, errors.New("not connected to the relay")
		}
		return srv.HTTPClient().Transport.RoundTrip(r)
	})}
}

func (a *agentMesh) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	var errs []error
	for _, l := range []*splitListener{a.private, a.public} {
		if l != nil {
			errs = append(errs, l.Close())
		}
	}
	a.private, a.public = nil, nil
	if a.epClose != nil {
		errs = append(errs, a.epClose())
		a.epClose = nil
	}
	if a.srv != nil {
		errs = append(errs, a.srv.Close())
		a.srv = nil
	}
	a.started.Store(false)
	return errors.Join(errs...)
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
