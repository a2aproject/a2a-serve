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

package network

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/google/go-cmp/cmp"
	"tailscale.com/ipn"
)

func TestProbeAgents(t *testing.T) {
	t.Parallel()
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/agent-card.json" {
			http.NotFound(w, r)
			return
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"name": "coder", "description": "opencode agent"}); err != nil {
			t.Error(err)
		}
	}))
	defer agent.Close()
	other := httptest.NewServer(http.NotFoundHandler())
	defer other.Close()

	got := ProbeAgents(t.Context(), http.DefaultClient, []PeerInfo{
		{Name: "laptop", URL: agent.URL, Online: true},
		{Name: "printer", URL: other.URL, Online: true},
	})
	want := []PeerInfo{{Name: "coder", URL: agent.URL, Description: "opencode agent", Online: true}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("ProbeAgents() wrong result (-want +got) diff = %s", diff)
	}
}

func TestSplitListener(t *testing.T) {
	t.Parallel()
	closed := false
	l := newSplitListener(&net.TCPAddr{}, func() error { closed = true; return nil })
	l.claim()
	a, b := net.Pipe()
	go l.deliver(a)
	c, err := l.Accept()
	if err != nil || c != a {
		t.Fatalf("Accept() = %v, %v, want the delivered connection", c, err)
	}
	if err := l.Close(); err != nil || !closed {
		t.Fatalf("Close() = %v, closer called %v, want nil, true", err, closed)
	}
	if _, err := l.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Accept() after Close() error = %v, want %v", err, net.ErrClosed)
	}
	l.deliver(b)
}

func TestSplitListenerUnclaimedClosesConnections(t *testing.T) {
	t.Parallel()
	l := newSplitListener(&net.TCPAddr{}, nil)
	a, b := net.Pipe()
	done := make(chan struct{})
	go func() {
		l.deliver(a)
		close(done)
	}()
	if _, err := b.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("Read() error = %v, want %v", err, io.EOF)
	}
	<-done
}

func TestLoginPromptShow(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		urls []string
		want []bool
	}{
		{
			name: "repeated link every 10th time",
			urls: []string{"a", "a", "a", "a", "a", "a", "a", "a", "a", "a", "a"},
			want: []bool{true, false, false, false, false, false, false, false, false, false, true},
		},
		{
			name: "new link at once",
			urls: []string{"a", "a", "b", "b", "a"},
			want: []bool{true, false, true, false, true},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var p loginPrompt
			var got []bool
			for _, u := range tc.urls {
				got = append(got, p.show(u))
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("loginPrompt.show() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestFunnelSource(t *testing.T) {
	t.Parallel()
	funnel := func(src string) net.Conn {
		c, _ := net.Pipe()
		return &ipn.FunnelConn{Conn: c, Src: netip.MustParseAddrPort(src)}
	}
	plain, _ := net.Pipe()
	tests := []struct {
		name   string
		conn   net.Conn
		want   string
		wantOK bool
	}{
		{name: "IPv4", conn: funnel("203.0.113.7:5555"), want: "203.0.113.7", wantOK: true},
		{name: "IPv4 over TLS", conn: tls.Server(funnel("203.0.113.7:5555"), &tls.Config{}), want: "203.0.113.7", wantOK: true},
		{name: "IPv4-mapped IPv6", conn: funnel("[::ffff:203.0.113.7]:5555"), want: "203.0.113.7", wantOK: true},
		{name: "IPv6", conn: funnel("[2001:db8:1:2:3:4:5:6]:5555"), want: "2001:db8:1:2::/64", wantOK: true},
		{name: "not through Funnel", conn: plain},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := FunnelSource(tc.conn)
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("FunnelSource() = %q, %v, want %q, %v", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}
