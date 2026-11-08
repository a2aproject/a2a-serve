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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// countingServer serves the card of an agent called name, or none when
// name is empty, and counts the requests it gets.
func countingServer(t *testing.T, name string) (string, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		if name == "" {
			http.NotFound(w, r)
			return
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"name": name}); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &n
}

func peerNames(peers []PeerInfo) []string {
	var out []string
	for _, p := range peers {
		out = append(out, p.Name)
	}
	return out
}

func TestCardCacheFetchesOnlyNewAndExpiredCards(t *testing.T) {
	t.Parallel()
	agentURL, agentFetches := countingServer(t, "coder")
	hostURL, hostFetches := countingServer(t, "")
	agent := PeerInfo{Name: "laptop", URL: agentURL, Online: true, Node: "n1"}
	host := PeerInfo{Name: "printer", URL: hostURL, Online: true, Node: "n2"}
	start := time.Unix(1000, 0)
	now := start
	cache := NewCardCache(10*time.Minute, time.Minute, func() time.Time { return now })
	steps := []struct {
		name             string
		at               time.Duration
		candidates       []PeerInfo
		wantAgents       []string
		wantAgentFetches int32
		wantHostFetches  int32
	}{
		{name: "first probe", candidates: []PeerInfo{agent, host}, wantAgents: []string{"coder"}, wantAgentFetches: 1, wantHostFetches: 1},
		{name: "within both TTLs", at: 59 * time.Second, candidates: []PeerInfo{agent, host}, wantAgents: []string{"coder"}},
		{name: "host expired", at: 61 * time.Second, candidates: []PeerInfo{agent, host}, wantAgents: []string{"coder"}, wantHostFetches: 1},
		{name: "agent expired", at: 10*time.Minute + time.Second, candidates: []PeerInfo{agent, host}, wantAgents: []string{"coder"}, wantAgentFetches: 1, wantHostFetches: 1},
		{name: "agent left", at: 10*time.Minute + 2*time.Second, candidates: []PeerInfo{host}},
		{name: "agent came back", at: 10*time.Minute + 3*time.Second, candidates: []PeerInfo{agent, host}, wantAgents: []string{"coder"}, wantAgentFetches: 1},
	}
	for _, step := range steps {
		now = start.Add(step.at)
		agentFetches.Store(0)
		hostFetches.Store(0)

		got := cache.ProbeAgents(t.Context(), testHTTP, step.candidates, nil)

		if diff := cmp.Diff(step.wantAgents, peerNames(got)); diff != "" {
			t.Fatalf("%s: CardCache.ProbeAgents() wrong result (-want +got) diff = %s", step.name, diff)
		}
		if a, h := agentFetches.Load(), hostFetches.Load(); a != step.wantAgentFetches || h != step.wantHostFetches {
			t.Fatalf("%s: card fetches of the agent, host = %d, %d, want %d, %d", step.name, a, h, step.wantAgentFetches, step.wantHostFetches)
		}
	}
}

func TestCardCacheDoesNotCacheCanceledProbes(t *testing.T) {
	t.Parallel()
	agentURL, _ := countingServer(t, "coder")
	candidates := []PeerInfo{{Name: "laptop", URL: agentURL, Online: true, Node: "n1"}}
	cache := newCardCache()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if got := cache.ProbeAgents(ctx, testHTTP, candidates, nil); len(got) != 0 {
		t.Fatalf("CardCache.ProbeAgents() with a canceled context = %v, want no agents", peerNames(got))
	}
	got := cache.ProbeAgents(t.Context(), testHTTP, candidates, nil)

	if diff := cmp.Diff([]string{"coder"}, peerNames(got)); diff != "" {
		t.Fatalf("CardCache.ProbeAgents() after a canceled probe wrong result (-want +got) diff = %s", diff)
	}
}

func TestCardCacheLimitsConcurrentProbes(t *testing.T) {
	t.Parallel()
	var (
		mu             sync.Mutex
		inFlight, most int
		opened         bool
		full           = make(chan struct{})
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		inFlight++
		most = max(most, inFlight)
		if inFlight == maxProbes && !opened {
			close(full)
			opened = true
		}
		mu.Unlock()
		<-full
		mu.Lock()
		inFlight--
		mu.Unlock()
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	var candidates []PeerInfo
	for i := range 3 * maxProbes {
		candidates = append(candidates, PeerInfo{Name: "m" + strconv.Itoa(i), URL: srv.URL + "/m" + strconv.Itoa(i), Online: true})
	}

	newCardCache().ProbeAgents(t.Context(), testHTTP, candidates, nil)

	mu.Lock()
	defer mu.Unlock()
	if most != maxProbes {
		t.Fatalf("most card fetches at once = %d, want %d", most, maxProbes)
	}
}
