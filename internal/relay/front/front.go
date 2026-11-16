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
// with the admin token and the relay's identity provider, refuses the
// backend's routes that the relay doesn't expose, and proxies everything
// else to the backend.
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

	"github.com/a2aproject/a2a-serve/internal/httpserver"
	"github.com/a2aproject/a2a-serve/internal/jsonhttp"
	"github.com/a2aproject/a2a-serve/internal/relay/control"
	"github.com/a2aproject/a2a-serve/internal/relay/signin"
)

// Routes describes the routes of the backend behind a front.
type Routes interface {
	// Blocked reports whether the front refuses the route p for method.
	Blocked(p, method string) bool
	// Enrollment reports whether p is a route where agents join the relay
	// or sign in to it.
	Enrollment(p string) bool
	// Ingress splits the part of a path after /ingress/ into the agent it
	// calls, in a canonical form, and that agent's own route. Either is ""
	// when the path names none.
	Ingress(rest string) (agent, route string)
}

// InfoPath is where a relay reports what it runs.
const InfoPath = "/.well-known/a2a-serve-relay"

// maxAgentSegment bounds the agent segment of an ingress path that the
// front passes on. No agent's segment is longer.
const maxAgentSegment = 512

// Front is the handler of a relay's public port.
type Front struct {
	info    any
	routes  Routes
	control http.Handler
	signIn  http.Handler
	proxy   atomic.Pointer[httputil.ReverseProxy]
	proxied *httpserver.KeyedLimiter
	// onboarding limits the requests to the routes of agents that serve
	// callers before they authenticate.
	onboarding *httpserver.KeyedLimiter
	// enrollments and ingress limit how often each client may call the
	// routes that the backend limits for all the relay's callers together,
	// since they all reach it from the relay's address. ingressClient
	// limits each client across agents, so that one client can't add keys
	// to ingress faster than it forgets them.
	enrollments, ingress, ingressClient *httpserver.KeyedRate
	// transport is shared by the proxies of each Open, so a reopened front
	// reuses its connections.
	transport *http.Transport
}

// maxProxiedPerClient is how many requests to the backend one client may
// have open, WebSockets of agents included.
const maxProxiedPerClient = 64

// maxOnboardingPerClient is how many requests one client may have open to
// the routes where agents serve callers before they authenticate, such as
// pairing and sign-in. An agent can't tell apart the callers the relay
// forwards, so the relay limits each of them well below the agent's limit
// for one client.
const maxOnboardingPerClient = 8

const (
	// Agent Mesh admits 10 enrollments a second, with bursts of 20, for the
	// whole relay. An agent polls its pending enrollment every 2 seconds, so
	// one client gets a tenth of that and agents that join at once from one
	// machine still fit.
	enrollmentRate  = 1
	enrollmentBurst = 10
	// Agent Mesh admits 200 anonymous requests a second to each agent's
	// public URLs from one address, and the front's address is every
	// caller's.
	ingressRate  = 20
	ingressBurst = 100
	// One client may call three agents at their full rate. Keys of ingress
	// last up to twice their refill time, so one client holds at most about
	// 900 of them.
	ingressClientRate  = 60
	ingressClientBurst = 300
	// maxRateClients bounds the clients that the rate limits track at once.
	maxRateClients = 10000
)

// New returns the front of a relay that reports info, proxies the backend
// that routes describes, and serves control, which checks the admin token
// itself, at [control.RemotePath], and signIn at [signin.Path], or nil when
// agents can't sign in. It answers 503 on proxied routes until
// [Front.Open].
func New(info any, routes Routes, ctl, signIn http.Handler) *Front {
	// The control API and the identity provider serve callers before they
	// authenticate, so they share a pool of their own.
	unauthenticated := httpserver.NewKeyedLimiter(httpserver.MaxUnauthenticated, httpserver.MaxUnauthenticatedPerClient, client)
	f := &Front{
		info:        info,
		routes:      routes,
		control:     unauthenticated.Handler(http.StripPrefix(control.RemotePath, ctl)),
		proxied:     httpserver.NewKeyedLimiter(httpserver.MaxConcurrent, maxProxiedPerClient, client),
		onboarding:  httpserver.NewKeyedLimiter(httpserver.MaxConcurrent, maxOnboardingPerClient, client),
		enrollments: httpserver.NewKeyedRate(enrollmentRate, enrollmentBurst, maxRateClients, client),
		transport:   upstreamTransport(),
	}
	f.ingress = httpserver.NewKeyedRate(ingressRate, ingressBurst, maxRateClients, f.clientOfAgent)
	f.ingressClient = httpserver.NewKeyedRate(ingressClientRate, ingressClientBurst, maxRateClients, client)
	if signIn != nil {
		f.signIn = unauthenticated.Handler(http.StripPrefix(signin.Path, signIn))
	}
	return f
}

// maxIdleUpstream is how many idle connections to the backend the front
// keeps, so that a burst of requests reuses them.
const maxIdleUpstream = 64

func upstreamTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConnsPerHost = maxIdleUpstream
	return t
}

// Open starts proxying to the backend at upstream.
func (f *Front) Open(upstream *url.URL) {
	p := &httputil.ReverseProxy{
		Transport: f.transport,
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(upstream)
			// The backend gets the path the front checked, so an escaped dot
			// segment can't lead it to a blocked route.
			r.Out.URL.Path, r.Out.URL.RawPath = cleanPath(r.In.URL.Path), ""
			r.Out.Host = r.In.Host
			r.SetXForwarded()
		},
		// Ingress streams A2A responses over SSE.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if !errors.Is(err, r.Context().Err()) {
				slog.Warn("proxying to the relay's backend", "path", r.URL.Path, "error", err)
			}
			jsonhttp.Error(w, http.StatusBadGateway, "the relay's backend didn't answer")
		},
	}
	f.proxy.Store(p)
}

// Close answers 503 on proxied routes again, while the backend stops.
func (f *Front) Close() {
	f.proxy.Store(nil)
	f.transport.CloseIdleConnections()
}

// methods are the methods the front passes on. Go's mux doesn't clean the
// path of a CONNECT request, and backends serve none of the others.
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
	case p == InfoPath:
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
	case f.routes.Blocked(p, r.Method):
		jsonhttp.Error(w, http.StatusNotFound, "not found")
		return
	}
	proxy := f.proxy.Load()
	if proxy == nil {
		w.Header().Set("Retry-After", "5")
		jsonhttp.Error(w, http.StatusServiceUnavailable, "the relay is starting")
		return
	}
	h := f.proxied.Handler(proxy)
	if f.isOnboarding(p) {
		h = f.onboarding.Handler(h)
	}
	switch {
	case f.routes.Enrollment(p):
		h = f.enrollments.Handler(h)
	case under(p, "/ingress"):
		h = f.ingressClient.Handler(f.ingress.Handler(refuseLongAgent(h)))
	}
	h.ServeHTTP(w, r)
}

// refuseLongAgent answers 400 for an ingress path whose agent segment no
// agent has. Agent Mesh's decoder of peer IDs takes time quadratic in its
// length.
func refuseLongAgent(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest, _ := strings.CutPrefix(path.Clean("/"+r.URL.Path), "/ingress/")
		if seg, _, _ := strings.Cut(rest, "/"); len(seg) > maxAgentSegment {
			jsonhttp.Error(w, http.StatusBadRequest, "no agent has this peer ID")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isOnboarding reports whether p reaches, through ingress, a route where an
// a2a-serve agent serves callers before they authenticate: pairing,
// sign-in, approval links and downloads.
func (f *Front) isOnboarding(p string) bool {
	rest, ok := strings.CutPrefix(p, "/ingress/")
	if !ok {
		return false
	}
	_, route := f.routes.Ingress(rest)
	if route == "" {
		return false
	}
	route = "/" + route
	for _, prefix := range []string{"/pair", "/oauth", "/hitl", "/download"} {
		if under(route, prefix) {
			return true
		}
	}
	return false
}

// Client identifies the client of a request for the limits of the handlers
// the front serves, as the front's own limits do.
func Client(r *http.Request) string {
	return client(r)
}

// clientOfAgent identifies the client of a request together with the agent
// whose public URL it calls, as Agent Mesh does for its ingress limit. The
// requests of a client to paths that name no agent share one key.
func (f *Front) clientOfAgent(r *http.Request) string {
	rest, _ := strings.CutPrefix(path.Clean("/"+r.URL.Path), "/ingress/")
	agent, _ := f.routes.Ingress(rest)
	if agent == "" {
		return client(r)
	}
	return client(r) + " " + agent
}

// client identifies the client of a request for its limits. A tunnel or a
// reverse proxy, such as cloudflared or Cloud Run's, connects from the
// relay's machine or a private network and appends the address it got the
// request from to X-Forwarded-For. The client is the last address there
// that isn't such a proxy. A header that reaches the relay from a public
// address may be forged, so the client is the remote address then. IPv6
// clients are told apart by their /64.
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
	ip = ip.Unmap()
	if ip.Is6() {
		// One subscriber usually holds a whole /64, so its addresses are one
		// client.
		return netip.PrefixFrom(ip, 64).Masked().String()
	}
	return ip.String()
}

// isProxy reports whether a connection from ip may come from a tunnel or a
// reverse proxy of the relay.
func isProxy(ip netip.Addr) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

func under(p, prefix string) bool {
	return p == prefix || strings.HasPrefix(p, prefix+"/")
}
