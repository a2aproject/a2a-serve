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
type RemovedAgents struct {
	path string
	mu   sync.Mutex
}

// NewRemovedAgents returns the removed agents kept in the file at path.
func NewRemovedAgents(path string) *RemovedAgents {
	return &RemovedAgents{path: path}
}

// load returns the peers of the removed agents by agent name.
func (r *RemovedAgents) load() (map[string]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.read()
}

// update applies fn to the peers by agent name and saves the result.
func (r *RemovedAgents) update(fn func(map[string]string)) error {
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
		return fmt.Errorf("saving the removed agents: %w", err)
	}
	return nil
}

func (r *RemovedAgents) read() (map[string]string, error) {
	data, err := os.ReadFile(r.path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the removed agents: %w", err)
	}
	peers := map[string]string{}
	if err := json.Unmarshal(data, &peers); err != nil {
		return nil, fmt.Errorf("reading the removed agents in %s: %w", r.path, err)
	}
	return peers, nil
}
