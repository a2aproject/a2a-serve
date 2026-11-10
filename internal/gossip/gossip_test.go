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

package gossip

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/network"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func openTable(t *testing.T, self string, c *clock) *Table {
	t.Helper()
	tb, err := Open(filepath.Join(t.TempDir(), "ads.json"), self)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	tb.SetClock(c.now)
	t.Cleanup(func() {
		if err := tb.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	return tb
}

// entriesOf returns n entries of host, which expire in an hour.
func entriesOf(c *clock, host string, n int) []Entry {
	out := make([]Entry, n)
	for i := range out {
		out[i] = Entry{Name: fmt.Sprintf("p%d", i), Host: host, Version: 1, Expires: c.t.Add(time.Hour)}
	}
	return out
}

func hostsOf(entries []Entry) map[string]int {
	out := map[string]int{}
	for _, e := range entries {
		out[e.Host]++
	}
	return out
}

// peerOf returns the agent of tb, on the machine named node-<name>.
func peerOf(tb *Table) Peer { return Peer{Name: tb.self, Node: "node-" + tb.self} }

// round swaps the tables of a and b the way the agents do over HTTP.
func round(t *testing.T, a, b *Table) {
	t.Helper()
	newer, want := b.Compare(a.self, a.Digest())
	if err := a.Merge(peerOf(b), newer); err != nil {
		t.Fatalf("Merge() error = %v", err)
	}
	if err := b.Merge(peerOf(a), a.Get(want)); err != nil {
		t.Fatalf("Merge() error = %v", err)
	}
}

func keys(entries []Entry) []string {
	out := []string{}
	for _, e := range entries {
		out = append(out, e.Key())
	}
	return out
}

func TestSpreadThroughAnotherAgent(t *testing.T) {
	t.Parallel()
	c := &clock{t: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)}
	ben, mid, anna := openTable(t, "ben", c), openTable(t, "mid", c), openTable(t, "anna", c)
	if err := ben.Advertise("grok", "Grok: web search"); err != nil {
		t.Fatalf("Advertise() error = %v", err)
	}
	round(t, mid, ben)
	round(t, anna, mid)
	if diff := cmp.Diff([]string{"grok@ben"}, keys(anna.Live())); diff != "" {
		t.Fatalf("Live() on anna wrong result (-want +got) diff = %s", diff)
	}
	if e, ok := anna.Lookup("grok@ben"); !ok || e.Description != "Grok: web search" {
		t.Fatalf("Lookup(grok@ben) = %+v, %v, want the advertisement", e, ok)
	}

	c.t = c.t.Add(time.Minute)
	if err := ben.Withdraw("grok"); err != nil {
		t.Fatalf("Withdraw() error = %v", err)
	}
	round(t, mid, ben)
	round(t, anna, mid)
	if got := anna.Live(); len(got) != 0 {
		t.Fatalf("Live() on anna after the withdrawal = %v, want none", keys(got))
	}
}

func TestListNamesHostsAsListed(t *testing.T) {
	t.Parallel()
	c := &clock{t: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)}
	ben, mid, anna := openTable(t, "reviewer", c), openTable(t, "mid", c), openTable(t, "anna", c)
	if err := ben.Advertise("grok", "Grok"); err != nil {
		t.Fatal(err)
	}
	if err := anna.Merge(Peer{Name: "old", Node: "node-old"}, []Entry{{Name: "legacy", Host: "carl", Version: 1, Expires: c.t.Add(time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	round(t, mid, ben)
	round(t, anna, mid)
	tests := []struct {
		name  string
		peers []network.PeerInfo
		want  map[string]string
	}{
		{
			name: "hosts found",
			peers: []network.PeerInfo{
				{Name: "reviewer-060d92a9", Node: "node-dave"},
				{Name: "carl", Node: "node-carl"},
				{Name: "reviewer-24a74ba3", Node: peerOf(ben).Node},
			},
			want: map[string]string{"grok@reviewer-24a74ba3": "reviewer-24a74ba3", "legacy@carl": "carl"},
		},
		{
			name:  "hosts offline",
			peers: []network.PeerInfo{{Name: "reviewer", Node: "node-dave"}},
			want:  map[string]string{"grok@reviewer": "", "legacy@carl": ""},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := map[string]string{}
			for _, l := range anna.List(tc.peers) {
				got[l.Key] = ""
				if l.Agent != nil {
					got[l.Key] = l.Agent.Name
				}
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("List() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestEntriesExpireWithoutRefresh(t *testing.T) {
	t.Parallel()
	c := &clock{t: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)}
	ben, anna := openTable(t, "ben", c), openTable(t, "anna", c)
	if err := ben.Advertise("grok", ""); err != nil {
		t.Fatal(err)
	}
	round(t, anna, ben)
	first := ben.Digest()["grok@ben"]

	c.t = c.t.Add(2 * Refresh)
	if err := ben.Renew(); err != nil {
		t.Fatal(err)
	}
	if ben.Digest()["grok@ben"] <= first {
		t.Fatalf("Renew() kept version %d, want a newer one", first)
	}
	c.t = c.t.Add(TTL)
	if err := anna.Expire(); err != nil {
		t.Fatal(err)
	}
	if got := anna.Live(); len(got) != 0 {
		t.Fatalf("Live() after the TTL = %v, want none", keys(got))
	}
}

func TestWithdrawnEntryIsForgottenAfterTTL(t *testing.T) {
	t.Parallel()
	c := &clock{t: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)}
	ben := openTable(t, "ben", c)
	if err := ben.Advertise("grok", ""); err != nil {
		t.Fatal(err)
	}
	if err := ben.Withdraw("grok"); err != nil {
		t.Fatal(err)
	}

	for elapsed := time.Duration(0); elapsed <= 2*TTL; elapsed += Refresh {
		c.t = c.t.Add(Refresh)
		if err := ben.Renew(); err != nil {
			t.Fatal(err)
		}
		if err := ben.Expire(); err != nil {
			t.Fatal(err)
		}
	}

	if _, ok := ben.Digest()["grok@ben"]; ok {
		t.Fatalf("Digest() after twice the TTL = %v, want grok@ben forgotten", ben.Digest())
	}
}

func TestMergeRefuses(t *testing.T) {
	t.Parallel()
	c := &clock{t: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)}
	anna := openTable(t, "anna", c)
	if err := anna.Advertise("helper", "mine"); err != nil {
		t.Fatal(err)
	}
	future := c.t.Add(time.Hour)
	err := anna.Merge(Peer{Name: "ben"}, []Entry{
		{Name: "helper", Host: "anna", Description: "forged", Version: future.UnixNano(), Expires: future},
		{Name: "bad name", Host: "ben", Version: 1, Expires: future},
		{Name: "old", Host: "ben", Version: 1, Expires: c.t.Add(-time.Minute)},
		{Name: "forever", Host: "ben", Version: 1, Expires: c.t.Add(10 * TTL)},
		{Name: "from-the-future", Host: "ben", Version: c.t.Add(2 * time.Hour).UnixNano(), Expires: future},
		{Name: "grok", Host: "ben", Version: 5, Expires: future},
		{Name: "grok", Host: "ben", Version: 3, Description: "older", Expires: future},
	})
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{"grok@ben"}, keys(anna.Live())); diff != "" {
		t.Fatalf("Live() wrong result (-want +got) diff = %s", diff)
	}
	if e, _ := anna.Lookup("grok@ben"); e.Version != 5 {
		t.Fatalf("Lookup(grok@ben).Version = %d, want the newest, 5", e.Version)
	}
	if own := anna.Own(); own["helper"].Description != "mine" {
		t.Fatalf("Own() = %v, want the agent's own entry kept", own)
	}
}

func TestRelayCantHideEntriesFromTheirHost(t *testing.T) {
	t.Parallel()
	c := &clock{t: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)}
	expires := c.t.Add(time.Hour)
	forged := c.t.Add(30 * time.Minute).UnixNano()
	tests := []struct {
		name  string
		entry Entry
	}{
		{name: "removal", entry: Entry{Name: "grok", Host: "ben", Version: forged, Expires: expires, Removed: true}},
		{name: "newer version", entry: Entry{Name: "grok", Host: "ben", Description: "forged", Version: forged, Expires: expires}},
		{name: "older version", entry: Entry{Name: "grok", Host: "ben", Description: "forged", Version: 1, Expires: expires}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ben, anna := openTable(t, "ben", c), openTable(t, "anna", c)
			if err := ben.Advertise("grok", "Grok"); err != nil {
				t.Fatal(err)
			}
			round(t, anna, ben)

			if err := anna.Merge(Peer{Name: "mallory"}, []Entry{tc.entry}); err != nil {
				t.Fatalf("Merge() error = %v", err)
			}

			if e, ok := anna.Lookup("grok@ben"); !ok || e.Description != "Grok" {
				t.Fatalf("Lookup(grok@ben) after a relayed %s = %+v, %v, want ben's advertisement", tc.name, e, ok)
			}
		})
	}
}

func TestHostReplacesRelayedEntries(t *testing.T) {
	t.Parallel()
	c := &clock{t: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)}
	for _, annaCalls := range []bool{true, false} {
		t.Run(fmt.Sprintf("anna calls %v", annaCalls), func(t *testing.T) {
			t.Parallel()
			ben, anna := openTable(t, "ben", c), openTable(t, "anna", c)
			if err := ben.Advertise("grok", "Grok"); err != nil {
				t.Fatal(err)
			}
			forged := Entry{Name: "grok", Host: "ben", Version: c.t.Add(30 * time.Minute).UnixNano(), Expires: c.t.Add(time.Hour), Removed: true}
			if err := anna.Merge(Peer{Name: "mallory"}, []Entry{forged}); err != nil {
				t.Fatalf("Merge() error = %v", err)
			}
			if _, ok := anna.Lookup("grok@ben"); ok {
				t.Fatalf("Lookup(grok@ben) after a relayed removal = found, want the removal kept")
			}

			if annaCalls {
				round(t, anna, ben)
			} else {
				round(t, ben, anna)
			}

			if e, ok := anna.Lookup("grok@ben"); !ok || e.Description != "Grok" {
				t.Fatalf("Lookup(grok@ben) after a round with ben = %+v, %v, want ben's advertisement", e, ok)
			}
		})
	}
}

func TestTableKeepsEntriesFromTheirHostAfterReopening(t *testing.T) {
	t.Parallel()
	c := &clock{t: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)}
	path := filepath.Join(t.TempDir(), "ads.json")
	expires := c.t.Add(time.Hour)
	tb, err := Open(path, "anna")
	if err != nil {
		t.Fatal(err)
	}
	tb.SetClock(c.now)
	if err := tb.Merge(Peer{Name: "ben"}, []Entry{{Name: "grok", Host: "ben", Description: "Grok", Version: 1, Expires: expires}}); err != nil {
		t.Fatal(err)
	}
	if err := tb.Close(); err != nil {
		t.Fatal(err)
	}

	again, err := Open(path, "anna")
	if err != nil {
		t.Fatal(err)
	}
	again.SetClock(c.now)
	if err := again.Merge(Peer{Name: "mallory"}, []Entry{{Name: "grok", Host: "ben", Version: 2, Expires: expires, Removed: true}}); err != nil {
		t.Fatal(err)
	}

	if e, ok := again.Lookup("grok@ben"); !ok || e.Description != "Grok" {
		t.Fatalf("Lookup(grok@ben) after reopening and a relayed removal = %+v, %v, want ben's advertisement", e, ok)
	}
}

func TestTablePersists(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "ads.json")
	tb, err := Open(path, "ben")
	if err != nil {
		t.Fatal(err)
	}
	if err := tb.Advertise("grok", "Grok"); err != nil {
		t.Fatal(err)
	}
	again, err := Open(path, "ben")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := again.Own()["grok"]; !ok {
		t.Fatalf("Own() after reopening = %v, want grok", again.Own())
	}
}

func TestMergeSavesWhenClosed(t *testing.T) {
	t.Parallel()
	c := &clock{t: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)}
	path := filepath.Join(t.TempDir(), "ads.json")
	tb, err := Open(path, "anna")
	if err != nil {
		t.Fatal(err)
	}
	tb.SetClock(c.now)
	reopen := func() []Entry {
		t.Helper()
		again, err := Open(path, "anna")
		if err != nil {
			t.Fatalf("Open() error = %v", err)
		}
		again.SetClock(c.now)
		return again.Live()
	}
	if err := tb.Merge(Peer{Name: "ben"}, []Entry{{Name: "grok", Host: "ben", Version: 1, Expires: c.t.Add(time.Hour)}}); err != nil {
		t.Fatalf("Merge() error = %v", err)
	}
	if got := reopen(); len(got) != 0 {
		t.Fatalf("Live() after reopening before Close() = %v, want none saved yet", keys(got))
	}

	if err := tb.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	if diff := cmp.Diff([]string{"grok@ben"}, keys(reopen())); diff != "" {
		t.Fatalf("Live() after Close() wrong result (-want +got) diff = %s", diff)
	}
}

func TestMergeCapsEntriesPerHost(t *testing.T) {
	t.Parallel()
	c := &clock{t: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)}
	tests := []struct {
		name string
		peer string
	}{
		{name: "from their host", peer: "mallory"},
		{name: "relayed", peer: "carl"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			anna := openTable(t, "anna", c)

			if err := anna.Merge(Peer{Name: tc.peer}, entriesOf(c, "mallory", MaxPerHost+50)); err != nil {
				t.Fatalf("Merge() error = %v", err)
			}

			if diff := cmp.Diff(map[string]int{"mallory": MaxPerHost}, hostsOf(anna.Live())); diff != "" {
				t.Fatalf("Live() entries by host wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestEntriesFromTheirHostEvictRelayedOnes(t *testing.T) {
	t.Parallel()
	c := &clock{t: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)}
	full := func(anna *Table) {
		t.Helper()
		for i := range MaxEntries / MaxPerHost {
			if err := anna.Merge(Peer{Name: "mallory"}, entriesOf(c, fmt.Sprintf("h%d", i), MaxPerHost)); err != nil {
				t.Fatalf("Merge() error = %v", err)
			}
		}
	}
	tests := []struct {
		name  string
		setup func(anna *Table)
	}{
		{name: "table full", setup: full},
		{name: "host full", setup: func(anna *Table) {
			if err := anna.Merge(Peer{Name: "mallory"}, entriesOf(c, "ben", MaxPerHost)); err != nil {
				t.Fatalf("Merge() error = %v", err)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			anna := openTable(t, "anna", c)
			tc.setup(anna)
			grok := Entry{Name: "grok", Host: "ben", Version: 1, Expires: c.t.Add(time.Hour)}

			if err := anna.Merge(Peer{Name: "mallory"}, []Entry{{Name: "relayed", Host: "dora", Version: 1, Expires: grok.Expires}, {Name: "relayed", Host: "ben", Version: 1, Expires: grok.Expires}}); err != nil {
				t.Fatalf("Merge() error = %v", err)
			}
			if err := anna.Merge(Peer{Name: "ben"}, []Entry{grok}); err != nil {
				t.Fatalf("Merge() error = %v", err)
			}

			if _, ok := anna.Lookup("relayed@ben"); ok {
				t.Fatalf("Lookup(relayed@ben) = found, want a relayed entry refused when there is no room")
			}
			if _, ok := anna.Lookup("grok@ben"); !ok {
				t.Fatalf("Lookup(grok@ben) = not found, want the entry from its host")
			}
		})
	}
}

func TestAdvertiseCapsEntries(t *testing.T) {
	t.Parallel()
	c := &clock{t: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)}
	ben, anna := openTable(t, "ben", c), openTable(t, "anna", c)
	for i := range MaxPerHost {
		if err := ben.Advertise(fmt.Sprintf("p%d", i), ""); err != nil {
			t.Fatalf("Advertise() error = %v", err)
		}
	}
	round(t, anna, ben)
	if err := ben.Advertise("more", ""); err == nil {
		t.Fatalf("Advertise() of one pairing too many error = nil, want error")
	}
	if err := ben.Advertise("p0", "renewed"); err != nil {
		t.Fatalf("Advertise() of an advertised pairing error = %v", err)
	}

	c.t = c.t.Add(time.Minute)
	if err := ben.Withdraw("p1"); err != nil {
		t.Fatalf("Withdraw() error = %v", err)
	}
	if err := ben.Advertise("more", ""); err != nil {
		t.Fatalf("Advertise() after a withdrawal error = %v", err)
	}
	round(t, anna, ben)

	if _, ok := anna.Lookup("more@ben"); !ok {
		t.Fatalf("Lookup(more@ben) on anna = not found, want the pairing advertised after a withdrawal")
	}
	if diff := cmp.Diff(map[string]int{"ben": MaxPerHost}, hostsOf(anna.Live())); diff != "" {
		t.Fatalf("Live() on anna entries by host wrong result (-want +got) diff = %s", diff)
	}
}
