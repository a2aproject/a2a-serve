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

// Package conformance tests that the endpoints of an a2a-run host, in any
// language, speak the protocols a2a-run expects. Run it from a checkout of
// a2a-serve with
//
//	go test ./internal/a2arun/conformance -egress-proxy <url> -intercept api.anthropic.com:443 -refuse blocked.example:443
//
// Without -egress-proxy it tests the reference host.
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
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-serve/core/egress"
)

// Config says which host endpoints to test.
type Config struct {
	// EgressProxy is the URL of the egress proxy: http://host:port or
	// unix:///path.
	EgressProxy string
	// Intercept is a host:port that the proxy intercepts when a2a-run
	// offers it; "" skips the check.
	Intercept string
	// Refuse is a host:port that the proxy refuses; "" skips the check.
	Refuse string
	// Forward is set when the proxy forwards plain HTTP requests to
	// 127.0.0.1, where the suite serves an upstream.
	Forward bool
}

// timeout bounds each step; a2a-run waits less for a decision.
const timeout = 10 * time.Second

// contextID is the context the suite sends.
const contextID = "conformance-context"

// Run runs the suite.
func Run(t *testing.T, cfg Config) {
	t.Helper()
	network, addr, err := proxyAddr(cfg.EgressProxy)
	if err != nil {
		t.Fatal(err)
	}
	p := proxy{network: network, addr: addr}
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

type proxy struct{ network, addr string }

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
	c, br := p.dial(t)
	req := &http.Request{Method: http.MethodConnect, Host: target, Header: http.Header{egress.ProxyHeaderContext: {contextID}}}
	req.URL = &neturl.URL{Opaque: target}
	if offered {
		req.Header.Set(egress.ProxyHeaderIntercept, egress.ProxyInterceptOffered)
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
	upstream := serveUpstream(t, next)
	c, br := p.dial(t)
	u := "http://" + upstream + "/events"
	if _, err := fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: %s\r\n%s: %s\r\n%s: conformance-task\r\n\r\n", u, upstream, egress.ProxyHeaderContext, contextID, egress.ProxyHeaderTask); err != nil {
		t.Fatalf("sending GET %s error = %v", u, err)
	}
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatalf("reading the answer to GET %s error = %v", u, err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s through the proxy status = %d, want 200", u, resp.StatusCode)
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

func testMalformed(t *testing.T, p proxy) {
	c, br := p.dial(t)
	if _, err := io.WriteString(c, "CONNECT no-port HTTP/1.1\r\nHost: no-port\r\n\r\n"); err != nil {
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
// the second once next closes, and returns its address.
func serveUpstream(t *testing.T, next <-chan struct{}) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{ReadHeaderTimeout: timeout, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	return ln.Addr().String()
}
