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
// can't hide or alter the advertisements of others.
package gossip

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-serve/internal/fsutil"
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
	// maxSkew is how far ahead of this agent's clock another host's clock
	// may run. Versions and expiry times beyond it are refused, so that
	// an entry from the future can't block the host's real updates.
	maxSkew = time.Hour
)

// Entry advertises a pairing of a host agent to the private network.
type Entry struct {
	// Name is the pairing's name on its host, such as grok.
	Name string `json:"name"`
	// Host is the name of the agent hosting it on the private network.
	Host        string `json:"host"`
	Description string `json:"description,omitempty"`
	// Version orders the updates of an entry; the host sets it.
	Version int64     `json:"version"`
	Expires time.Time `json:"expires"`
	// Removed marks an entry the host stopped advertising, so that the
	// removal spreads like an update.
	Removed bool `json:"removed,omitempty"`
}

// Key is name@host, how other agents refer to the entry.
func (e Entry) Key() string { return e.Name + "@" + e.Host }

var namePart = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func (e Entry) validate() error {
	if !namePart.MatchString(e.Name) || !namePart.MatchString(e.Host) {
		return fmt.Errorf("invalid advertisement %q", e.Key())
	}
	if len(e.Description) > 1000 {
		return fmt.Errorf("advertisement %s has a too long description", e.Key())
	}
	return nil
}

// ParseKey splits name@host.
func ParseKey(key string) (name, host string, ok bool) {
	name, host, ok = strings.Cut(key, "@")
	if !ok || !namePart.MatchString(name) || !namePart.MatchString(host) {
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
	return fsutil.WriteAtomic(t.path, data, 0o600)
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

// Advertise adds or renews an entry of this agent.
func (t *Table) Advertise(name, description string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	e := Entry{Name: name, Host: t.self, Description: description, Expires: t.now().Add(TTL)}
	if err := e.validate(); err != nil {
		return err
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

// Compare returns the entries newer here than in the digest of the agent
// named peer, and the keys peer has newer. Entries of this agent and keys
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

// Merge keeps the entries newer than the ones here, which the agent named
// peer sent; peer is empty when the sender is unknown. Entries of this agent
// only come from this agent, and invalid, expired or future ones are
// dropped. Entries of peer itself replace the ones relayed by others. A
// relayed entry never replaces one that came from its host.
func (t *Table) Merge(peer string, entries []Entry) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	changed := false
	for _, e := range entries {
		now := t.now()
		if strings.EqualFold(e.Host, t.self) || e.validate() != nil || now.After(e.Expires) || e.Expires.After(now.Add(TTL+maxSkew)) || e.Version > now.Add(maxSkew).UnixNano() {
			continue
		}
		key := e.Key()
		direct := peer != "" && strings.EqualFold(e.Host, peer)
		cur, ok := t.entries[key]
		switch {
		case !ok && len(t.entries) >= MaxEntries:
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
	return t.save()
}
