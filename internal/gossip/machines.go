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
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-serve/internal/fsutil"
)

// minExtension is the least a claim must extend a machine's expiry to
// count as a change, so that relaying a claim back and forth doesn't save
// the table each time.
const minExtension = time.Minute

// Claim says that a machine of the private network runs an agent. The
// agent's sandbox reaches the network through it, so the user running the
// agents isn't the owner from it.
type Claim struct {
	// Node identifies the machine, as [network.Peer.Node] does.
	Node string `json:"node"`
	// TTL is the number of seconds the claim has left. A lifetime, unlike
	// an expiry time, means the same on hosts whose clocks differ.
	TTL int64 `json:"ttl"`
}

// machine is a claim as the table file keeps it.
type machine struct {
	Node    string    `json:"node"`
	Expires time.Time `json:"expires"`
}

// Machines are the machines that run agents, which agents claim for the
// machines they run on and pass on with their advertisements. A claim only
// lowers the role of callers from the machine, so it needs no proof: any
// agent may make or relay one. A claim lives for TTL unless its agent renews
// it. The table holds at most MaxEntries claims, besides the claims of this
// agent's machine and of the devices of the user running it, so that claims
// of other machines can't crowd those out.
type Machines struct {
	path string
	now  func() time.Time

	mu      sync.Mutex
	expires map[string]time.Time
	// dirty is set while changes wait for saveTimer to save them.
	dirty     bool
	saveTimer *time.Timer
	closed    bool
}

// OpenMachines reads the machines kept at path. A missing file is empty.
func OpenMachines(path string, now func() time.Time) (*Machines, error) {
	m := &Machines{path: path, now: now, expires: map[string]time.Time{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	var list []machine
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	for _, e := range list {
		m.expires[e.Node] = e.Expires
	}
	return m, nil
}

// Claim adds the machine this agent runs on, and renews the claim once it
// is older than Refresh.
func (m *Machines) Claim(node string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if cur, ok := m.expires[node]; ok && cur.Sub(now) > TTL-Refresh {
		return nil
	}
	m.expires[node] = now.Add(TTL)
	return m.saveLater()
}

// Merge adds the claims another agent sent, and keeps the later expiry of
// a machine it has, for at most TTL. Invalid and expired claims are
// dropped. Once the table is full, a claim of a new machine is kept only if
// own reports the machine as a device of the user running this agent. own
// runs without the table's lock, since it may ask the network.
func (m *Machines) Merge(claims []Claim, own func(node string) bool) error {
	claims = slices.DeleteFunc(slices.Clone(claims), func(c Claim) bool {
		return c.Node == "" || len(c.Node) > maxNode || c.TTL <= 0
	})
	owned := map[string]bool{}
	if fresh := m.fresh(claims); len(fresh) > 0 && own != nil {
		for _, node := range fresh {
			owned[node] = own(node)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	changed := false
	for _, c := range claims {
		exp := now.Add(time.Duration(min(c.TTL, int64(TTL/time.Second))) * time.Second)
		cur, ok := m.expires[c.Node]
		switch {
		case ok && exp.Sub(cur) < minExtension:
			continue
		case !ok && len(m.expires) >= MaxEntries && !owned[c.Node]:
			continue
		}
		m.expires[c.Node] = exp
		changed = true
	}
	if !changed {
		return nil
	}
	return m.saveLater()
}

// fresh returns the machines of claims that the table doesn't have, if
// they don't all fit in it.
func (m *Machines) fresh(claims []Claim) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	seen := map[string]bool{}
	for _, c := range claims {
		if _, ok := m.expires[c.Node]; !ok && !seen[c.Node] {
			seen[c.Node] = true
			out = append(out, c.Node)
		}
	}
	if len(m.expires)+len(out) <= MaxEntries {
		return nil
	}
	return out
}

// Claims returns the live claims, by node.
func (m *Machines) Claims() []Claim {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	out := []Claim{}
	for node, exp := range m.expires {
		if ttl := int64(exp.Sub(now) / time.Second); ttl > 0 {
			out = append(out, Claim{Node: node, TTL: ttl})
		}
	}
	slices.SortFunc(out, func(a, b Claim) int { return strings.Compare(a.Node, b.Node) })
	return out
}

// RunsAgent reports whether a live claim says that the machine node runs
// an agent.
func (m *Machines) RunsAgent(node string) bool {
	if node == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	exp, ok := m.expires[node]
	return ok && !m.now().After(exp)
}

// Expire drops the claims nobody renewed within TTL.
func (m *Machines) Expire() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	changed := false
	for node, exp := range m.expires {
		if now.After(exp) {
			delete(m.expires, node)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return m.saveLater()
}

// Close saves the changes waiting to be saved. The table saves every change
// right away after it.
func (m *Machines) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	if m.saveTimer != nil {
		m.saveTimer.Stop()
		m.saveTimer = nil
	}
	if !m.dirty {
		return nil
	}
	return m.save()
}

// saveLater saves the table within saveDelay, or now once it is closed.
// m.mu is held.
func (m *Machines) saveLater() error {
	if m.closed {
		return m.save()
	}
	m.dirty = true
	if m.saveTimer == nil {
		m.saveTimer = time.AfterFunc(saveDelay, m.flush)
	}
	return nil
}

func (m *Machines) flush() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saveTimer = nil
	if !m.dirty || m.closed {
		return
	}
	if err := m.save(); err != nil {
		slog.Warn("saving the machines that run agents", "error", err)
	}
}

// save writes the table. m.mu is held.
func (m *Machines) save() error {
	list := make([]machine, 0, len(m.expires))
	for node, exp := range m.expires {
		list = append(list, machine{Node: node, Expires: exp})
	}
	slices.SortFunc(list, func(a, b machine) int { return strings.Compare(a.Node, b.Node) })
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := fsutil.WriteAtomic(m.path, data, 0o600); err != nil {
		return err
	}
	m.dirty = false
	return nil
}
