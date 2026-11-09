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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/agentmesh/api"
	"github.com/google/agentmesh/sdk/meshnet"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"

	"github.com/a2aproject/a2a-serve/internal/agentmesh"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
	"github.com/a2aproject/a2a-serve/internal/names"
)

// AgentMeshBuilt reports whether this a2a-serve can join Agent Mesh relays.
const AgentMeshBuilt = true

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
	health     Health
	probedAt   time.Time
	stopHealth func()
	cards      *CardCache
}

// NewRelay returns the provider of an Agent Mesh relay.
func NewRelay(cfg RelayConfig) (Provider, error) {
	if err := agentmesh.CheckRelayURL(cfg.URL); err != nil {
		return nil, fmt.Errorf("invalid relay URL %q: %w", cfg.URL, err)
	}
	return &agentMesh{cfg: cfg, cards: newCardCache()}, nil
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

// savedPeerID returns the peer ID of the mesh identity in stateDir, or "".
func savedPeerID(stateDir string) string {
	key, _, err := meshnet.ReadStateDir(stateDir)
	if err != nil {
		return ""
	}
	priv, err := crypto.UnmarshalPrivateKey(key)
	if err != nil {
		return ""
	}
	id, err := peer.IDFromPrivateKey(priv)
	if err != nil {
		return ""
	}
	return id.String()
}

// inviteHashFile keeps a hash of the invite (--relay-token) the agent last
// joined the relay with.
const inviteHashFile = "invite-hash"

// useInvite prepares stateDir for a join with token. A new invite drops the
// saved mesh credential and keeps the identity key, so the agent enrolls
// again with the same peer ID. Routers keep refusing the credential of a
// removed agent after the relay lets it back, and meshnet resumes a saved
// credential before it looks at the token.
func useInvite(stateDir, token string) error {
	sum := sha256.Sum256([]byte(token))
	hash := hex.EncodeToString(sum[:])
	path := filepath.Join(stateDir, inviteHashFile)
	saved, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// An agent that joined before its invite was recorded keeps its
		// credential: its invite may be used up.
	case err != nil:
		return fmt.Errorf("reading the relay's last invite: %w", err)
	case strings.TrimSpace(string(saved)) == hash:
		return nil
	default:
		if err := dropMeshCredential(stateDir); err != nil {
			return fmt.Errorf("dropping the relay credential of the last invite: %w", err)
		}
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	return fsutil.WriteAtomic(path, []byte(hash+"\n"), 0o600)
}

func dropMeshCredential(stateDir string) error {
	key, cred, err := meshnet.ReadStateDir(stateDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(cred.GetBiscuit()) == 0 {
		return nil
	}
	return meshnet.WriteStateDir(stateDir, key, &api.MemberCredential{ControlPlaneUrl: cred.GetControlPlaneUrl()})
}

func (a *agentMesh) start(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.started.Load() {
		return nil
	}
	if a.cfg.Token != "" {
		if err := useInvite(a.cfg.StateDir, a.cfg.Token); err != nil {
			return fmt.Errorf("joining the relay %s: %w", a.cfg.URL, err)
		}
	}
	srv := &meshnet.Server{
		StateDir:        a.cfg.StateDir,
		ControlPlaneURL: a.cfg.URL,
		BootstrapToken:  a.cfg.Token,
		AllowInsecure:   agentmesh.InsecureLoopback(a.cfg.URL),
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
		refresh, err := savedRefreshToken(a.cfg.StateDir)
		if err != nil {
			slog.Warn("reading the relay's refresh token", "error", err)
		}
		if !hasMeshIdentity(a.cfg.StateDir) && refresh.Token == "" {
			a.logf("… joining the relay %s; this needs a sign-in on the first run (or --relay-token)", a.cfg.URL)
		}
	}
	st, err := srv.Up(ctx)
	err = explainJoin(err)
	if rerr := RecordJoin(a.cfg.StateDir, err); rerr != nil {
		slog.Warn("recording the relay join", "error", rerr)
	}
	if err != nil {
		if peerID := savedPeerID(a.cfg.StateDir); peerID != "" {
			if rerr := relayRemoval(ctx, &http.Client{Timeout: 10 * time.Second}, a.cfg, peerID); rerr != nil {
				err = rerr
			}
		}
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
	watchCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go a.watchHealth(watchCtx, done)
	a.stopHealth = func() { cancel(); <-done }
	return nil
}

// Join joins the relay, so that a refused invite fails before the agent
// starts.
func (a *agentMesh) Join(ctx context.Context) error { return a.start(ctx) }

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
		// The service name is chosen by the member and reaches the agent's
		// prompt as the peer's name, so it has to be a valid agent name.
		if p.GetPeerId() == "" || !names.Valid(p.GetSrvName()) || p.GetPeerId() == self {
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
	return a.cards.ProbeAgents(ctx, a.HTTPClient(), candidates, []string{a.cfg.AgentName}), nil
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
		return checkPeerResponse(srv.HTTPClient().Transport.RoundTrip(r))
	})}
}

func (a *agentMesh) Close() error {
	a.mu.Lock()
	stopHealth := a.stopHealth
	a.stopHealth = nil
	a.mu.Unlock()
	// The health check takes the lock, so it stops before Close takes it.
	if stopHealth != nil {
		stopHealth()
	}
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
