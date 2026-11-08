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
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sync"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/cryptoutil"
	"github.com/a2aproject/a2a-serve/internal/egress"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
	"github.com/a2aproject/a2a-serve/internal/harness"
	"github.com/a2aproject/a2a-serve/internal/sandboxnet"
)

// egressSockets opens the egress socket of each container, each alone in
// a directory of dir, and decides their connections with the agent's
// current config.
type egressSockets struct {
	dir    string
	config func() *agentdir.Config
	blocks *sandboxnet.Blocks

	mu sync.Mutex
	// mode is the egress mode the containers started with, and problem
	// why they have Docker's own network although the mode asks for the
	// boundary.
	mode, problem string
}

func newEgressSockets(dir string, config func() *agentdir.Config) *egressSockets {
	return &egressSockets{dir: dir, config: config, blocks: &sandboxnet.Blocks{}}
}

// opener returns the function that opens the egress socket of the
// container named sandbox, which keeps mode until it restarts. Changes of
// the allow rules apply to its next connection.
func (e *egressSockets) opener(sandbox, mode string) harness.OpenEgress {
	return func(gateway harness.HostGatewayFunc) (string, func() error, error) {
		if err := fsutil.MkdirPrivate(e.dir); err != nil {
			return "", nil, err
		}
		dir := filepath.Join(e.dir, cryptoutil.RandomString(agentdir.HarnessSocketIDLen, handleAlphabet))
		if err := os.Mkdir(dir, 0o700); err != nil {
			return "", nil, err
		}
		sock := filepath.Join(dir, sandboxnet.SocketName)
		policy := func() egress.Policy { return e.policy(mode) }
		b, err := sandboxnet.Listen(sock, sandboxnet.BoundaryConfig{Sandbox: sandbox, Policy: policy, HostGateway: gateway, Blocks: e.blocks})
		if err != nil {
			return "", nil, errors.Join(err, os.RemoveAll(dir))
		}
		return sock, func() error { return errors.Join(b.Close(), os.RemoveAll(dir)) }, nil
	}
}

// policy returns the policy of mode with the allow rules of the current
// config. Rules that the server can't read allow nothing, as the server
// validated them before.
func (e *egressSockets) policy(mode string) egress.Policy {
	p, err := egress.NewPolicy(mode, e.config().Egress.Allow)
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
	e.mode, e.problem = cmp.Or(mode, egress.ModeInternet), problem
}

// status returns what status shows about the sandbox's connections: the
// mode the containers run with, the current allow rules, and the latest
// refused destinations that the rules still refuse.
func (e *egressSockets) status(cfg *agentdir.Config) *controlapi.EgressStatus {
	if cfg.Sandbox == harness.SandboxLocal && cfg.Egress.Mode == "" && len(cfg.Egress.Allow) == 0 {
		return nil
	}
	e.mu.Lock()
	st := &controlapi.EgressStatus{Mode: cmp.Or(e.mode, cfg.Egress.Mode, egress.ModeInternet), Allow: cfg.Egress.Allow, Problem: e.problem}
	e.mu.Unlock()
	if st.Problem == "" {
		st.Problem = harness.EgressWarning(cfg, harness.Launcher)
	}
	var recent []string
	st.Blocked, recent = e.blocks.Snapshot()
	p := e.policy(st.Mode)
	for _, dest := range recent {
		if !allowedByRule(p, dest) {
			st.Recent = append(st.Recent, dest)
		}
	}
	return st
}

// allowedByRule reports whether a rule of p allows the destination host:port.
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

// egressSandbox gives spec the egress socket of its container.
func (s *Server) egressSandbox(spec *harness.Spec) {
	spec.EgressMode = s.rt.Config().Egress.Mode
	spec.OpenEgress = s.egress.opener(spec.Name, spec.EgressMode)
}
