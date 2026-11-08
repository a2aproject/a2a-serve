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
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func openMachines(t *testing.T, path string, c *clock) *Machines {
	t.Helper()
	if path == "" {
		path = filepath.Join(t.TempDir(), "machines.json")
	}
	m, err := OpenMachines(path, c.now)
	if err != nil {
		t.Fatalf("OpenMachines() error = %v", err)
	}
	t.Cleanup(func() {
		if err := m.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	return m
}

func noneOwn(string) bool { return false }

func TestMachinesSpreadThroughAnotherAgent(t *testing.T) {
	t.Parallel()
	c := &clock{t: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)}
	ben, mid, anna := openMachines(t, "", c), openMachines(t, "", c), openMachines(t, "", c)
	if err := ben.Claim("ben-laptop"); err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	if err := mid.Merge(ben.Claims(), noneOwn); err != nil {
		t.Fatalf("Merge() error = %v", err)
	}
	if err := anna.Merge(mid.Claims(), noneOwn); err != nil {
		t.Fatalf("Merge() error = %v", err)
	}
	if !anna.RunsAgent("ben-laptop") {
		t.Fatalf("RunsAgent(ben-laptop) on anna = false, want true")
	}
	if anna.RunsAgent("anna-phone") {
		t.Fatalf("RunsAgent(anna-phone) on anna = true, want false")
	}
}

func TestMachinesMerge(t *testing.T) {
	t.Parallel()
	hour := int64(time.Hour / time.Second)
	tests := []struct {
		name string
		have []Claim
		in   []Claim
		want []Claim
	}{
		{name: "new claim", in: []Claim{{Node: "n1", TTL: hour}}, want: []Claim{{Node: "n1", TTL: hour}}},
		{name: "expired", in: []Claim{{Node: "n1", TTL: 0}}, want: []Claim{}},
		{name: "longer than TTL", in: []Claim{{Node: "n1", TTL: 100 * hour}}, want: []Claim{{Node: "n1", TTL: int64(TTL / time.Second)}}},
		{name: "huge TTL", in: []Claim{{Node: "n1", TTL: math.MaxInt64}}, want: []Claim{{Node: "n1", TTL: int64(TTL / time.Second)}}},
		{name: "no node", in: []Claim{{TTL: hour}}, want: []Claim{}},
		{name: "too long node", in: []Claim{{Node: strings.Repeat("n", maxNode+1), TTL: hour}}, want: []Claim{}},
		{name: "later expiry extends", have: []Claim{{Node: "n1", TTL: hour}}, in: []Claim{{Node: "n1", TTL: 2 * hour}}, want: []Claim{{Node: "n1", TTL: 2 * hour}}},
		{name: "earlier expiry is ignored", have: []Claim{{Node: "n1", TTL: 2 * hour}}, in: []Claim{{Node: "n1", TTL: hour}}, want: []Claim{{Node: "n1", TTL: 2 * hour}}},
		{name: "duplicates keep the later expiry", in: []Claim{{Node: "n1", TTL: 2 * hour}, {Node: "n1", TTL: hour}}, want: []Claim{{Node: "n1", TTL: 2 * hour}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := openMachines(t, "", &clock{t: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)})
			if err := m.Merge(tc.have, noneOwn); err != nil {
				t.Fatalf("Merge(have) error = %v", err)
			}
			if err := m.Merge(tc.in, noneOwn); err != nil {
				t.Fatalf("Merge() error = %v", err)
			}
			if diff := cmp.Diff(tc.want, m.Claims()); diff != "" {
				t.Fatalf("Claims() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestMachinesClaimRenewsAfterRefresh(t *testing.T) {
	t.Parallel()
	c := &clock{t: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)}
	m := openMachines(t, "", c)
	if err := m.Claim("n1"); err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	c.t = c.t.Add(Refresh / 2)
	if err := m.Claim("n1"); err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	if diff := cmp.Diff([]Claim{{Node: "n1", TTL: int64((TTL - Refresh/2) / time.Second)}}, m.Claims()); diff != "" {
		t.Fatalf("Claims() before Refresh wrong result (-want +got) diff = %s", diff)
	}
	c.t = c.t.Add(Refresh)
	if err := m.Claim("n1"); err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	if diff := cmp.Diff([]Claim{{Node: "n1", TTL: int64(TTL / time.Second)}}, m.Claims()); diff != "" {
		t.Fatalf("Claims() after Refresh wrong result (-want +got) diff = %s", diff)
	}
}

func TestMachinesExpire(t *testing.T) {
	t.Parallel()
	c := &clock{t: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)}
	m := openMachines(t, "", c)
	if err := m.Merge([]Claim{{Node: "n1", TTL: 3600}}, noneOwn); err != nil {
		t.Fatalf("Merge() error = %v", err)
	}
	c.t = c.t.Add(time.Hour)
	if !m.RunsAgent("n1") {
		t.Fatalf("RunsAgent(n1) at its expiry = false, want true")
	}
	c.t = c.t.Add(time.Second)
	if m.RunsAgent("n1") {
		t.Fatalf("RunsAgent(n1) after its expiry = true, want false")
	}
	if err := m.Expire(); err != nil {
		t.Fatalf("Expire() error = %v", err)
	}
	if got := m.Claims(); len(got) != 0 {
		t.Fatalf("Claims() after Expire() = %v, want none", got)
	}
}

func TestMachinesFullTable(t *testing.T) {
	t.Parallel()
	c := &clock{t: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)}
	m := openMachines(t, "", c)
	flood := make([]Claim, MaxEntries+1)
	for i := range flood {
		flood[i] = Claim{Node: fmt.Sprintf("n%04d", i), TTL: 3600}
	}
	if err := m.Merge(flood, noneOwn); err != nil {
		t.Fatalf("Merge() error = %v", err)
	}
	if got := len(m.Claims()); got != MaxEntries {
		t.Fatalf("len(Claims()) = %d, want %d", got, MaxEntries)
	}
	own := func(node string) bool { return node == "my-laptop" }
	if err := m.Merge([]Claim{{Node: "stranger", TTL: 3600}, {Node: "my-laptop", TTL: 3600}, {Node: "n0000", TTL: 7200}}, own); err != nil {
		t.Fatalf("Merge() error = %v", err)
	}
	if err := m.Claim("this-machine"); err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	for node, want := range map[string]bool{"stranger": false, "my-laptop": true, "this-machine": true} {
		if got := m.RunsAgent(node); got != want {
			t.Fatalf("RunsAgent(%s) of a full table = %v, want %v", node, got, want)
		}
	}
	ttls := map[string]int64{}
	for _, cl := range m.Claims() {
		ttls[cl.Node] = cl.TTL
	}
	if got := ttls["n0000"]; got != 7200 {
		t.Fatalf("Claims() of a full table has n0000 with TTL %d, want the renewal to 7200", got)
	}
}

func TestMachinesSurviveReopen(t *testing.T) {
	t.Parallel()
	c := &clock{t: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)}
	path := filepath.Join(t.TempDir(), "machines.json")
	m, err := OpenMachines(path, c.now)
	if err != nil {
		t.Fatalf("OpenMachines() error = %v", err)
	}
	if err := m.Merge([]Claim{{Node: "n1", TTL: 3600}}, noneOwn); err != nil {
		t.Fatalf("Merge() error = %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if !openMachines(t, path, c).RunsAgent("n1") {
		t.Fatalf("RunsAgent(n1) after reopening = false, want true")
	}
}

func TestOpenMachinesRefusesCorruptFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "machines.json")
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenMachines(path, time.Now); err == nil {
		t.Fatalf("OpenMachines() of a corrupt file error = nil, want error")
	}
}
