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

// Package egress decides the connections of a client and adds
// credentials to its requests for chosen hosts. The client reaches it as
// an HTTP CONNECT proxy, and sees placeholders in place of the
// credentials. A [Policy] decides by rules; a [Router] decides each
// connection in code, and a [ProxyRouter] asks an HTTP proxy.
package egress

import (
	"cmp"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"sync"

	"golang.org/x/net/http/httpguts"

	"github.com/a2aproject/a2a-serve/internal/boundary"
	"github.com/a2aproject/a2a-serve/internal/egressrules"
)

func init() {
	boundary.RegisterEndpoint(func(v any) (boundary.Endpoint, bool) {
		e, ok := v.(*Endpoint)
		if !ok || e == nil {
			return boundary.Endpoint{}, false
		}
		return boundary.Endpoint{
			Open:        e.newBoundary,
			CA:          e.CA(),
			Mode:        func() string { return string(e.b.mode()) },
			Credentials: len(e.inject) > 0,
			Router:      e.b.router != nil,
		}, true
	})
	boundary.RegisterCredential(func(inj boundary.Injection) any { return injection(inj) })
}

// Mode is what a policy lets the client reach.
type Mode string

const (
	// ModeInternet allows the public internet and the destinations of
	// Allow. It is the mode of an empty Mode.
	ModeInternet Mode = "internet"
	// ModeAllowlist allows only the destinations of Allow.
	ModeAllowlist Mode = "allowlist"
	// ModeUnrestricted gives a sandbox the runtime's own network, without
	// the boundary. A client that an Endpoint serves directly gets
	// ModeInternet.
	ModeUnrestricted Mode = "unrestricted"
)

// Policy decides where the client may connect. Names that resolve to this
// host, its link or private networks stay out of reach unless Allow names
// them.
type Policy struct {
	Mode Mode
	// Allow are the destinations the policy admits in every mode: a host
	// such as nas.lan, *.example.com for the names under example.com, an
	// IP address or a CIDR range, each with an optional :port.
	Allow []string
}

// Validate reports an unknown mode or a rule that doesn't parse.
func (p Policy) Validate() error {
	_, err := egressrules.NewPolicy(string(p.Mode), p.Allow)
	return err
}

// Config configures a [Boundary]. Exactly one of Policy and Router is
// set.
type Config struct {
	// Policy returns the policy, which decides every connection when it
	// opens, so that a change applies to the next one.
	Policy func() Policy
	// Router decides every connection in place of a policy. A sandbox
	// runtime that can't force the client's connections through the
	// boundary refuses an endpoint of a Router, and one that can never
	// gives the client another network.
	Router Router
	// Log receives one record per decision; nil uses the default logger.
	Log *slog.Logger
}

// Boundary decides connections by policy or by router, and adds
// credentials. One Boundary serves many clients; each gets an [Endpoint]
// with its own credentials. It records the connections it refuses.
type Boundary struct {
	policy func() Policy
	router Router
	log    *slog.Logger
	blocks *boundary.Blocks
	deps   deps

	mu sync.Mutex
	// ca signs the hosts of the endpoints' credentials and of a router's
	// InterceptHosts, and anyCA any host, for a router without them.
	ca        *boundary.CA
	anyCA     *boundary.CA
	endpoints map[*Endpoint]struct{}
	closed    bool
}

// New returns a boundary that decides by cfg.Policy, whose current value
// must be valid, or by cfg.Router.
func New(cfg Config) (*Boundary, error) { return newBoundary(cfg, deps{}) }

// deps are what a boundary uses of this host; their zero values use the
// host's own.
type deps struct {
	// roots verify the hosts that get credentials.
	roots *x509.CertPool
	// resolver resolves the names the client asks for.
	resolver boundary.Resolver
}

func newBoundary(cfg Config, d deps) (*Boundary, error) {
	switch {
	case cfg.Policy == nil && cfg.Router == nil:
		return nil, errors.New("the egress boundary needs a policy or a router")
	case cfg.Policy != nil && cfg.Router != nil:
		return nil, errors.New("the egress boundary takes a policy or a router, not both")
	case cfg.Policy != nil:
		if err := cfg.Policy().Validate(); err != nil {
			return nil, err
		}
	}
	return &Boundary{
		policy:    cfg.Policy,
		router:    cfg.Router,
		log:       cmp.Or(cfg.Log, slog.Default()),
		blocks:    &boundary.Blocks{},
		deps:      d,
		endpoints: map[*Endpoint]struct{}{},
	}, nil
}

// Open returns an endpoint for one client, named name in the log, whose
// connections a router sees with key as [Flow.Key]. Its requests to the
// hosts of creds get them, and those hosts are allowed in every mode.
//
// With a [Router], creds apply on top of its routes for their hosts:
// [Direct] becomes [HTTP] that adds the credential, an HTTP middleware
// sees each request before the credential is added, and [Refuse] refuses.
// Any other route for such a host refuses the connection, since it can't
// carry the credential.
func (b *Boundary) Open(name, key string, creds ...Credential) (*Endpoint, error) {
	inject, env, allow, err := injections(creds)
	if err != nil {
		return nil, err
	}
	e := &Endpoint{b: b, name: name, key: key, inject: inject, env: env, allow: allow, boundaries: map[*boundary.Boundary]struct{}{}, listeners: map[net.Listener]struct{}{}}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, errors.New("the egress boundary is closed")
	}
	if e.ca, err = b.endpointCA(inject); err != nil {
		return nil, err
	}
	main, err := e.newBoundary(nil)
	if err != nil {
		return nil, err
	}
	e.main = main
	b.endpoints[e] = struct{}{}
	return e, nil
}

// injections returns the injections of creds, the placeholders the client
// gets, and the allow entries of their hosts.
func injections(creds []Credential) (inject []boundary.Injection, env, allow []string, err error) {
	for _, c := range creds {
		inj, vars, err := c.injection()
		if err != nil {
			return nil, nil, nil, err
		}
		target := net.JoinHostPort(inj.Host, strconv.Itoa(int(inj.Port)))
		if slices.ContainsFunc(inject, func(o boundary.Injection) bool { return o.Host == inj.Host && o.Port == inj.Port }) {
			return nil, nil, nil, fmt.Errorf("two credentials for %s; give each host one", target)
		}
		inject = append(inject, inj)
		env = append(env, vars...)
		allow = append(allow, target)
	}
	return inject, env, allow, nil
}

// endpointCA returns the CA of an endpoint with inject: none without hosts
// to intercept, a CA that signs any host for a router that doesn't name
// its hosts, and otherwise one constrained to the hosts. It runs with b.mu
// held.
func (b *Boundary) endpointCA(inject []boundary.Injection) (*boundary.CA, error) {
	hosts := injectHosts(inject)
	if b.router != nil {
		if u, ok := b.router.(interface{ unconstrained() bool }); ok && u.unconstrained() {
			return b.unconstrainedCA()
		}
		ih, ok := b.router.(InterceptHosts)
		if !ok {
			return b.unconstrainedCA()
		}
		for _, h := range ih.Hosts() {
			if _, err := netip.ParseAddr(h); err == nil {
				continue
			}
			if h, err := egressrules.NormalizeHost(h); err == nil && !slices.Contains(hosts, h) {
				hosts = append(hosts, h)
			}
		}
	}
	if len(hosts) == 0 {
		return nil, nil
	}
	return b.authority(hosts)
}

// unconstrainedCA returns the CA that signs any host, making it on the
// first call. It runs with b.mu held.
func (b *Boundary) unconstrainedCA() (*boundary.CA, error) {
	if b.anyCA != nil {
		return b.anyCA, nil
	}
	ca, err := boundary.NewUnconstrainedCA()
	if err != nil {
		return nil, err
	}
	b.anyCA = ca
	return ca, nil
}

// authority returns the CA of hosts, making a new one when the current
// one doesn't cover them all. An endpoint keeps the CA it opened with. It
// runs with b.mu held.
func (b *Boundary) authority(hosts []string) (*boundary.CA, error) {
	if b.ca != nil && !slices.ContainsFunc(hosts, func(h string) bool { return !b.ca.Covers(h) }) {
		return b.ca, nil
	}
	ca, err := boundary.NewCA(hosts)
	if err != nil {
		return nil, err
	}
	b.ca = ca
	return ca, nil
}

// rules returns the current policy with extra allowed. A policy that
// doesn't parse allows nothing until it is fixed.
func (b *Boundary) rules(extra []string) egressrules.Policy {
	r, err := policyRules(b.policy(), extra)
	if err != nil {
		b.log.Error("reading the egress rules; the client reaches nothing until they are fixed", "error", err)
		return egressrules.Policy{Mode: egressrules.ModeAllowlist}
	}
	return r
}

// policyRules returns the rules of p with extra allowed. A boundary
// decides by the internet mode where p gives the runtime's own network.
func policyRules(p Policy, extra []string) (egressrules.Policy, error) {
	mode := p.Mode
	if mode == ModeUnrestricted {
		mode = ModeInternet
	}
	return egressrules.NewPolicy(string(mode), append(slices.Clone(p.Allow), extra...))
}

// mode returns the mode of the current policy, or "" for a router.
func (b *Boundary) mode() Mode {
	if b.router != nil {
		return ""
	}
	return cmp.Or(b.policy().Mode, ModeInternet)
}

// Status returns the current mode and the connections the boundary
// refused.
func (b *Boundary) Status() Status {
	n, recent := b.blocks.Snapshot()
	st := Status{Mode: b.mode(), Refused: n}
	for _, blk := range recent {
		st.Recent = append(st.Recent, Refusal{Destination: blk.Destination, Rule: blk.Allow, Unresolved: blk.Unresolved})
	}
	return st
}

// Close closes the endpoints and their connections.
func (b *Boundary) Close() error {
	b.mu.Lock()
	b.closed = true
	endpoints := make([]*Endpoint, 0, len(b.endpoints))
	for e := range b.endpoints {
		endpoints = append(endpoints, e)
	}
	b.mu.Unlock()
	var errs []error
	for _, e := range endpoints {
		errs = append(errs, e.Close())
	}
	return errors.Join(errs...)
}

// Status is what a boundary knows of its connections.
type Status struct {
	// Mode is the mode of the policy, or "" for a router.
	Mode Mode
	// Refused counts the connections the boundary refused.
	Refused int
	// Recent are the latest refused destinations, newest first. A
	// destination leaves the list once a connection to it succeeds.
	Recent []Refusal
}

// Refusal is a destination the boundary refused.
type Refusal struct {
	// Destination is the host:port the client asked for.
	Destination string
	// Rule is the Allow entry that admits the destination, or "" when
	// none can, such as for a tailnet address that only its name can, or
	// when a router refused it.
	Rule string
	// Unresolved is set for a name that didn't resolve.
	Unresolved bool
}

// Credential is a [Header] or a [ModelKey].
type Credential interface {
	injection() (boundary.Injection, []string, error)
}

// injection is a credential the frontend of this module made from its own
// rules, which the endpoint adds as it is.
type injection boundary.Injection

func (i injection) injection() (boundary.Injection, []string, error) {
	return boundary.Injection(i), nil, nil
}

// Header is a header added to the HTTPS requests to Host, in place of
// the one the client sent.
type Header struct {
	// Host is a host name, with :port for a port other than 443.
	Host  string
	Name  string
	Value string
}

func (h Header) injection() (boundary.Injection, []string, error) {
	host, port, err := egressrules.ParseInjectTarget(h.Host)
	if err != nil {
		return boundary.Injection{}, nil, fmt.Errorf("invalid header credential: the host %w", err)
	}
	name := http.CanonicalHeaderKey(h.Name)
	switch {
	case !httpguts.ValidHeaderFieldName(h.Name):
		return boundary.Injection{}, nil, fmt.Errorf("invalid header credential for %s: %q isn't a header name", h.Host, h.Name)
	case egressrules.ReservedHeader(name):
		return boundary.Injection{}, nil, fmt.Errorf("invalid header credential for %s: the boundary can't set the header %s", h.Host, name)
	case !httpguts.ValidHeaderFieldValue(h.Value):
		return boundary.Injection{}, nil, fmt.Errorf("invalid header credential for %s: the value of %s has a character that a header can't carry", h.Host, name)
	}
	return boundary.Injection{Host: host, Port: port, Header: name, Value: h.Value, Secrets: []string{name}}, nil, nil
}

// ModelKey is the API key of a model provider: anthropic, gemini, openai
// or openrouter. The boundary sets it only on the provider's inference
// and model list requests, and the client gets a placeholder in the
// provider's conventional variable, such as ANTHROPIC_API_KEY.
type ModelKey struct {
	Provider string
	Key      string
}

func (k ModelKey) injection() (boundary.Injection, []string, error) {
	p, ok := egressrules.LookupProvider(k.Provider)
	if !ok {
		return boundary.Injection{}, nil, egressrules.UnknownProvider(k.Provider)
	}
	header, scheme := p.Credential(p.KeyEnv)
	if !httpguts.ValidHeaderFieldValue(scheme + k.Key) {
		return boundary.Injection{}, nil, fmt.Errorf("the %s key has a character that a header can't carry", k.Provider)
	}
	inj := boundary.Injection{
		Host: p.Host, Port: egressrules.HTTPSPort, Header: header, Value: scheme + k.Key,
		Model: true, QueryKey: p.QueryKey, Endpoints: p.Endpoints, Secrets: []string{p.KeyEnv},
	}
	return inj, []string{p.KeyEnv + "=" + p.Placeholder(p.KeyEnv)}, nil
}
