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

// Package network defines how an agent server is reached beyond localhost:
// a private network of the owner's machines and team, and the public
// internet. Each [Provider] implements what it supports.
package network

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/a2aproject/a2a-serve/internal/cardinfo"
)

// ErrUnsupported is returned by providers for capabilities they don't have.
var ErrUnsupported = errors.New("not supported by this network provider")

// ErrNoIdentity is returned by WhoIs when the caller can't be identified.
var ErrNoIdentity = errors.New("caller has no verified network identity")

// Peer is a verified caller on the private network.
type Peer struct {
	// Email is the verified email of the person the calling machine
	// belongs to, if the network proves one.
	Email string
	// Node identifies the calling machine: a tailnet StableID or a relay
	// peer ID.
	Node    string
	Machine string
	// Self is true when the caller belongs to the same user as this server.
	Self bool
}

// PeerInfo is another agent server found on the private network.
type PeerInfo struct {
	Name        string `json:"name"`
	URL         string `json:"url"`
	Description string `json:"description,omitempty"`
	Online      bool   `json:"online"`
	// Node identifies the machine of the agent, as [Peer.Node] does.
	Node string `json:"-"`
	// CardName is the name in the agent's card, the name it advertises its
	// pairings under. Name differs from it when another agent has it.
	CardName string `json:"-"`
	// Card is what the agent's card says about it.
	Card cardinfo.Summary `json:"-"`
}

// Provider is one way of reaching the agent server. It implements what it
// supports and returns [ErrUnsupported] for the rest.
type Provider interface {
	// Name is the provider name, such as tailscale.
	Name() string
	// ListenPrivate listens on a network limited to the owner's machines or team.
	ListenPrivate(ctx context.Context) (net.Listener, *url.URL, error)
	// ListenPublic listens on the internet, with TLS terminated.
	ListenPublic(ctx context.Context) (net.Listener, *url.URL, error)
	// WhoIs identifies a caller on the private listener, or returns ErrNoIdentity.
	WhoIs(ctx context.Context, remoteAddr string) (*Peer, error)
	// Member returns the machine node of the private network as WhoIs
	// would identify it now, or ErrNoIdentity once the network no longer
	// has it.
	Member(ctx context.Context, node string) (*Peer, error)
	// Peers lists other agent servers reachable on the private network.
	Peers(ctx context.Context) ([]PeerInfo, error)
	// HTTPClient makes outbound calls to peers.
	HTTPClient() *http.Client
	// Close stops the provider.
	Close() error
}

// Provider names.
const (
	ProviderTailscale = "tailscale"
	ProviderRelay     = "agentmesh"
)

// Listener kinds, which tell authenticators how a request arrived.
const (
	ListenerInProcess = "inprocess"
	ListenerLocal     = "local"
	ListenerPrivate   = "private"
	ListenerPublic    = "public"
	// ListenerHarness serves the agent's own harness its calls to other agents.
	ListenerHarness = "harness"
)

// IsEmail reports whether s is a plain email address with a domain that
// has a dot, such as anna@example.com. Logins such as anna@github aren't.
func IsEmail(s string) bool {
	a, err := mail.ParseAddress(s)
	if err != nil || a.Address != s || a.Name != "" {
		return false
	}
	_, domain, ok := strings.Cut(s, "@")
	return ok && strings.Contains(strings.Trim(domain, "."), ".")
}

// ProbeAgents keeps the candidates which serve an A2A agent card, filling in
// their description from it. Candidates come named by the network, and taken
// are the network names of the other machines, such as this one and offline
// ones. See [nameAgents] for the names the agents get.
func ProbeAgents(ctx context.Context, client *http.Client, candidates []PeerInfo, taken []string) []PeerInfo {
	var (
		mu     sync.Mutex
		wg     sync.WaitGroup
		agents []PeerInfo
	)
	for _, c := range candidates {
		wg.Go(func() {
			card, err := FetchCard(ctx, client, c.URL)
			if err != nil {
				return
			}
			c.Description = card.Description
			c.CardName = card.Name
			c.Card = cardinfo.Of(card)
			mu.Lock()
			agents = append(agents, c)
			mu.Unlock()
		})
	}
	wg.Wait()
	machines := slices.Clone(taken)
	for _, c := range candidates {
		machines = append(machines, c.Name)
	}
	return nameAgents(agents, machines)
}

// nameAgents names each agent by its network name, or by its card name when
// no other agent declares it and no other machine has it as its network name.
// Agents that still share a name get a suffix derived from their node, so a
// name never stands for two agents. machines are the network names of every
// machine on the network but this one.
func nameAgents(agents []PeerInfo, machines []string) []PeerInfo {
	machineNames, cardNames := map[string]int{}, map[string]int{}
	for _, m := range machines {
		machineNames[strings.ToLower(m)]++
	}
	for _, a := range agents {
		cardNames[strings.ToLower(a.CardName)]++
	}
	out := make([]PeerInfo, 0, len(agents))
	for _, a := range agents {
		card := strings.ToLower(a.CardName)
		othersNamed := machineNames[card]
		if card == strings.ToLower(a.Name) {
			othersNamed--
		}
		if a.CardName != "" && cardNames[card] == 1 && othersNamed == 0 {
			a.Name = a.CardName
		}
		out = append(out, a)
	}
	counts := countNames(out)
	for i := range out {
		if counts[strings.ToLower(out[i].Name)] > 1 {
			out[i].Name += "-" + nodeSuffix(out[i].Node)
		}
	}
	// A suffixed name still collides only with a name picked to match it.
	// The agents left sharing a name are dropped.
	counts = countNames(out)
	out = slices.DeleteFunc(out, func(p PeerInfo) bool { return counts[strings.ToLower(p.Name)] > 1 })
	slices.SortFunc(out, func(a, b PeerInfo) int { return strings.Compare(a.Name, b.Name) })
	return out
}

func countNames(peers []PeerInfo) map[string]int {
	counts := map[string]int{}
	for _, p := range peers {
		counts[strings.ToLower(p.Name)]++
	}
	return counts
}

func nodeSuffix(node string) string {
	sum := sha256.Sum256([]byte(node))
	return hex.EncodeToString(sum[:4])
}

// FetchCard fetches the agent card served at base.
func FetchCard(ctx context.Context, client *http.Client, base string) (*a2a.AgentCard, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(base, "/")+a2asrv.WellKnownAgentCardPath, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			slog.Debug("closing card response", "error", err)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", base, resp.Status)
	}
	var card a2a.AgentCard
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&card); err != nil {
		return nil, err
	}
	return &card, nil
}

// splitListener is a net.Listener fed from another listener's accept loop.
// Until it is claimed, nobody accepts from it, so it closes what it gets.
type splitListener struct {
	addr    net.Addr
	conns   chan net.Conn
	done    chan struct{}
	once    sync.Once
	closer  func() error
	claimed atomic.Bool
}

func newSplitListener(addr net.Addr, closer func() error) *splitListener {
	return &splitListener{addr: addr, conns: make(chan net.Conn), done: make(chan struct{}), closer: closer}
}

func (l *splitListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *splitListener) Close() error {
	var err error
	l.once.Do(func() {
		close(l.done)
		if l.closer != nil {
			err = l.closer()
		}
	})
	return err
}

func (l *splitListener) Addr() net.Addr { return l.addr }

// claim marks the listener as accepted from.
func (l *splitListener) claim() { l.claimed.Store(true) }

// deliver hands c to the listener, or closes it if the listener is closed or
// unclaimed.
func (l *splitListener) deliver(c net.Conn) {
	if !l.claimed.Load() {
		if err := c.Close(); err != nil {
			slog.Debug("closing connection", "error", err)
		}
		return
	}
	select {
	case l.conns <- c:
	case <-l.done:
		if err := c.Close(); err != nil {
			slog.Debug("closing connection", "error", err)
		}
	}
}
