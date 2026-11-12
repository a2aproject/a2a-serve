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

package front_test

import (
	"bufio"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multibase"
	mh "github.com/multiformats/go-multihash"

	"github.com/a2aproject/a2a-serve/internal/agentmesh"
	"github.com/a2aproject/a2a-serve/internal/relay/front"
)

// testHTTP has a transport of its own: httptest.Server.Close closes the idle
// connections of http.DefaultTransport, which breaks requests that parallel
// tests send through it at the same time.
var testHTTP = &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()}

var info = agentmesh.Info{A2AServe: "v1.0.0", AgentMesh: "v0.2.0"}

// upstream fakes agentmesh-one: it answers with the method and path it
// got, and echoes an upgraded connection.
func upstream(t *testing.T) *url.URL {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") == "websocket" {
			conn, rw, err := http.NewResponseController(w).Hijack()
			if err != nil {
				t.Errorf("Hijack() error = %v", err)
				return
			}
			defer func() {
				if err := conn.Close(); err != nil {
					t.Logf("closing: %v", err)
				}
			}()
			if _, err := rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"); err != nil {
				return
			}
			if err := rw.Flush(); err != nil {
				return
			}
			if _, err := io.Copy(conn, rw); err != nil {
				return
			}
			return
		}
		if _, err := fmt.Fprintf(w, "mesh %s %s from %s", r.Method, r.URL.Path, r.Header.Get("X-Forwarded-For")); err != nil {
			t.Errorf("writing: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// echo answers with its name and the path it got.
func echo(t *testing.T, name string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := fmt.Fprintf(w, "%s %s", name, r.URL.Path); err != nil {
			t.Errorf("writing: %v", err)
		}
	})
}

func newFront(t *testing.T) (*front.Front, *httptest.Server) {
	t.Helper()
	return newFrontWith(t, echo(t, "signin"))
}

func newFrontWith(t *testing.T, signIn http.Handler) (*front.Front, *httptest.Server) {
	t.Helper()
	f := front.New(info, echo(t, "control"), signIn)
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func do(t *testing.T, method, rawURL string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := testHTTP.Transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("%s %s error = %v", method, rawURL, err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Error(err)
		}
	}()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

func TestFrontRoutes(t *testing.T) {
	t.Parallel()
	f, srv := newFront(t)
	f.Open(upstream(t))
	tests := []struct {
		method, path string
		wantStatus   int
		wantPrefix   string
	}{
		{method: "GET", path: "/info", wantStatus: 200, wantPrefix: "mesh GET /info from 127.0.0.1"},
		{method: "POST", path: "/register", wantStatus: 200, wantPrefix: "mesh POST /register"},
		{method: "GET", path: "/policies", wantStatus: 200, wantPrefix: "mesh GET /policies"},
		{method: "GET", path: "/ingress/12D3/a2a/coder", wantStatus: 200, wantPrefix: "mesh GET /ingress/12D3/a2a/coder"},
		{method: "GET", path: "/a2a-serve/control/status", wantStatus: 200, wantPrefix: "control /status"},
		{method: "GET", path: "/a2a-serve/signin/.well-known/openid-configuration", wantStatus: 200, wantPrefix: "signin /.well-known/openid-configuration"},
		{method: "POST", path: "/a2a-serve/signin/token", wantStatus: 200, wantPrefix: "signin /token"},
		{method: "POST", path: "/policies", wantStatus: 404},
		{method: "PUT", path: "/policies", wantStatus: 404},
		{method: "GET", path: "/admin/status", wantStatus: 404},
		{method: "POST", path: "/admin/bootstrap-tokens", wantStatus: 404},
		{method: "GET", path: "/admin", wantStatus: 404},
		{method: "GET", path: "/console/", wantStatus: 404},
		{method: "GET", path: "/metrics", wantStatus: 404},
		{method: "GET", path: "/x/../admin/status", wantStatus: 404},
		{method: "GET", path: "//admin/status", wantStatus: 404},
		{method: "GET", path: "/console/%2e%2e/admin/status", wantStatus: 404},
		{method: "GET", path: "/info/%2E%2E/console/", wantStatus: 404},
		{method: "GET", path: "/x/%2e%2e/info", wantStatus: 200, wantPrefix: "mesh GET /info from"},
		{method: "GET", path: "/ingress/12D3/a2a/coder/", wantStatus: 200, wantPrefix: "mesh GET /ingress/12D3/a2a/coder/ from"},
		{method: "GET", path: "/ingress/" + strings.Repeat("1", 513) + "/a2a/coder", wantStatus: 400},
		{method: "CONNECT", path: "/admin/nodes/../../x", wantStatus: 405},
		{method: "TRACE", path: "/info", wantStatus: 405},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			t.Parallel()
			status, body := do(t, tt.method, srv.URL+tt.path)
			if status != tt.wantStatus || !strings.HasPrefix(body, tt.wantPrefix) {
				t.Fatalf("%s %s = %d %q, want %d %q…", tt.method, tt.path, status, body, tt.wantStatus, tt.wantPrefix)
			}
		})
	}
}

func TestFrontWithoutSignIn(t *testing.T) {
	t.Parallel()
	f, srv := newFrontWith(t, nil)
	f.Open(upstream(t))

	if status, body := do(t, http.MethodGet, srv.URL+"/a2a-serve/signin/.well-known/openid-configuration"); status != http.StatusNotFound {
		t.Fatalf("GET /a2a-serve/signin/... without sign-in = %d %q, want 404", status, body)
	}
}

func TestFrontReportsVersions(t *testing.T) {
	t.Parallel()
	_, srv := newFront(t)

	status, body := do(t, http.MethodGet, srv.URL+agentmesh.InfoPath)

	if status != http.StatusOK {
		t.Fatalf("GET %s status = %d, want %d", agentmesh.InfoPath, status, http.StatusOK)
	}
	var got agentmesh.Info
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if diff := cmp.Diff(info, got); diff != "" {
		t.Fatalf("GET %s wrong result (-want +got) diff = %s", agentmesh.InfoPath, diff)
	}
}

func TestFrontAnswers503UntilOpenAndAfterClose(t *testing.T) {
	t.Parallel()
	f, srv := newFront(t)

	if status, _ := do(t, http.MethodGet, srv.URL+"/info"); status != http.StatusServiceUnavailable {
		t.Fatalf("GET /info before Open() status = %d, want %d", status, http.StatusServiceUnavailable)
	}
	f.Open(upstream(t))
	if status, _ := do(t, http.MethodGet, srv.URL+"/info"); status != http.StatusOK {
		t.Fatalf("GET /info after Open() status = %d, want %d", status, http.StatusOK)
	}
	f.Close()
	if status, _ := do(t, http.MethodGet, srv.URL+"/info"); status != http.StatusServiceUnavailable {
		t.Fatalf("GET /info after Close() status = %d, want %d", status, http.StatusServiceUnavailable)
	}
}

func TestFrontProxiesWebSockets(t *testing.T) {
	t.Parallel()
	f, srv := newFront(t)
	f.Open(upstream(t))
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(srv.URL, "http://"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}

	if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost: relay\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("reading the upgrade response: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade status = %d, want %d", resp.StatusCode, http.StatusSwitchingProtocols)
	}
	if _, err := io.WriteString(conn, "ping"); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(br, got); err != nil {
		t.Fatalf("reading the echo: %v", err)
	}
	if string(got) != "ping" {
		t.Fatalf("echo = %q, want %q", got, "ping")
	}
}

// holder holds the requests it gets until the test ends.
type holder struct {
	entered chan struct{}
	release chan struct{}
}

func newHolder() *holder {
	return &holder{entered: make(chan struct{}, 128), release: make(chan struct{})}
}

func (h *holder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.entered <- struct{}{}
	select {
	case <-h.release:
	case <-r.Context().Done():
	}
}

// client is where a request comes from: the connection's address and the
// X-Forwarded-For header.
type client struct{ remoteAddr, forwardedFor string }

// tryer sends requests to a front whose routes a holder serves.
type tryer struct {
	f    *front.Front
	h    *holder
	path string
	wg   sync.WaitGroup
}

func newTryer(t *testing.T, f *front.Front, h *holder, path string) *tryer {
	t.Helper()
	tr := &tryer{f: f, h: h, path: path}
	t.Cleanup(func() {
		close(h.release)
		tr.wg.Wait()
	})
	return tr
}

// try sends a request from c and returns its status, or 0 once the holder
// holds it.
func (tr *tryer) try(t *testing.T, c client) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, tr.path, nil)
	req.RemoteAddr = c.remoteAddr
	if c.forwardedFor != "" {
		req.Header.Set("X-Forwarded-For", c.forwardedFor)
	}
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	tr.wg.Go(func() {
		defer close(done)
		tr.f.ServeHTTP(rec, req)
	})
	select {
	case <-done:
		return rec.Code
	case <-tr.h.entered:
		return 0
	case <-time.After(time.Minute):
		t.Fatalf("GET %s from %v: neither answered nor held", tr.path, c)
		return 0
	}
}

func TestFrontLimitsEachClient(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		path    string
		proxied bool
		limit   int
	}{
		{name: "proxied", path: "/ingress/12D3/a2a/coder", proxied: true, limit: 64},
		{name: "agent pairing", path: "/ingress/12D3/a2a/coder/pair", proxied: true, limit: 8},
		{name: "agent sign-in", path: "/ingress/12D3/a2a/coder/oauth/token", proxied: true, limit: 8},
		{name: "agent approval link", path: "/ingress/12D3/a2a/coder/hitl/abc", proxied: true, limit: 8},
		{name: "agent download", path: "/ingress/12D3/a2a/coder/download/a2a-serve", proxied: true, limit: 8},
		{name: "agent A2A route", path: "/ingress/12D3/a2a/coder/tasks", proxied: true, limit: 64},
		{name: "sign-in", path: "/a2a-serve/signin/token", limit: 32},
		{name: "control", path: "/a2a-serve/control/status", limit: 32},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHolder()
			f := front.New(info, h, h)
			if tt.proxied {
				mesh := httptest.NewServer(h)
				t.Cleanup(mesh.Close)
				u, err := url.Parse(mesh.URL)
				if err != nil {
					t.Fatal(err)
				}
				f.Open(u)
			}
			tr := newTryer(t, f, h, tt.path)
			a := client{remoteAddr: "127.0.0.1:1000", forwardedFor: "198.51.100.1"}
			b := client{remoteAddr: "127.0.0.1:1001", forwardedFor: "198.51.100.2"}

			for i := range tt.limit {
				if got := tr.try(t, a); got != 0 {
					t.Fatalf("request %d of a client status = %d, want it held", i+1, got)
				}
			}
			overLimit := tr.try(t, a)
			other := tr.try(t, b)

			if overLimit != http.StatusTooManyRequests {
				t.Fatalf("request over the client's limit status = %d, want %d", overLimit, http.StatusTooManyRequests)
			}
			if other != 0 {
				t.Fatalf("request of another client status = %d, want it held", other)
			}
		})
	}
}

func TestFrontLimitsTheRateOfEachClient(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, method, path string
		burst              int
	}{
		{name: "registration", method: http.MethodPost, path: "/register", burst: 10},
		{name: "enrollment", method: http.MethodPost, path: "/enroll", burst: 10},
		{name: "enrollment status", method: http.MethodGet, path: "/enroll/status", burst: 10},
		{name: "agent's public URL", method: http.MethodGet, path: "/ingress/12D3/a2a/coder/tasks", burst: 100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f, _ := newFront(t)
			f.Open(upstream(t))
			serve := func(c client) *httptest.ResponseRecorder {
				req := httptest.NewRequest(tt.method, tt.path, nil)
				req.RemoteAddr = c.remoteAddr
				req.Header.Set("X-Forwarded-For", c.forwardedFor)
				rec := httptest.NewRecorder()
				f.ServeHTTP(rec, req)
				return rec
			}
			a := client{remoteAddr: "127.0.0.1:1000", forwardedFor: "198.51.100.1"}
			b := client{remoteAddr: "127.0.0.1:1001", forwardedFor: "198.51.100.2"}

			for i := range tt.burst {
				if got := serve(a).Code; got != http.StatusOK {
					t.Fatalf("%s %s %d of a client status = %d, want %d", tt.method, tt.path, i+1, got, http.StatusOK)
				}
			}
			var refused *httptest.ResponseRecorder
			for range tt.burst {
				if rec := serve(a); rec.Code != http.StatusOK {
					refused = rec
					break
				}
			}
			other := serve(b)

			if refused == nil {
				t.Fatalf("%s %s past the client's burst status = %d, want %d", tt.method, tt.path, http.StatusOK, http.StatusTooManyRequests)
			}
			if refused.Code != http.StatusTooManyRequests || refused.Header().Get("Retry-After") == "" || refused.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("%s %s past the client's burst = %d with Retry-After %q and Content-Type %q, want %d with Retry-After and JSON", tt.method, tt.path, refused.Code, refused.Header().Get("Retry-After"), refused.Header().Get("Content-Type"), http.StatusTooManyRequests)
			}
			if other.Code != http.StatusOK {
				t.Fatalf("%s %s of another client status = %d, want %d", tt.method, tt.path, other.Code, http.StatusOK)
			}
		})
	}
}

// testPeer is a peer ID as libp2p builds it for an Ed25519 key: the
// identity multihash of the key's protobuf encoding.
type testPeer struct{ hash mh.Multihash }

// String returns the base58 form of the peer ID.
func (p testPeer) String() string { return p.hash.B58String() }

// cid returns the peer ID as a CID of a libp2p key.
func (p testPeer) cid() cid.Cid { return cid.NewCidV1(cid.Libp2pKey, p.hash) }

// newPeer returns a new peer ID, as an agent on the relay has.
func newPeer(t *testing.T) testPeer {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	// PublicKey{Type: Ed25519, Data: pub} in protobuf.
	key := append([]byte{0x08, 0x01, 0x12, byte(len(pub))}, pub...)
	hash, err := mh.Sum(key, mh.IDENTITY, -1)
	if err != nil {
		t.Fatal(err)
	}
	return testPeer{hash: hash}
}

// ingressTryer sends GET requests to an open front from one client and
// returns their status.
func ingressTryer(t *testing.T, forwardedFor string) (f *front.Front, serve func(segment string) int) {
	t.Helper()
	f, _ = newFront(t)
	f.Open(upstream(t))
	return f, func(segment string) int {
		req := httptest.NewRequest(http.MethodGet, "/ingress/"+url.PathEscape(segment)+"/a2a/coder/tasks", nil)
		req.RemoteAddr = "127.0.0.1:1000"
		req.Header.Set("X-Forwarded-For", forwardedFor)
		rec := httptest.NewRecorder()
		f.ServeHTTP(rec, req)
		return rec.Code
	}
}

// refusedWithin sends n requests that cycle through segments and reports
// whether one got 429.
func refusedWithin(serve func(string) int, segments []string, n int) bool {
	for i := range n {
		if serve(segments[i%len(segments)]) == http.StatusTooManyRequests {
			return true
		}
	}
	return false
}

func TestFrontLimitsTheIngressRateOfEachAgent(t *testing.T) {
	t.Parallel()
	limited, other := newPeer(t), newPeer(t)
	tests := []struct {
		name     string
		segments []string
		other    string
	}{
		{name: "an agent", segments: []string{limited.String()}, other: other.String()},
		{name: "spellings of one agent", segments: append([]string{limited.String()}, spellings(t, limited)...), other: other.cid().String()},
		{name: "segments that aren't agents", segments: []string{"not-a-peer", "also-not-a-peer", "1" + strings.Repeat("z", 1000)}, other: limited.String()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, serve := ingressTryer(t, "198.51.100.1")

			refused := refusedWithin(serve, tt.segments, 200)
			got := serve(tt.other)

			if !refused {
				t.Fatalf("GET /ingress/<%s> 200 times status = %d, want %d past one agent's burst", tt.name, http.StatusOK, http.StatusTooManyRequests)
			}
			if got != http.StatusOK {
				t.Fatalf("GET /ingress/%.20s… of another agent status = %d, want %d", tt.other, got, http.StatusOK)
			}
		})
	}
}

// spellings returns the CID forms of id in every multibase that fits in a
// path segment, which Agent Mesh reads as id.
func spellings(t *testing.T, id testPeer) []string {
	t.Helper()
	var all []string
	for base := range multibase.EncodingToStr {
		s, err := id.cid().StringOfBase(base)
		if err != nil {
			t.Fatal(err)
		}
		if base == multibase.Identity || strings.Contains(s, "/") {
			continue
		}
		all = append(all, s)
	}
	return all
}

func TestFrontLimitsTheIngressRateOfEachClientAcrossAgents(t *testing.T) {
	t.Parallel()
	f, serve := ingressTryer(t, "198.51.100.1")

	refused := false
	for range 1000 {
		if serve(newPeer(t).String()) == http.StatusTooManyRequests {
			refused = true
			break
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/ingress/"+newPeer(t).String()+"/a2a/coder/tasks", nil)
	req.RemoteAddr = "127.0.0.1:1001"
	req.Header.Set("X-Forwarded-For", "198.51.100.2")
	rec := httptest.NewRecorder()
	f.ServeHTTP(rec, req)

	if !refused {
		t.Fatalf("GET /ingress/<new agent> from a client spraying agents status = %d, want %d", http.StatusOK, http.StatusTooManyRequests)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /ingress/<new agent> from another client status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestFrontTellsClientsApart(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		held, next client
		wantSame   bool
	}{
		{
			name: "clients behind a local proxy",
			held: client{remoteAddr: "127.0.0.1:1000", forwardedFor: "198.51.100.1"},
			next: client{remoteAddr: "127.0.0.1:1001", forwardedFor: "198.51.100.2"},
		},
		{
			name: "clients behind Cloud Run",
			held: client{remoteAddr: "169.254.1.1:1000", forwardedFor: "198.51.100.1"},
			next: client{remoteAddr: "169.254.1.1:1001", forwardedFor: "198.51.100.2"},
		},
		{
			name: "clients behind a chain of local proxies",
			held: client{remoteAddr: "127.0.0.1:1000", forwardedFor: "198.51.100.1, 127.0.0.1"},
			next: client{remoteAddr: "127.0.0.1:1001", forwardedFor: "198.51.100.2, 127.0.0.1"},
		},
		{
			name:     "clients with headers that aren't addresses",
			held:     client{remoteAddr: "127.0.0.1:1000", forwardedFor: "first"},
			next:     client{remoteAddr: "127.0.0.1:1001", forwardedFor: "second"},
			wantSame: true,
		},
		{
			name:     "a client with a forged header behind a private proxy",
			held:     client{remoteAddr: "10.0.0.1:1000", forwardedFor: "198.51.100.1"},
			next:     client{remoteAddr: "10.0.0.1:1001", forwardedFor: "203.0.113.9, 198.51.100.1"},
			wantSame: true,
		},
		{
			name:     "a direct client with forged headers",
			held:     client{remoteAddr: "203.0.113.5:1000", forwardedFor: "198.51.100.1"},
			next:     client{remoteAddr: "203.0.113.5:1001", forwardedFor: "198.51.100.2"},
			wantSame: true,
		},
		{
			name: "direct clients",
			held: client{remoteAddr: "203.0.113.5:1000"},
			next: client{remoteAddr: "203.0.113.6:1000"},
		},
		{
			name:     "direct IPv6 clients in one /64",
			held:     client{remoteAddr: "[2001:db8:1:2::1]:1000"},
			next:     client{remoteAddr: "[2001:db8:1:2:ffff::9]:1000"},
			wantSame: true,
		},
		{
			name: "direct IPv6 clients in different /64s",
			held: client{remoteAddr: "[2001:db8:1:2::1]:1000"},
			next: client{remoteAddr: "[2001:db8:1:3::1]:1000"},
		},
		{
			name:     "IPv6 clients in one /64 behind a local proxy",
			held:     client{remoteAddr: "127.0.0.1:1000", forwardedFor: "2001:db8:1:2::1"},
			next:     client{remoteAddr: "127.0.0.1:1001", forwardedFor: "2001:db8:1:2::2"},
			wantSame: true,
		},
		{
			name:     "local clients without a header",
			held:     client{remoteAddr: "127.0.0.1:1000"},
			next:     client{remoteAddr: "127.0.0.1:1001"},
			wantSame: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHolder()
			tr := newTryer(t, front.New(info, h, h), h, "/a2a-serve/signin/token")
			for i := range 32 {
				if got := tr.try(t, tt.held); got != 0 {
					t.Fatalf("request %d of the first client status = %d, want it held", i+1, got)
				}
			}

			got := tr.try(t, tt.next)

			want := 0
			if tt.wantSame {
				want = http.StatusTooManyRequests
			}
			if got != want {
				t.Fatalf("request of the second client status = %d, want %d", got, want)
			}
		})
	}
}

func TestFrontReusesUpstreamConnectionsAfterABurst(t *testing.T) {
	t.Parallel()
	const burst = 20
	var (
		mu       sync.Mutex
		waiting  int
		release  = make(chan struct{})
		newConns atomic.Int32
	)
	mesh := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		waiting++
		ch := release
		if waiting == burst {
			close(release)
			release, waiting = make(chan struct{}), 0
		}
		mu.Unlock()
		<-ch
		if _, err := io.WriteString(w, "ok"); err != nil {
			t.Errorf("writing: %v", err)
		}
	}))
	mesh.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			newConns.Add(1)
		}
	}
	mesh.Start()
	t.Cleanup(mesh.Close)
	u, err := url.Parse(mesh.URL)
	if err != nil {
		t.Fatal(err)
	}
	f := front.New(info, echo(t, "control"), nil)
	f.Open(u)
	t.Cleanup(f.Close)
	sendBurst := func() {
		var wg sync.WaitGroup
		for range burst {
			wg.Go(func() {
				rec := httptest.NewRecorder()
				f.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/policies", nil))
				if rec.Code != http.StatusOK {
					t.Errorf("GET /policies status = %d, want %d", rec.Code, http.StatusOK)
				}
			})
		}
		wg.Wait()
	}

	sendBurst()
	sendBurst()

	if got := newConns.Load(); got != burst {
		t.Fatalf("connections to Agent Mesh after two bursts of %d = %d, want %d", burst, got, burst)
	}
}
