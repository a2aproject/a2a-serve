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

// Package conformance tests that the endpoints of an a2a-serve-sidecar host, in any
// language, speak the protocols a2a-serve-sidecar expects. Run it from a checkout of
// a2a-serve with
//
//	go test ./sidecar/conformance -egress-proxy <url> -intercept api.anthropic.com:443 -refuse blocked.example:443
//	go test ./sidecar/conformance -tools <url>
//	go test ./sidecar/conformance -workspace <url>
//
// A proxy must refuse this host's loopback; -allow-loopback skips that
// check for one that means to reach it. The tools endpoint is http://,
// https:// or unix:///path, and the suite checks the hooks it advertises.
// The workspace is the file server of a2a-serve-sidecar --workspace, http://,
// https:// or unix:///path; the suite works in a directory of its own in
// the tree and removes it at the end.
//
// Without -egress-proxy, -tools or -workspace it tests the reference host.
//
// A host in Go can call [Run] from a test of its own instead.
package conformance

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	neturl "net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-serve/core/egress"
	"github.com/a2aproject/a2a-serve/internal/sidecar/hosttools"
)

// Config says which host endpoints to test.
type Config struct {
	// EgressProxy is the URL of the egress proxy: http://host:port or
	// unix:///path.
	EgressProxy string
	// Intercept is a host:port that the proxy intercepts when a2a-serve-sidecar
	// offers it; "" skips the check.
	Intercept string
	// Refuse is a host:port that the proxy refuses; "" skips the check.
	Refuse string
	// Forward is set when the proxy forwards plain HTTP requests to
	// 127.0.0.1, where the suite serves an upstream. It needs
	// AllowLoopback.
	Forward bool
	// AllowLoopback is set for a proxy that means to reach this host's
	// loopback, which skips the check that it refuses it.
	AllowLoopback bool
	// Tools is the URL of the tools endpoint; "" skips its tests.
	Tools string
	// Workspace is the URL of the file server of the workspace; "" skips
	// its tests.
	Workspace string
	// Token, when non-empty, is sent in A2A-Sidecar-Token on every request
	// and checked to be required by each endpoint.
	Token string
}

// timeout bounds each step; a2a-serve-sidecar waits less for a decision.
const timeout = 10 * time.Second

// contextID is the context the suite sends.
const contextID = "conformance-context"

// Run runs the suite: the tests of each endpoint that cfg names.
func Run(t *testing.T, cfg Config) {
	t.Helper()
	t.Run("egress proxy", func(t *testing.T) {
		if cfg.EgressProxy == "" {
			t.Skip("no egress proxy")
		}
		testEgressProxy(t, cfg)
	})
	t.Run("tools", func(t *testing.T) {
		if cfg.Tools == "" {
			t.Skip("no tools endpoint")
		}
		ep, err := hosttools.Parse(cfg.Tools)
		if err != nil {
			t.Fatalf("invalid tools URL: %v", err)
		}
		testTools(t, ep, cfg.Token)
	})
	t.Run("workspace", func(t *testing.T) {
		if cfg.Workspace == "" {
			t.Skip("no workspace")
		}
		testWorkspace(t, cfg.Workspace, cfg.Token)
	})
}

func testEgressProxy(t *testing.T, cfg Config) {
	network, addr, err := proxyAddr(cfg.EgressProxy)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Forward && !cfg.AllowLoopback {
		t.Fatal("Forward needs AllowLoopback: the suite's upstream listens on 127.0.0.1")
	}
	p := proxy{network: network, addr: addr, token: cfg.Token}
	t.Run("egress proxy refuses missing or wrong token", func(t *testing.T) {
		if cfg.Token == "" {
			t.Skip("no token configured")
		}
		testProxyAuth(t, p)
	})
	t.Run("egress proxy refuses", func(t *testing.T) {
		if cfg.Refuse == "" {
			t.Skip("no host to refuse")
		}
		testRefuse(t, p, cfg.Refuse)
	})
	t.Run("egress proxy intercepts only when offered", func(t *testing.T) {
		if cfg.Intercept == "" {
			t.Skip("no host to intercept")
		}
		testIntercept(t, p, cfg.Intercept)
	})
	t.Run("egress proxy refuses loopback", func(t *testing.T) {
		if cfg.AllowLoopback {
			t.Skip("the proxy means to reach loopback")
		}
		testLoopback(t, p)
	})
	t.Run("egress proxy forwards and streams", func(t *testing.T) {
		if !cfg.Forward {
			t.Skip("the proxy doesn't forward to 127.0.0.1")
		}
		testForward(t, p)
	})
	t.Run("egress proxy answers a malformed CONNECT", func(t *testing.T) { testMalformed(t, p) })
}

// proxyAddr returns the network and the address of the proxy URL u.
func proxyAddr(u string) (string, string, error) {
	if path, ok := strings.CutPrefix(u, "unix://"); ok && strings.HasPrefix(path, "/") {
		return "unix", path, nil
	}
	if host, ok := strings.CutPrefix(u, "http://"); ok && host != "" {
		return "tcp", strings.TrimSuffix(host, "/"), nil
	}
	return "", "", fmt.Errorf("invalid egress proxy URL %q: want http://host:port or unix:///path", u)
}

type proxy struct{ network, addr, token string }

func (p proxy) tokenHeader() string {
	if p.token == "" {
		return ""
	}
	return egress.ProxyHeaderToken + ": " + p.token + "\r\n"
}

func (p proxy) dial(t *testing.T) (net.Conn, *bufio.Reader) {
	t.Helper()
	c, err := net.DialTimeout(p.network, p.addr, timeout)
	if err != nil {
		t.Fatalf("dialing the egress proxy error = %v", err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("closing the connection to the egress proxy error = %v", err)
		}
	})
	if err := c.SetDeadline(time.Now().Add(timeout)); err != nil {
		t.Fatal(err)
	}
	return c, bufio.NewReader(c)
}

// connect sends CONNECT target, with an offer to intercept when offered,
// and returns the answer.
func (p proxy) connect(t *testing.T, target string, offered bool) *http.Response {
	t.Helper()
	return p.connectWithToken(t, target, offered, p.token)
}

func (p proxy) connectWithToken(t *testing.T, target string, offered bool, token string) *http.Response {
	t.Helper()
	c, br := p.dial(t)
	req := &http.Request{Method: http.MethodConnect, Host: target, Header: http.Header{egress.ProxyHeaderContext: {contextID}}}
	req.URL = &neturl.URL{Opaque: target}
	if offered {
		req.Header.Set(egress.ProxyHeaderIntercept, egress.ProxyInterceptOffered)
	}
	if token != "" {
		req.Header.Set(egress.ProxyHeaderToken, token)
	}
	if err := req.Write(c); err != nil {
		t.Fatalf("sending CONNECT %s error = %v", target, err)
	}
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		t.Fatalf("reading the answer to CONNECT %s error = %v, want an answer within %v", target, err, timeout)
	}
	return resp
}

func testProxyAuth(t *testing.T, p proxy) {
	for _, tc := range []struct {
		name  string
		token string
	}{
		{name: "missing", token: ""},
		{name: "wrong", token: p.token + "-wrong"},
	} {
		resp := p.connectWithToken(t, "example.com:443", true, tc.token)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("CONNECT with %s token status = %d, want 401", tc.name, resp.StatusCode)
		}
	}
}

func testRefuse(t *testing.T, p proxy, target string) {
	resp := p.connect(t, target, true)
	if resp.StatusCode < 400 || resp.StatusCode > 599 {
		t.Fatalf("CONNECT %s status = %d, want 4xx or 5xx", target, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if err != nil {
		t.Fatalf("reading the refusal of %s error = %v", target, err)
	}
	t.Logf("CONNECT %s refused with %d: %q", target, resp.StatusCode, body)
}

func testIntercept(t *testing.T, p proxy, target string) {
	resp := p.connect(t, target, true)
	if resp.StatusCode != http.StatusNoContent || !strings.EqualFold(resp.Header.Get(egress.ProxyHeaderIntercept), egress.ProxyInterceptAccept) {
		t.Fatalf("CONNECT %s with %s: %s = %d with %s %q, want 204 with %q", target, egress.ProxyHeaderIntercept, egress.ProxyInterceptOffered, resp.StatusCode, egress.ProxyHeaderIntercept, resp.Header.Get(egress.ProxyHeaderIntercept), egress.ProxyInterceptAccept)
	}
	resp = p.connect(t, target, false)
	if resp.StatusCode == http.StatusNoContent && resp.Header.Get(egress.ProxyHeaderIntercept) != "" {
		t.Fatalf("CONNECT %s without an offer = 204 with %s, want a tunnel or a refusal", target, egress.ProxyHeaderIntercept)
	}
}

func testForward(t *testing.T, p proxy) {
	next := make(chan struct{})
	upstream, tokens := serveUpstream(t, next)
	c, br := p.dial(t)
	u := "http://" + upstream + "/events"
	if _, err := fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: %s\r\n%s: %s\r\n%s: conformance-task\r\n%s\r\n", u, upstream, egress.ProxyHeaderContext, contextID, egress.ProxyHeaderTask, p.tokenHeader()); err != nil {
		t.Fatalf("sending GET %s error = %v", u, err)
	}
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatalf("reading the answer to GET %s error = %v", u, err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s through the proxy status = %d, want 200", u, resp.StatusCode)
	}
	if <-tokens {
		t.Fatalf("GET %s reached the upstream with %s, want the proxy to strip it", u, egress.ProxyHeaderToken)
	}
	body := bufio.NewReader(resp.Body)
	first, err := body.ReadString('\n')
	if err != nil || first != "data: one\n" {
		t.Fatalf("first event of GET %s = %q, %v, want it before the upstream sends the second: the proxy must stream answers", u, first, err)
	}
	close(next)
	rest, err := io.ReadAll(body)
	if err != nil || string(rest) != "\ndata: two\n\n" {
		t.Fatalf("rest of GET %s = %q, %v, want the second event", u, rest, err)
	}
}

// testLoopback checks that neither a tunnel nor a forwarded request
// reaches a listener on this host's loopback, by address or by a name.
func testLoopback(t *testing.T, p proxy) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	reached := make(chan struct{}, 1)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			select {
			case reached <- struct{}{}:
			default:
			}
			if err := c.Close(); err != nil {
				t.Logf("closing a loopback connection: %v", err)
			}
		}
	}()
	t.Cleanup(func() {
		if err := ln.Close(); err != nil {
			t.Errorf("closing the loopback listener error = %v", err)
		}
	})
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	for _, target := range []string{net.JoinHostPort("127.0.0.1", port), net.JoinHostPort("localhost", port)} {
		if resp := p.connect(t, target, false); resp.StatusCode < 400 {
			t.Fatalf("CONNECT %s = %d, want 4xx or 5xx: the proxy must refuse loopback destinations it doesn't mean to expose", target, resp.StatusCode)
		}
		c, br := p.dial(t)
		u := "http://" + target + "/"
		if _, err := fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: %s\r\n%s: %s\r\n%s\r\n", u, target, egress.ProxyHeaderContext, contextID, p.tokenHeader()); err != nil {
			t.Fatalf("sending GET %s error = %v", u, err)
		}
		resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
		if err != nil {
			t.Fatalf("reading the answer to GET %s error = %v", u, err)
		}
		if resp.StatusCode < 400 {
			t.Fatalf("GET %s through the proxy = %d, want 4xx or 5xx", u, resp.StatusCode)
		}
	}
	select {
	case <-reached:
		t.Fatal("a connection through the proxy reached this host's loopback")
	default:
	}
}

func testMalformed(t *testing.T, p proxy) {
	c, br := p.dial(t)
	if _, err := io.WriteString(c, "CONNECT no-port HTTP/1.1\r\nHost: no-port\r\n"+p.tokenHeader()+"\r\n"); err != nil {
		t.Fatalf("sending a malformed CONNECT error = %v", err)
	}
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("reading the answer to a malformed CONNECT error = %v, want an answer within %v", err, timeout)
	}
	if resp.StatusCode < 400 {
		t.Fatalf("CONNECT no-port status = %d, want 4xx or 5xx", resp.StatusCode)
	}
}

// serveUpstream serves /events on 127.0.0.1, which streams two events,
// the second once next closes. It returns its address, and a channel that
// reports for each request whether it carried A2A-Sidecar-Token.
func serveUpstream(t *testing.T, next <-chan struct{}) (string, <-chan bool) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tokens := make(chan bool, 1)
	srv := &http.Server{ReadHeaderTimeout: timeout, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case tokens <- r.Header.Get(egress.ProxyHeaderToken) != "":
		default:
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if _, err := io.WriteString(w, "data: one\n\n"); err != nil {
			return
		}
		if err := http.NewResponseController(w).Flush(); err != nil {
			return
		}
		select {
		case <-next:
		case <-r.Context().Done():
			return
		case <-time.After(timeout):
			return
		}
		if _, err := io.WriteString(w, "data: two\n\n"); err != nil {
			return
		}
	})}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("serving the upstream error = %v", err)
		}
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Errorf("stopping the upstream error = %v", err)
		}
	})
	return ln.Addr().String(), tokens
}
