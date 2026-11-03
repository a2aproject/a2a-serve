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
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/google/agentmesh/api"
	"github.com/google/agentmesh/sdk/meshnet"
	"github.com/google/go-cmp/cmp"
)

type fakeMeshSession struct {
	callers   map[string]*meshnet.CallerInfo
	providers []*api.DiscoveredProvider
	hc        *http.Client
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
	}}
	tests := []struct {
		name  string
		owner string
		addr  string
		want  *Peer
	}{
		{name: "same person", owner: "ben@example.com", addr: "mine", want: &Peer{ID: "Ben@example.com", Machine: "p1", Self: true}},
		{name: "another person", owner: "ben@example.com", addr: "anna", want: &Peer{ID: "anna@example.com", Machine: "p2"}},
		{name: "agent joined with a token", owner: "ben@example.com", addr: "server", want: &Peer{ID: "node:p3", Machine: "p3"}},
		{name: "this agent joined with a token", owner: "", addr: "mine", want: &Peer{ID: "Ben@example.com", Machine: "p1"}},
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
	want := []PeerInfo{{Name: "anna", URL: "http://agentmesh.local/mesh/p2/a2a/anna", Description: "Anna's coder", Online: true}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("Peers() wrong result (-want +got) diff = %s", diff)
	}
}
