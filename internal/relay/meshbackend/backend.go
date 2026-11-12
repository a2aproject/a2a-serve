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

// Package meshbackend is the Agent Mesh backend of a relay: it runs Agent
// Mesh's agentmesh-one on loopback, and keeps the relay's members with its
// admin API.
package meshbackend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/a2aproject/a2a-serve/internal/agentmesh"
	"github.com/a2aproject/a2a-serve/internal/relay"
	"github.com/a2aproject/a2a-serve/internal/relay/control"
	"github.com/a2aproject/a2a-serve/internal/relay/front"
	"github.com/a2aproject/a2a-serve/internal/relay/meshadmin"
	"github.com/a2aproject/a2a-serve/internal/relay/signin"
	"github.com/a2aproject/a2a-serve/internal/relaydir"
)

// readyTimeout bounds how long agentmesh-one takes to become ready. A first
// start on PostgreSQL migrates the database.
const readyTimeout = 2 * time.Minute

// readyProbeTimeout bounds one readiness probe. A process that took the port
// of agentmesh-one may accept connections and never answer.
const readyProbeTimeout = time.Second

// startAttempts bounds how often Start starts agentmesh-one again on another
// port.
const startAttempts = 3

// Options configure a [Backend].
type Options struct {
	// Binary is the agentmesh-one to run.
	Binary string
	// Log gets the output of agentmesh-one.
	Log io.Writer
	Now func() time.Time
}

// Backend runs agentmesh-one behind a relay.
type Backend struct {
	opts Options
}

// New returns the Agent Mesh backend.
func New(opts Options) *Backend {
	return &Backend{opts: opts}
}

var _ relay.Backend = (*Backend)(nil)

// Name returns "Agent Mesh".
func (*Backend) Name() string { return "Agent Mesh" }

// Version returns the Agent Mesh version the relay runs.
func (*Backend) Version() string { return agentmesh.Version }

// Prepare upgrades the data of agentmesh-one to Version, after a backup.
func (*Backend) Prepare(ctx context.Context, d relaydir.Dir, conf *relaydir.Resolved, now time.Time) (relay.Prepared, error) {
	up, err := PrepareData(ctx, d, agentmesh.Version, conf.Database != "", now)
	if err != nil {
		return relay.Prepared{}, err
	}
	p := relay.Prepared{To: up.To, Backup: up.Backup}
	if up.Upgraded() {
		p.From = up.From
	}
	return p, nil
}

// Info reports the versions the relay runs, which agents check before they
// join.
func (*Backend) Info(a2aServe string, signIn bool) any {
	return agentmesh.Info{A2AServe: a2aServe, AgentMesh: agentmesh.Version, SignIn: signIn}
}

// Routes describes agentmesh-one's routes.
func (*Backend) Routes() front.Routes { return Routes{} }

// errMeshPortTaken is returned when agentmesh-one exited while starting
// because another process took the port picked for it.
var errMeshPortTaken = errors.New("another process took the port of agentmesh-one")

// Start starts agentmesh-one, applies the relay's policy to it, and returns
// once it is ready.
func (b *Backend) Start(ctx context.Context, s relay.Start) (relay.Running, error) {
	if s.Issuer != "" {
		if err := checkIssuer(ctx, s.URL); err != nil {
			return nil, err
		}
	}
	for attempt := 1; ; attempt++ {
		r, err := b.start(ctx, s)
		if !errors.Is(err, errMeshPortTaken) || attempt == startAttempts {
			return r, err
		}
		slog.Debug("starting agentmesh-one again on another port", "error", err)
	}
}

func (b *Backend) start(ctx context.Context, s relay.Start) (relay.Running, error) {
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	meshURL := &url.URL{Scheme: "http", Host: net.JoinHostPort("127.0.0.1", strconv.Itoa(port))}
	admin := meshadmin.New(meshURL.String(), s.Config.AdminToken, &http.Client{Timeout: 30 * time.Second})
	svc := NewService(ServiceOptions{
		Mesh:        admin,
		Removed:     NewRemovedAgents(s.Dir.RemovedAgentsPath()),
		InviteNames: NewInviteNames(s.Dir.InviteNamesPath()),
		Policy:      Policy(s.Config.SignIn()),
		Status:      s.Status,
		Admits:      s.Admits,
		Now:         b.opts.Now,
	})
	mesh, err := b.startMesh(ctx, MeshConfig{URL: s.URL, Port: port, DataDir: s.Dir.MeshDir(), AdminToken: s.Config.AdminToken, Database: s.Config.Database, Issuer: s.Issuer})
	if err != nil {
		return nil, err
	}
	r := &running{upstream: meshURL, svc: svc, mesh: mesh}
	if err := waitReady(ctx, admin, mesh, port); err != nil {
		return nil, errors.Join(err, mesh.stop())
	}
	if err := svc.ApplyPolicy(ctx); err != nil {
		return nil, errors.Join(err, mesh.stop())
	}
	return r, nil
}

// running is a started agentmesh-one.
type running struct {
	upstream *url.URL
	svc      *Service
	mesh     *meshProcess
}

func (r *running) Upstream() *url.URL       { return r.upstream }
func (r *running) Service() control.Service { return r.svc }
func (r *running) Exited() <-chan struct{}  { return r.mesh.exited }
func (r *running) Stop() error              { return r.mesh.stop() }

func (r *running) Reconcile(ctx context.Context) (removed, readmitted []control.Member, err error) {
	return r.svc.Reconcile(ctx)
}

func (r *running) Err() error {
	return fmt.Errorf("agentmesh-one exited: %v", r.mesh.err)
}

// issuerTimeout bounds how long the relay waits to reach its own identity
// provider at its URL.
const issuerTimeout = 30 * time.Second

// checkIssuer checks that the relay reaches its identity provider at its
// public URL, as agentmesh-one does when it starts, so that a URL that
// doesn't lead back to the relay fails with a reason.
func checkIssuer(ctx context.Context, publicURL string) error {
	u := signin.DiscoveryURL(publicURL)
	ctx, cancel := context.WithTimeout(ctx, issuerTimeout)
	defer cancel()
	hc := &http.Client{Timeout: 10 * time.Second}
	var last error
	for {
		last = getOK(ctx, hc, u)
		if last == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("agentmesh-one reads the relay's sign-in at %s, and this machine can't reach it: %v; check that the relay's URL leads back to it", u, last)
		case <-time.After(time.Second):
		}
	}
}

func getOK(ctx context.Context, hc *http.Client, u string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	closeLogged(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return errors.New(resp.Status)
	}
	return nil
}

// meshProcess is a running agentmesh-one. err is set when exited closes.
type meshProcess struct {
	exited  chan struct{}
	err     error
	cancel  context.CancelFunc
	once    sync.Once
	stopErr error
}

func (b *Backend) startMesh(ctx context.Context, c MeshConfig) (*meshProcess, error) {
	if err := os.MkdirAll(c.DataDir, 0o700); err != nil {
		return nil, err
	}
	// agentmesh-one outlives Start, so it stops on stop and not when ctx
	// ends.
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	cmd := exec.CommandContext(ctx, b.opts.Binary, MeshArgs(c)...)
	cmd.Env = MeshEnv(c, os.Environ())
	cmd.Dir = c.DataDir
	cmd.Stdout, cmd.Stderr = b.opts.Log, b.opts.Log
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 15 * time.Second
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("starting agentmesh-one: %w", err)
	}
	p := &meshProcess{exited: make(chan struct{}), cancel: cancel}
	go func() {
		p.err = cmd.Wait()
		close(p.exited)
	}()
	return p, nil
}

// stop stops agentmesh-one and waits for it.
func (p *meshProcess) stop() error {
	p.once.Do(func() {
		p.cancel()
		<-p.exited
		var exit *exec.ExitError
		if p.err != nil && !errors.As(p.err, &exit) && !errors.Is(p.err, context.Canceled) {
			p.stopErr = p.err
		}
	})
	return p.stopErr
}

func waitReady(ctx context.Context, admin *meshadmin.Client, mesh *meshProcess, port int) error {
	ctx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		probeCtx, cancelProbe := context.WithTimeout(ctx, readyProbeTimeout)
		err := admin.Ready(probeCtx)
		cancelProbe()
		if err == nil {
			return nil
		}
		// A probe can outlast a tick, and select picks a ready case at
		// random, so the exit goes first: the port of an exited
		// agentmesh-one has to be checked while whoever took it holds it.
		select {
		case <-mesh.exited:
			return meshExitError(mesh, port)
		default:
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("agentmesh-one didn't become ready within %v", readyTimeout)
		case <-mesh.exited:
			return meshExitError(mesh, port)
		case <-tick.C:
		}
	}
}

// meshExitError reports that agentmesh-one exited while starting, and
// whether another process took its port.
func meshExitError(mesh *meshProcess, port int) error {
	err := fmt.Errorf("agentmesh-one exited while starting: %v; its log says why", mesh.err)
	if portTaken(port) {
		return fmt.Errorf("%w: %w", errMeshPortTaken, err)
	}
	return err
}

// portTaken reports whether another process listens on port. freePort
// can't reserve the port it returns.
func portTaken(port int) bool {
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return errors.Is(err, syscall.EADDRINUSE)
	}
	closeLogged(ln)
	return false
}

// Ports from freePortMin up to freePortMax lie below the ephemeral ranges of
// Linux, macOS and Windows, and below the ports the agentsrv tests give their
// agents, which run next to the relay tests.
const (
	freePortMin = 10000
	freePortMax = 20000
)

// freePort returns a free loopback port for agentmesh-one, which listens on it
// after it starts. The system hands out ephemeral ports to outgoing
// connections and to listeners on port 0 too, so one of those would often be
// taken by then; ports below the ephemeral range are only taken by another
// program that listens on that exact port.
func freePort() (int, error) {
	for range 20 {
		port := freePortMin + rand.IntN(freePortMax-freePortMin)
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			continue
		}
		return port, ln.Close()
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	return port, ln.Close()
}

func closeLogged(c io.Closer) {
	if err := c.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		slog.Debug("closing", "error", err)
	}
}
