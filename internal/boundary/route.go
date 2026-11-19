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

package boundary

import (
	"cmp"
	"context"
	"net"
	"net/http"

	"github.com/a2aproject/a2a-serve/internal/egressrules"
)

// Router decides each connection of a boundary by its destination: the
// normalized host the sandbox asked for, and the port.
type Router func(ctx context.Context, host string, port uint16) (Route, error)

type routeKind int

const (
	routeNone routeKind = iota
	routeRefuse
	routeDirect
	routeRedirect
	routeDial
	routeServe
	routeHTTP
)

// Route is the decision about one connection. Its zero value refuses the
// connection as a failure of the router.
type Route struct {
	kind   routeKind
	status int
	reason string
	host   string
	port   uint16
	dial   func(context.Context) (net.Conn, error)
	serve  func(net.Conn)
	wrap   func(http.RoundTripper) http.RoundTripper
	// rules check the addresses of the route when ruled is set, and
	// otherwise the internet mode with no rules does. suggest makes a
	// refusal suggest the rule that allows the destination.
	rules   egressrules.Policy
	ruled   bool
	suggest bool
	// secrets name the secrets the route adds, for the log, and tlsOnly
	// refuses an intercepted connection without TLS.
	secrets []string
	tlsOnly bool
	// upgrades forwards requests to switch protocols, which only the
	// boundary's own middleware does; next refuses them under the
	// embedder's.
	upgrades bool
}

// Refuse refuses the connection with status and reason.
func Refuse(status int, reason string) Route {
	return Route{kind: routeRefuse, status: status, reason: reason}
}

// Direct dials the destination after the address checks.
func Direct() Route { return Route{kind: routeDirect} }

// Redirect dials host and port after the address checks.
func Redirect(host string, port uint16) Route {
	return Route{kind: routeRedirect, host: host, port: port}
}

// Dial tunnels the connection to what dial returns, unchecked.
func Dial(dial func(ctx context.Context) (net.Conn, error)) Route {
	if dial == nil {
		return Route{}
	}
	return Route{kind: routeDial, dial: dial}
}

// Serve hands the connection to serve.
func Serve(serve func(net.Conn)) Route {
	if serve == nil {
		return Route{}
	}
	return Route{kind: routeServe, serve: serve}
}

// HTTP intercepts the connection and sends each of its requests through
// wrap. A nil wrap sends them to the destination. Requests to switch
// protocols get 501 from the transport that wrap gets.
func HTTP(wrap func(next http.RoundTripper) http.RoundTripper) Route {
	return Route{kind: routeHTTP, wrap: wrap}
}

// PolicyRouter returns the router that decides by the policy that policy
// returns at each connection. It intercepts the requests to the hosts of
// inject to add their headers, and dials other destinations directly, each
// after the policy's address checks.
func PolicyRouter(policy func() egressrules.Policy, inject []Injection) Router {
	return func(_ context.Context, host string, port uint16) (Route, error) {
		r := Route{kind: routeDirect, rules: policy(), ruled: true, suggest: true}
		for _, inj := range inject {
			if inj.Host == host && inj.Port == port {
				r.kind, r.wrap, r.secrets, r.tlsOnly, r.upgrades = routeHTTP, inj.wrap, inj.Secrets, true, true
				break
			}
		}
		return r, nil
	}
}

// Inject returns r for a connection to the host of inj, which gets its
// header: a Direct route becomes an HTTP route that adds it, an HTTP route
// adds it to the requests its middleware sends to next, and a Refuse route
// stays. Other routes can't carry the header and refuse. rules check the
// addresses of the host.
func (r Route) Inject(inj Injection, rules egressrules.Policy) Route {
	switch r.kind {
	case routeRefuse:
		return r
	case routeDirect:
		r.kind, r.wrap, r.upgrades = routeHTTP, inj.wrap, true
	case routeHTTP:
		if wrap := r.wrap; wrap != nil {
			r.wrap = func(next http.RoundTripper) http.RoundTripper { return wrap(inj.wrap(next)) }
		} else {
			r.wrap = inj.wrap
		}
	default:
		return Refuse(http.StatusBadGateway, "route-without-credential")
	}
	r.rules, r.ruled, r.secrets, r.tlsOnly = rules, true, inj.Secrets, true
	return r
}

// checks returns the rules that check the addresses of r.
func (r Route) checks() egressrules.Policy {
	if r.ruled {
		return r.rules
	}
	return egressrules.Policy{Mode: egressrules.ModeInternet}
}

// refusal returns the refusal of a Refuse route, with a status that
// refuses.
func (r Route) refusal() *refusal {
	status := r.status
	if status < 400 || status > 599 {
		status = http.StatusForbidden
	}
	return refuse(status, cmp.Or(r.reason, "refused"))
}
