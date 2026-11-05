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

package agentdir

import (
	"slices"
)

// Conflict is an agent setting, or an entry of a list setting, changed both
// locally and in the recipe, where the local value is kept.
type Conflict struct {
	Setting string `json:"setting"`
	// Key names the entry of a list setting, such as the destination of a
	// mount.
	Key string `json:"key,omitempty"`
}

// Merge3 merges the agent settings changed from base to upstream into
// local, setting by setting: a setting local didn't change takes the value
// of upstream, and a setting upstream didn't change keeps the local value.
// Lists merge entry by entry, with the keys `config set` uses. When both
// changed a setting or entry differently, the local value stays and it is
// returned as a conflict. Owner settings stay as they are locally.
func Merge3(base, local, upstream *Config) (*Config, []Conflict) {
	out := local.Clone()
	var conflicts []Conflict
	for _, f := range fields {
		if f.scope != scopeAgent {
			continue
		}
		b, l, u := f.get(base), f.get(local), f.get(upstream)
		if !f.list {
			v, ok := merge3(entry(b), entry(l), entry(u))
			if !ok {
				conflicts = append(conflicts, Conflict{Setting: f.flag})
			}
			f.put(out, v.values())
			continue
		}
		merged, keys := mergeList(b, l, u, f.key)
		for _, k := range keys {
			conflicts = append(conflicts, Conflict{Setting: f.flag, Key: k})
		}
		f.put(out, merged)
	}
	return out, conflicts
}

// maybe is a value that may be absent.
type maybe struct {
	v  string
	ok bool
}

func entry(values []string) maybe {
	if len(values) == 0 {
		return maybe{}
	}
	return maybe{v: values[0], ok: true}
}

func (m maybe) values() []string {
	if !m.ok {
		return nil
	}
	return []string{m.v}
}

// merge3 merges one value. It reports false for a conflict, keeping l.
func merge3(b, l, u maybe) (maybe, bool) {
	switch {
	case l == b:
		return u, true
	case u == b, u == l:
		return l, true
	}
	return l, false
}

// mergeList merges list entries by key, in the local order with entries new
// in upstream after them. It returns the keys of conflicting entries.
func mergeList(b, l, u []string, key func(string) string) ([]string, []string) {
	index := func(list []string) map[string]string {
		m := make(map[string]string, len(list))
		for _, e := range list {
			m[key(e)] = e
		}
		return m
	}
	bm, lm, um := index(b), index(l), index(u)
	var order []string
	for _, list := range [][]string{l, u, b} {
		for _, e := range list {
			if k := key(e); !slices.Contains(order, k) {
				order = append(order, k)
			}
		}
	}
	var out, conflicts []string
	for _, k := range order {
		get := func(m map[string]string) maybe {
			v, ok := m[k]
			return maybe{v: v, ok: ok}
		}
		v, ok := merge3(get(bm), get(lm), get(um))
		if !ok {
			conflicts = append(conflicts, k)
		}
		if v.ok {
			out = append(out, v.v)
		}
	}
	return out, conflicts
}

// UnusedSecrets returns the names of the stored secrets old uses and next
// doesn't, to delete.
func UnusedSecrets(old, next *Config) []string {
	after := next.referencedSecrets()
	var out []string
	for name := range old.referencedSecrets() {
		if !after[name] {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}
