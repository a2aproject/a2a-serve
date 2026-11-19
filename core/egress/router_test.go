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

package egress_test

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/a2aproject/a2a-serve/core/egress"
)

func newRouterBoundary(t *testing.T, r egress.Router) *egress.Boundary {
	t.Helper()
	b, err := egress.NewWithHosts(egress.Config{Router: r, Log: slog.New(slog.DiscardHandler)}, nil)
	if err != nil {
		t.Fatalf("egress.New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := b.Close(); err != nil {
			t.Errorf("Boundary.Close() error = %v", err)
		}
	})
	return b
}

// openEndpoint opens an endpoint of b and serves it, returning its address.
func openEndpoint(t *testing.T, b *egress.Boundary, key string, creds ...egress.Credential) (*egress.Endpoint, string) {
	t.Helper()
	ep, err := b.Open("test", key, creds...)
	if err != nil {
		t.Fatalf("Boundary.Open() error = %v", err)
	}
	return ep, serve(t, ep)
}

// tunnel opens a CONNECT tunnel to target through the endpoint at addr and
// returns the status, the Proxy-Status header and the tunnel.
func tunnel(ctx context.Context, addr, target string) (int, string, net.Conn, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return 0, "", nil, err
	}
	if _, err := fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
		return 0, "", nil, errors.Join(err, c.Close())
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		return 0, "", nil, errors.Join(err, c.Close())
	}
	return resp.StatusCode, resp.Header.Get("Proxy-Status"), &bufferedConn{Conn: c, r: br}, nil
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// client returns a client whose connections tunnel through the endpoint at
// addr and trust the PEM certificate ca.
func client(t *testing.T, addr string, ca []byte) *http.Client {
	t.Helper()
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca)
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, target string) (net.Conn, error) {
			status, _, c, err := tunnel(ctx, addr, target)
			if err != nil {
				return nil, err
			}
			if status != http.StatusOK {
				return nil, errors.Join(fmt.Errorf("CONNECT %s status = %d", target, status), c.Close())
			}
			return c, nil
		},
		TLSClientConfig: &tls.Config{RootCAs: roots},
	}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: 10 * time.Second}
}

// get sends GET url with header and returns the status and the body.
func get(t *testing.T, c *http.Client, url string, header http.Header) (int, string, error) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header = header
	resp, err := c.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("closing the body error = %v", err)
		}
	}()
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), err
}

// answer is a middleware that answers each request itself with its method,
// URL and the header X-Seen.
func answer(http.RoundTripper) http.RoundTripper {
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		text := r.Method + " " + r.URL.String() + " " + r.Header.Get("X-Seen")
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(text)), ContentLength: int64(len(text)), Request: r}, nil
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRouterRoutes(t *testing.T) {
	t.Parallel()
	echo := echoServer(t)
	routes := map[string]egress.Route{
		"refuse.test":   egress.Refuse(http.StatusUnavailableForLegalReasons, "held for review"),
		"default.test":  egress.Refuse(http.StatusOK, ""),
		"direct.test":   egress.Direct(),
		"loop.test":     egress.Redirect("127.0.0.1", 80),
		"redirect.test": egress.Redirect("missing.test", 443),
		"dial.test":     egress.Dial(func(ctx context.Context) (net.Conn, error) { return (&net.Dialer{}).DialContext(ctx, "tcp", echo) }),
		"serve.test": egress.Serve(func(c net.Conn) {
			if _, err := io.WriteString(c, "served"); err != nil {
				t.Errorf("writing to the served connection error = %v", err)
			}
		}),
		"zero.test": {},
	}
	var mu sync.Mutex
	var flows []egress.Flow
	b := newRouterBoundary(t, egress.RouterFunc(func(_ context.Context, f egress.Flow) (egress.Route, error) {
		mu.Lock()
		flows = append(flows, f)
		mu.Unlock()
		if r, ok := routes[f.Host]; ok {
			return r, nil
		}
		return egress.Route{}, errors.New("no route")
	}))
	_, addr := openEndpoint(t, b, "ctx-1")
	tests := []struct {
		target      string
		wantStatus  int
		wantProxy   string
		wantTunnel  string
		wantRefusal *egress.Refusal
	}{
		{target: "refuse.test:443", wantStatus: http.StatusUnavailableForLegalReasons, wantProxy: `a2a-serve; error=http_request_error; reason="held for review"`, wantRefusal: &egress.Refusal{Destination: "refuse.test:443"}},
		{target: "default.test:443", wantStatus: http.StatusForbidden, wantProxy: "a2a-serve; error=http_request_denied; reason=refused", wantRefusal: &egress.Refusal{Destination: "default.test:443"}},
		{target: "direct.test:443", wantStatus: http.StatusForbidden, wantProxy: "a2a-serve; error=destination_ip_prohibited; reason=resolved-address-denied", wantRefusal: &egress.Refusal{Destination: "direct.test:443"}},
		{target: "loop.test:443", wantStatus: http.StatusForbidden, wantProxy: "a2a-serve; error=destination_ip_prohibited; reason=ip-not-on-allowlist", wantRefusal: &egress.Refusal{Destination: "loop.test:443"}},
		{target: "redirect.test:443", wantStatus: http.StatusBadGateway, wantProxy: "a2a-serve; error=dns_error; reason=resolve-failed", wantRefusal: &egress.Refusal{Destination: "redirect.test:443", Unresolved: true}},
		{target: "dial.test:22", wantStatus: http.StatusOK, wantTunnel: "ping"},
		{target: "serve.test:80", wantStatus: http.StatusOK, wantTunnel: "served"},
		{target: "zero.test:80", wantStatus: http.StatusBadGateway, wantProxy: "a2a-serve; error=destination_unavailable; reason=router-failed"},
		{target: "error.test:80", wantStatus: http.StatusBadGateway, wantProxy: "a2a-serve; error=destination_unavailable; reason=router-failed"},
	}
	for _, tc := range tests {
		t.Run(tc.target, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			status, proxy, c, err := tunnel(ctx, addr, tc.target)
			if err != nil {
				t.Fatalf("CONNECT %s error = %v", tc.target, err)
			}
			defer func() {
				if err := c.Close(); err != nil {
					t.Errorf("closing the tunnel error = %v", err)
				}
			}()
			if status != tc.wantStatus || proxy != tc.wantProxy {
				t.Fatalf("CONNECT %s = %d %q, want %d %q", tc.target, status, proxy, tc.wantStatus, tc.wantProxy)
			}
			if tc.wantTunnel == "" {
				return
			}
			if tc.wantTunnel == "ping" {
				if _, err := io.WriteString(c, "ping"); err != nil {
					t.Fatal(err)
				}
			}
			got := make([]byte, len(tc.wantTunnel))
			if _, err := io.ReadFull(c, got); err != nil || string(got) != tc.wantTunnel {
				t.Fatalf("tunnel to %s read %q, %v, want %q", tc.target, got, err, tc.wantTunnel)
			}
		})
	}
	t.Cleanup(func() {
		want := egress.Status{}
		for _, tc := range tests {
			if tc.wantRefusal != nil {
				want.Refused++
				want.Recent = append(want.Recent, *tc.wantRefusal)
			}
		}
		byDestination := cmpopts.SortSlices(func(a, b egress.Refusal) bool { return a.Destination < b.Destination })
		if diff := cmp.Diff(want, b.Status(), byDestination); diff != "" {
			t.Errorf("Boundary.Status() wrong result (-want +got) diff = %s", diff)
		}
		mu.Lock()
		defer mu.Unlock()
		for _, f := range flows {
			if f.Sandbox != "test" || f.Key != "ctx-1" || f.Port == 0 {
				t.Errorf("router got flow %+v, want sandbox test, key ctx-1 and a port", f)
			}
		}
	})
}

func TestRouterHTTP(t *testing.T) {
	t.Parallel()
	upstream := upstreamEcho(t)
	elsewhere := &http.Transport{}
	t.Cleanup(elsewhere.CloseIdleConnections)
	rewrite := func(http.RoundTripper) http.RoundTripper {
		return roundTripFunc(func(r *http.Request) (*http.Response, error) {
			out := r.Clone(r.Context())
			out.URL.Scheme, out.URL.Host, out.Host = "http", upstream, ""
			out.Header.Set("X-Seen", "rewritten")
			return elsewhere.RoundTrip(out)
		})
	}
	wraps := map[string]func(http.RoundTripper) http.RoundTripper{"answer.test": answer, "rewrite.test": rewrite}
	b := newRouterBoundary(t, egress.RouterFunc(func(_ context.Context, f egress.Flow) (egress.Route, error) {
		return egress.HTTP(wraps[f.Host]), nil
	}))
	ep, addr := openEndpoint(t, b, "")
	c := client(t, addr, ep.CA())
	tests := []struct {
		name, url  string
		header     http.Header
		wantStatus int
		wantBody   string
	}{
		{name: "answers itself over TLS", url: "https://answer.test/v1?a=1", header: http.Header{"X-Seen": {"kept"}}, wantStatus: http.StatusOK, wantBody: "GET https://answer.test/v1?a=1 kept"},
		{name: "answers itself over plain HTTP", url: "http://answer.test:8080/v1", wantStatus: http.StatusOK, wantBody: "GET http://answer.test:8080/v1 "},
		{name: "rewrites and sends elsewhere", url: "https://rewrite.test/path", wantStatus: http.StatusOK, wantBody: "GET /path rewritten"},
		{name: "next keeps the address checks", url: "https://next.test/", wantStatus: http.StatusForbidden},
		{name: "next refuses upgrades", url: "http://next.test/ws", header: http.Header{"Connection": {"Upgrade"}, "Upgrade": {"websocket"}}, wantStatus: http.StatusNotImplemented, wantBody: "the sandbox can't switch protocols on this connection\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			status, body, err := get(t, c, tc.url, tc.header)
			if err != nil {
				t.Fatalf("GET %s error = %v", tc.url, err)
			}
			if status != tc.wantStatus || tc.wantBody != "" && body != tc.wantBody {
				t.Fatalf("GET %s = %d %q, want %d %q", tc.url, status, body, tc.wantStatus, tc.wantBody)
			}
		})
	}
}

// upstreamEcho serves plain HTTP on loopback, answering each request with
// its path and the header X-Seen, and returns its address.
func upstreamEcho(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.WriteString(w, r.Method+" "+r.URL.Path+" "+r.Header.Get("X-Seen")); err != nil {
			t.Errorf("writing the answer error = %v", err)
		}
	}), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("serving the upstream error = %v", err)
		}
	}()
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Errorf("closing the upstream error = %v", err)
		}
	})
	return ln.Addr().String()
}

// echoServer echoes what each connection sends, and returns its address.
func echoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := ln.Close(); err != nil {
			t.Errorf("closing the echo server error = %v", err)
		}
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				if _, err := io.Copy(c, c); err != nil {
					t.Logf("echo: %v", err)
				}
				if err := c.Close(); err != nil {
					t.Logf("closing an echo connection: %v", err)
				}
			}()
		}
	}()
	return ln.Addr().String()
}

// hostsRouter answers every request itself and intercepts only hosts.
type hostsRouter struct{ hosts []string }

func (r hostsRouter) Route(context.Context, egress.Flow) (egress.Route, error) {
	return egress.HTTP(answer), nil
}

func (r hostsRouter) Hosts() []string { return r.hosts }

func TestRouterCA(t *testing.T) {
	t.Parallel()
	policyRouter, err := egress.Policy{}.Router()
	if err != nil {
		t.Fatalf("Policy.Router() error = %v", err)
	}
	tests := []struct {
		name string
		r    egress.Router
		// wantNames are the names the CA is constrained to, nil for any.
		wantCA    bool
		wantNames []string
		wantTLS   map[string]bool
	}{
		{name: "router without hosts", r: egress.RouterFunc(func(context.Context, egress.Flow) (egress.Route, error) { return egress.HTTP(answer), nil }),
			wantCA: true, wantTLS: map[string]bool{"api.test": true, "other.test": true}},
		{name: "router with hosts", r: hostsRouter{hosts: []string{"API.test.", "10.0.0.1"}}, wantCA: true, wantNames: []string{"api.test"},
			wantTLS: map[string]bool{"api.test": true, "other.test": false}},
		{name: "router with no hosts", r: hostsRouter{hosts: []string{}}, wantTLS: map[string]bool{"api.test": false}},
		{name: "policy without credentials", r: policyRouter},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ep, addr := openEndpoint(t, newRouterBoundary(t, tc.r), "")
			if got := len(ep.CA()) > 0; got != tc.wantCA {
				t.Fatalf("Endpoint.CA() set = %v, want %v", got, tc.wantCA)
			}
			if tc.wantCA {
				block, _ := pem.Decode(ep.CA())
				cert, err := x509.ParseCertificate(block.Bytes)
				if err != nil {
					t.Fatalf("parsing Endpoint.CA() error = %v", err)
				}
				if diff := cmp.Diff(tc.wantNames, cert.PermittedDNSDomains); diff != "" {
					t.Fatalf("Endpoint.CA() names wrong result (-want +got) diff = %s", diff)
				}
			}
			c := client(t, addr, ep.CA())
			for host, want := range tc.wantTLS {
				status, _, err := get(t, c, "https://"+host+"/", nil)
				if got := err == nil && status == http.StatusOK; got != want {
					t.Fatalf("GET https://%s/ = %d, %v, want success %v", host, status, err, want)
				}
				if status, _, err := get(t, c, "http://"+host+"/", nil); err != nil || status != http.StatusOK {
					t.Fatalf("GET http://%s/ = %d, %v, want 200", host, status, err)
				}
			}
		})
	}
}

func TestPolicyRouterDecidesAsPolicy(t *testing.T) {
	t.Parallel()
	allowed := upstream(t)
	policy := egress.Policy{Mode: egress.ModeAllowlist, Allow: []string{"127.0.0.1"}}
	r, err := policy.Router()
	if err != nil {
		t.Fatalf("Policy.Router() error = %v", err)
	}
	byPolicy := newBoundary(t, func() egress.Policy { return policy }, nil)
	byRouter := newRouterBoundary(t, r)
	for _, b := range []*egress.Boundary{byPolicy, byRouter} {
		_, addr := openEndpoint(t, b, "")
		for _, target := range []string{allowed, "denied.test:443", "127.0.0.1:1"} {
			connect(t, addr, target)
		}
	}
	want := byPolicy.Status()
	want.Mode = ""
	if diff := cmp.Diff(want, byRouter.Status()); diff != "" {
		t.Fatalf("Boundary.Status() of the policy's router wrong result (-want +got) diff = %s", diff)
	}
}

func TestPolicyRouterAddsCredentials(t *testing.T) {
	t.Parallel()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.WriteString(w, r.Header.Get("Authorization")); err != nil {
			t.Errorf("writing the answer error = %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	host := net.JoinHostPort("example.com", port)
	r, err := egress.Policy{Mode: egress.ModeAllowlist, Allow: []string{"127.0.0.1"}}.Router(egress.Header{Host: host, Name: "Authorization", Value: "Bearer secret"})
	if err != nil {
		t.Fatalf("Policy.Router() error = %v", err)
	}
	if got, want := r.(egress.InterceptHosts).Hosts(), []string{"example.com"}; !cmp.Equal(got, want) {
		t.Fatalf("Policy.Router().Hosts() = %q, want %q", got, want)
	}
	b, err := egress.NewWithHosts(egress.Config{Router: r, Log: slog.New(slog.DiscardHandler)}, roots)
	if err != nil {
		t.Fatalf("egress.New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := b.Close(); err != nil {
			t.Errorf("Boundary.Close() error = %v", err)
		}
	})
	ep, addr := openEndpoint(t, b, "")
	status, body, err := get(t, client(t, addr, ep.CA()), "https://"+host+"/", http.Header{"Authorization": {"Bearer placeholder"}})
	if err != nil || status != http.StatusOK || body != "Bearer secret" {
		t.Fatalf("GET https://%s/ = %d %q, %v, want 200 %q", host, status, body, err, "Bearer secret")
	}
}

func TestRouterWithCredentials(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		route      egress.Route
		wantStatus int
	}{
		{name: "refuse stays", route: egress.Refuse(http.StatusForbidden, "no"), wantStatus: http.StatusForbidden},
		{name: "serve can't carry the credential", route: egress.Serve(func(net.Conn) {}), wantStatus: http.StatusBadGateway},
		{name: "dial can't carry the credential", route: egress.Dial(func(context.Context) (net.Conn, error) { return nil, errors.New("unused") }), wantStatus: http.StatusBadGateway},
		{name: "direct is intercepted", route: egress.Direct(), wantStatus: http.StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := newRouterBoundary(t, egress.RouterFunc(func(context.Context, egress.Flow) (egress.Route, error) { return tc.route, nil }))
			_, addr := openEndpoint(t, b, "", egress.Header{Host: "api.test", Name: "Authorization", Value: "Bearer secret"})
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			status, _, c, err := tunnel(ctx, addr, "api.test:443")
			if err != nil {
				t.Fatalf("CONNECT api.test:443 error = %v", err)
			}
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			if status != tc.wantStatus {
				t.Fatalf("CONNECT api.test:443 status = %d, want %d", status, tc.wantStatus)
			}
		})
	}
}

func TestNewTakesPolicyOrRouter(t *testing.T) {
	t.Parallel()
	r := egress.RouterFunc(func(context.Context, egress.Flow) (egress.Route, error) { return egress.Direct(), nil })
	tests := []struct {
		name    string
		cfg     egress.Config
		wantErr bool
	}{
		{name: "policy", cfg: egress.Config{Policy: func() egress.Policy { return egress.Policy{} }}},
		{name: "router", cfg: egress.Config{Router: r}},
		{name: "neither", cfg: egress.Config{}, wantErr: true},
		{name: "both", cfg: egress.Config{Policy: func() egress.Policy { return egress.Policy{} }, Router: r}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b, err := egress.New(tc.cfg)
			if (err != nil) != tc.wantErr {
				t.Fatalf("egress.New() error = %v, want error %v", err, tc.wantErr)
			}
			if err == nil {
				if err := b.Close(); err != nil {
					t.Fatalf("Boundary.Close() error = %v", err)
				}
			}
		})
	}
}
