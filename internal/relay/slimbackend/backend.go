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

package slimbackend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/a2aproject/a2a-serve/internal/fsutil"
	"github.com/a2aproject/a2a-serve/internal/httpserver"
	"github.com/a2aproject/a2a-serve/internal/relay"
	"github.com/a2aproject/a2a-serve/internal/relay/control"
	"github.com/a2aproject/a2a-serve/internal/relay/front"
	"github.com/a2aproject/a2a-serve/internal/relaydir"
)

// readyTimeout bounds how long the node takes to listen.
const readyTimeout = 30 * time.Second

// startAttempts bounds how often Start starts the node again on another
// port.
const startAttempts = 3

// Options configure a [Backend].
type Options struct {
	// Binary is the slimctl that runs the node.
	Binary string
	// Log gets the output of the node.
	Log io.Writer
	Now func() time.Time
}

// Backend runs a SLIM node behind a relay.
type Backend struct {
	opts Options
}

// New returns the SLIM backend.
func New(opts Options) *Backend {
	return &Backend{opts: opts}
}

var _ relay.Backend = (*Backend)(nil)

// Name returns "SLIM".
func (*Backend) Name() string { return Name }

// Version returns the version of the relay's slimctl.
func (*Backend) Version() string { return Version }

// Prepare does nothing: the members file needs no migration yet.
func (*Backend) Prepare(context.Context, relaydir.Dir, *relaydir.Resolved, time.Time) (relay.Prepared, error) {
	return relay.Prepared{To: Version}, nil
}

// Info is what a SLIM relay reports at its info path. Backend tells agents
// which network provider joins it.
type Info struct {
	A2AServe string `json:"a2a_serve"`
	Backend  string `json:"backend"`
	SLIM     string `json:"slim"`
	SignIn   bool   `json:"signin"`
}

// Info reports the relay's backend and versions.
func (*Backend) Info(a2aServe string, signIn bool) any {
	return Info{A2AServe: a2aServe, Backend: relaydir.BackendSLIM, SLIM: Version, SignIn: signIn}
}

// Routes describes the gate's routes.
func (*Backend) Routes() front.Routes { return Routes{} }

// Start starts the node and the gate in front of it.
func (b *Backend) Start(ctx context.Context, s relay.Start) (relay.Running, error) {
	if s.Issuer != "" {
		return nil, errors.New("people can't sign in to a SLIM relay yet")
	}
	if err := fsutil.MkdirPrivate(s.Dir.SlimDir()); err != nil {
		return nil, err
	}
	st, err := openStore(filepath.Join(s.Dir.SlimDir(), "members.json"), b.opts.Now)
	if err != nil {
		return nil, err
	}
	var node *nodeProcess
	for attempt := 1; ; attempt++ {
		node, err = b.startNode(ctx, s.Dir.SlimDir())
		if !errors.Is(err, errNodePortTaken) || attempt == startAttempts {
			break
		}
		slog.Debug("starting the SLIM node again on another port", "error", err)
	}
	if err != nil {
		return nil, err
	}
	cs := newConns(b.opts.Now)
	svc := &service{store: st, conns: cs, status: s.Status, now: b.opts.Now}
	g := newGate(svc, GateAudience(s.URL), node.url, b.opts.Now)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, errors.Join(err, node.stop())
	}
	r := &running{
		upstream: &url.URL{Scheme: "http", Host: ln.Addr().String()},
		svc:      svc,
		node:     node,
		server:   httpserver.New(context.WithoutCancel(ctx), g),
		served:   make(chan error, 1),
		exited:   make(chan struct{}),
	}
	go func() { r.served <- r.server.Serve(ln) }()
	go r.watch()
	return r, nil
}

// running is a started node and gate.
type running struct {
	upstream *url.URL
	svc      *service
	node     *nodeProcess
	server   *http.Server
	served   chan error
	exited   chan struct{}
	err      error
	stopOnce sync.Once
	stopErr  error
}

func (r *running) Upstream() *url.URL       { return r.upstream }
func (r *running) Service() control.Service { return r.svc }
func (r *running) Exited() <-chan struct{}  { return r.exited }
func (r *running) Err() error               { return r.err }

// Reconcile does nothing: nobody signs in to a SLIM relay.
func (r *running) Reconcile(context.Context) (removed, readmitted []control.Member, err error) {
	return nil, nil, nil
}

// watch closes exited when the node exits or the gate stops serving.
func (r *running) watch() {
	select {
	case <-r.node.exited:
		r.err = fmt.Errorf("the SLIM node exited: %v", r.node.err)
	case err := <-r.served:
		r.err = fmt.Errorf("the relay's SLIM gate stopped: %v", err)
	}
	close(r.exited)
}

func (r *running) Stop() error {
	r.stopOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// Shutdown doesn't wait for hijacked WebSockets; closing the
		// node ends them.
		err := r.server.Shutdown(ctx)
		if errors.Is(err, context.DeadlineExceeded) {
			err = r.server.Close()
		}
		r.stopErr = errors.Join(err, r.node.stop())
	})
	return r.stopErr
}

// errNodePortTaken is returned when the node exited while starting because
// another process took its port.
var errNodePortTaken = errors.New("another process took the port of the SLIM node")

// nodeProcess is a running SLIM node. err is set when exited closes.
type nodeProcess struct {
	url     *url.URL
	exited  chan struct{}
	err     error
	cancel  context.CancelFunc
	once    sync.Once
	stopErr error
}

// nodeConfig is the node's configuration: WebSocket on loopback, where only
// the gate reaches it.
func nodeConfig(port int) string {
	return `# Written by a2a-serve on every start of the relay.
tracing:
  log_level: warn
runtime:
  n_cores: 0
  thread_name: "slim-data-plane"
  drain_timeout: 5s
services:
  slim/0:
    node_id: a2a-serve-relay
    dataplane:
      servers:
        - endpoint: "ws://127.0.0.1:` + strconv.Itoa(port) + `"
          tls:
            insecure: true
      clients: []
`
}

func (b *Backend) startNode(ctx context.Context, dir string) (*nodeProcess, error) {
	port, err := relay.FreePort()
	if err != nil {
		return nil, err
	}
	config := filepath.Join(dir, "node.yaml")
	if err := fsutil.WriteAtomic(config, []byte(nodeConfig(port)), 0o600); err != nil {
		return nil, err
	}
	// The node outlives Start, so it stops on stop and not when ctx ends.
	procCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	cmd := exec.CommandContext(procCtx, b.opts.Binary, "slim", "start", "--config", config)
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = b.opts.Log, b.opts.Log
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 15 * time.Second
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("starting the SLIM node: %w", err)
	}
	p := &nodeProcess{
		url:    &url.URL{Scheme: "http", Host: net.JoinHostPort("127.0.0.1", strconv.Itoa(port))},
		exited: make(chan struct{}),
		cancel: cancel,
	}
	go func() {
		p.err = cmd.Wait()
		close(p.exited)
	}()
	if err := p.waitReady(ctx, port); err != nil {
		return nil, errors.Join(err, p.stop())
	}
	return p, nil
}

// waitReady waits until the node listens on port.
func (p *nodeProcess) waitReady(ctx context.Context, port int) error {
	ctx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	d := net.Dialer{Timeout: time.Second}
	for {
		select {
		case <-p.exited:
			return p.exitError(port)
		default:
		}
		c, err := d.DialContext(ctx, "tcp", p.url.Host)
		if err == nil {
			if err := c.Close(); err != nil {
				slog.Debug("closing the readiness probe", "error", err)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the SLIM node didn't listen within %v", readyTimeout)
		case <-p.exited:
			return p.exitError(port)
		case <-tick.C:
		}
	}
}

func (p *nodeProcess) exitError(port int) error {
	err := fmt.Errorf("the SLIM node exited while starting: %v; its log says why", p.err)
	if relay.PortTaken(port) {
		return fmt.Errorf("%w: %w", errNodePortTaken, err)
	}
	return err
}

// stop stops the node and waits for it.
func (p *nodeProcess) stop() error {
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

// Routes describes the gate's routes to the relay's front.
type Routes struct{}

// Blocked refuses nothing: the gate serves only its own routes.
func (Routes) Blocked(string, string) bool { return false }

// Enrollment reports whether p is where agents join the relay.
func (Routes) Enrollment(p string) bool { return p == EnrollPath }

// Ingress splits rest, which is <agent>/<route>, into the agent and its
// route.
func (Routes) Ingress(rest string) (agent, route string) {
	agent, route, _ = strings.Cut(rest, "/")
	return agent, route
}
