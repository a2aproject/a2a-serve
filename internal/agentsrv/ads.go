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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	"github.com/a2aproject/a2a-serve/internal/gossip"
	"github.com/a2aproject/a2a-serve/internal/jsonhttp"
	"github.com/a2aproject/a2a-serve/internal/network"
)

// Advertised pairings spread over the private network by gossip: every
// gossipInterval each agent swaps its table with gossipFanout random peers.
var (
	gossipInterval = time.Minute
	gossipFanout   = 3
)

const (
	gossipPath        = "/gossip"
	gossipEntriesPath = "/gossip/entries"
	maxGossipBody     = 4 << 20

	// gossipRediscoverEvery bounds how often gossip callers missing from
	// the cached list of agents make the server find the agents again.
	gossipRediscoverEvery = 5 * time.Second
)

type gossipRequest struct {
	Digest gossip.Digest `json:"digest"`
}

type gossipResponse struct {
	// Entries are newer here than in the request's digest.
	Entries []gossip.Entry `json:"entries"`
	// Want are the keys the caller has newer, to send to gossipEntriesPath.
	Want []string `json:"want"`
}

type gossipEntries struct {
	Entries []gossip.Entry `json:"entries"`
}

// registerGossip serves the exchange of advertisements. Only agents on the
// private network reach it; an advertisement only names a pairing, and a
// call to it goes to the host found by name on the private network. The
// entries a caller sends of its own host replace the ones others relayed.
func (s *Server) registerGossip(mux *http.ServeMux) {
	mux.Handle("POST "+gossipPath, jsonhttp.LimitBodyTo(maxGossipBody, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req gossipRequest
		if !jsonhttp.Read(w, r, &req) {
			return
		}
		newer, want := s.ads.Compare(s.gossipPeer(r), req.Digest)
		jsonhttp.Write(w, http.StatusOK, gossipResponse{Entries: newer, Want: want})
	})))
	mux.Handle("POST "+gossipEntriesPath, jsonhttp.LimitBodyTo(maxGossipBody, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req gossipEntries
		if !jsonhttp.Read(w, r, &req) {
			return
		}
		if err := s.ads.Merge(s.gossipPeer(r), req.Entries); err != nil {
			jsonhttp.Error(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})))
}

// gossipPeer returns the name of the agent on the private network which
// sent r, the agent whose machine the network identifies as the caller's,
// or "" if there is none.
func (s *Server) gossipPeer(r *http.Request) string {
	who, err := s.private.WhoIs(r.Context(), r.RemoteAddr)
	if err != nil {
		if !errors.Is(err, network.ErrNoIdentity) {
			slog.Warn("identifying a gossip peer", "error", err)
		}
		return ""
	}
	peers, err := s.clients.Discover(r.Context())
	name, ok := agentOnNode(peers, who.Node)
	if err == nil && !ok {
		// A peer which joined since the last discovery is found by a new one.
		peers, err = s.clients.Rediscover(r.Context(), gossipRediscoverEvery)
		name, _ = agentOnNode(peers, who.Node)
	}
	if err != nil {
		slog.Warn("finding gossip peers", "error", err)
		return ""
	}
	return name
}

// agentOnNode returns the name of the agent of peers on the machine node.
func agentOnNode(peers []network.PeerInfo, node string) (string, bool) {
	for _, p := range peers {
		if p.Node != "" && p.Node == node {
			return p.Name, true
		}
	}
	return "", false
}

// runGossip renews this agent's advertisements, drops stale ones and swaps
// tables with a few random peers, until ctx ends.
func (s *Server) runGossip(ctx context.Context) {
	if s.private == nil {
		return
	}
	for {
		if err := s.ads.Renew(); err != nil {
			slog.Warn("renewing advertised pairings", "error", err)
		}
		if err := s.ads.Expire(); err != nil {
			slog.Warn("expiring advertised pairings", "error", err)
		}
		s.gossipRound(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(gossipInterval):
		}
	}
}

func (s *Server) gossipRound(ctx context.Context) {
	peers, err := s.clients.Discover(ctx)
	if err != nil {
		slog.Debug("finding peers to gossip with", "error", err)
		return
	}
	self := s.rt.Config().Name
	var targets []network.PeerInfo
	for _, p := range peers {
		if p.Online && !strings.EqualFold(p.Name, self) {
			targets = append(targets, p)
		}
	}
	rand.Shuffle(len(targets), func(i, j int) { targets[i], targets[j] = targets[j], targets[i] })
	for _, p := range targets[:min(gossipFanout, len(targets))] {
		if err := s.gossipWith(ctx, s.private.HTTPClient(), p); err != nil {
			slog.Debug("gossiping with a peer", "peer", p.Name, "error", err)
		}
	}
}

// gossipWith swaps tables with the agent peer.
func (s *Server) gossipWith(ctx context.Context, hc *http.Client, peer network.PeerInfo) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var resp gossipResponse
	if err := postJSON(ctx, hc, trimSlash(peer.URL)+gossipPath, gossipRequest{Digest: s.ads.Digest()}, &resp); err != nil {
		return err
	}
	if err := s.ads.Merge(peer.Name, resp.Entries); err != nil {
		return err
	}
	if len(resp.Want) == 0 {
		return nil
	}
	return postJSON(ctx, hc, trimSlash(peer.URL)+gossipEntriesPath, gossipEntries{Entries: s.ads.Get(resp.Want)}, nil)
}

func postJSON(ctx context.Context, hc *http.Client, url string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			slog.Debug("closing a gossip response", "error", err)
		}
	}()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxGossipBody))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s: %s", url, jsonhttp.ErrorMessage(resp.Status, data))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}
