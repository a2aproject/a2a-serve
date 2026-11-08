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

package network

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/a2aproject/a2a-serve/internal/cardinfo"
	"github.com/a2aproject/a2a-serve/internal/names"
)

// Card cache defaults: a peer's changed card shows up within AgentCardTTL,
// and a host that starts an agent within NoAgentTTL.
const (
	AgentCardTTL = 10 * time.Minute
	NoAgentTTL   = time.Minute
	// maxProbes is how many cards a cache fetches at once.
	maxProbes = 16
)

// CardCache keeps the agent cards of peers, so that finding the agents on
// the private network fetches only the cards of new peers and of peers
// whose card expired.
type CardCache struct {
	agentTTL time.Duration
	noAgent  time.Duration
	now      func() time.Time
	probes   chan struct{}

	mu      sync.Mutex
	entries map[string]cardEntry
}

// cardEntry is what a peer's URL served when it was fetched.
type cardEntry struct {
	agent       bool
	description string
	cardName    string
	card        cardinfo.Summary
	fetchedAt   time.Time
}

// NewCardCache returns a cache that keeps a card for agentTTL, and the
// failure of a host that serves none for noAgentTTL.
func NewCardCache(agentTTL, noAgentTTL time.Duration, now func() time.Time) *CardCache {
	return &CardCache{
		agentTTL: agentTTL,
		noAgent:  noAgentTTL,
		now:      now,
		probes:   make(chan struct{}, maxProbes),
		entries:  map[string]cardEntry{},
	}
}

func newCardCache() *CardCache { return NewCardCache(AgentCardTTL, NoAgentTTL, time.Now) }

// ProbeAgents keeps the candidates which serve an A2A agent card, filling in
// their description from it. Candidates come named by the network, and taken
// are the network names of the other machines, such as this one and offline
// ones. See [nameAgents] for the names the agents get. The cards of
// peers that aren't candidates anymore are dropped.
func (c *CardCache) ProbeAgents(ctx context.Context, client *http.Client, candidates []PeerInfo, taken []string) []PeerInfo {
	c.prune(candidates)
	var (
		mu     sync.Mutex
		wg     sync.WaitGroup
		agents []PeerInfo
	)
	for _, p := range candidates {
		wg.Go(func() {
			e, ok := c.entry(ctx, client, p.URL)
			if !ok || !e.agent {
				return
			}
			p.Description, p.CardName, p.Card = e.description, e.cardName, e.card
			mu.Lock()
			agents = append(agents, p)
			mu.Unlock()
		})
	}
	wg.Wait()
	machines := slices.Clone(taken)
	for _, p := range candidates {
		machines = append(machines, p.Name)
	}
	return nameAgents(agents, machines)
}

// entry returns the cached card of the peer at u, or fetches it when it
// expired. It reports false when ctx ended before the fetch.
func (c *CardCache) entry(ctx context.Context, client *http.Client, u string) (cardEntry, bool) {
	if e, ok := c.fresh(u); ok {
		return e, true
	}
	select {
	case c.probes <- struct{}{}:
	case <-ctx.Done():
		return cardEntry{}, false
	}
	defer func() { <-c.probes }()
	card, err := FetchCard(ctx, client, u)
	if err != nil && ctx.Err() != nil {
		return cardEntry{}, false
	}
	e := cardEntry{fetchedAt: c.now()}
	if err == nil {
		e.agent, e.description, e.card = true, card.Description, cardinfo.Of(card)
		if names.Valid(card.Name) {
			e.cardName = card.Name
		}
	}
	c.mu.Lock()
	c.entries[u] = e
	c.mu.Unlock()
	return e, true
}

func (c *CardCache) fresh(u string) (cardEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[u]
	if !ok {
		return cardEntry{}, false
	}
	ttl := c.noAgent
	if e.agent {
		ttl = c.agentTTL
	}
	return e, c.now().Sub(e.fetchedAt) < ttl
}

func (c *CardCache) prune(candidates []PeerInfo) {
	keep := make(map[string]bool, len(candidates))
	for _, p := range candidates {
		keep[p.URL] = true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for u := range c.entries {
		if !keep[u] {
			delete(c.entries, u)
		}
	}
}
