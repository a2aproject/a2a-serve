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
	"errors"
	"net"
	"net/netip"
	"slices"
	"sync"

	"github.com/a2aproject/a2a-serve/internal/boundary"
	"github.com/a2aproject/a2a-serve/internal/egressrules"
)

// Endpoint is one client's way into a [Boundary].
type Endpoint struct {
	b      *Boundary
	name   string
	inject []boundary.Injection
	env    []string
	allow  []string
	ca     *boundary.CA
	// main serves the connections of Serve and ServeConn.
	main *boundary.Boundary

	mu         sync.Mutex
	boundaries map[*boundary.Boundary]struct{}
	listeners  map[net.Listener]struct{}
	closed     bool
}

// newBoundary returns a boundary of e whose connections to
// host.docker.internal reach what hostGateway returns, which e closes
// when it closes.
func (e *Endpoint) newBoundary(hostGateway func(ctx context.Context, port uint16) (netip.AddrPort, bool)) (*boundary.Boundary, error) {
	var b *boundary.Boundary
	b, err := boundary.New(boundary.BoundaryConfig{
		Sandbox:       e.name,
		Policy:        func() egressrules.Policy { return e.b.rules(e.allow) },
		HostGateway:   hostGateway,
		Log:           e.b.log,
		Blocks:        e.b.blocks,
		Inject:        e.inject,
		CA:            e.ca,
		UpstreamRoots: e.b.deps.roots,
		Resolver:      e.b.deps.resolver,
		OnClose: func() {
			e.mu.Lock()
			delete(e.boundaries, b)
			e.mu.Unlock()
		},
	})
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, errors.Join(errors.New("the egress endpoint is closed"), b.Close())
	}
	e.boundaries[b] = struct{}{}
	return b, nil
}

// Serve answers the HTTP CONNECT requests of the connections ln accepts,
// until ln or e closes. It closes ln.
func (e *Endpoint) Serve(ln net.Listener) error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return errors.Join(net.ErrClosed, ln.Close())
	}
	e.listeners[ln] = struct{}{}
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.listeners, ln)
		e.mu.Unlock()
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			e.mu.Lock()
			closed := e.closed
			e.mu.Unlock()
			if closed {
				return net.ErrClosed
			}
			return errors.Join(err, closeListener(ln))
		}
		e.main.ServeConn(c)
	}
}

// ServeConn answers the HTTP CONNECT requests of c, and closes it.
func (e *Endpoint) ServeConn(c net.Conn) { e.main.ServeConn(c) }

// CA returns the PEM certificate that the client must trust for the hosts
// that get credentials, or nil without any.
func (e *Endpoint) CA() []byte {
	if e.ca == nil {
		return nil
	}
	return e.ca.PEM()
}

// Env returns the placeholders of the model keys as NAME=value, such as
// ANTHROPIC_API_KEY=sk-ant-api03-…, for the client's environment.
func (e *Endpoint) Env() []string { return slices.Clone(e.env) }

// Close stops serving and closes the connections of e.
func (e *Endpoint) Close() error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil
	}
	e.closed = true
	listeners := make([]net.Listener, 0, len(e.listeners))
	for ln := range e.listeners {
		listeners = append(listeners, ln)
	}
	boundaries := make([]*boundary.Boundary, 0, len(e.boundaries))
	for b := range e.boundaries {
		boundaries = append(boundaries, b)
	}
	e.mu.Unlock()
	var errs []error
	for _, ln := range listeners {
		errs = append(errs, closeListener(ln))
	}
	for _, b := range boundaries {
		errs = append(errs, b.Close())
	}
	e.b.mu.Lock()
	delete(e.b.endpoints, e)
	e.b.mu.Unlock()
	return errors.Join(errs...)
}

func closeListener(ln net.Listener) error {
	if err := ln.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return err
	}
	return nil
}
