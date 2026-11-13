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

package agentsrv

import (
	"cmp"
	"log/slog"
	"maps"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/egress"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
	"github.com/a2aproject/a2a-serve/internal/harness"
	"github.com/a2aproject/a2a-serve/internal/sandbox"
	"github.com/a2aproject/a2a-serve/internal/sandboxnet"
)

// egressSockets decides the connections of the agent's containers with
// the agent's current config, through the boundaries it opens for them,
// and keeps what status reports about them.
type egressSockets struct {
	config     func() *agentdir.Config
	blocks     *sandboxnet.Blocks
	boundaries *sandbox.Boundaries

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
	ca *sandboxnet.CA
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

func newEgressSockets(dir string, config func() *agentdir.Config) *egressSockets {
	blocks := &sandboxnet.Blocks{}
	return &egressSockets{config: config, blocks: blocks, boundaries: sandbox.NewBoundaries(dir, agentdir.HarnessSocketIDLen, handleAlphabet, blocks), fallbacks: map[int]func() string{}}
}

// opener returns the function that opens the boundary of the container
// named name, which keeps mode and the headers of inject until it
// restarts. Changes of the allow rules apply to its next connection.
func (e *egressSockets) opener(name, mode string, inject []sandboxnet.Injection, ca *sandboxnet.CA) sandbox.OpenEgress {
	allow := injectAllow(inject)
	return e.boundaries.Opener(name, sandbox.BoundaryRules{Policy: func() egress.Policy { return e.policy(mode, allow...) }, Inject: inject, CA: ca})
}

// policy returns the policy of mode with the allow rules of the current
// config and extra. Rules that the server can't read allow nothing, as the
// server validated them before.
func (e *egressSockets) policy(mode string, extra ...string) egress.Policy {
	p, err := egress.NewPolicy(mode, append(slices.Clone(e.config().Egress.Allow), extra...))
	if err != nil {
		slog.Error("reading the egress rules; the sandbox reaches nothing until they are fixed", "error", err)
		return egress.Policy{Mode: egress.ModeAllowlist}
	}
	return p
}

// started records the egress mode a container started with, and the
// problem that kept it from the boundary.
func (e *egressSockets) started(mode, problem string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.mode, e.problem, e.prepared = cmp.Or(mode, egress.ModeInternet), problem, true
}

// boundary reports whether the containers of cfg get the egress boundary:
// as the last container started, or as [harness.DecideEgress] decides
// before the first.
func (e *egressSockets) boundary(cfg *agentdir.Config) bool {
	if e == nil {
		return harness.HostEgress(cfg).Boundary
	}
	fellBack := e.fellBack()
	e.mu.Lock()
	prepared, problem := e.prepared, cmp.Or(e.problem, fellBack)
	e.mu.Unlock()
	if prepared {
		return problem == "" && cfg.Sandbox != harness.SandboxLocal && cfg.Egress.Mode != egress.ModeUnrestricted
	}
	return harness.HostEgress(cfg).Boundary
}

// status returns what status shows about the sandbox's connections: the
// mode the containers run with, the current allow rules, and the latest
// refused destinations.
func (e *egressSockets) status(cfg *agentdir.Config) *controlapi.EgressStatus {
	if cfg.Sandbox == harness.SandboxLocal && cfg.Egress.Mode == "" && len(cfg.Egress.Allow) == 0 {
		return nil
	}
	fellBack := e.fellBack()
	e.mu.Lock()
	st := &controlapi.EgressStatus{Mode: cmp.Or(e.mode, cfg.Egress.Mode, egress.ModeInternet), Allow: cfg.Egress.Allow, Problem: cmp.Or(e.problem, fellBack)}
	e.mu.Unlock()
	for _, r := range cfg.Inject {
		st.Inject = append(st.Inject, controlapi.EgressInjection{Host: r.Target(), Secrets: r.Secrets()})
	}
	if st.Problem == "" {
		st.Problem = harness.HostEgress(cfg).Warning
	}
	var recent []sandboxnet.Block
	st.Blocked, recent = e.blocks.Snapshot()
	p := e.policy(st.Mode)
	for _, b := range recent {
		st.Recent = append(st.Recent, controlapi.EgressBlock{Destination: b.Destination, Allow: b.Allow, Unresolved: b.Unresolved, Allowed: !b.Unresolved && allowedByRule(p, b.Allow)})
	}
	return st
}

// allowedByRule reports whether a rule of p names the destination host:port.
func allowedByRule(p egress.Policy, dest string) bool {
	host, port, err := net.SplitHostPort(dest)
	if err != nil {
		return false
	}
	n, err := egress.ParsePort(port)
	if err != nil {
		return false
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		_, ok := p.AddrRule(addr, n)
		return ok
	}
	_, ok := p.NameRule(host, n)
	return ok
}

// egressSandbox gives spec the egress socket of its container, whose
// boundary adds the headers of inject.
func (s *Server) egressSandbox(spec *harness.Spec, inject []sandboxnet.Injection) error {
	cfg := s.rt.Config()
	spec.EgressMode = cfg.Egress.Mode
	spec.EgressAllow = cfg.Egress.Allow
	spec.Inject = cfg.Inject
	var ca *sandboxnet.CA
	if len(inject) > 0 {
		var err error
		if ca, err = s.egress.authority(injectHosts(cfg)); err != nil {
			return err
		}
		spec.EgressCA = ca.PEM()
	}
	spec.OpenEgress = s.egress.opener(spec.Base.Name, spec.EgressMode, inject, ca)
	return nil
}

// authority returns the CA of the hosts that get headers, making one at
// the first call, or when the rules name a host it doesn't cover. A
// container keeps the CA it started with.
func (e *egressSockets) authority(hosts []string) (*sandboxnet.CA, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ca != nil && !slices.ContainsFunc(hosts, func(h string) bool { return !e.ca.Covers(h) }) {
		return e.ca, nil
	}
	ca, err := sandboxnet.NewCA(hosts)
	if err != nil {
		return nil, err
	}
	e.ca = ca
	return ca, nil
}

// egressNoticeFile records in the state directory that start told the
// owner that the sandbox blocks private networks.
const egressNoticeFile = "egress-notice"

// noticeEgress tells the owner of an agent that an earlier version created
// once, at the first start with the egress boundary, that the sandbox now
// blocks the LAN and the tailnet. A new agent only records it.
func (s *Server) noticeEgress(p *harness.Prepared) {
	cfg := s.rt.Config()
	if !p.Egress || cfg.Egress.Mode == egress.ModeAllowlist {
		return
	}
	marker := filepath.Join(s.dir.StateDir(), egressNoticeFile)
	if _, err := os.Stat(marker); err == nil {
		return
	}
	if err := fsutil.WriteAtomic(marker, nil, 0o600); err != nil {
		slog.Warn("recording the egress notice", "error", err)
		return
	}
	if !s.existed {
		return
	}
	s.logf("! the sandbox now blocks the LAN, the tailnet and this machine; allow a host with a2a-serve config set --egress-allow <host>")
}
