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

package agentsrv

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-serve/internal/network"
)

// peerCards are the cards of the agents on the private network as of the
// last refresh, so that what the agent is told about them changes only
// then. A refresh replaces them whole, which drops the agents that left.
type peerCards struct {
	mu    sync.Mutex
	peers map[string]AgentRef
}

func newPeerCards() *peerCards { return &peerCards{peers: map[string]AgentRef{}} }

// replace keeps the cards of peers, and reports whether they changed.
func (p *peerCards) replace(peers []network.PeerInfo) bool {
	next := make(map[string]AgentRef, len(peers))
	for _, pi := range peers {
		next[strings.ToLower(pi.Name)] = AgentRef{Name: pi.Name, Kind: peerKind, Card: pi.Card}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if maps.EqualFunc(p.peers, next, func(a, b AgentRef) bool { return a.Name == b.Name && a.Card.Equal(b.Card) }) {
		return false
	}
	p.peers = next
	return true
}

// refs returns the peers of the last refresh, sorted by name.
func (p *peerCards) refs() []AgentRef {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := slices.Collect(maps.Values(p.peers))
	slices.SortFunc(out, func(a, b AgentRef) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// ref returns the peer called name, with its card as of the last refresh,
// or none yet for a peer that came since.
func (p *peerCards) ref(name string) AgentRef {
	p.mu.Lock()
	defer p.mu.Unlock()
	return AgentRef{Name: name, Kind: peerKind, Card: p.peers[strings.ToLower(name)].Card}
}

// callablePeers returns the agents on the private network the agent may
// call by name now, with their cards as of the last refresh.
func (s *Server) callablePeers(ctx context.Context) []AgentRef {
	names := s.clients.CallablePeers(ctx)
	out := make([]AgentRef, len(names))
	for i, name := range names {
		out[i] = s.peerCards.ref(name)
	}
	return out
}

// refreshCards fetches the cards of the contacts and of the agents on the
// private network, and rewrites the instruction files when one changed.
func (s *Server) refreshCards(ctx context.Context) {
	if _, err := s.contacts.Refresh(ctx); err != nil && ctx.Err() == nil {
		slog.Warn("refreshing the cards of contacts", "error", err)
	}
	if s.private == nil {
		return
	}
	peers, err := s.clients.Discover(ctx)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("refreshing the cards of the agents on the private network", "error", err)
		}
		return
	}
	if s.peerCards.replace(peers) && s.clients.CallsPeers() {
		s.refreshInstructions(ctx)
	}
}

// refreshCardsLoop refreshes the cards now and every --contact-refresh
// until ctx ends. A change of the setting refreshes them at once.
func (s *Server) refreshCardsLoop(ctx context.Context) {
	for {
		s.refreshCards(ctx)
		t := time.NewTimer(s.cardRefreshInterval())
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-s.cardRefreshChanged:
			t.Stop()
			continue
		case <-t.C:
		}
	}
}

func (s *Server) cardRefreshInterval() time.Duration {
	if s.timings.CardRefresh > 0 {
		return s.timings.CardRefresh
	}
	return s.rt.Config().ContactRefreshDuration()
}
