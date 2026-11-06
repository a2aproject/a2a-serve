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
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/agentmesh/api"
	"github.com/google/agentmesh/sdk/meshnet"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

type fakeMeshSession struct {
	callers    map[string]*meshnet.CallerInfo
	providers  []*api.DiscoveredProvider
	hc         *http.Client
	refreshErr error
	refreshes  atomic.Int32

	mu      sync.Mutex
	routers []string
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

func (f *fakeMeshSession) Close() error { return nil }

func startedMesh(s meshSession, owner, self string) *agentMesh {
	a := &agentMesh{srv: s, owner: owner, peerID: self}
	a.started.Store(true)
	return a
}

func TestAgentMeshWhoIs(t *testing.T) {
	t.Parallel()
	s := &fakeMeshSession{callers: map[string]*meshnet.CallerInfo{
		"mine":   {PeerID: "p1", Principal: "Ben@example.com"},
		"anna":   {PeerID: "p2", Principal: "anna@example.com"},
		"server": {PeerID: "p3", Principal: "node:p3"},
		"login":  {PeerID: "p4", Principal: "ben@github"},
	}}
	tests := []struct {
		name  string
		owner string
		addr  string
		want  *Peer
	}{
		{name: "same person", owner: "ben@example.com", addr: "mine", want: &Peer{Email: "Ben@example.com", Node: "p1", Machine: "p1", Self: true}},
		{name: "another person", owner: "ben@example.com", addr: "anna", want: &Peer{Email: "anna@example.com", Node: "p2", Machine: "p2"}},
		{name: "agent joined with a token", owner: "ben@example.com", addr: "server", want: &Peer{Node: "p3", Machine: "p3"}},
		{name: "principal that isn't an email", owner: "ben@github", addr: "login", want: &Peer{Node: "p4", Machine: "p4"}},
		{name: "this agent joined with a token", owner: "", addr: "mine", want: &Peer{Email: "Ben@example.com", Node: "p1", Machine: "p1"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := startedMesh(s, tc.owner, "self").WhoIs(context.Background(), tc.addr)
			if err != nil {
				t.Fatalf("WhoIs() error = %v", err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("WhoIs() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
	if _, err := startedMesh(s, "", "self").WhoIs(context.Background(), "unknown"); err != ErrNoIdentity {
		t.Fatalf("WhoIs() of an unknown caller error = %v, want %v", err, ErrNoIdentity)
	}
}

func TestAgentMeshPeersAndHTTPClient(t *testing.T) {
	t.Parallel()
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/mesh/p2/a2a/anna/.well-known/agent-card.json" {
			if _, err := io.WriteString(w, `{"name":"anna","description":"Anna's coder","supportedInterfaces":[{"url":"http://agentmesh.local/mesh/p2/a2a/anna","protocolBinding":"JSONRPC"}]}`); err != nil {
				t.Errorf("writing: %v", err)
			}
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(agent.Close)
	target, err := url.Parse(agent.URL)
	if err != nil {
		t.Fatal(err)
	}
	viaMesh := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		r.URL.Scheme, r.URL.Host = target.Scheme, target.Host
		return http.DefaultTransport.RoundTrip(r)
	})}
	s := &fakeMeshSession{hc: viaMesh, providers: []*api.DiscoveredProvider{
		{PeerId: "self", SrvName: "ben"},
		{PeerId: "p2", SrvName: "anna", SrvDescription: "Anna's coder"},
	}}
	got, err := startedMesh(s, "", "self").Peers(context.Background())
	if err != nil {
		t.Fatalf("Peers() error = %v", err)
	}
	want := []PeerInfo{{Name: "anna", URL: "http://agentmesh.local/mesh/p2/a2a/anna", Description: "Anna's coder", Online: true, Node: "p2"}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("Peers() wrong result (-want +got) diff = %s", diff)
	}
}

func TestRelayLoginDecodesControlPlaneInfoProto(t *testing.T) {
	t.Parallel()
	token := fakeIDToken(t, map[string]any{"email": "Ben@example.com", "email_verified": true})
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("GET /info", func(w http.ResponseWriter, r *http.Request) {
		payload, err := proto.Marshal(&api.ControlPlaneInfoResponse{
			OidcIssuer:      srv.URL + "/idp",
			ClientId:        "mesh-client",
			Audience:        "mesh-aud",
			RouterAddresses: []string{"/ip4/127.0.0.1/tcp/4001/p2p/12D3KooWRouter"},
			BannedPeerIds:   []string{"12D3KooWBanned"},
			Routers:         []*api.RouterInfo{{PeerId: "12D3KooWRouter", Connections: 2, ConnectionLimit: 100}},
			Directory:       api.NewDirectoryParameters(3, 5*time.Minute),
		})
		if err != nil {
			t.Errorf("proto.Marshal() error = %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
		if _, err := w.Write(payload); err != nil {
			t.Errorf("Write() error = %v", err)
		}
	})
	mux.HandleFunc("GET /idp/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		if err := json.NewEncoder(w).Encode(map[string]any{
			"device_authorization_endpoint": srv.URL + "/idp/device",
			"token_endpoint":                srv.URL + "/idp/token",
		}); err != nil {
			t.Errorf("Encode() error = %v", err)
		}
	})
	mux.HandleFunc("POST /idp/device", func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("client_id") != "mesh-client" || r.FormValue("audience") != "mesh-aud" {
			http.Error(w, "bad client or audience", http.StatusBadRequest)
			return
		}
		if err := json.NewEncoder(w).Encode(map[string]any{
			"device_code":               "dc-proto",
			"user_code":                 "WXYZ",
			"verification_uri_complete": srv.URL + "/idp/activate?code=WXYZ",
			"interval":                  0,
			"expires_in":                60,
		}); err != nil {
			t.Errorf("Encode() error = %v", err)
		}
	})
	mux.HandleFunc("POST /idp/token", func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("device_code") != "dc-proto" {
			http.Error(w, "bad code", http.StatusBadRequest)
			return
		}
		if err := json.NewEncoder(w).Encode(map[string]string{"id_token": token}); err != nil {
			t.Errorf("Encode() error = %v", err)
		}
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	l := &RelayLogin{RelayURL: srv.URL, StateDir: dir, HTTP: srv.Client(), Poll: time.Millisecond}
	got, err := l.Token(context.Background())
	if err != nil {
		t.Fatalf("RelayLogin.Token() error = %v, want nil", err)
	}
	if got != token {
		t.Fatalf("RelayLogin.Token() = %q, want %q", got, token)
	}
	if owner := SavedOwner(dir); owner != "ben@example.com" {
		t.Fatalf("SavedOwner() = %q, want %q", owner, "ben@example.com")
	}
}

func marshalLibp2pEd25519Key(t *testing.T) []byte {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey() error = %v", err)
	}
	var b []byte
	b = protowire.AppendTag(b, 1, protowire.VarintType)
	b = protowire.AppendVarint(b, 1)
	b = protowire.AppendTag(b, 2, protowire.BytesType)
	b = protowire.AppendBytes(b, priv)
	return b
}

func TestHasMeshIdentity(t *testing.T) {
	t.Parallel()
	privBytes := marshalLibp2pEd25519Key(t)

	interruptedDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(interruptedDir, "agent.db"), []byte("bbolt"), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	emptyBiscuitDir := t.TempDir()
	if err := meshnet.WriteStateDir(emptyBiscuitDir, privBytes, &api.MemberCredential{ControlPlaneUrl: "https://relay.example.com"}); err != nil {
		t.Fatalf("meshnet.WriteStateDir() error = %v", err)
	}

	enrolledDir := t.TempDir()
	if err := meshnet.WriteStateDir(enrolledDir, privBytes, &api.MemberCredential{
		ControlPlaneUrl: "https://relay.example.com",
		Biscuit:         []byte("enrolled-biscuit"),
	}); err != nil {
		t.Fatalf("meshnet.WriteStateDir() error = %v", err)
	}

	tests := []struct {
		name string
		dir  string
		want bool
	}{
		{name: "empty dir", dir: t.TempDir(), want: false},
		{name: "interrupted first run with agent.db only", dir: interruptedDir, want: false},
		{name: "saved key without biscuit", dir: emptyBiscuitDir, want: false},
		{name: "enrolled credential with biscuit", dir: enrolledDir, want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := hasMeshIdentity(tc.dir); got != tc.want {
				t.Fatalf("hasMeshIdentity(%q) = %v, want %v", tc.dir, got, tc.want)
			}
		})
	}
}

func TestSavedPeerID(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := meshnet.WriteStateDir(dir, marshalLibp2pEd25519Key(t), &api.MemberCredential{ControlPlaneUrl: "https://relay.example.com", Biscuit: []byte("biscuit")}); err != nil {
		t.Fatalf("meshnet.WriteStateDir() error = %v", err)
	}

	if got := savedPeerID(dir); !strings.HasPrefix(got, "12D3KooW") {
		t.Fatalf("savedPeerID() = %q, want an Ed25519 peer ID", got)
	}
	if got := savedPeerID(t.TempDir()); got != "" {
		t.Fatalf("savedPeerID() of an empty directory = %q, want none", got)
	}
}

func TestNewRelayAttachAndClose(t *testing.T) {
	t.Parallel()
	if _, err := NewRelay(RelayConfig{URL: "tailscale"}); err == nil {
		t.Fatal("NewRelay(invalid) error = nil, want error")
	}

	p, err := NewRelay(RelayConfig{URL: "https://relay.example.com", Private: true, Public: false})
	if err != nil {
		t.Fatalf("NewRelay() error = %v, want nil", err)
	}
	am := p.(*agentMesh)
	privLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	pubLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	privURL, err := url.Parse("http://agentmesh.local/mesh/p1/a2a/coder")
	if err != nil {
		t.Fatalf("url.Parse() error = %v", err)
	}
	pubURL, err := url.Parse("https://relay.example.com/ingress/p1/a2a/coder")
	if err != nil {
		t.Fatalf("url.Parse() error = %v", err)
	}
	var epClosed bool
	am.srv = &fakeMeshSession{}
	am.epClose = func() error { epClosed = true; return privLn.Close() }
	am.attach(privLn, privURL, pubLn, pubURL)
	am.started.Store(true)

	if _, err := pubLn.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("unused public listener Accept() error = %v, want %v", err, net.ErrClosed)
	}
	gotLn, gotURL, err := am.ListenPrivate(context.Background())
	if err != nil {
		t.Fatalf("ListenPrivate() error = %v, want nil", err)
	}
	if gotLn == nil || gotURL.String() != privURL.String() {
		t.Fatalf("ListenPrivate() = (%v, %v), want listener at %v", gotLn, gotURL, privURL)
	}
	if _, _, err := am.ListenPublic(context.Background()); err == nil {
		t.Fatal("ListenPublic() when Public=false error = nil, want error")
	}
	if err := am.Close(); err != nil {
		t.Fatalf("Close() error = %v, want nil", err)
	}
	if !epClosed {
		t.Fatal("Close() did not call endpoint closer")
	}
}

func TestAgentMeshHealth(t *testing.T) {
	t.Parallel()
	const relay = "https://relay.example.com"
	banned := errors.New("refresh refused (403 Forbidden): Node is banned")
	down := errors.New("http request failed: connection refused")
	tests := []struct {
		name       string
		routers    []string
		refreshErr error
		want       HealthState
	}{
		{name: "attached to a router", routers: []string{"r1"}, refreshErr: banned, want: HealthOK},
		{name: "relay refuses the credential", refreshErr: banned, want: HealthRemoved},
		{name: "relay unreachable", refreshErr: down, want: HealthDisconnected},
		{name: "control plane answers without routers", want: HealthDisconnected},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := &fakeMeshSession{routers: tc.routers, refreshErr: tc.refreshErr}
			a := startedMesh(s, "", "self")
			a.cfg.URL = relay
			a.checkHealth(context.Background(), time.Now())
			got := a.Health()
			if got.State != tc.want {
				t.Fatalf("Health() = %+v, want state %q", got, tc.want)
			}
			if got.OK() != (got.Problem == "") {
				t.Fatalf("Health() = %+v, want a problem exactly when not OK", got)
			}
		})
	}
}

func TestAgentMeshHealthRecoversAndProbesSparingly(t *testing.T) {
	t.Parallel()
	s := &fakeMeshSession{refreshErr: errors.New("refresh refused (403 Forbidden): Node is banned")}
	a := startedMesh(s, "", "self")
	a.cfg.URL = "https://relay.example.com"
	now := time.Now()
	a.checkHealth(context.Background(), now)
	a.checkHealth(context.Background(), now.Add(healthProbeEvery/2))
	if got := s.refreshes.Load(); got != 1 {
		t.Fatalf("Refresh() calls within %v = %d, want 1", healthProbeEvery, got)
	}
	if got := a.Health().State; got != HealthRemoved {
		t.Fatalf("Health() state between probes = %q, want %q", got, HealthRemoved)
	}
	a.checkHealth(context.Background(), now.Add(healthProbeEvery))
	if got := s.refreshes.Load(); got != 2 {
		t.Fatalf("Refresh() calls after %v = %d, want 2", healthProbeEvery, got)
	}

	s.setRouters([]string{"r1"})
	a.checkHealth(context.Background(), now.Add(2*healthProbeEvery))
	if got := a.Health(); !got.OK() {
		t.Fatalf("Health() after reattaching = %+v, want OK", got)
	}
}
