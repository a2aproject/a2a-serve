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
	"log/slog"
	"maps"
	"slices"
	"sync"

	"github.com/a2aproject/a2a-serve/internal/agentprofile"
	"github.com/a2aproject/a2a-serve/internal/boundary"
	"github.com/a2aproject/a2a-serve/internal/egressrules"
	"github.com/a2aproject/a2a-serve/internal/sandboxrt"
)

// egressSockets decides the connections of the agent's containers with
// the agent's current settings, through the boundaries it opens for them,
// and keeps what it knows about them.
type egressSockets struct {
	settings   func() agentprofile.Egress
	blocks     *boundary.Blocks
	boundaries *sandboxrt.Boundaries

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
	// ca signs the certificates of the hosts that get headers.
	ca *boundary.CA
}

// watch adds fellBack, which tells why a container has Docker's own
// network, to what status reports, until forget.
func (e *egressSockets) watch(fellBack func() string) (forget func()) {
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
func (e *egressSockets) fellBack() string {
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

func newEgressSockets(dir string, settings func() agentprofile.Egress) *egressSockets {
	blocks := &boundary.Blocks{}
	return &egressSockets{settings: settings, blocks: blocks, boundaries: sandboxrt.NewBoundaries(dir, sandboxrt.SocketIDLen, idAlphabet, blocks), fallbacks: map[int]func() string{}}
}

// opener returns the function that opens the boundary of the container
// named name, which keeps mode and the headers of inject until it
// restarts. Changes of the allow rules apply to its next connection.
func (e *egressSockets) opener(name, mode string, inject []boundary.Injection, ca *boundary.CA) sandboxrt.OpenEgress {
	allow := injectAllow(inject)
	return e.boundaries.Opener(name, sandboxrt.BoundaryRules{Policy: func() egressrules.Policy { return e.policy(mode, allow...) }, Inject: inject, CA: ca})
}

// policy returns the policy of mode with the allow rules of the current
// settings and extra. Rules that the runner can't read allow nothing, as
// the caller validated them before.
func (e *egressSockets) policy(mode string, extra ...string) egressrules.Policy {
	p, err := egressrules.NewPolicy(mode, append(slices.Clone(e.settings().Allow), extra...))
	if err != nil {
		slog.Error("reading the egress rules; the sandbox reaches nothing until they are fixed", "error", err)
		return egressrules.Policy{Mode: egressrules.ModeAllowlist}
	}
	return p
}

// started records the egress mode a container started with, and the
// problem that kept it from the boundary.
func (e *egressSockets) started(mode, problem string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.mode, e.problem, e.prepared = cmp.Or(mode, egressrules.ModeInternet), problem, true
}

// state returns what the runner knows of the egress of the containers.
func (e *egressSockets) state() EgressState {
	fellBack := e.fellBack()
	e.mu.Lock()
	st := EgressState{Started: e.prepared, Mode: e.mode, Problem: cmp.Or(e.problem, fellBack)}
	e.mu.Unlock()
	st.Blocked, st.Recent = e.blocks.Snapshot()
	return st
}

// egressSandbox gives spec the egress socket of its container, whose
// boundary adds the headers of inject.
func (r *Runner) egressSandbox(spec *agentprofile.Spec, cfg Spec, inject []boundary.Injection) error {
	spec.EgressMode = cfg.EgressMode
	spec.EgressAllow = cfg.EgressAllow
	spec.Inject = cfg.Inject
	var ca *boundary.CA
	if len(inject) > 0 {
		var err error
		if ca, err = r.egress.authority(injectHosts(cfg.Inject)); err != nil {
			return err
		}
		spec.EgressCA = ca.PEM()
	}
	spec.OpenEgress = r.egress.opener(spec.Base.Name, spec.EgressMode, inject, ca)
	return nil
}

// authority returns the CA of the hosts that get headers, making one at
// the first call, or when the rules name a host it doesn't cover. A
// container keeps the CA it started with.
func (e *egressSockets) authority(hosts []string) (*boundary.CA, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ca != nil && !slices.ContainsFunc(hosts, func(h string) bool { return !e.ca.Covers(h) }) {
		return e.ca, nil
	}
	ca, err := boundary.NewCA(hosts)
	if err != nil {
		return nil, err
	}
	e.ca = ca
	return ca, nil
}
