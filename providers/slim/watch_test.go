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
	"sync"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/netprovider"
)

type fakeHost struct {
	mu     sync.Mutex
	health []netprovider.Health
}

func (h *fakeHost) Health(health netprovider.Health) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.health = append(h.health, health)
}

func (h *fakeHost) states() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, u := range h.health {
		out = append(out, u.State)
	}
	return out
}

type fakeNode struct {
	mu        sync.Mutex
	up        bool
	listening bool
	closed    bool
}

func (n *fakeNode) connected() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.up
}

func (n *fakeNode) setUp(up bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.up = up
}

func (n *fakeNode) listen(a2asrv.RequestHandler) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.listening = true
}

func (n *fakeNode) transport(nm name) (a2aclient.Transport, error) {
	return &fakeTransport{name: nm, destroyed: &destroyCounter{counts: map[string]int{}}}, nil
}

func (n *fakeNode) close() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.closed = true
	return nil
}

func (n *fakeNode) state() (listening, closed bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.listening, n.closed
}

// fakeNetwork opens fake nodes while the node is reachable.
type fakeNetwork struct {
	mu        sync.Mutex
	reachable bool
	opened    []*fakeNode
}

func (f *fakeNetwork) open(context.Context, settings, identity, name) (node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.reachable {
		return nil, errors.New("connection refused")
	}
	n := &fakeNode{up: true}
	f.opened = append(f.opened, n)
	return n, nil
}

func (f *fakeNetwork) setReachable(r bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reachable = r
}

func (f *fakeNetwork) nodes() []*fakeNode {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*fakeNode(nil), f.opened...)
}

func joinedProvider(t *testing.T, h *fakeHost, f *fakeNetwork) *provider {
	t.Helper()
	p := newProvider(h, f.open)
	if _, err := p.Join(t.Context(), netprovider.JoinRequest{Agent: "coder", URL: "http://127.0.0.1:46357?namespace=demo", Token: testSecret, Private: true}); err != nil {
		t.Fatalf("provider.Join() error = %v", err)
	}
	p.mu.Lock()
	p.handler = &inbound{}
	p.node.listen(p.handler)
	p.mu.Unlock()
	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Errorf("provider.Close() error = %v", err)
		}
	})
	return p
}

func TestCheckJoinsTheNodeAgain(t *testing.T) {
	t.Parallel()
	h := &fakeHost{}
	f := &fakeNetwork{reachable: true}
	p := joinedProvider(t, h, f)
	first := f.nodes()[0]

	p.check(t.Context())
	if got := h.states(); got != nil {
		t.Fatalf("check() with the connection up health states = %v, want none", got)
	}

	first.setUp(false)
	f.setReachable(false)
	p.check(t.Context())
	p.check(t.Context())
	f.setReachable(true)
	p.check(t.Context())
	p.check(t.Context())

	if diff := cmp.Diff([]string{netprovider.HealthDisconnected, netprovider.HealthOK}, h.states()); diff != "" {
		t.Fatalf("check() health states wrong result (-want +got) diff = %s", diff)
	}
	nodes := f.nodes()
	if len(nodes) != 2 {
		t.Fatalf("check() opened %d nodes, want 2", len(nodes))
	}
	if _, closed := first.state(); !closed {
		t.Fatalf("check() left the lost node open")
	}
	if listening, closed := nodes[1].state(); !listening || closed {
		t.Fatalf("check() new node listening = %v, closed = %v; want listening and open", listening, closed)
	}
	tr, err := p.peerTransport(name{org: "a2a", ns: "demo", agent: "reviewer"})
	if err != nil {
		t.Fatalf("provider.peerTransport() error = %v", err)
	}
	destroy(tr)
}
