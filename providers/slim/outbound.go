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

package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

const (
	// maxPeers bounds the agents the provider keeps a SLIM channel to.
	maxPeers = 64
	// cardTimeout bounds fetching an agent's card.
	cardTimeout = 30 * time.Second
	// keepAlive is how often streams to the server send a comment while no
	// event comes, as the server's own streams do.
	keepAlive = 15 * time.Second
)

// outbound serves the server's requests for http://slim.local/<org>/<ns>/<agent>
// by calling the agent over SLIMRPC.
type outbound struct {
	peers *peers
}

func (o *outbound) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.EqualFold(hostname(r.Host), Host) {
		http.Error(w, "this network provider serves only "+Host, http.StatusMisdirectedRequest)
		return
	}
	n, rest, ok := route(r.URL.EscapedPath())
	if !ok {
		http.Error(w, "want http://"+Host+"/<org>/<namespace>/<agent>", http.StatusNotFound)
		return
	}
	p, release, err := o.peers.acquire(n)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer release()
	switch {
	// a2a-go's card resolver fetches a URL with a path as it is.
	case r.Method == http.MethodGet && (rest == "/" || rest == a2asrv.WellKnownAgentCardPath || rest == "/.well-known/agent.json"):
		serveCard(w, r, n, p.t)
	case r.Method == http.MethodPost && rest == "/":
		p.jsonrpc.ServeHTTP(w, r)
	default:
		path, err := url.PathUnescape(rest)
		if err != nil {
			http.Error(w, "invalid URL path", http.StatusBadRequest)
			return
		}
		r2 := new(http.Request)
		*r2 = *r
		r2.URL = new(url.URL)
		*r2.URL = *r.URL
		r2.URL.Path, r2.URL.RawPath = path, rest
		p.rest.ServeHTTP(w, r2)
	}
}

func hostname(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

func serveCard(w http.ResponseWriter, r *http.Request, n name, t a2aclient.Transport) {
	ctx, cancel := context.WithTimeout(r.Context(), cardTimeout)
	defer cancel()
	card, err := t.GetExtendedAgentCard(ctx, nil, &a2a.GetExtendedAgentCardRequest{})
	if err != nil {
		http.Error(w, "the agent "+n.String()+" didn't send its card over SLIM: "+err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(card); err != nil {
		slog.Debug("writing a card", "error", err)
	}
}

// peers keeps a transport for each agent the server calls, up to maxPeers.
// A transport evicted while requests use it is destroyed after the last.
type peers struct {
	newTransport func(name) (a2aclient.Transport, error)
	max          int

	mu     sync.Mutex
	byName map[name]*peer
	// order lists the names, least recently used first.
	order  []name
	closed bool
}

type peer struct {
	t       a2aclient.Transport
	jsonrpc http.Handler
	rest    http.Handler
	refs    int
	evicted bool
}

func newPeers(max int, newTransport func(name) (a2aclient.Transport, error)) *peers {
	return &peers{newTransport: newTransport, max: max, byName: map[name]*peer{}}
}

var errClosed = errors.New("the network provider is closing")

// acquire returns the peer n, which stays usable until release.
func (ps *peers) acquire(n name) (*peer, func(), error) {
	var evicted []a2aclient.Transport
	defer func() { destroyAll(evicted) }()
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.closed {
		return nil, nil, errClosed
	}
	p, ok := ps.byName[n]
	if ok {
		ps.order = slices.DeleteFunc(ps.order, func(x name) bool { return x == n })
	} else {
		t, err := ps.newTransport(n)
		if err != nil {
			return nil, nil, err
		}
		f := &forwarder{open: func(context.Context) (a2aclient.Transport, func(), error) { return t, func() {}, nil }}
		p = &peer{
			t:       t,
			jsonrpc: a2asrv.NewJSONRPCHandler(f, a2asrv.WithTransportKeepAlive(keepAlive)),
			rest:    a2asrv.NewRESTHandler(f, a2asrv.WithTransportKeepAlive(keepAlive)),
		}
		ps.byName[n] = p
	}
	ps.order = append(ps.order, n)
	for len(ps.order) > ps.max {
		evicted = ps.evictLocked(ps.order[0], evicted)
	}
	p.refs++
	return p, func() { ps.release(p) }, nil
}

func (ps *peers) release(p *peer) {
	ps.mu.Lock()
	p.refs--
	last := p.evicted && p.refs == 0
	ps.mu.Unlock()
	if last {
		destroy(p.t)
	}
}

// evictLocked forgets the peer n, and appends its transport to idle when no
// request uses it.
func (ps *peers) evictLocked(n name, idle []a2aclient.Transport) []a2aclient.Transport {
	p := ps.byName[n]
	delete(ps.byName, n)
	ps.order = slices.DeleteFunc(ps.order, func(x name) bool { return x == n })
	p.evicted = true
	if p.refs == 0 {
		idle = append(idle, p.t)
	}
	return idle
}

// close destroys the transports, each once no request uses it.
func (ps *peers) close() {
	var idle []a2aclient.Transport
	ps.mu.Lock()
	ps.closed = true
	for len(ps.order) > 0 {
		idle = ps.evictLocked(ps.order[0], idle)
	}
	ps.mu.Unlock()
	destroyAll(idle)
}

func destroyAll(ts []a2aclient.Transport) {
	for _, t := range ts {
		destroy(t)
	}
}
