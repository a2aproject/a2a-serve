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

// Package refhost is a reference host of a2a-run. Its egress proxy refuses,
// tunnels or intercepts each connection, answers or forwards the requests
// it intercepts, and records what it saw; it refuses loopback, link-local
// and private destinations unless told otherwise. Its tools endpoint serves
// MCP with one tool and the hooks of a2a-run, and its workspace endpoint a
// file tree with the workspace protocol. One listener can serve all three.
// It serves the tests and the conformance suite.
package refhost

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/http/httpguts"

	"github.com/a2aproject/a2a-serve/core/egress"
	"github.com/a2aproject/a2a-serve/internal/boundary"
	"github.com/a2aproject/a2a-serve/internal/egressrules"
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
	// AllowPrivate lets tunnels and forwarded requests reach loopback,
	// link-local and private addresses.
	AllowPrivate bool
	// Tools serves the requests in origin form, such as the MCP endpoint
	// of [NewTools]; nil refuses them.
	Tools http.Handler
	// Workspace serves the requests in origin form to the paths of the
	// workspace protocol, such as fsys.Handler of a tree, in place of
	// Tools; nil leaves them to Tools.
	Workspace http.Handler
	// ModelKeys are the API keys of model providers by provider name, as
	// [ParseModelKey] returns them. The forwarded requests to a provider's
	// host lose the agent's placeholder key, and its inference endpoints
	// get the real key, as with a2a-run --inject-model-key. New ignores
	// unknown providers.
	ModelKeys map[string]string
}

// ParseModelKey parses a provider=NAME argument and reads the key from
// the variable NAME with getenv. Its errors leave out the key.
func ParseModelKey(arg string, getenv func(string) string) (provider, key string, _ error) {
	provider, name, ok := strings.Cut(arg, "=")
	if !ok || provider == "" || name == "" {
		return "", "", fmt.Errorf("invalid model key %q: want provider=NAME, such as gemini=GEMINI_API_KEY", arg)
	}
	p, ok := egressrules.LookupProvider(provider)
	if !ok {
		return "", "", egressrules.UnknownProvider(provider)
	}
	key = getenv(name)
	if key == "" {
		return "", "", fmt.Errorf("model key %s: the variable %s is unset or empty", provider, name)
	}
	if _, scheme := p.Credential(p.KeyEnv); !httpguts.ValidHeaderFieldValue(scheme + key) {
		return "", "", fmt.Errorf("model key %s: the variable %s has a character that a header can't carry", provider, name)
	}
	return provider, key, nil
}

// modelKeys returns the injections of keys by the hosts of their
// providers.
func modelKeys(keys map[string]string) map[string]boundary.Injection {
	out := map[string]boundary.Injection{}
	for name, key := range keys {
		p, ok := egressrules.LookupProvider(name)
		if !ok {
			continue
		}
		header, scheme := p.Credential(p.KeyEnv)
		out[p.Host] = boundary.Injection{
			Host: p.Host, Port: egressrules.HTTPSPort, Header: header, Value: scheme + key,
			Model: true, QueryKey: p.QueryKey, Endpoints: p.Endpoints, Secrets: []string{p.KeyEnv},
		}
	}
	return out
}

// workspacePaths are the paths of the requests of the workspace protocol.
var workspacePaths = []string{"/stat", "/list", "/file", "/mkdir", "/symlink", "/rename", "/changes"}

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
	h := &Host{cfg: cfg}
	keys := modelKeys(cfg.ModelKeys)
	if cfg.Upstream == nil {
		cfg.Upstream = &http.Transport{DialContext: h.dial, ForceAttemptHTTP2: true, DisableCompression: true}
	}
	h.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			for name := range pr.Out.Header {
				if strings.HasPrefix(name, "A2a-Run-") {
					pr.Out.Header.Del(name)
				}
			}
			u := pr.Out.URL
			if inj, ok := keys[u.Hostname()]; ok && u.Scheme == "https" && (u.Port() == "" || u.Port() == "443") {
				inj.Apply(pr.Out)
			}
		},
		Transport:     cfg.Upstream,
		FlushInterval: -1,
		ErrorLog:      log.New(io.Discard, "", 0),
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			if errors.Is(err, errPrivate) {
				http.Error(w, errPrivate.Error(), http.StatusForbidden)
				return
			}
			http.Error(w, "forwarding the request failed", http.StatusBadGateway)
		},
	}
	return h
}

// errPrivate refuses a destination that isn't a public address.
var errPrivate = errors.New("the destination is loopback, link-local or private")

// dial dials addr with Config.Dial, at an address it resolves to. Unless
// Config.AllowPrivate, it refuses a name with any address that isn't
// public, so that the agent can't reach this host or its network.
func (h *Host) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	if h.cfg.AllowPrivate {
		return h.cfg.Dial(ctx, network, addr)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	var ips []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		ips = []netip.Addr{ip}
	} else if ips, err = net.DefaultResolver.LookupNetIP(ctx, "ip", host); err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("%s has no address", host)
	}
	for _, ip := range ips {
		if !egressrules.Public(ip) {
			return nil, errPrivate
		}
	}
	return h.cfg.Dial(ctx, network, net.JoinHostPort(ips[0].Unmap().String(), port))
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
	if !r.URL.IsAbs() && h.cfg.Workspace != nil && slices.Contains(workspacePaths, r.URL.Path) {
		h.cfg.Workspace.ServeHTTP(w, r)
		return
	}
	if !r.URL.IsAbs() && h.cfg.Tools != nil {
		h.cfg.Tools.ServeHTTP(w, r)
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
	upstream, err := h.dial(r.Context(), "tcp", r.Host)
	if errors.Is(err, errPrivate) {
		http.Error(w, errPrivate.Error(), http.StatusForbidden)
		return
	}
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
