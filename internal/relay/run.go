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
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/a2aproject/a2a-serve/internal/backoff"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
	"github.com/a2aproject/a2a-serve/internal/googleauth"
	"github.com/a2aproject/a2a-serve/internal/httpserver"
	"github.com/a2aproject/a2a-serve/internal/relay/control"
	"github.com/a2aproject/a2a-serve/internal/relay/front"
	"github.com/a2aproject/a2a-serve/internal/relay/signin"
	"github.com/a2aproject/a2a-serve/internal/relaydir"
)

// ErrRestart is returned by [Relay.Run] when `relay upgrade` asked the relay
// to restart, so that the a2a-serve binary on disk runs it.
var ErrRestart = errors.New("the relay restarts")

// Options configure a [Relay].
type Options struct {
	Dir relaydir.Dir
	// Backend is the network the relay runs behind its front.
	Backend Backend
	// Cloudflared runs a quick tunnel when the relay has --tunnel cloudflare.
	Cloudflared string
	// A2AServe is the version of this a2a-serve.
	A2AServe string
	// Google are the endpoints of Google's sign-in, which agents sign in to
	// the relay with.
	Google googleauth.Endpoints
	// Out gets progress lines for the user, and TunnelLog the output of
	// cloudflared.
	Out, TunnelLog io.Writer
	Now            func() time.Time
}

// Relay runs a relay from its relay directory.
type Relay struct {
	opts Options
}

// New returns a relay.
func New(opts Options) *Relay {
	return &Relay{opts: opts}
}

// Run runs the relay until ctx ends, the backend or the tunnel exits, or
// `relay upgrade` asks for a restart, which returns [ErrRestart]. The caller
// holds the relay's lock.
func (r *Relay) Run(ctx context.Context) (err error) {
	d, b := r.opts.Dir, r.opts.Backend
	conf, err := d.Resolve()
	if err != nil {
		return err
	}
	up, err := b.Prepare(ctx, d, conf, r.opts.Now())
	if err != nil {
		return err
	}
	if up.Upgraded() {
		r.printf("✓ upgraded the relay from %s %s to %s", b.Name(), up.From, up.To)
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
		t, err := startTunnel(ctx, r.opts.Cloudflared, "http://"+ln.Addr().String(), r.opts.TunnelLog)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, t.close()) }()
		publicURL, tunnelExited = t.url, t.exited
	}

	started := r.opts.Now()
	versions := control.Versions{A2AServe: r.opts.A2AServe, Backend: b.Name(), BackendVersion: b.Version()}
	if conf.BackendName() == relaydir.BackendAgentMesh {
		versions.AgentMesh = b.Version()
	}
	status := func() control.Status {
		return control.Status{
			URL: publicURL, PID: os.Getpid(), Started: started,
			Versions: versions,
			Join:     joinedBy(&conf.Config),
		}
	}
	admits := func(m control.Member) bool {
		return conf.SignIn() && googleauth.Allowed(conf.AllowEmails, googleauth.Claims{Email: m.Email, EmailVerified: true, HostedDomain: m.Domain})
	}
	var signIn http.Handler
	issuer := ""
	if conf.SignIn() {
		iss, err := signin.New(signin.Options{
			URL: publicURL, Secret: conf.AdminToken, AllowEmails: conf.AllowEmails,
			ClientID: conf.GoogleClientID, ClientSecret: conf.GoogleClientSecret,
			Google: r.opts.Google, HTTP: &http.Client{Timeout: 30 * time.Second}, Now: r.opts.Now,
			Client: front.Client,
		})
		if err != nil {
			return err
		}
		signIn, issuer = iss, signin.URL(publicURL)
	}

	svc := &startingService{}
	restart := make(chan struct{})
	var restartOnce sync.Once
	sock, err := listenSocket(d.SocketPath())
	if err != nil {
		return err
	}
	defer removeSocket(d.SocketPath())
	local := httpserver.New(ctx, control.Handler(svc, func() { restartOnce.Do(func() { close(restart) }) }))
	remote := control.RequireToken(conf.AdminToken, backoff.NewFailures(time.Second, 10*time.Second, r.opts.Now), control.Handler(svc, nil))
	fr := front.New(b.Info(r.opts.A2AServe, conf.SignIn()), b.Routes(), remote, signIn)
	public := httpserver.New(ctx, fr)

	serveErr := make(chan error, 2)
	go func() { serveErr <- serve(local, sock) }()
	go func() { serveErr <- serve(public, ln) }()
	defer shutdown(local, public)

	running, err := b.Start(ctx, Start{Dir: d, Config: conf, URL: publicURL, Issuer: issuer, Status: status, Admits: admits})
	if err != nil {
		return stoppedOr(ctx, err)
	}
	defer func() { err = errors.Join(err, running.Stop()) }()
	svc.set(running.Service())
	removed, readmitted, err := running.Reconcile(ctx)
	if err != nil {
		return stoppedOr(ctx, fmt.Errorf("applying the allowlist to the relay's members: %w", err))
	}
	for _, m := range removed {
		r.printf("✓ removed %s: the relay no longer lets %s sign in", memberName(m), m.Email)
	}
	for _, m := range readmitted {
		r.printf("✓ let back %s: the relay lets %s sign in again", memberName(m), m.Email)
	}
	fr.Open(running.Upstream())
	defer fr.Close()
	r.printf("relay %s (%s %s) in %s", publicURL, b.Name(), b.Version(), d.Path())
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
	case <-running.Exited():
		return running.Err()
	case <-tunnelExited:
		return errors.New("cloudflared exited, so the relay's quick tunnel is gone")
	case err := <-serveErr:
		return err
	case <-restart:
		return ErrRestart
	}
}

// memberName names m by its agent, or by its peer ID when the backend no
// longer knows the agent of a removed member.
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

// errStarting is returned by the control API until the backend serves.
var errStarting = errors.New("the relay is starting; try again in a moment")

// startingService serves the control API, and refuses it until the backend
// started.
type startingService struct {
	svc atomic.Pointer[control.Service]
}

func (s *startingService) set(svc control.Service) { s.svc.Store(&svc) }

func (s *startingService) get() (control.Service, error) {
	p := s.svc.Load()
	if p == nil {
		return nil, errStarting
	}
	return *p, nil
}

func (s *startingService) Status(ctx context.Context) (*control.Status, error) {
	svc, err := s.get()
	if err != nil {
		return nil, err
	}
	return svc.Status(ctx)
}

func (s *startingService) Invite(ctx context.Context, req control.InviteRequest) (*control.Invite, error) {
	svc, err := s.get()
	if err != nil {
		return nil, err
	}
	return svc.Invite(ctx, req)
}

func (s *startingService) Members(ctx context.Context, banned bool) ([]control.Member, error) {
	svc, err := s.get()
	if err != nil {
		return nil, err
	}
	return svc.Members(ctx, banned)
}

func (s *startingService) Remove(ctx context.Context, req control.RemoveRequest) (*control.RemoveResponse, error) {
	svc, err := s.get()
	if err != nil {
		return nil, err
	}
	return svc.Remove(ctx, req)
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
