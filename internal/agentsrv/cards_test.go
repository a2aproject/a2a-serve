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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/cardinfo"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/network"
)

func TestPeerCardsReplace(t *testing.T) {
	t.Parallel()
	ben := network.PeerInfo{Name: "ben", Card: cardinfo.Summary{Description: "Reviews Go."}}
	coder := network.PeerInfo{Name: "coder", Card: cardinfo.Summary{Description: "Writes Go."}}
	tests := []struct {
		name        string
		next        []network.PeerInfo
		wantChanged bool
		want        []AgentRef
	}{
		{
			name: "same peers",
			next: []network.PeerInfo{coder, ben},
			want: []AgentRef{{Name: "ben", Kind: peerKind, Card: ben.Card}, {Name: "coder", Kind: peerKind, Card: coder.Card}},
		},
		{
			name:        "changed card",
			next:        []network.PeerInfo{{Name: "ben", Card: cardinfo.Summary{Description: "Reviews Rust."}}, coder},
			wantChanged: true,
			want:        []AgentRef{{Name: "ben", Kind: peerKind, Card: cardinfo.Summary{Description: "Reviews Rust."}}, {Name: "coder", Kind: peerKind, Card: coder.Card}},
		},
		{
			name:        "peer left",
			next:        []network.PeerInfo{coder},
			wantChanged: true,
			want:        []AgentRef{{Name: "coder", Kind: peerKind, Card: coder.Card}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := newPeerCards()
			p.replace([]network.PeerInfo{ben, coder})

			if got := p.replace(tc.next); got != tc.wantChanged {
				t.Fatalf("peerCards.replace() = %v, want %v", got, tc.wantChanged)
			}
			if diff := cmp.Diff(tc.want, p.refs()); diff != "" {
				t.Fatalf("peerCards.refs() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestPeerCardsRefOfNewPeer(t *testing.T) {
	t.Parallel()
	p := newPeerCards()
	p.replace([]network.PeerInfo{{Name: "Ben", Card: cardinfo.Summary{Description: "Reviews Go."}}})
	tests := []struct {
		name string
		want AgentRef
	}{
		{name: "ben", want: AgentRef{Name: "ben", Kind: peerKind, Card: cardinfo.Summary{Description: "Reviews Go."}}},
		{name: "dana", want: AgentRef{Name: "dana", Kind: peerKind}},
	}
	for _, tc := range tests {
		if diff := cmp.Diff(tc.want, p.ref(tc.name)); diff != "" {
			t.Fatalf("peerCards.ref(%q) wrong result (-want +got) diff = %s", tc.name, diff)
		}
	}
}

func TestServerRefreshesContactCards(t *testing.T) {
	t.Parallel()
	var desc atomic.Value
	desc.Store("Writes Go.")
	contact := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		card := a2a.AgentCard{Name: "helper", Description: desc.Load().(string)}
		if err := json.NewEncoder(w).Encode(card); err != nil {
			t.Errorf("encoding the card: %v", err)
		}
	}))
	t.Cleanup(contact.Close)
	ts := startServer(t, serverOpts{timings: func(tm *Timings) { tm.CardRefresh = 20 * time.Millisecond }})
	ctx := context.Background()
	client := controlapi.NewClient(ts.dir)
	added, err := client.AddContact(ctx, controlapi.ContactRequest{Name: "helper", URL: contact.URL})
	if err != nil {
		t.Fatalf("AddContact() error = %v", err)
	}
	if added.Description != "Writes Go." {
		t.Fatalf("AddContact() description = %q, want %q", added.Description, "Writes Go.")
	}

	desc.Store("Reviews Go.")

	waitFor(t, "the refreshed card of the contact", func() bool {
		cs, err := client.Contacts(ctx)
		if err != nil {
			t.Fatalf("Contacts() error = %v", err)
		}
		return len(cs) == 1 && cs[0].Description == "Reviews Go."
	})
}
