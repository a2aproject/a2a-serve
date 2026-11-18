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

// Package egress decides the connections of a client by policy and adds
// credentials to its requests for chosen hosts. The client reaches it as
// an HTTP CONNECT proxy, and sees placeholders in place of the
// credentials.
package egress

import (
	"cmp"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
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
		}, true
	})
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

// Config configures a [Boundary].
type Config struct {
	// Policy returns the policy, which decides every connection when it
	// opens, so that a change applies to the next one.
	Policy func() Policy
	// Log receives one record per decision; nil uses the default logger.
	Log *slog.Logger
}

// Boundary decides connections by policy and adds credentials. One
// Boundary serves many clients; each gets an [Endpoint] with its own
// credentials. It records the connections its policy refuses.
type Boundary struct {
	policy func() Policy
	log    *slog.Logger
	blocks *boundary.Blocks
	deps   deps

	mu        sync.Mutex
	ca        *boundary.CA
	endpoints map[*Endpoint]struct{}
	closed    bool
}

// New returns a boundary that decides by cfg.Policy, whose current value
// must be valid.
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
	if cfg.Policy == nil {
		return nil, errors.New("the egress boundary needs a policy")
	}
	if err := cfg.Policy().Validate(); err != nil {
		return nil, err
	}
	return &Boundary{
		policy:    cfg.Policy,
		log:       cmp.Or(cfg.Log, slog.Default()),
		blocks:    &boundary.Blocks{},
		deps:      d,
		endpoints: map[*Endpoint]struct{}{},
	}, nil
}

// Open returns an endpoint for one client, named in the log, whose
// requests to the hosts of creds get them. Those hosts are allowed in
// every mode.
func (b *Boundary) Open(name string, creds ...Credential) (*Endpoint, error) {
	e := &Endpoint{b: b, name: name, boundaries: map[*boundary.Boundary]struct{}{}, listeners: map[net.Listener]struct{}{}}
	for _, c := range creds {
		inj, env, err := c.injection()
		if err != nil {
			return nil, err
		}
		if slices.ContainsFunc(e.inject, func(o boundary.Injection) bool { return o.Host == inj.Host && o.Port == inj.Port }) {
			return nil, fmt.Errorf("two credentials for %s; give each host one", net.JoinHostPort(inj.Host, strconv.Itoa(int(inj.Port))))
		}
		e.inject = append(e.inject, inj)
		e.env = append(e.env, env...)
		e.allow = append(e.allow, net.JoinHostPort(inj.Host, strconv.Itoa(int(inj.Port))))
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, errors.New("the egress boundary is closed")
	}
	if len(e.inject) > 0 {
		ca, err := b.authority(e.inject)
		if err != nil {
			return nil, err
		}
		e.ca = ca
	}
	main, err := e.newBoundary(nil)
	if err != nil {
		return nil, err
	}
	e.main = main
	b.endpoints[e] = struct{}{}
	return e, nil
}

// authority returns the CA of the hosts of inject, making a new one when
// the current one doesn't cover them all. An endpoint keeps the CA it
// opened with. It runs with b.mu held.
func (b *Boundary) authority(inject []boundary.Injection) (*boundary.CA, error) {
	var hosts []string
	for _, inj := range inject {
		if !slices.Contains(hosts, inj.Host) {
			hosts = append(hosts, inj.Host)
		}
	}
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
	p := b.policy()
	mode := p.Mode
	if mode == ModeUnrestricted {
		mode = ModeInternet
	}
	r, err := egressrules.NewPolicy(string(mode), append(slices.Clone(p.Allow), extra...))
	if err != nil {
		b.log.Error("reading the egress rules; the client reaches nothing until they are fixed", "error", err)
		return egressrules.Policy{Mode: egressrules.ModeAllowlist}
	}
	return r
}

func (b *Boundary) mode() Mode { return cmp.Or(b.policy().Mode, ModeInternet) }

// Status returns the current mode and the connections the policy refused.
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
	Mode Mode
	// Refused counts the connections the policy refused.
	Refused int
	// Recent are the latest refused destinations, newest first. A
	// destination leaves the list once a connection to it succeeds.
	Recent []Refusal
}

// Refusal is a destination the policy refused.
type Refusal struct {
	// Destination is the host:port the client asked for.
	Destination string
	// Rule is the Allow entry that admits the destination, or "" when
	// none can, such as for a tailnet address that only its name can.
	Rule string
	// Unresolved is set for a name that didn't resolve.
	Unresolved bool
}

// Credential is a [Header] or a [ModelKey].
type Credential interface {
	injection() (boundary.Injection, []string, error)
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
