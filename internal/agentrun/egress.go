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

package agentrun

import (
	"cmp"
	"maps"
	"slices"
	"sync"

	"github.com/a2aproject/a2a-serve/internal/egressrules"
)

// egressState keeps what the runner knows about the egress of the agent's
// containers.
type egressState struct {
	mu sync.Mutex
	// mode is the egress mode the containers started with, and problem
	// why they have Docker's own network although the mode asks for the
	// boundary.
	mode, problem string
	prepared      bool
	// fallbacks tell whether the containers that run fell back to Docker's
	// own network, keyed by the order they were added in.
	fallbacks map[int]func() string
	nextWatch int
}

func newEgressState() *egressState {
	return &egressState{fallbacks: map[int]func() string{}}
}

// watch adds fellBack, which tells why a container has Docker's own
// network, to what status reports, until forget.
func (e *egressState) watch(fellBack func() string) (forget func()) {
	e.mu.Lock()
	defer e.mu.Unlock()
	id := e.nextWatch
	e.nextWatch++
	e.fallbacks[id] = fellBack
	return func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		delete(e.fallbacks, id)
	}
}

// fellBack returns why a container fell back to Docker's own network, or "".
func (e *egressState) fellBack() string {
	e.mu.Lock()
	fns := slices.Collect(maps.Values(e.fallbacks))
	e.mu.Unlock()
	for _, f := range fns {
		if w := f(); w != "" {
			return w
		}
	}
	return ""
}

// started records the egress mode a container started with, and the
// problem that kept it from the boundary.
func (e *egressState) started(mode, problem string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.mode, e.problem, e.prepared = cmp.Or(mode, egressrules.ModeInternet), problem, true
}

// state returns what the runner knows of the egress of the containers.
func (e *egressState) state() EgressState {
	fellBack := e.fellBack()
	e.mu.Lock()
	defer e.mu.Unlock()
	return EgressState{Started: e.prepared, Mode: e.mode, Problem: cmp.Or(e.problem, fellBack)}
}
