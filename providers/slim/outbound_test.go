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
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/google/go-cmp/cmp"
)

// fakeTransport answers as the agent it was opened for. Calls it doesn't
// implement panic.
type fakeTransport struct {
	a2aclient.Transport
	name      name
	destroyed *destroyCounter
}

func (f *fakeTransport) GetExtendedAgentCard(context.Context, a2aclient.ServiceParams, *a2a.GetExtendedAgentCardRequest) (*a2a.AgentCard, error) {
	return &a2a.AgentCard{Name: f.name.String(), SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface(f.name.privateURL(), a2a.TransportProtocolJSONRPC)}}, nil
}

func (f *fakeTransport) SendMessage(_ context.Context, _ a2aclient.ServiceParams, req *a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
	return a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(f.name.agent+" got "+text(req.Message))), nil
}

func (f *fakeTransport) Destroy() error {
	f.destroyed.add(f.name.agent)
	return nil
}

type destroyCounter struct {
	mu     sync.Mutex
	counts map[string]int
}

func (d *destroyCounter) add(agent string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.counts[agent]++
}

func (d *destroyCounter) get() map[string]int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return maps.Clone(d.counts)
}

func newFakePeers(max int) (*peers, *destroyCounter) {
	d := &destroyCounter{counts: map[string]int{}}
	return newPeers(max, func(n name) (a2aclient.Transport, error) {
		return &fakeTransport{name: n, destroyed: d}, nil
	}), d
}

func text(m *a2a.Message) string {
	if m == nil {
		return ""
	}
	var s string
	for _, p := range m.Parts {
		if t, ok := p.Content.(a2a.Text); ok {
			s += string(t)
		}
	}
	return s
}

func resultText(t *testing.T, res a2a.SendMessageResult) string {
	t.Helper()
	switch r := res.(type) {
	case *a2a.Message:
		return text(r)
	case *a2a.Task:
		var s string
		for _, a := range r.Artifacts {
			s += text(&a2a.Message{Parts: a.Parts})
		}
		return s
	}
	t.Fatalf("result = %T, want a message or a task", res)
	return ""
}

// slimClient returns a client that sends requests for any URL to srv.
func slimClient(srv *httptest.Server) *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", srv.Listener.Addr().String())
	}}}
}

func TestOutbound(t *testing.T) {
	t.Parallel()
	ps, _ := newFakePeers(maxPeers)
	srv := httptest.NewServer(&outbound{peers: ps})
	t.Cleanup(srv.Close)
	client := slimClient(srv)
	coderURL := "http://slim.local/a2a/demo/coder"
	msg := &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi"))}

	tests := []struct {
		name string
		do   func(ctx context.Context) (string, error)
		want string
	}{
		{
			name: "card",
			do: func(ctx context.Context) (string, error) {
				card, err := agentcard.NewResolver(client).Resolve(ctx, coderURL, agentcard.WithPath(a2asrv.WellKnownAgentCardPath))
				if err != nil {
					return "", err
				}
				return card.Name + " " + card.SupportedInterfaces[0].URL, nil
			},
			want: "a2a/demo/coder " + coderURL,
		},
		{
			name: "card at the agent's URL",
			do: func(ctx context.Context) (string, error) {
				card, err := agentcard.NewResolver(client).Resolve(ctx, coderURL)
				if err != nil {
					return "", err
				}
				return card.Name, nil
			},
			want: "a2a/demo/coder",
		},
		{
			name: "JSON-RPC",
			do: func(ctx context.Context) (string, error) {
				res, err := a2aclient.NewJSONRPCTransport(coderURL, client).SendMessage(ctx, nil, msg)
				if err != nil {
					return "", err
				}
				return resultText(t, res), nil
			},
			want: "coder got hi",
		},
		{
			name: "REST",
			do: func(ctx context.Context) (string, error) {
				u, err := url.Parse("http://slim.local/a2a/demo/reviewer")
				if err != nil {
					return "", err
				}
				res, err := a2aclient.NewRESTTransport(u, client).SendMessage(ctx, nil, msg)
				if err != nil {
					return "", err
				}
				return resultText(t, res), nil
			},
			want: "reviewer got hi",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := tt.do(t.Context())
			if err != nil {
				t.Fatalf("request error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("request = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestOutboundRefuses(t *testing.T) {
	t.Parallel()
	ps, _ := newFakePeers(maxPeers)
	srv := httptest.NewServer(&outbound{peers: ps})
	t.Cleanup(srv.Close)
	client := slimClient(srv)
	tests := []struct {
		url  string
		want int
	}{
		{url: "http://other.local/a2a/demo/coder/.well-known/agent-card.json", want: http.StatusMisdirectedRequest},
		{url: "http://slim.local/a2a/demo", want: http.StatusNotFound},
		{url: "http://slim.local/a2a/demo/co%2Fder/.well-known/agent-card.json", want: http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			t.Parallel()
			resp, err := client.Get(tt.url)
			if err != nil {
				t.Fatalf("Get(%s) error = %v", tt.url, err)
			}
			defer func() {
				if err := resp.Body.Close(); err != nil {
					t.Errorf("closing the body: %v", err)
				}
			}()
			if _, err := io.Copy(io.Discard, resp.Body); err != nil {
				t.Fatalf("reading the body: %v", err)
			}
			if resp.StatusCode != tt.want {
				t.Fatalf("Get(%s) status = %d, want %d", tt.url, resp.StatusCode, tt.want)
			}
		})
	}
}

func TestPeersEvictAndDestroy(t *testing.T) {
	t.Parallel()
	ps, destroyed := newFakePeers(2)
	n := func(agent string) name { return name{org: "a2a", ns: "demo", agent: agent} }
	acquire := func(agent string) func() {
		t.Helper()
		_, release, err := ps.acquire(n(agent))
		if err != nil {
			t.Fatalf("peers.acquire(%s) error = %v", agent, err)
		}
		return release
	}
	check := func(step string, want map[string]int) {
		t.Helper()
		if diff := cmp.Diff(want, destroyed.get()); diff != "" {
			t.Fatalf("after %s: destroyed transports wrong result (-want +got) diff = %s", step, diff)
		}
	}

	acquire("a")()
	acquire("b")()
	acquire("c")()
	check("a third peer", map[string]int{"a": 1})

	releaseB := acquire("b")
	acquire("a")()
	acquire("d")()
	check("evicting c and the busy b", map[string]int{"a": 1, "c": 1})
	releaseB()
	check("releasing b", map[string]int{"a": 1, "b": 1, "c": 1})

	ps.close()
	check("close", map[string]int{"a": 2, "b": 1, "c": 1, "d": 1})
	if _, _, err := ps.acquire(n("a")); !errors.Is(err, errClosed) {
		t.Fatalf("peers.acquire() after close error = %v, want %v", err, errClosed)
	}
}
