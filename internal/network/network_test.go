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
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/google/go-cmp/cmp"
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

func TestOnThisMachine(t *testing.T) {
	t.Parallel()
	ipNet := func(s string) net.Addr {
		ip, n, err := net.ParseCIDR(s)
		if err != nil {
			t.Fatalf("net.ParseCIDR(%q) error = %v", s, err)
		}
		n.IP = ip
		return n
	}
	local := []net.Addr{ipNet("127.0.0.1/8"), ipNet("192.168.1.10/24"), ipNet("100.64.0.7/32"), ipNet("fd7a:115c:a1e0::7/128")}
	tests := []struct {
		name string
		node []netip.Prefix
		want bool
	}{
		{name: "this machine's IPv4", node: []netip.Prefix{netip.MustParsePrefix("100.64.0.7/32"), netip.MustParsePrefix("fd7a:115c:a1e0::8/128")}, want: true},
		{name: "this machine's IPv6", node: []netip.Prefix{netip.MustParsePrefix("fd7a:115c:a1e0::7/128")}, want: true},
		{name: "another machine", node: []netip.Prefix{netip.MustParsePrefix("100.64.0.8/32"), netip.MustParsePrefix("fd7a:115c:a1e0::8/128")}},
		{name: "no addresses"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := onThisMachine(tc.node, local); got != tc.want {
				t.Fatalf("onThisMachine() = %v, want %v", got, tc.want)
			}
		})
	}
}
