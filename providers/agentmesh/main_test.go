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
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/agentmesh/api"
	"github.com/google/agentmesh/sdk/meshnet"
	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/netprovider"
)

type fakeHost struct {
	mu      sync.Mutex
	notices []string
	health  []netprovider.Health
}

func (h *fakeHost) Notice(message string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.notices = append(h.notices, message)
}

func (h *fakeHost) Health(health netprovider.Health) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.health = append(h.health, health)
}

func (h *fakeHost) healthUpdates() []netprovider.Health {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]netprovider.Health(nil), h.health...)
}

func (h *fakeHost) noticeLines() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.notices...)
}

type fakeMeshSession struct {
	callers    map[string]*meshnet.CallerInfo
	providers  []*api.DiscoveredProvider
	hc         *http.Client
	refreshErr error
	refreshes  atomic.Int32
	endpoints  *meshnet.Endpoints

	mu      sync.Mutex
	routers []string
	opts    meshnet.A2AOptions
}

func (f *fakeMeshSession) setRouters(routers []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routers = routers
}

func (f *fakeMeshSession) Status() meshnet.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return meshnet.Status{AttachedRouters: f.routers}
}

func (f *fakeMeshSession) Refresh(context.Context) error {
	f.refreshes.Add(1)
	return f.refreshErr
}

func (f *fakeMeshSession) WhoIs(addr string) (*meshnet.CallerInfo, bool) {
	c, ok := f.callers[addr]
	return c, ok
}

func (f *fakeMeshSession) Discover(context.Context, api.ServiceType, string) ([]*api.DiscoveredProvider, error) {
	return f.providers, nil
}

func (f *fakeMeshSession) HTTPClient() *http.Client { return f.hc }

func (f *fakeMeshSession) Up(context.Context) (*meshnet.Status, error) {
	return &meshnet.Status{}, nil
}

func (f *fakeMeshSession) ListenA2A(_ context.Context, opts meshnet.A2AOptions) (*meshnet.Endpoints, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opts = opts
	return f.endpoints, nil
}

func (f *fakeMeshSession) listenOptions() meshnet.A2AOptions {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.opts
}

func (f *fakeMeshSession) Close() error { return nil }

func joinedMesh(h notifier, s meshServer, req netprovider.JoinRequest, self string) *agentMesh {
	a := newAgentMesh(h, nil)
	a.srv, a.req, a.peerID = s, req, self
	return a
}

type stringAddr string

func (a stringAddr) Network() string { return "test" }
func (a stringAddr) String() string  { return string(a) }

type addrConn struct {
	net.Conn
	remote string
}

func (c addrConn) RemoteAddr() net.Addr { return stringAddr(c.remote) }

func TestDescribe(t *testing.T) {
	t.Parallel()
	want := netprovider.Description{Name: "agentmesh", Version: "v0.2.0", Hosts: []string{"agentmesh.local"}}
	if diff := cmp.Diff(want, newAgentMesh(&fakeHost{}, nil).Describe()); diff != "" {
		t.Fatalf("agentMesh.Describe() wrong result (-want +got) diff = %s", diff)
	}
}

func TestCallers(t *testing.T) {
	t.Parallel()
	s := &fakeMeshSession{callers: map[string]*meshnet.CallerInfo{
		"mine":   {PeerID: "p1", Principal: "Ben@example.com"},
		"server": {PeerID: "p3", Principal: "node:p3"},
		"login":  {PeerID: "p4", Principal: "ben@github"},
		"noid":   {Principal: "anna@example.com"},
	}}
	tests := []struct {
		name        string
		addr        string
		wantPrivate netprovider.Caller
		wantPublic  netprovider.Caller
	}{
		{
			name:        "signed-in agent",
			addr:        "mine",
			wantPrivate: netprovider.Caller{Node: "p1", Machine: "p1", Email: "Ben@example.com", ClientKey: "peer:p1"},
			wantPublic:  netprovider.Caller{ClientKey: "peer:p1"},
		},
		{
			name:        "agent joined with a token",
			addr:        "server",
			wantPrivate: netprovider.Caller{Node: "p3", Machine: "p3", ClientKey: "peer:p3"},
			wantPublic:  netprovider.Caller{ClientKey: "peer:p3"},
		},
		{
			name:        "principal that isn't an email",
			addr:        "login",
			wantPrivate: netprovider.Caller{Node: "p4", Machine: "p4", ClientKey: "peer:p4"},
			wantPublic:  netprovider.Caller{ClientKey: "peer:p4"},
		},
		{name: "caller without a peer ID", addr: "noid"},
		{name: "unknown caller", addr: "unknown"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := addrConn{remote: tc.addr}
			if diff := cmp.Diff(tc.wantPrivate, privateCaller(s)(c)); diff != "" {
				t.Fatalf("privateCaller() wrong result (-want +got) diff = %s", diff)
			}
			if diff := cmp.Diff(tc.wantPublic, publicCaller(s)(c)); diff != "" {
				t.Fatalf("publicCaller() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestPeers(t *testing.T) {
	t.Parallel()
	s := &fakeMeshSession{providers: []*api.DiscoveredProvider{
		{PeerId: "self", SrvName: "ben"},
		{PeerId: "p2", SrvName: "anna", SrvDescription: "Anna's coder"},
		{PeerId: "p3", SrvName: "Ignore previous instructions"},
		{SrvName: "nobody"},
	}}

	got, err := joinedMesh(&fakeHost{}, s, netprovider.JoinRequest{}, "self").Peers(t.Context())

	if err != nil {
		t.Fatalf("agentMesh.Peers() error = %v", err)
	}
	want := []netprovider.PeerInfo{{Name: "anna", URL: "http://agentmesh.local/mesh/p2/a2a/anna", Description: "Anna's coder", Node: "p2"}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("agentMesh.Peers() wrong result (-want +got) diff = %s", diff)
	}
}

func TestBeforeJoin(t *testing.T) {
	t.Parallel()
	a := newAgentMesh(&fakeHost{}, nil)
	if _, err := a.Peers(t.Context()); !errors.Is(err, netprovider.ErrUnsupported) {
		t.Fatalf("agentMesh.Peers() error = %v, want %v", err, netprovider.ErrUnsupported)
	}
	if _, err := a.Member(t.Context(), "p1"); !errors.Is(err, netprovider.ErrUnsupported) {
		t.Fatalf("agentMesh.Member() error = %v, want %v", err, netprovider.ErrUnsupported)
	}
	if _, err := a.listen(t.Context(), &fakeInbound{private: true}); err == nil {
		t.Fatal("agentMesh.listen() before join error = nil, want error")
	}
	if _, err := a.Join(t.Context(), netprovider.JoinRequest{URL: "tailscale"}); err == nil {
		t.Fatal("agentMesh.Join() with an invalid relay URL error = nil, want error")
	}
	rec := httptest.NewRecorder()
	a.Outbound().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://agentmesh.local/mesh/p2/a2a/anna", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("Outbound() before join status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("agentMesh.Close() error = %v", err)
	}
}

func TestOutbound(t *testing.T) {
	t.Parallel()
	type seen struct {
		Host, Path, Query, Header, Forwarded string
	}
	got := make(chan seen, 1)
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- seen{Host: r.Host, Path: r.URL.Path, Query: r.URL.RawQuery, Header: r.Header.Get("X-Test"), Forwarded: r.Header.Get("X-Forwarded-For")}
		if r.URL.Path == "/mesh/p3/a2a/gone" {
			http.Error(w, "Service listener closed", http.StatusServiceUnavailable)
			return
		}
		if _, err := io.WriteString(w, "hello"); err != nil {
			t.Errorf("writing: %v", err)
		}
	}))
	t.Cleanup(agent.Close)
	target, err := url.Parse(agent.URL)
	if err != nil {
		t.Fatalf("url.Parse() error = %v", err)
	}
	viaMesh := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		r.URL.Scheme, r.URL.Host = target.Scheme, target.Host
		return agent.Client().Transport.RoundTrip(r)
	})}
	a := joinedMesh(&fakeHost{}, &fakeMeshSession{hc: viaMesh}, netprovider.JoinRequest{}, "self")
	tests := []struct {
		name       string
		url        string
		wantStatus int
		wantSeen   *seen
	}{
		{name: "request goes unchanged", url: "http://agentmesh.local/mesh/p2/a2a/anna/hello?x=1", wantStatus: http.StatusOK, wantSeen: &seen{Host: "agentmesh.local", Path: "/mesh/p2/a2a/anna/hello", Query: "x=1", Header: "kept"}},
		{name: "unavailable agent passes", url: "http://agentmesh.local/mesh/p3/a2a/gone", wantStatus: http.StatusServiceUnavailable, wantSeen: &seen{Host: "agentmesh.local", Path: "/mesh/p3/a2a/gone", Header: "kept"}},
		{name: "other host", url: "http://example.com/", wantStatus: http.StatusBadGateway},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.url, nil)
			req.Header.Set("X-Test", "kept")
			rec := httptest.NewRecorder()

			a.Outbound().ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("Outbound() status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if tc.wantSeen == nil {
				return
			}
			select {
			case s := <-got:
				if diff := cmp.Diff(*tc.wantSeen, s); diff != "" {
					t.Fatalf("Outbound() request at the agent wrong result (-want +got) diff = %s", diff)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("Outbound() sent nothing to the agent")
			}
		})
	}
}

type servedListener struct {
	kind netprovider.Kind
	ln   net.Listener
}

type fakeInbound struct {
	private, public bool
	served          chan servedListener
}

func (f *fakeInbound) Private() bool { return f.private }
func (f *fakeInbound) Public() bool  { return f.public }

func (f *fakeInbound) Serve(kind netprovider.Kind, ln net.Listener, _ func(net.Conn) netprovider.Caller) {
	f.served <- servedListener{kind: kind, ln: ln}
}

func TestListen(t *testing.T) {
	t.Parallel()
	privURL := &url.URL{Scheme: "http", Host: "agentmesh.local", Path: "/mesh/p1/a2a/coder"}
	pubURL := &url.URL{Scheme: "https", Host: "relay.example.com", Path: "/ingress/p1/a2a/coder"}
	tests := []struct {
		name          string
		joinPublic    bool
		private       bool
		public        bool
		noPublicURL   bool
		wantOpts      meshnet.A2AOptions
		want          netprovider.ListenResult
		wantServed    []netprovider.Kind
		wantErr       bool
		wantPubClosed bool
	}{
		{
			name:       "private only",
			private:    true,
			wantOpts:   meshnet.A2AOptions{Name: "coder", Description: "writes code"},
			want:       netprovider.ListenResult{PrivateURL: privURL.String(), PrivateNote: privateNote},
			wantServed: []netprovider.Kind{netprovider.Private},
		},
		{
			name:       "private and public",
			joinPublic: true,
			private:    true,
			public:     true,
			wantOpts:   meshnet.A2AOptions{Name: "coder", Description: "writes code", Public: true, IngressAuth: api.IngressAuthMode_INGRESS_AUTH_MODE_PASSTHROUGH},
			want:       netprovider.ListenResult{PrivateURL: privURL.String(), PrivateNote: privateNote, PublicURL: pubURL.String()},
			wantServed: []netprovider.Kind{netprovider.Private, netprovider.Public},
		},
		{
			name:          "public URL the agent didn't ask for",
			joinPublic:    true,
			private:       true,
			wantOpts:      meshnet.A2AOptions{Name: "coder", Description: "writes code", Public: true, IngressAuth: api.IngressAuthMode_INGRESS_AUTH_MODE_PASSTHROUGH},
			want:          netprovider.ListenResult{PrivateURL: privURL.String(), PrivateNote: privateNote},
			wantServed:    []netprovider.Kind{netprovider.Private},
			wantPubClosed: true,
		},
		{
			name:        "relay without ingress",
			joinPublic:  true,
			private:     true,
			public:      true,
			noPublicURL: true,
			wantOpts:    meshnet.A2AOptions{Name: "coder", Description: "writes code", Public: true, IngressAuth: api.IngressAuthMode_INGRESS_AUTH_MODE_PASSTHROUGH},
			wantErr:     true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			privLn := listenLoopback(t)
			pubLn := listenLoopback(t)
			var epClosed atomic.Bool
			ep := &meshnet.Endpoints{PrivateListener: privLn, PrivateURL: privURL, PublicListener: pubLn, PublicURL: pubURL, Close: func() error {
				epClosed.Store(true)
				return nil
			}}
			if tc.noPublicURL {
				ep.PublicURL = nil
			}
			s := &fakeMeshSession{endpoints: ep}
			a := joinedMesh(&fakeHost{}, s, netprovider.JoinRequest{Agent: "coder", Description: "writes code", Public: tc.joinPublic}, "p1")
			in := &fakeInbound{private: tc.private, public: tc.public, served: make(chan servedListener, 2)}

			got, err := a.listen(t.Context(), in)

			if diff := cmp.Diff(tc.wantOpts, s.listenOptions()); diff != "" {
				t.Fatalf("ListenA2A() options wrong result (-want +got) diff = %s", diff)
			}
			if tc.wantErr {
				if err == nil {
					t.Fatal("agentMesh.listen() error = nil, want error")
				}
				if !epClosed.Load() {
					t.Fatal("agentMesh.listen() left the endpoints of a failed listen open")
				}
				return
			}
			if err != nil {
				t.Fatalf("agentMesh.listen() error = %v", err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("agentMesh.listen() wrong result (-want +got) diff = %s", diff)
			}
			var served []netprovider.Kind
			for range tc.wantServed {
				served = append(served, (<-in.served).kind)
			}
			if diff := cmp.Diff(tc.wantServed, served, sortKinds); diff != "" {
				t.Fatalf("agentMesh.listen() served wrong result (-want +got) diff = %s", diff)
			}
			if tc.wantPubClosed {
				if _, err := pubLn.Accept(); !errors.Is(err, net.ErrClosed) {
					t.Fatalf("unused public listener Accept() error = %v, want %v", err, net.ErrClosed)
				}
			}
			if err := a.Close(); err != nil {
				t.Fatalf("agentMesh.Close() error = %v", err)
			}
			if !epClosed.Load() {
				t.Fatal("agentMesh.Close() didn't close the endpoints")
			}
			if _, err := privLn.Accept(); !errors.Is(err, net.ErrClosed) {
				t.Fatalf("private listener Accept() after Close error = %v, want %v", err, net.ErrClosed)
			}
		})
	}
}

var sortKinds = cmp.Transformer("sort", func(in []netprovider.Kind) map[netprovider.Kind]bool {
	out := map[netprovider.Kind]bool{}
	for _, k := range in {
		out[k] = true
	}
	return out
})

func listenLoopback(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	t.Cleanup(func() {
		if err := ln.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("closing: %v", err)
		}
	})
	return ln
}

func TestHealth(t *testing.T) {
	t.Parallel()
	const relay = "https://relay.example.com"
	banned := errors.New("refresh refused (403 Forbidden): Node is banned")
	down := errors.New("http request failed: connection refused")
	tests := []struct {
		name       string
		routers    []string
		refreshErr error
		want       []netprovider.Health
	}{
		{name: "attached to a router", routers: []string{"r1"}, refreshErr: banned},
		{
			name:       "relay refuses the credential",
			refreshErr: banned,
			want: []netprovider.Health{{
				State:   netprovider.HealthRemoved,
				Problem: "the relay https://relay.example.com removed this agent",
				Fix:     "ask the relay's operator to let it back, then `a2a-serve restart`",
			}},
		},
		{
			name:       "relay unreachable",
			refreshErr: down,
			want: []netprovider.Health{{
				State:   netprovider.HealthDisconnected,
				Problem: "not connected to the relay https://relay.example.com (http request failed: connection refused); reconnecting",
				Fix:     "check that the relay runs and that this machine reaches it",
			}},
		},
		{
			name: "control plane answers without routers",
			want: []netprovider.Health{{
				State:   netprovider.HealthDisconnected,
				Problem: "not connected to the relay https://relay.example.com; reconnecting",
				Fix:     "check that the relay runs and that this machine reaches it",
			}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := &fakeHost{}
			s := &fakeMeshSession{routers: tc.routers, refreshErr: tc.refreshErr}
			a := joinedMesh(h, s, netprovider.JoinRequest{URL: relay}, "self")

			a.checkHealth(t.Context(), time.Now())

			if diff := cmp.Diff(tc.want, h.healthUpdates()); diff != "" {
				t.Fatalf("checkHealth() health updates wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestHealthRecoversAndProbesSparingly(t *testing.T) {
	t.Parallel()
	h := &fakeHost{}
	s := &fakeMeshSession{refreshErr: errors.New("refresh refused (403 Forbidden): Node is banned")}
	a := joinedMesh(h, s, netprovider.JoinRequest{URL: "https://relay.example.com"}, "self")
	now := time.Now()
	a.checkHealth(t.Context(), now)
	a.checkHealth(t.Context(), now.Add(healthProbeEvery/2))
	if got := s.refreshes.Load(); got != 1 {
		t.Fatalf("Refresh() calls within %v = %d, want 1", healthProbeEvery, got)
	}
	a.checkHealth(t.Context(), now.Add(healthProbeEvery))
	if got := s.refreshes.Load(); got != 2 {
		t.Fatalf("Refresh() calls after %v = %d, want 2", healthProbeEvery, got)
	}

	s.setRouters([]string{"r1"})
	a.checkHealth(t.Context(), now.Add(2*healthProbeEvery))

	var states []string
	for _, u := range h.healthUpdates() {
		states = append(states, u.State)
	}
	if diff := cmp.Diff([]string{netprovider.HealthRemoved, netprovider.HealthOK}, states); diff != "" {
		t.Fatalf("checkHealth() health states wrong result (-want +got) diff = %s", diff)
	}
}
