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
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"slices"
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
	prepared      bool
	// fallbacks tell whether the containers that run fell back to Docker's
	// own network, keyed by the order they were added in.
	fallbacks map[int]func() string
	nextWatch int
	// relays takes the connections of relay containers, which reach it at
	// relayAddr, mounting relayMount on Linux.
	relays                *sandboxnet.RelayServer
	relayAddr, relayMount string
	// launcherCopy is the copy of the running a2a-serve.
	launcherCopy string

	// probeMu serializes the check of the containers' egress setup, which
	// runs once for the server, and probed and probeErr are its result.
	probeMu  sync.Mutex
	probed   bool
	probeErr error
}

// probeOnce runs probe, the check of a container's egress setup, at the
// first call, and returns its result to every call. Each start of the
// server checks again.
func (e *egressSockets) probeOnce(probe func() error) error {
	e.probeMu.Lock()
	defer e.probeMu.Unlock()
	if !e.probed {
		e.probeErr, e.probed = probe(), true
	}
	return e.probeErr
}

// watch adds fellBack, which tells whether a container fell back to
// Docker's own network, to what status reports, until forget.
func (e *egressSockets) watch(fellBack func() string) (forget func()) {
	if fellBack == nil {
		return func() {}
	}
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
	return &egressSockets{dir: dir, config: config, blocks: &sandboxnet.Blocks{}, fallbacks: map[int]func() string{}}
}

// opener returns the function that opens the boundary of the container
// named sandbox, which keeps mode until it restarts. Changes of the allow
// rules apply to its next connection.
func (e *egressSockets) opener(sandbox, mode string) harness.OpenEgress {
	return func(gateway harness.HostGatewayFunc, relay bool) (harness.EgressEndpoint, error) {
		cfg := sandboxnet.BoundaryConfig{Sandbox: sandbox, Policy: func() egress.Policy { return e.policy(mode) }, HostGateway: gateway, Blocks: e.blocks}
		if relay {
			return e.openRelayed(cfg)
		}
		if err := fsutil.MkdirPrivate(e.dir); err != nil {
			return harness.EgressEndpoint{}, err
		}
		dir := filepath.Join(e.dir, cryptoutil.RandomString(agentdir.HarnessSocketIDLen, handleAlphabet))
		if err := os.Mkdir(dir, 0o700); err != nil {
			return harness.EgressEndpoint{}, err
		}
		sock := filepath.Join(dir, sandboxnet.SocketName)
		b, err := sandboxnet.Listen(sock, cfg)
		if err != nil {
			return harness.EgressEndpoint{}, errors.Join(err, os.RemoveAll(dir))
		}
		return harness.EgressEndpoint{Socket: sock, Close: func() error { return errors.Join(b.Close(), os.RemoveAll(dir)) }}, nil
	}
}

// openRelayed opens a boundary that a relay container reaches through the
// server's relay listener, with a key of its own.
func (e *egressSockets) openRelayed(cfg sandboxnet.BoundaryConfig) (harness.EgressEndpoint, error) {
	relays, addr, mount, err := e.relayListener()
	if err != nil {
		return harness.EgressEndpoint{}, err
	}
	b, err := sandboxnet.New(cfg)
	if err != nil {
		return harness.EgressEndpoint{}, err
	}
	key, unregister, err := relays.Register(b)
	if err != nil {
		return harness.EgressEndpoint{}, errors.Join(err, b.Close())
	}
	return harness.EgressEndpoint{RelayAddr: addr, RelayKey: key, RelayMount: mount, Close: func() error {
		unregister()
		return b.Close()
	}}, nil
}

// relayListener returns the server's listener for relay containers,
// listening on the first call. With Docker Desktop it is a TCP port on
// the host's loopback, which containers reach at host.docker.internal. On
// Linux, where containers can't reach the host's loopback, it is a socket
// that relays mount.
func (e *egressSockets) relayListener() (relays *sandboxnet.RelayServer, addr, mount string, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.relays != nil {
		return e.relays, e.relayAddr, e.relayMount, nil
	}
	var ln net.Listener
	if bind, dial, tcp := harness.TCPBridge(harness.SandboxDocker); tcp {
		if ln, err = net.Listen("tcp", net.JoinHostPort(bind, "0")); err != nil {
			return nil, "", "", fmt.Errorf("listening for egress containers: %w", err)
		}
		_, port, err := net.SplitHostPort(ln.Addr().String())
		if err != nil {
			return nil, "", "", errors.Join(err, ln.Close())
		}
		addr = "tcp://" + net.JoinHostPort(dial, port)
	} else {
		mount = filepath.Join(e.dir, "relay")
		if err := fsutil.MkdirPrivate(e.dir); err != nil {
			return nil, "", "", err
		}
		if err := fsutil.MkdirPrivate(mount); err != nil {
			return nil, "", "", err
		}
		if ln, err = net.Listen("unix", filepath.Join(mount, relaySocketName)); err != nil {
			return nil, "", "", fmt.Errorf("listening for egress containers: %w", err)
		}
		addr = "unix://" + harness.RelayEgressDir + "/" + relaySocketName
	}
	e.relays, e.relayAddr, e.relayMount = sandboxnet.ServeRelays(ln), addr, mount
	return e.relays, addr, mount, nil
}

// relaySocketName is the name of the socket of the relay listener on Linux.
const relaySocketName = "relay.sock"

// close stops the relay listener.
func (e *egressSockets) close(context.Context) error {
	e.mu.Lock()
	relays := e.relays
	e.relays = nil
	e.mu.Unlock()
	if relays == nil {
		return nil
	}
	return relays.Close()
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
	e.mode, e.problem, e.prepared = cmp.Or(mode, egress.ModeInternet), problem, true
}

// boundary reports whether the containers of cfg get the egress boundary:
// as the last container started, or as [harness.DecideEgress] decides
// before the first.
func (e *egressSockets) boundary(cfg *agentdir.Config) bool {
	if e == nil {
		return harness.DecideEgress(cfg, harness.HostEgressEnv()).Boundary
	}
	fellBack := e.fellBack()
	e.mu.Lock()
	prepared, problem := e.prepared, cmp.Or(e.problem, fellBack)
	e.mu.Unlock()
	if prepared {
		return problem == "" && cfg.Sandbox != harness.SandboxLocal && cfg.Egress.Mode != egress.ModeUnrestricted
	}
	return harness.DecideEgress(cfg, harness.HostEgressEnv()).Boundary
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
	if st.Problem == "" {
		st.Problem = harness.DecideEgress(cfg, harness.HostEgressEnv()).Warning
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

// launcherDir and launcherFile are the copy of the running a2a-serve, next
// to the egress sockets, that containers run as their launcher, so that a
// binary upgraded in place doesn't change under a running server. The
// directory is emptied at each start and stays out of backups. The copy is
// executable by everyone: with Docker, container root runs it without
// CAP_DAC_OVERRIDE, as another user than its owner. It holds no secret.
const (
	launcherDir  = "launcher"
	launcherFile = "a2a-init"
)

// launcher copies the running a2a-serve once, and returns the copy, or ""
// to use the binary itself where there is no copy.
func (e *egressSockets) launcher() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.launcherCopy != "" || runtime.GOOS != "linux" {
		return e.launcherCopy
	}
	if err := fsutil.MkdirPrivate(e.dir); err != nil {
		slog.Warn("copying a2a-serve for the containers; they run the installed binary", "error", err)
		return ""
	}
	dir := filepath.Join(e.dir, launcherDir)
	if err := os.Mkdir(dir, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		slog.Warn("copying a2a-serve for the containers; they run the installed binary", "error", err)
		return ""
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		slog.Warn("copying a2a-serve for the containers; they run the installed binary", "error", err)
		return ""
	}
	dst := filepath.Join(dir, launcherFile)
	// /proc/self/exe is the binary this process runs, even after an upgrade
	// replaced its file.
	if err := copyExecutable("/proc/self/exe", dst); err != nil {
		slog.Warn("copying a2a-serve for the containers; they run the installed binary", "error", err)
		return ""
	}
	e.launcherCopy = dst
	return dst
}

// copyExecutable copies src to dst atomically, executable by everyone.
func copyExecutable(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, in.Close()) }()
	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+"-*")
	if err != nil {
		return err
	}
	if _, err := io.Copy(tmp, in); err != nil {
		return errors.Join(err, tmp.Close(), os.Remove(tmp.Name()))
	}
	if err := tmp.Chmod(0o755); err != nil {
		return errors.Join(err, tmp.Close(), os.Remove(tmp.Name()))
	}
	if err := tmp.Close(); err != nil {
		return errors.Join(err, os.Remove(tmp.Name()))
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return errors.Join(err, os.Remove(tmp.Name()))
	}
	return nil
}

// egressSandbox gives spec the egress socket of its container.
func (s *Server) egressSandbox(spec *harness.Spec) {
	cfg := s.rt.Config()
	if cfg.Sandbox != harness.SandboxLocal && cfg.Egress.Mode != egress.ModeUnrestricted {
		spec.Launcher = s.egress.launcher()
	}
	spec.EgressMode = s.rt.Config().Egress.Mode
	spec.EgressAllow = s.rt.Config().Egress.Allow
	spec.ProbeEgress = s.egress.probeOnce
	spec.OpenEgress = s.egress.opener(spec.Name, spec.EgressMode)
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
