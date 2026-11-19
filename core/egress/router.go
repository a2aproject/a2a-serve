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

package egress

import (
	"context"
	"net"
	"net/http"
	"slices"

	"github.com/a2aproject/a2a-serve/internal/boundary"
	"github.com/a2aproject/a2a-serve/internal/egressrules"
)

// Flow is one connection the client opens.
type Flow struct {
	// Sandbox names the client, as [Boundary.Open] got it.
	Sandbox string
	// Key is the key the endpoint was opened with, such as the key of an
	// agent process.
	Key string
	// Host is the name or the literal address the client asked for. Names
	// are lowercase ASCII without a trailing dot.
	Host string
	Port uint16
}

// Router decides each connection of a client.
type Router interface {
	// Route decides the connection f. It runs once per connection, so a
	// changed decision applies to the next one. An error refuses the
	// connection.
	Route(ctx context.Context, f Flow) (Route, error)
}

// RouterFunc is a [Router] of a function.
type RouterFunc func(ctx context.Context, f Flow) (Route, error)

// Route calls f.
func (f RouterFunc) Route(ctx context.Context, fl Flow) (Route, error) { return f(ctx, fl) }

// InterceptHosts is implemented by a router that knows in advance the
// hosts for which it may return [HTTP]. The CA that clients trust then
// signs only those names, and HTTP for another host fails closed when
// the client speaks TLS. A router without it gets a CA that signs any
// name, whose key stays in the memory of this process.
type InterceptHosts interface {
	// Hosts returns the host names the router may intercept with TLS;
	// none makes no CA. Addresses are left out, so TLS to an address
	// fails closed. [Boundary.Open] reads them once.
	Hosts() []string
}

// Route is the decision about one connection. Only this package builds
// one; the zero Route refuses the connection as a failure of the router.
type Route struct{ r boundary.Route }

// Refuse refuses the connection with an HTTP status of 4xx or 5xx, and a
// reason that the client sees in the Proxy-Status header of the answer.
// Another status refuses with 403.
func Refuse(status int, reason string) Route {
	return Route{boundary.Refuse(status, reason)}
}

// Direct dials the destination after the boundary's address checks: it
// refuses loopback, link-local and this host's addresses, and addresses
// that this host doesn't reach through its default route, such as those
// of the LAN or a VPN.
func Direct() Route { return Route{boundary.Direct()} }

// Redirect dials host and port in place of the destination, with the
// checks of [Direct].
func Redirect(host string, port uint16) Route {
	return Route{boundary.Redirect(host, port)}
}

// Dial hands the connection to a dialer of the caller, such as another
// proxy, a tunnel, a Unix socket or a test server. The boundary doesn't
// check what it reaches. ctx ends when the dial must end.
func Dial(dial func(ctx context.Context) (net.Conn, error)) Route {
	return Route{boundary.Dial(dial)}
}

// Serve hands the client's connection to serve, after the boundary
// accepted it. The boundary closes the connection when serve returns, when
// it carried no data for the idle timeout, or when the endpoint closes;
// serve must return once the connection is closed.
func Serve(serve func(net.Conn)) Route { return Route{boundary.Serve(serve)} }

// HTTP terminates the connection's TLS, or reads its plain HTTP, and
// passes each request through wrap, with an https or http URL of the
// destination. next reaches the destination after the checks of
// [Direct], and answers a request to switch protocols, such as to a
// WebSocket, with 501. wrap may answer itself, rewrite the request or the
// answer, or send the request elsewhere. A nil wrap sends each request to
// next.
//
// For TLS the client must trust [Endpoint.CA]. A connection to a host
// that the CA can't sign, as [InterceptHosts] decides, closes after the
// CONNECT succeeds.
func HTTP(wrap func(next http.RoundTripper) http.RoundTripper) Route {
	return Route{boundary.HTTP(wrap)}
}

// Router returns the router that decides by p, as a [Boundary] of p does:
// [Direct] with the address checks of p, and for the hosts of creds
// [HTTP] that adds the credential. Its refusals suggest the Allow entry
// that admits the destination. The hosts of creds are allowed in every
// mode, and are the router's [InterceptHosts].
//
// The client gets the placeholders of creds only from an endpoint that
// [Boundary.Open] gave them.
func (p Policy) Router(creds ...Credential) (Router, error) {
	inject, _, allow, err := injections(creds)
	if err != nil {
		return nil, err
	}
	rules, err := policyRules(p, allow)
	if err != nil {
		return nil, err
	}
	return &policyRouter{rules: func() egressrules.Policy { return rules }, inject: inject}, nil
}

// policyRouter decides by the policy that rules returns at each
// connection, and adds the headers of inject.
type policyRouter struct {
	rules  func() egressrules.Policy
	inject []boundary.Injection
}

func (r *policyRouter) Route(ctx context.Context, f Flow) (Route, error) {
	br, err := boundary.PolicyRouter(r.rules, r.inject)(ctx, f.Host, f.Port)
	return Route{br}, err
}

func (r *policyRouter) Hosts() []string { return injectHosts(r.inject) }

// injectHosts returns the hosts of inject, each once.
func injectHosts(inject []boundary.Injection) []string {
	var hosts []string
	for _, inj := range inject {
		if !slices.Contains(hosts, inj.Host) {
			hosts = append(hosts, inj.Host)
		}
	}
	return hosts
}
