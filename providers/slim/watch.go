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
	"log/slog"
	"time"

	"github.com/a2aproject/a2a-serve/netprovider"
)

// checkEvery is how often the provider checks the connection to the node,
// and, while it is down, joins the node again.
const checkEvery = 5 * time.Second

// watch checks the connection to the node until ctx is done.
func (p *provider) watch(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	t := time.NewTicker(checkEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.check(ctx)
		}
	}
}

// check joins the node again when the connection to it is down, and tells
// the server while it is. The bindings reconnect for a few seconds, and
// subscribe the app again when they succeed; after that they drop the
// connection, and the agent has to join the node anew: a new connection,
// its subscription and its server.
func (p *provider) check(ctx context.Context) {
	p.mu.Lock()
	cur, set, id, self := p.node, p.set, p.id, p.self
	p.mu.Unlock()
	if cur == nil || cur.connected() {
		return
	}
	if !p.lost {
		p.lost = true
		slog.Warn("lost the SLIM node; joining it again", "node", set.endpoint)
		p.host.Health(netprovider.Health{
			State:   netprovider.HealthDisconnected,
			Problem: "not connected to the SLIM node at " + set.endpoint + "; reconnecting",
			Fix:     "check that the SLIM node runs and that this machine reaches it",
		})
	}
	n, err := p.open(ctx, set, id, self)
	if err != nil {
		slog.Debug("joining the SLIM node again", "error", err)
		return
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		closeNode(n)
		return
	}
	p.node = n
	if p.handler != nil {
		n.listen(p.handler)
	}
	p.mu.Unlock()
	p.out.peers.reset()
	closeNode(cur)
	p.lost = false
	slog.Info("joined the SLIM node again", "node", set.endpoint)
	p.host.Health(netprovider.Health{State: netprovider.HealthOK})
}

func closeNode(n node) {
	if err := n.close(); err != nil {
		slog.Warn("leaving the SLIM node", "error", err)
	}
}
