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

package relay

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
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/a2aproject/a2a-serve/internal/agentmesh"
	"github.com/a2aproject/a2a-serve/internal/backoff"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
	"github.com/a2aproject/a2a-serve/internal/googleauth"
	"github.com/a2aproject/a2a-serve/internal/httpserver"
	"github.com/a2aproject/a2a-serve/internal/relay/control"
	"github.com/a2aproject/a2a-serve/internal/relay/front"
	"github.com/a2aproject/a2a-serve/internal/relay/meshadmin"
	"github.com/a2aproject/a2a-serve/internal/relay/signin"
	"github.com/a2aproject/a2a-serve/internal/relaydir"
)

// ErrRestart is returned by [Relay.Run] when `relay upgrade` asked the relay
// to restart, so that the a2a-serve binary on disk runs it.
var ErrRestart = errors.New("the relay restarts")

// readyTimeout bounds how long agentmesh-one takes to become ready. A first
// start on PostgreSQL migrates the database.
const readyTimeout = 2 * time.Minute

// readyProbeTimeout bounds one readiness probe. A process that took the port
// of agentmesh-one may accept connections and never answer.
const readyProbeTimeout = time.Second

// Options configure a [Relay].
type Options struct {
	Dir relaydir.Dir
	// Binary is the agentmesh-one to run.
	Binary string
	// Cloudflared runs a quick tunnel when the relay has --tunnel cloudflare.
	Cloudflared string
	// A2AServe is the version of this a2a-serve.
	A2AServe string
	// Google are the endpoints of Google's sign-in, which agents sign in to
	// the relay with.
	Google googleauth.Endpoints
	// Out gets progress lines for the user, and MeshLog the output of
	// agentmesh-one and cloudflared.
	Out, MeshLog io.Writer
	Now          func() time.Time
}

// Relay runs a relay from its relay directory.
type Relay struct {
	opts Options
}

// New returns a relay.
func New(opts Options) *Relay {
	return &Relay{opts: opts}
}

// errMeshPortTaken is returned by run when agentmesh-one exited while
// starting because another process took the port picked for it.
var errMeshPortTaken = errors.New("another process took the port of agentmesh-one")

// meshStartAttempts bounds how often Run starts again on another port.
const meshStartAttempts = 3

// Run runs the relay until ctx ends, agentmesh-one or the tunnel exits, or
// `relay upgrade` asks for a restart, which returns [ErrRestart]. The caller
// holds the relay's lock.
func (r *Relay) Run(ctx context.Context) error {
	for attempt := 1; ; attempt++ {
		err := r.run(ctx)
		if !errors.Is(err, errMeshPortTaken) || attempt == meshStartAttempts {
			return err
		}
		slog.Debug("starting the relay again on another port", "error", err)
	}
}

func (r *Relay) run(ctx context.Context) (err error) {
	d := r.opts.Dir
	conf, err := d.Resolve()
	if err != nil {
		return err
	}
	up, err := PrepareData(ctx, d, agentmesh.Version, conf.Database != "", r.opts.Now())
	if err != nil {
		return err
	}
	if up.Upgraded() {
		r.printf("✓ upgraded the relay from Agent Mesh %s to %s", up.From, up.To)
	}
	if up.Backup != "" {
		r.printf("  backup of the relay's database: %s", up.Backup)
	}

	ln, err := net.Listen("tcp", conf.ListenAddr())
	if err != nil {
		return fmt.Errorf("listening on %s: %w", conf.ListenAddr(), err)
	}
	defer closeLogged(ln)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	publicURL := conf.URL
	var tunnelExited <-chan struct{}
	if conf.Tunnel == relaydir.TunnelCloudflare {
		t, err := startTunnel(ctx, r.opts.Cloudflared, "http://"+ln.Addr().String(), r.opts.MeshLog)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, t.close()) }()
		publicURL, tunnelExited = t.url, t.exited
	}

	port, err := freePort()
	if err != nil {
		return err
	}
	meshURL := &url.URL{Scheme: "http", Host: net.JoinHostPort("127.0.0.1", strconv.Itoa(port))}
	admin := meshadmin.New(meshURL.String(), conf.AdminToken, &http.Client{Timeout: 30 * time.Second})
	started := r.opts.Now()
	svc := control.NewService(control.Options{
		Mesh:        admin,
		Removed:     control.NewRemovedAgents(d.RemovedAgentsPath()),
		InviteNames: control.NewInviteNames(d.InviteNamesPath()),
		Policy:      Policy(conf.SignIn()),
		Status: func() control.Status {
			return control.Status{
				URL: publicURL, PID: os.Getpid(), Started: started,
				Versions: control.Versions{A2AServe: r.opts.A2AServe, AgentMesh: agentmesh.Version},
				Join:     joinedBy(&conf.Config),
			}
		},
		Admits: func(m control.Member) bool {
			return conf.SignIn() && googleauth.Allowed(conf.AllowEmails, googleauth.Claims{Email: m.Email, EmailVerified: true, HostedDomain: m.Domain})
		},
		Now: r.opts.Now,
	})
	var signIn http.Handler
	issuer := ""
	if conf.SignIn() {
		iss, err := signin.New(signin.Options{
			URL: publicURL, Secret: conf.AdminToken, AllowEmails: conf.AllowEmails,
			ClientID: conf.GoogleClientID, ClientSecret: conf.GoogleClientSecret,
			Google: r.opts.Google, HTTP: &http.Client{Timeout: 30 * time.Second}, Now: r.opts.Now,
		})
		if err != nil {
			return err
		}
		signIn, issuer = iss, signin.URL(publicURL)
	}

	restart := make(chan struct{})
	var restartOnce sync.Once
	sock, err := listenSocket(d.SocketPath())
	if err != nil {
		return err
	}
	defer removeSocket(d.SocketPath())
	local := httpserver.New(ctx, control.Handler(svc, func() { restartOnce.Do(func() { close(restart) }) }))
	remote := control.RequireToken(conf.AdminToken, backoff.NewFailures(time.Second, 10*time.Second, r.opts.Now), control.Handler(svc, nil))
	fr := front.New(agentmesh.Info{A2AServe: r.opts.A2AServe, AgentMesh: agentmesh.Version, SignIn: conf.SignIn()}, remote, signIn)
	public := httpserver.New(ctx, fr)

	serveErr := make(chan error, 2)
	go func() { serveErr <- serve(local, sock) }()
	go func() { serveErr <- serve(public, ln) }()
	defer shutdown(local, public)

	if issuer != "" {
		if err := checkIssuer(ctx, publicURL); err != nil {
			return stoppedOr(ctx, err)
		}
	}
	mesh, err := r.startMesh(ctx, MeshConfig{URL: publicURL, Port: port, DataDir: d.MeshDir(), AdminToken: conf.AdminToken, Database: conf.Database, Issuer: issuer})
	if err != nil {
		return stoppedOr(ctx, err)
	}
	defer func() { err = errors.Join(err, mesh.stop()) }()
	if err := waitReady(ctx, admin, mesh, port); err != nil {
		return stoppedOr(ctx, err)
	}
	if err := svc.ApplyPolicy(ctx); err != nil {
		return stoppedOr(ctx, err)
	}
	removed, readmitted, err := svc.Reconcile(ctx)
	if err != nil {
		return stoppedOr(ctx, fmt.Errorf("applying the allowlist to the relay's members: %w", err))
	}
	for _, m := range removed {
		r.printf("✓ removed %s: the relay no longer lets %s sign in", memberName(m), m.Email)
	}
	for _, m := range readmitted {
		r.printf("✓ let back %s: the relay lets %s sign in again", memberName(m), m.Email)
	}
	fr.Open(meshURL)
	defer fr.Close()
	r.printf("relay %s (Agent Mesh %s) in %s", publicURL, agentmesh.Version, d.Path())
	r.printf("✓ ready; serving HTTP on %s", ln.Addr())
	if conf.Tunnel != "" {
		r.printf("  the quick tunnel's URL changes on every start, and agents have to join again then")
	}
	if conf.SignIn() {
		r.printf("→ people sign in from their agents (allowed: %s):", strings.Join(conf.AllowEmails, ", "))
		r.printf("  a2a-serve config set --private %s --public %s", publicURL, publicURL)
		r.printf("  on a new agent: a2a-serve config set --agent <agent> --private %s --public %s", publicURL, publicURL)
	}
	r.printf("→ invite an agent: a2a-serve relay invite <name>")

	select {
	case <-ctx.Done():
		return nil
	case <-mesh.exited:
		return fmt.Errorf("agentmesh-one exited: %v", mesh.err)
	case <-tunnelExited:
		return errors.New("cloudflared exited, so the relay's quick tunnel is gone")
	case err := <-serveErr:
		return err
	case <-restart:
		return ErrRestart
	}
}

// memberName names m by its agent, or by its peer ID when Agent Mesh no
// longer knows the agent of a banned node.
func memberName(m control.Member) string {
	if m.Agent != "" {
		return m.Agent
	}
	return "agent " + m.Peer
}

// joinedBy says who can join a relay with conf.
func joinedBy(conf *relaydir.Config) string {
	if !conf.SignIn() {
		return "invites"
	}
	return "sign-in (" + strings.Join(conf.AllowEmails, ", ") + ") and invites"
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

// stoppedOr returns nil when ctx ended, since the relay was asked to stop
// while it started, and err otherwise.
func stoppedOr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return nil
	}
	return err
}

func (r *Relay) printf(format string, args ...any) {
	if _, err := fmt.Fprintf(r.opts.Out, format+"\n", args...); err != nil {
		slog.Debug("writing progress", "error", err)
	}
}

// meshProcess is a running agentmesh-one. err is set when exited closes.
type meshProcess struct {
	exited  chan struct{}
	err     error
	cancel  context.CancelFunc
	once    sync.Once
	stopErr error
}

func (r *Relay) startMesh(ctx context.Context, c MeshConfig) (*meshProcess, error) {
	if err := os.MkdirAll(c.DataDir, 0o700); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(ctx, r.opts.Binary, MeshArgs(c)...)
	cmd.Env = MeshEnv(c, os.Environ())
	cmd.Dir = c.DataDir
	cmd.Stdout, cmd.Stderr = r.opts.MeshLog, r.opts.MeshLog
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

// listenSocket listens on the control socket. The caller holds the relay's
// lock, so a socket file left there belongs to a relay that died.
func listenSocket(path string) (net.Listener, error) {
	if err := fsutil.MkdirPrivate(filepath.Dir(path)); err != nil {
		return nil, err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return net.Listen("unix", path)
}

func removeSocket(path string) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("removing the control socket", "error", err)
	}
}

func serve(s *http.Server, ln net.Listener) error {
	if err := s.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func shutdown(servers ...*http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, s := range servers {
		if err := s.Shutdown(ctx); err != nil {
			slog.Debug("shutting down", "error", err)
			if err := s.Close(); err != nil {
				slog.Debug("closing", "error", err)
			}
		}
	}
}

func closeLogged(c io.Closer) {
	if err := c.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		slog.Debug("closing", "error", err)
	}
}
