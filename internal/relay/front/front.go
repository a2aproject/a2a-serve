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

// Package front is the HTTP front of a relay. It serves the relay's public
// port: it reports the relay's versions, serves the control API to callers
// with the admin token and the relay's identity provider, blocks Agent
// Mesh's admin, console and metrics routes, and proxies everything else to
// agentmesh-one.
package front

import (
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"path"
	"strings"
	"sync/atomic"

	"github.com/a2aproject/a2a-serve/internal/agentmesh"
	"github.com/a2aproject/a2a-serve/internal/httpserver"
	"github.com/a2aproject/a2a-serve/internal/jsonhttp"
	"github.com/a2aproject/a2a-serve/internal/relay/control"
	"github.com/a2aproject/a2a-serve/internal/relay/signin"
)

// Front is the handler of a relay's public port.
type Front struct {
	info    agentmesh.Info
	control http.Handler
	signIn  http.Handler
	proxy   atomic.Pointer[httputil.ReverseProxy]
	proxied *httpserver.KeyedLimiter
	// transport is shared by the proxies of each Open, so a reopened front
	// reuses its connections.
	transport *http.Transport
}

// maxProxiedPerClient is how many requests to Agent Mesh one client may
// have open, WebSockets of agents included.
const maxProxiedPerClient = 64

// New returns the front of a relay that reports info and serves control,
// which checks the admin token itself, at [control.RemotePath], and signIn
// at [signin.Path], or nil when agents can't sign in. It answers 503 on
// proxied routes until [Front.Open].
func New(info agentmesh.Info, ctl, signIn http.Handler) *Front {
	// The control API and the identity provider serve callers before they
	// authenticate, so they share a pool of their own.
	unauthenticated := httpserver.NewKeyedLimiter(httpserver.MaxUnauthenticated, httpserver.MaxUnauthenticatedPerClient, client)
	f := &Front{
		info:      info,
		control:   unauthenticated.Handler(http.StripPrefix(control.RemotePath, ctl)),
		proxied:   httpserver.NewKeyedLimiter(httpserver.MaxConcurrent, maxProxiedPerClient, client),
		transport: upstreamTransport(),
	}
	if signIn != nil {
		f.signIn = unauthenticated.Handler(http.StripPrefix(signin.Path, signIn))
	}
	return f
}

// maxIdleUpstream is how many idle connections to agentmesh-one the front
// keeps, so that a burst of requests reuses them.
const maxIdleUpstream = 64

func upstreamTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConnsPerHost = maxIdleUpstream
	return t
}

// Open starts proxying to the agentmesh-one at upstream.
func (f *Front) Open(upstream *url.URL) {
	p := &httputil.ReverseProxy{
		Transport: f.transport,
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(upstream)
			// Agent Mesh gets the path the front checked, so an escaped dot
			// segment can't lead it to a blocked route.
			r.Out.URL.Path, r.Out.URL.RawPath = cleanPath(r.In.URL.Path), ""
			r.Out.Host = r.In.Host
			r.SetXForwarded()
		},
		// Ingress streams A2A responses over SSE.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if !errors.Is(err, r.Context().Err()) {
				slog.Warn("proxying to Agent Mesh", "path", r.URL.Path, "error", err)
			}
			jsonhttp.Error(w, http.StatusBadGateway, "the relay's Agent Mesh didn't answer")
		},
	}
	f.proxy.Store(p)
}

// Close answers 503 on proxied routes again, while agentmesh-one stops.
func (f *Front) Close() {
	f.proxy.Store(nil)
	f.transport.CloseIdleConnections()
}

// methods are the methods the front passes on. Go's mux doesn't clean the
// path of a CONNECT request, and Agent Mesh serves none of the others.
var methods = map[string]bool{
	http.MethodGet: true, http.MethodHead: true, http.MethodPost: true, http.MethodPut: true,
	http.MethodPatch: true, http.MethodDelete: true, http.MethodOptions: true,
}

// cleanPath returns p without dot segments or repeated slashes, keeping a
// trailing slash.
func cleanPath(p string) string {
	c := path.Clean("/" + p)
	if strings.HasSuffix(p, "/") && c != "/" {
		c += "/"
	}
	return c
}

func (f *Front) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !methods[r.Method] {
		jsonhttp.Error(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	p := path.Clean("/" + r.URL.Path)
	switch {
	case p == agentmesh.InfoPath:
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			jsonhttp.Error(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		jsonhttp.Write(w, http.StatusOK, f.info)
		return
	case under(p, control.RemotePath):
		f.control.ServeHTTP(w, r)
		return
	case under(p, signin.Path):
		if f.signIn == nil {
			jsonhttp.Error(w, http.StatusNotFound, "this relay doesn't let agents sign in; join it with an invite")
			return
		}
		f.signIn.ServeHTTP(w, r)
		return
	case blocked(p, r.Method):
		jsonhttp.Error(w, http.StatusNotFound, "not found")
		return
	}
	proxy := f.proxy.Load()
	if proxy == nil {
		w.Header().Set("Retry-After", "5")
		jsonhttp.Error(w, http.StatusServiceUnavailable, "the relay is starting")
		return
	}
	f.proxied.Handler(proxy).ServeHTTP(w, r)
}

// Client identifies the client of a request for the limits of the handlers
// the front serves, as the front's own limits do.
func Client(r *http.Request) string {
	return client(r)
}

// client identifies the client of a request for its limits. A tunnel or a
// reverse proxy, such as cloudflared or Cloud Run's, connects from the
// relay's machine or a private network and appends the address it got the
// request from to X-Forwarded-For. The client is the last address there
// that isn't such a proxy. A header that reaches the relay from a public
// address may be forged, so the client is the remote address then.
func client(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	forwarded := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(forwarded) - 1; i >= 0 && isProxy(ip); i-- {
		next, err := netip.ParseAddr(strings.TrimSpace(forwarded[i]))
		if err != nil {
			break
		}
		ip = next
	}
	return ip.Unmap().String()
}

// isProxy reports whether a connection from ip may come from a tunnel or a
// reverse proxy of the relay.
func isProxy(ip netip.Addr) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

func under(p, prefix string) bool {
	return p == prefix || strings.HasPrefix(p, prefix+"/")
}

// blocked reports whether the front refuses a route of Agent Mesh. The
// relay is managed through the control API and its policy comes from its
// settings; the console and metrics answer without authentication.
func blocked(p, method string) bool {
	for _, prefix := range []string{"/admin", "/console", "/metrics"} {
		if under(p, prefix) {
			return true
		}
	}
	return p == "/policies" && method != http.MethodGet && method != http.MethodOptions
}
