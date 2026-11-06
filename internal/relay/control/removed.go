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

package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"sync"

	"github.com/a2aproject/a2a-serve/internal/fsutil"
)

// RemovedAgents remembers the peer of each removed invited agent by the
// agent's name. Agent Mesh forgets what a banned node serves, so without it
// a new invite couldn't tell which removed agent to let back.
type RemovedAgents struct{ stringMap }

// NewRemovedAgents returns the removed agents kept in the file at path.
func NewRemovedAgents(path string) *RemovedAgents {
	return &RemovedAgents{stringMap{path: path, what: "the removed agents"}}
}

// InviteNames remembers the name of the invite each invited agent joined
// with, by its peer. Agent Mesh forgets which token enrolled a node some
// time after the node joins, and the agent stays on the relay longer.
type InviteNames struct{ stringMap }

// NewInviteNames returns the invite names kept in the file at path.
func NewInviteNames(path string) *InviteNames {
	return &InviteNames{stringMap{path: path, what: "the invite names"}}
}

// stringMap is a map of strings kept in a JSON file.
type stringMap struct {
	path string
	what string
	mu   sync.Mutex
}

// load returns the map.
func (r *stringMap) load() (map[string]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.read()
}

// update applies fn to the map and saves the result.
func (r *stringMap) update(fn func(map[string]string)) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	peers, err := r.read()
	if err != nil {
		return err
	}
	before := maps.Clone(peers)
	fn(peers)
	if maps.Equal(before, peers) {
		return nil
	}
	data, err := json.Marshal(peers)
	if err != nil {
		return err
	}
	if err := fsutil.WriteAtomic(r.path, data, 0o600); err != nil {
		return fmt.Errorf("saving %s: %w", r.what, err)
	}
	return nil
}

func (r *stringMap) read() (map[string]string, error) {
	data, err := os.ReadFile(r.path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", r.what, err)
	}
	peers := map[string]string{}
	if err := json.Unmarshal(data, &peers); err != nil {
		return nil, fmt.Errorf("reading %s in %s: %w", r.what, r.path, err)
	}
	return peers, nil
}
