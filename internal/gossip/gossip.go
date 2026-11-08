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

// Package gossip spreads the advertised pairings of agents across a private
// network. Each agent keeps a table of advertisements, its own and the ones
// it heard of; agents swap the names and versions they know with a few
// random peers, and download the entries the other has newer. After a few
// rounds every agent has every entry, including agents which were offline
// or can't reach the host directly. An entry an agent got from its host
// can only change by another one from the host, so a peer relaying entries
// can't hide or alter the advertisements of others. The agent that gets an
// entry from its host records the host's machine in it, so that agents find
// the host among the agents of the private network even when its name is
// listed with a suffix. Agents also spread the machines that run agents,
// see [Machines].
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

	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
	"github.com/a2aproject/a2a-serve/internal/names"
	"github.com/a2aproject/a2a-serve/internal/network"
)

// Lifetimes of advertisements.
const (
	// Refresh is how often the host renews its entries.
	Refresh = time.Hour
	// TTL is how long an entry lives without a refresh, so the entries of a
	// deleted agent go away.
	TTL = 24 * time.Hour
	// MaxEntries bounds the table against a peer sending junk.
	MaxEntries = 1000
	// MaxPerHost bounds the entries of one host, so that one agent can't
	// fill the table.
	MaxPerHost = 100
	// maxNode bounds the machine ID of an entry.
	maxNode = 256
	// saveDelay is how long the table waits to save merged entries, so
	// that the merges of a gossip round write it once.
	saveDelay = 10 * time.Second
	// maxSkew is how far ahead of this agent's clock another host's clock
	// may run. Versions and expiry times beyond it are refused, so that
	// an entry from the future can't block the host's real updates.
	maxSkew = time.Hour
)

// Entry advertises a pairing of a host agent to the private network.
type Entry struct {
	// Name is the pairing's name on its host, such as grok.
	Name string `json:"name"`
	// Host is the name of the agent hosting it, as its agent card says.
	Host string `json:"host"`
	// Node identifies the host's machine on the private network, as
	// [network.PeerInfo.Node] does. The agent that got the entry from its
	// host sets it; it is empty in the host's own table.
	Node        string `json:"node,omitempty"`
	Description string `json:"description,omitempty"`
	// Version orders the updates of an entry; the host sets it.
	Version int64     `json:"version"`
	Expires time.Time `json:"expires"`
	// Removed marks an entry the host stopped advertising, so that the
	// removal spreads like an update.
	Removed bool `json:"removed,omitempty"`
}

// Key is name@host, how the table refers to the entry. Other agents refer to
// it with the host's listed name, see [Table.List].
func (e Entry) Key() string { return e.Name + "@" + e.Host }

func (e Entry) validate() error {
	if !names.Valid(e.Name) || !names.Valid(e.Host) {
		return fmt.Errorf("invalid advertisement %q", e.Key())
	}
	if len(e.Node) > maxNode {
		return fmt.Errorf("advertisement %s has a too long machine ID", e.Key())
	}
	if len(e.Description) > 1000 {
		return fmt.Errorf("advertisement %s has a too long description", e.Key())
	}
	return nil
}

// ParseKey splits name@host.
func ParseKey(key string) (name, host string, ok bool) {
	name, host, ok = strings.Cut(key, "@")
	if !ok || !names.Valid(name) || !names.Valid(host) {
		return "", "", false
	}
	return name, host, true
}

// Table is an agent's advertisements, kept in a JSON file.
type Table struct {
	path string
	self string
	now  func() time.Time

	mu      sync.Mutex
	entries map[string]Entry
	// direct are the keys of the entries which came from their host.
	direct map[string]bool
	// dirty is set while merged entries wait for saveTimer to save them.
	dirty     bool
	saveTimer *time.Timer
	closed    bool
}

// stored is an entry as the table file keeps it.
type stored struct {
	Entry
	Direct bool `json:"direct,omitempty"`
}

// Open reads the table at path of the agent named self. A missing file is
// empty.
func Open(path, self string) (*Table, error) {
	t := &Table{path: path, self: self, now: time.Now, entries: map[string]Entry{}, direct: map[string]bool{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return t, nil
	}
	if err != nil {
		return nil, err
	}
	var list []stored
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	for _, e := range list {
		t.entries[e.Key()] = e.Entry
		if e.Direct {
			t.direct[e.Key()] = true
		}
	}
	return t, nil
}

// SetClock replaces time.Now, for tests.
func (t *Table) SetClock(now func() time.Time) { t.now = now }

// save writes the table. t.mu is held.
func (t *Table) save() error {
	list := make([]stored, 0, len(t.entries))
	for _, k := range sortedKeys(t.entries) {
		list = append(list, stored{Entry: t.entries[k], Direct: t.direct[k]})
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := fsutil.WriteAtomic(t.path, data, 0o600); err != nil {
		return err
	}
	t.dirty = false
	return nil
}

// saveLater saves the table within saveDelay, or now once it is closed.
// t.mu is held.
func (t *Table) saveLater() error {
	if t.closed {
		return t.save()
	}
	t.dirty = true
	if t.saveTimer == nil {
		t.saveTimer = time.AfterFunc(saveDelay, t.flush)
	}
	return nil
}

func (t *Table) flush() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.saveTimer = nil
	if !t.dirty || t.closed {
		return
	}
	if err := t.save(); err != nil {
		slog.Warn("saving advertised pairings", "error", err)
	}
}

// Close saves the merged entries waiting to be saved. The table saves every
// change right away after it.
func (t *Table) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	if t.saveTimer != nil {
		t.saveTimer.Stop()
		t.saveTimer = nil
	}
	if !t.dirty {
		return nil
	}
	return t.save()
}

func sortedKeys(m map[string]Entry) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// version returns a version newer than the current one of key. t.mu is held.
func (t *Table) version(key string) int64 {
	v := t.now().UnixNano()
	if cur, ok := t.entries[key]; ok && cur.Version >= v {
		v = cur.Version + 1
	}
	return v
}

// Advertise adds or renews an entry of this agent. An agent advertises at
// most MaxPerHost entries.
func (t *Table) Advertise(name, description string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	e := Entry{Name: name, Host: t.self, Description: description, Expires: t.now().Add(TTL)}
	if err := e.validate(); err != nil {
		return err
	}
	own := func(_ string, cur Entry) bool { return cur.Host == t.self && !cur.Removed }
	if cur, ok := t.entries[e.Key()]; (!ok || cur.Removed) && t.count(own) >= MaxPerHost {
		return agenterr.Conflictf("this agent already advertises %d pairings, the most it can; stop advertising one with `a2a-serve pair set <name> --advertised=false`", MaxPerHost)
	}
	e.Version = t.version(e.Key())
	t.entries[e.Key()] = e
	return t.save()
}

// Withdraw marks an entry of this agent as removed. It is a no-op for
// entries which aren't advertised.
func (t *Table) Withdraw(name string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := name + "@" + t.self
	e, ok := t.entries[key]
	if !ok || e.Removed {
		return nil
	}
	e.Removed, e.Version, e.Expires = true, t.version(key), t.now().Add(TTL)
	t.entries[key] = e
	return t.save()
}

// Own returns the names this agent advertises.
func (t *Table) Own() map[string]Entry {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := map[string]Entry{}
	for _, e := range t.entries {
		if e.Host == t.self && !e.Removed {
			out[e.Name] = e
		}
	}
	return out
}

// Advertised reports whether this agent advertises name.
func (t *Table) Advertised(name string) bool {
	_, ok := t.Own()[name]
	return ok
}

// Renew advertises again the entries of this agent older than Refresh.
func (t *Table) Renew() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	changed := false
	now := t.now()
	for k, e := range t.entries {
		if e.Host != t.self || now.Sub(time.Unix(0, e.Version)) < Refresh {
			continue
		}
		e.Version, e.Expires = t.version(k), now.Add(TTL)
		t.entries[k] = e
		changed = true
	}
	if !changed {
		return nil
	}
	return t.save()
}

// Expire drops the entries nobody refreshed within TTL.
func (t *Table) Expire() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	changed := false
	for k, e := range t.entries {
		if t.now().After(e.Expires) {
			delete(t.entries, k)
			delete(t.direct, k)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return t.save()
}

// Live returns the entries of other agents which are advertised, by key.
func (t *Table) Live() []Entry {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []Entry
	for _, k := range sortedKeys(t.entries) {
		e := t.entries[k]
		if e.Host != t.self && !e.Removed && t.now().Before(e.Expires) {
			out = append(out, e)
		}
	}
	return out
}

// Listing is a live entry of another agent, with its host among the agents
// found on the private network.
type Listing struct {
	Entry
	// Key is name@agent with the host's listed name, or the entry's own key
	// when the host isn't on the private network now.
	Key string
	// Agent is the host, or nil when it isn't on the private network now.
	Agent *network.PeerInfo
}

// List returns the live entries of other agents with their hosts among
// peers, the agents found on the private network. The host is the agent on
// the entry's machine; an entry with no machine, which an older version
// relayed, falls back to the agent listed by its host's name. Entries that
// end up with the same key are listed once.
func (t *Table) List(peers []network.PeerInfo) []Listing {
	var out []Listing
	seen := map[string]bool{}
	for _, e := range t.Live() {
		l := Listing{Entry: e, Key: e.Key()}
		if i := slices.IndexFunc(peers, e.hostedBy); i >= 0 {
			l.Agent = &peers[i]
			l.Key = e.Name + "@" + peers[i].Name
		}
		if k := strings.ToLower(l.Key); !seen[k] {
			seen[k] = true
			out = append(out, l)
		}
	}
	return out
}

func (e Entry) hostedBy(p network.PeerInfo) bool {
	if e.Node != "" {
		return e.Node == p.Node
	}
	return strings.EqualFold(e.Host, p.Name)
}

// Lookup returns the live entry of key.
func (t *Table) Lookup(key string) (Entry, bool) {
	for _, e := range t.Live() {
		if e.Key() == key {
			return e, true
		}
	}
	return Entry{}, false
}

// Digest is the versions of the entries an agent has, by key.
type Digest map[string]int64

// Digest returns the versions of every entry.
func (t *Table) Digest() Digest {
	t.mu.Lock()
	defer t.mu.Unlock()
	d := Digest{}
	for k, e := range t.entries {
		d[k] = e.Version
	}
	return d
}

// Peer is an agent that gossips with this one.
type Peer struct {
	// Name is the agent's name as its agent card says, which its entries
	// carry as their host. It is empty when the sender is unknown.
	Name string
	// Node identifies its machine on the private network.
	Node string
}

// Compare returns the entries newer here than in the digest of the agent
// named peer, as its agent card says, and the keys peer has newer. Entries of this agent and keys
// of peer's own entries count as newer whenever their versions differ, so
// that an entry from its host replaces a relayed one of a higher version.
func (t *Table) Compare(peer string, d Digest) (newer []Entry, want []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, k := range sortedKeys(t.entries) {
		e := t.entries[k]
		v, ok := d[k]
		if e.Version > v || ok && e.Host == t.self && e.Version != v {
			newer = append(newer, e)
		}
	}
	for k, v := range d {
		e, ok := t.entries[k]
		_, host, _ := ParseKey(k)
		if !ok || v > e.Version || peer != "" && strings.EqualFold(host, peer) && v != e.Version {
			want = append(want, k)
		}
	}
	slices.Sort(want)
	return newer, want
}

// Get returns the entries of keys.
func (t *Table) Get(keys []string) []Entry {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []Entry
	for _, k := range keys {
		if e, ok := t.entries[k]; ok {
			out = append(out, e)
		}
	}
	return out
}

// Merge keeps the entries newer than the ones here, which peer sent.
// Entries of this agent only come from this agent, and invalid, expired or
// future ones are dropped. Entries of peer itself replace the ones relayed
// by others, and get peer's machine. A relayed entry never replaces one that
// came from its host. When the table
// or the host has no room left, see [Table.makeRoom], new entries are
// dropped. Merged entries are saved within saveDelay.
func (t *Table) Merge(peer Peer, entries []Entry) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	// Removals go first, so that the room they free takes the host's new
	// entries.
	entries = slices.Clone(entries)
	slices.SortStableFunc(entries, func(a, b Entry) int {
		switch {
		case a.Removed == b.Removed:
			return 0
		case a.Removed:
			return -1
		}
		return 1
	})
	changed := false
	for _, e := range entries {
		now := t.now()
		if strings.EqualFold(e.Host, t.self) || e.validate() != nil || now.After(e.Expires) || e.Expires.After(now.Add(TTL+maxSkew)) || e.Version > now.Add(maxSkew).UnixNano() {
			continue
		}
		key := e.Key()
		direct := peer.Name != "" && strings.EqualFold(e.Host, peer.Name)
		if direct {
			e.Node = peer.Node
		}
		cur, ok := t.entries[key]
		switch {
		case !ok && !t.makeRoom(e, direct):
			continue
		case !ok:
		case t.direct[key] && (!direct || cur.Version >= e.Version):
			continue
		case !t.direct[key] && !direct && cur.Version >= e.Version:
			continue
		}
		t.entries[key] = e
		if direct {
			t.direct[key] = true
		}
		changed = true
	}
	if !changed {
		return nil
	}
	return t.saveLater()
}

// makeRoom reports whether the table has room for the new entry e. A host
// has room for MaxPerHost entries and the table for MaxEntries; an entry
// from its host makes room by evicting a relayed one, or a removal of the
// host. t.mu is held.
func (t *Table) makeRoom(e Entry, direct bool) bool {
	evicts := direct && !e.Removed
	ofHost := func(_ string, cur Entry) bool { return strings.EqualFold(cur.Host, e.Host) }
	if t.count(ofHost) >= MaxPerHost {
		relayed := func(k string, cur Entry) bool { return ofHost(k, cur) && !t.direct[k] }
		removed := func(k string, cur Entry) bool { return ofHost(k, cur) && cur.Removed }
		return evicts && (t.evict(relayed) || t.evict(removed))
	}
	if len(t.entries) < MaxEntries {
		return true
	}
	return evicts && t.evict(func(k string, cur Entry) bool { return !t.direct[k] && !strings.EqualFold(cur.Host, t.self) })
}

// count returns the number of entries that match. t.mu is held.
func (t *Table) count(match func(key string, e Entry) bool) int {
	n := 0
	for k, e := range t.entries {
		if match(k, e) {
			n++
		}
	}
	return n
}

// evict drops an entry that matches, of the host with the most entries, and
// reports whether there was one. t.mu is held.
func (t *Table) evict(match func(key string, e Entry) bool) bool {
	perHost := map[string]int{}
	for _, e := range t.entries {
		perHost[strings.ToLower(e.Host)]++
	}
	victim, most := "", 0
	for _, k := range sortedKeys(t.entries) {
		e := t.entries[k]
		if n := perHost[strings.ToLower(e.Host)]; n > most && match(k, e) {
			victim, most = k, n
		}
	}
	if victim == "" {
		return false
	}
	delete(t.entries, victim)
	delete(t.direct, victim)
	return true
}
