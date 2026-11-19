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

// Package refhost is a reference host of a2a-run: an egress proxy that
// refuses, tunnels or intercepts each connection, answers or forwards the
// requests it intercepts, and records what it saw. It serves the tests and
// the conformance suite.
package refhost

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-serve/core/egress"
)

// Decision is what the proxy does with a connection.
type Decision struct {
	kind   int
	reason string
}

const (
	kindIntercept = iota
	kindTunnel
	kindRefuse
)

// Intercept asks for the connection's requests, when a2a-run offers it;
// otherwise it tunnels.
func Intercept() Decision { return Decision{kind: kindIntercept} }

// Tunnel tunnels the connection to its destination.
func Tunnel() Decision { return Decision{kind: kindTunnel} }

// Refuse refuses the connection with 403 and reason.
func Refuse(reason string) Decision { return Decision{kind: kindRefuse, reason: reason} }

// Connect is a CONNECT request the proxy got.
type Connect struct {
	Context string
	// Target is host:port.
	Target  string
	Offered bool
}

// Request is a request in absolute form the proxy got.
type Request struct {
	Context string
	Task    string
	Method  string
	// URL is the absolute URL, such as https://api.anthropic.com/v1/messages.
	URL string
}

// Config configures a [Host].
type Config struct {
	// Decide decides each CONNECT request; nil intercepts every one.
	Decide func(Connect) Decision
	// Answer answers the requests in absolute form; nil forwards them to
	// their destination through Upstream.
	Answer http.Handler
	// Upstream forwards requests; nil uses a transport of its own.
	Upstream http.RoundTripper
	// Dial dials the destinations of tunnels; nil dials them directly.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// maxRecords bounds what a Host records.
const maxRecords = 1000

// Host is the reference host's egress proxy.
type Host struct {
	cfg   Config
	proxy *httputil.ReverseProxy

	mu       sync.Mutex
	connects []Connect
	requests []Request
	conns    sync.WaitGroup
}

// New returns the host of cfg.
func New(cfg Config) *Host {
	if cfg.Decide == nil {
		cfg.Decide = func(Connect) Decision { return Intercept() }
	}
	if cfg.Dial == nil {
		cfg.Dial = (&net.Dialer{Timeout: 10 * time.Second}).DialContext
	}
	if cfg.Upstream == nil {
		cfg.Upstream = &http.Transport{DialContext: cfg.Dial, ForceAttemptHTTP2: true, DisableCompression: true}
	}
	h := &Host{cfg: cfg}
	h.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			for name := range pr.Out.Header {
				if strings.HasPrefix(name, "A2a-Run-") {
					pr.Out.Header.Del(name)
				}
			}
		},
		Transport:     cfg.Upstream,
		FlushInterval: -1,
		ErrorLog:      log.New(io.Discard, "", 0),
	}
	return h
}

// Connects returns the CONNECT requests the host got.
func (h *Host) Connects() []Connect {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.connects)
}

// Requests returns the requests in absolute form the host got.
func (h *Host) Requests() []Request {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.requests)
}

// Serve serves the proxy on ln until ctx ends, then waits for its
// tunnels to close.
func (h *Host) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, ErrorLog: log.New(io.Discard, "", 0)}
	stop := context.AfterFunc(ctx, func() {
		if err := srv.Close(); err != nil {
			return
		}
	})
	defer stop()
	err := srv.Serve(ln)
	h.conns.Wait()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (h *Host) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		h.connect(w, r)
		return
	}
	if !r.URL.IsAbs() {
		http.Error(w, "this is an egress proxy: send requests in absolute form", http.StatusBadRequest)
		return
	}
	h.record(func() {
		h.requests = append(h.requests, Request{
			Context: r.Header.Get(egress.ProxyHeaderContext), Task: r.Header.Get(egress.ProxyHeaderTask),
			Method: r.Method, URL: r.URL.String(),
		})
	})
	if h.cfg.Answer != nil {
		h.cfg.Answer.ServeHTTP(w, r)
		return
	}
	h.proxy.ServeHTTP(w, r)
}

func (h *Host) connect(w http.ResponseWriter, r *http.Request) {
	c := Connect{
		Context: r.Header.Get(egress.ProxyHeaderContext),
		Target:  r.Host,
		Offered: strings.EqualFold(r.Header.Get(egress.ProxyHeaderIntercept), egress.ProxyInterceptOffered),
	}
	h.record(func() { h.connects = append(h.connects, c) })
	d := h.cfg.Decide(c)
	if d.kind == kindIntercept && !c.Offered {
		d = Tunnel()
	}
	switch d.kind {
	case kindRefuse:
		w.Header().Set("Connection", "close")
		http.Error(w, d.reason, http.StatusForbidden)
	case kindIntercept:
		w.Header().Set(egress.ProxyHeaderIntercept, egress.ProxyInterceptAccept)
		w.Header().Set("Connection", "close")
		w.WriteHeader(http.StatusNoContent)
	default:
		h.tunnel(w, r)
	}
}

func (h *Host) tunnel(w http.ResponseWriter, r *http.Request) {
	upstream, err := h.cfg.Dial(r.Context(), "tcp", r.Host)
	if err != nil {
		http.Error(w, "dialing the destination failed", http.StatusBadGateway)
		return
	}
	c, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		closeQuietly(upstream)
		http.Error(w, "the connection can't tunnel", http.StatusInternalServerError)
		return
	}
	h.conns.Add(1)
	defer h.conns.Done()
	if _, err := io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		closeQuietly(c, upstream)
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := io.Copy(upstream, brw); err != nil {
			closeQuietly(c)
		}
		closeWrite(upstream)
	}()
	if _, err := io.Copy(c, upstream); err != nil {
		closeQuietly(upstream)
	}
	closeWrite(c)
	<-done
	closeQuietly(c, upstream)
}

// record runs add with the records locked, unless they are full.
func (h *Host) record(add func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.connects)+len(h.requests) < maxRecords {
		add()
	}
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		if err := cw.CloseWrite(); err != nil {
			closeQuietly(c)
		}
	}
}

// closeQuietly closes cs, whose errors only mean that a tunnel ended.
func closeQuietly(cs ...net.Conn) {
	for _, c := range cs {
		if err := c.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			log.Printf("refhost: closing a tunnel: %v", err)
		}
	}
}
