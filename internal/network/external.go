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

package network

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/a2aproject/a2a-serve/internal/names"
	"github.com/a2aproject/a2a-serve/netprovider"
	"github.com/a2aproject/a2a-serve/netprovider/host"
)

// ExternalConfig configures a network provider that runs as its own
// process.
type ExternalConfig struct {
	// Name is the name the server gives the provider, such as agentmesh.
	// Grants and the access model use it.
	Name string
	// Binary is the provider's executable.
	Binary string
	// AgentName and Description are the agent's.
	AgentName   string
	Description string
	// URL is the --private or --public value.
	URL string
	// StateDir keeps the agent's identity on the network.
	StateDir string
	// Token is the --network-token.
	Token string
	// Private and Public select the URLs the agent wants.
	Private bool
	Public  bool
	// Logf prints the provider's notices for the user.
	Logf func(string, ...any)
}

// external is a [Provider] that runs a network provider binary and drives
// it over the protocol of package netprovider. The provider proves who is
// calling; the server decides who the owner is and checks what the
// provider returns.
type external struct {
	cfg   ExternalConfig
	cards *CardCache

	mu         sync.Mutex
	proc       *host.Process
	transport  *http.Transport
	runDir     string
	hosts      []string
	owner      string
	node       string
	joined     bool
	endpoints  *host.Endpoints
	privateURL *url.URL
	publicURL  *url.URL
	note       string
	closed     bool

	// healthMu guards health apart from mu, which calls to the provider
	// hold: the provider reports its health while it answers them.
	healthMu sync.Mutex
	health   Health
}

// NewExternal returns a provider that runs cfg.Binary. It starts the
// process when the server joins or listens.
func NewExternal(cfg ExternalConfig) Provider {
	return &external{cfg: cfg, cards: newCardCache()}
}

func (e *external) Name() string { return e.cfg.Name }

func (e *external) logf(format string, args ...any) {
	if e.cfg.Logf != nil {
		e.cfg.Logf(format, args...)
	}
}

// Bounds of the calls to the provider. Join has none, as it may wait for a
// person to sign in.
const (
	callTimeout   = 30 * time.Second
	listenTimeout = 2 * time.Minute
)

// maxNoticeText bounds a notice the provider shows the user, such as a
// sign-in link.
const maxNoticeText = 1000

// startLocked starts the process. It holds e.mu.
func (e *external) startLocked(ctx context.Context) error {
	if e.closed {
		return errors.New("the network provider is closed")
	}
	if e.proc != nil {
		return nil
	}
	dir, err := os.MkdirTemp("", "a2a-net-")
	if err != nil {
		return fmt.Errorf("creating the run directory of the network provider: %w", err)
	}
	proc, err := host.Start(host.Config{
		Path:   e.cfg.Binary,
		RunDir: dir,
		Env:    host.Environ(os.Environ()),
		Stderr: &logWriter{provider: e.cfg.Name},
		OnNotice: func(n netprovider.Notice) {
			if msg := Sanitize(n.Message, maxNoticeText); msg != "" {
				e.logf("%s", msg)
			}
		},
		OnHealth: e.setHealth,
	})
	if err != nil {
		return errors.Join(err, os.RemoveAll(dir))
	}
	dctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	d, err := proc.Describe(dctx)
	if err != nil {
		return errors.Join(fmt.Errorf("network provider %s: %w", e.cfg.Name, providerError(err)), proc.Close(), os.RemoveAll(dir))
	}
	e.proc, e.transport, e.runDir, e.hosts = proc, proc.Transport(), dir, nil
	for _, h := range d.Hosts {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" && !strings.ContainsAny(h, ":/ ") {
			e.hosts = append(e.hosts, h)
		}
	}
	go e.watchExit(proc)
	slog.Info("started the network provider", "provider", e.cfg.Name, "name", d.Name, "version", d.Version, "binary", e.cfg.Binary)
	return nil
}

func (e *external) watchExit(proc *host.Process) {
	<-proc.Done()
	e.mu.Lock()
	closed := e.closed
	e.mu.Unlock()
	if closed {
		return
	}
	slog.Warn("the network provider exited", "provider", e.cfg.Name, "error", proc.Err())
	e.setHealth(netprovider.Health{
		State:   netprovider.HealthDisconnected,
		Problem: Sanitize(proc.Err().Error(), maxProviderText),
		Fix:     "run `a2a-serve restart`",
	})
}

func (e *external) setHealth(h netprovider.Health) {
	state := HealthState(h.State)
	switch state {
	case HealthOK, HealthDisconnected, HealthRemoved:
	default:
		state = HealthDisconnected
	}
	health := Health{State: state}
	if state != HealthOK {
		health.Problem = cmpOr(Sanitize(h.Problem, maxProviderText), "the network doesn't reach the agent")
		health.Fix = Sanitize(h.Fix, maxProviderText)
	}
	e.healthMu.Lock()
	changed := e.health != health
	e.health = health
	e.healthMu.Unlock()
	if changed {
		e.logHealth(health)
	}
}

// shownURL is the network URL without its query, which may carry settings
// the user doesn't want in logs.
func (e *external) shownURL() string {
	u, err := url.Parse(e.cfg.URL)
	if err != nil {
		return "the network"
	}
	u.RawQuery, u.User = "", nil
	return u.String()
}

// logHealth tells the user that the network reaches the agent again, or
// what is wrong.
func (e *external) logHealth(h Health) {
	label := "network"
	if e.cfg.Name == ProviderAgentMesh {
		label = "relay"
	}
	switch h.State {
	case HealthOK:
		slog.Info("reconnected to the network", "provider", e.cfg.Name, "url", e.shownURL())
		e.logf("✓ %s: reconnected to %s", label, e.shownURL())
	case HealthRemoved:
		slog.Error("the network removed this agent", "provider", e.cfg.Name, "url", e.shownURL())
		e.logf("✗ %s: %v", label, h.Err())
	default:
		slog.Warn("not connected to the network", "provider", e.cfg.Name, "url", e.shownURL(), "problem", h.Problem)
		e.logf("! %s: %s", label, h.Problem)
	}
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// Health implements [HealthReporter].
func (e *external) Health() Health {
	e.healthMu.Lock()
	defer e.healthMu.Unlock()
	return e.health
}

// Join implements [Joiner].
func (e *external) Join(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.joinLocked(ctx)
}

func (e *external) joinLocked(ctx context.Context) error {
	if e.joined {
		return nil
	}
	if err := e.startLocked(ctx); err != nil {
		return err
	}
	if err := os.MkdirAll(e.cfg.StateDir, 0o700); err != nil {
		return err
	}
	res, err := e.proc.Join(ctx, netprovider.JoinRequest{
		Agent: e.cfg.AgentName, Description: e.cfg.Description, URL: e.cfg.URL, Token: e.cfg.Token,
		StateDir: e.cfg.StateDir, Private: e.cfg.Private, Public: e.cfg.Public,
	})
	err = e.explainJoin(err)
	if rerr := RecordJoin(e.cfg.StateDir, err); rerr != nil {
		slog.Warn("recording the network join", "error", rerr)
	}
	if err != nil {
		return err
	}
	e.node = Sanitize(res.Node, maxProviderText)
	// An agent that joined with a token didn't sign in, so nobody on the
	// network is its owner, whatever the provider says.
	if e.cfg.Token == "" && IsEmail(res.Owner) {
		e.owner = strings.ToLower(res.Owner)
	}
	e.joined = true
	return nil
}

// explainJoin turns the provider errors the user acts on into errors that
// say what to do.
func (e *external) explainJoin(err error) error {
	var pe *netprovider.Error
	if !errors.As(err, &pe) {
		return providerError(err)
	}
	problem, fix := Sanitize(pe.Problem, maxProviderText), Sanitize(pe.Fix, maxProviderText)
	switch pe.Kind {
	case netprovider.KindToken:
		return &TokenError{Problem: cmpOr(problem, "the network refused the agent's token"), Fix: tokenFix(fix)}
	case netprovider.KindRemoved:
		if fix == "" {
			return errors.New(problem)
		}
		return errors.New(problem + "; " + fix)
	}
	return providerError(err)
}

func (e *external) listen(ctx context.Context) (*host.Endpoints, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.endpoints != nil {
		return e.endpoints, nil
	}
	if err := e.joinLocked(ctx); err != nil {
		return nil, err
	}
	lctx, cancel := context.WithTimeout(ctx, listenTimeout)
	defer cancel()
	ep, err := e.proc.Listen(lctx, e.cfg.Private, e.cfg.Public)
	if err != nil {
		return nil, fmt.Errorf("network provider %s: %w", e.cfg.Name, providerError(err))
	}
	if e.cfg.Private {
		if e.privateURL, err = e.checkURL(ep.PrivateURL); err != nil {
			return nil, errors.Join(fmt.Errorf("network provider %s gave the private URL %q: %w", e.cfg.Name, ep.PrivateURL, err), closeEndpoints(ep))
		}
	}
	if e.cfg.Public {
		if e.publicURL, err = e.checkURL(ep.PublicURL); err != nil {
			return nil, errors.Join(fmt.Errorf("network provider %s gave the public URL %q: %w", e.cfg.Name, ep.PublicURL, err), closeEndpoints(ep))
		}
	}
	e.note = Sanitize(ep.PrivateNote, maxProviderText)
	e.endpoints = ep
	return ep, nil
}

func closeEndpoints(ep *host.Endpoints) error {
	var errs []error
	for _, l := range []*host.Listener{ep.Private, ep.Public} {
		if l != nil {
			errs = append(errs, l.Close())
		}
	}
	return errors.Join(errs...)
}

func (e *external) checkURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("want an http or https URL")
	}
	return u, nil
}

func (e *external) ListenPrivate(ctx context.Context) (net.Listener, *url.URL, error) {
	ep, err := e.listen(ctx)
	if err != nil {
		return nil, nil, err
	}
	if ep.Private == nil {
		return nil, nil, ErrUnsupported
	}
	return ep.Private, e.privateURL, nil
}

func (e *external) ListenPublic(ctx context.Context) (net.Listener, *url.URL, error) {
	ep, err := e.listen(ctx)
	if err != nil {
		return nil, nil, err
	}
	if ep.Public == nil {
		return nil, nil, errors.New("the network gives this agent no public URL")
	}
	return ep.Public, e.publicURL, nil
}

// PrivateNote says where the private URL works, when the provider said.
func (e *external) PrivateNote() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.note
}

func (e *external) caller(remoteAddr string, public bool) (netprovider.Caller, bool) {
	e.mu.Lock()
	ep := e.endpoints
	e.mu.Unlock()
	if ep == nil {
		return netprovider.Caller{}, false
	}
	l := ep.Private
	if public {
		l = ep.Public
	}
	if l == nil {
		return netprovider.Caller{}, false
	}
	return l.Caller(remoteAddr)
}

// WhoIs identifies a caller on the private listener by what the provider
// put in the connection's header. An email that isn't a plain address is
// dropped; the caller is this agent's user when its email is the one the
// agent signed in with.
func (e *external) WhoIs(_ context.Context, remoteAddr string) (*Peer, error) {
	c, ok := e.caller(remoteAddr, false)
	if !ok || !c.Identified() {
		return nil, ErrNoIdentity
	}
	return e.peer(c), nil
}

func (e *external) peer(c netprovider.Caller) *Peer {
	p := &Peer{Node: c.Node, Machine: cmpOr(c.Machine, c.Node)}
	if IsEmail(c.Email) {
		p.Email = strings.ToLower(c.Email)
		e.mu.Lock()
		owner := e.owner
		e.mu.Unlock()
		p.Self = owner != "" && p.Email == owner
	}
	return p
}

// ClientKey implements [ClientKeyer].
func (e *external) ClientKey(remoteAddr string, public bool) string {
	c, ok := e.caller(remoteAddr, public)
	if !ok || c.ClientKey == "" {
		return ""
	}
	return "net:" + c.ClientKey
}

func (e *external) process() *host.Process {
	proc, _ := e.session()
	return proc
}

// session returns the process and the transport through its tunnel once
// the agent joined, and nil before.
func (e *external) session() (*host.Process, *http.Transport) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.joined {
		return nil, nil
	}
	return e.proc, e.transport
}

func (e *external) Member(ctx context.Context, node string) (*Peer, error) {
	proc := e.process()
	if proc == nil {
		return nil, ErrUnsupported
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	c, err := proc.Member(ctx, node)
	switch {
	case errors.Is(err, netprovider.ErrUnsupported):
		return nil, ErrUnsupported
	case errors.Is(err, netprovider.ErrNotMember):
		return nil, ErrNoIdentity
	case err != nil:
		return nil, fmt.Errorf("network provider %s: %w", e.cfg.Name, providerError(err))
	}
	if c.Node != node {
		return nil, ErrNoIdentity
	}
	return e.peer(c), nil
}

func (e *external) Peers(ctx context.Context) ([]PeerInfo, error) {
	proc := e.process()
	if proc == nil {
		return nil, ErrUnsupported
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	found, err := proc.Peers(ctx)
	if errors.Is(err, netprovider.ErrUnsupported) {
		return nil, ErrUnsupported
	}
	if err != nil {
		return nil, fmt.Errorf("finding agents on the network: %w", providerError(err))
	}
	e.mu.Lock()
	self := e.node
	e.mu.Unlock()
	var candidates []PeerInfo
	for _, p := range found {
		// The name is chosen by the peer and reaches the agent's prompt as
		// the peer's name, so it has to be a valid agent name.
		if !names.Valid(p.Name) || (self != "" && p.Node == self) || !e.routed(p.URL) {
			continue
		}
		candidates = append(candidates, PeerInfo{
			Name: p.Name, URL: p.URL, Description: Sanitize(p.Description, maxProviderText), Online: true,
			Node: cmpOr(Sanitize(p.Node, maxProviderText), p.URL),
		})
	}
	return e.cards.ProbeAgents(ctx, e.HTTPClient(), candidates, []string{e.cfg.AgentName}), nil
}

// routed reports whether raw is an http URL on a host the provider serves.
func (e *external) routed(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Contains(e.hosts, strings.ToLower(u.Hostname()))
}

// HTTPClient sends requests to the provider's hosts through its CONNECT
// tunnel, and others over the internet.
func (e *external) HTTPClient() *http.Client {
	return &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL == nil || !e.routed(r.URL.String()) {
			return http.DefaultTransport.RoundTrip(r)
		}
		_, tr := e.session()
		if tr == nil {
			return nil, errors.New("not connected to the network")
		}
		return checkPeerResponse(tr.RoundTrip(r))
	})}
}

func (e *external) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closed = true
	var errs []error
	if e.proc != nil {
		e.transport.CloseIdleConnections()
		errs = append(errs, e.proc.Close())
		e.proc, e.transport = nil, nil
	}
	if e.runDir != "" {
		errs = append(errs, os.RemoveAll(e.runDir))
		e.runDir = ""
	}
	e.endpoints, e.joined = nil, false
	return errors.Join(errs...)
}

// ClientKeyer is a [Provider] whose listeners are tunneled: callers arrive
// through the provider and share the address of its connections, so
// per-caller limits key on what the provider says.
type ClientKeyer interface {
	// ClientKey returns the key of the caller of the connection with the
	// remote address remoteAddr, or "" when the provider didn't say.
	ClientKey(remoteAddr string, public bool) string
}

// PrivateNoter is a [Provider] that says where its private URL works.
type PrivateNoter interface {
	// PrivateNote says where the private URL works, or "".
	PrivateNote() string
}

// maxErrorText bounds the error of a provider the server shows.
const maxErrorText = 1000

// providerError returns err of a provider call with its text sanitized,
// keeping cancellations as they are.
func providerError(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return errors.New(Sanitize(err.Error(), maxErrorText))
}

// maxProviderText bounds text from a provider the server shows or keeps.
const maxProviderText = 300

// ansiEscape matches the terminal escape sequences of colored logs.
var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

// Sanitize returns text from a network without terminal escapes or control
// characters, so that it can't rewrite the terminal, and at most n runes
// long.
func Sanitize(s string, n int) string {
	s = ansiEscape.ReplaceAllString(s, "")
	var b strings.Builder
	count := 0
	for _, r := range s {
		if unicode.IsControl(r) {
			continue
		}
		if count == n {
			b.WriteString("…")
			break
		}
		b.WriteRune(r)
		count++
	}
	return strings.TrimSpace(b.String())
}

// logWriter logs the lines a provider writes to stderr.
type logWriter struct {
	provider string
	mu       sync.Mutex
	part     []byte
}

func (w *logWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.part = append(w.part, b...)
	r := bufio.NewReader(strings.NewReader(string(w.part)))
	consumed := 0
	for {
		line, err := r.ReadString('\n')
		if errors.Is(err, io.EOF) {
			break
		}
		consumed += len(line)
		if line = Sanitize(line, 2000); line != "" {
			slog.Info("network provider", "provider", w.provider, "log", line)
		}
	}
	w.part = w.part[consumed:]
	if len(w.part) > 8192 {
		w.part = w.part[:0]
	}
	return len(b), nil
}
