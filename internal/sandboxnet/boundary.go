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

package sandboxnet

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"

	"github.com/a2aproject/a2a-serve/internal/egress"
)

// Limits of one boundary, which serves one container.
const (
	// DefaultMaxTunnels bounds the open tunnels.
	DefaultMaxTunnels = 256
	// maxHead bounds a request head.
	maxHead = 64 << 10
	// headTimeout bounds reading a request head.
	headTimeout = 15 * time.Second
	// dialTimeout bounds deciding, resolving and dialing a destination.
	dialTimeout = 15 * time.Second
	// DefaultIdleTimeout closes a tunnel that carried no data for so long.
	DefaultIdleTimeout = time.Hour
	// tunnelRate and tunnelBurst bound how fast tunnels open; requests
	// over the rate wait.
	tunnelRate  = 50
	tunnelBurst = 200
	// maxLogLines bounds the decisions a boundary logs a minute; the
	// log counts the rest.
	maxLogLines = 600
)

// BoundaryConfig configures a [Boundary].
type BoundaryConfig struct {
	// Sandbox names the container in the log.
	Sandbox string
	// Policy returns the policy that decides each request, so that a
	// changed policy applies to the next one.
	Policy func() egress.Policy
	// HostGateway returns the address that a connection to
	// [egress.HostGateway] on port reaches, or false when the host can't
	// be reached on port.
	HostGateway func(ctx context.Context, port uint16) (netip.AddrPort, bool)
	// Network tells the addresses and routes of this host; nil uses the
	// host's own.
	Network Network
	// Resolver resolves names; nil uses the system resolver.
	Resolver Resolver
	// Log receives one record per decision; nil uses the default logger.
	Log *slog.Logger
	// Blocks records the requests the policy refuses; nil records none.
	Blocks *Blocks
	// MaxTunnels and IdleTimeout default to [DefaultMaxTunnels] and
	// [DefaultIdleTimeout].
	MaxTunnels  int
	IdleTimeout time.Duration
}

// Network tells what the policy needs to know about this host's network.
type Network interface {
	// OwnAddr reports whether addr is an address of this host.
	OwnAddr(addr netip.Addr) (bool, error)
	// DefaultRoute reports whether this host reaches addr through a default
	// route of its main routing table. A route of a link, a tunnel, a
	// bridge or another table leads to a private network.
	DefaultRoute(addr netip.Addr) (bool, error)
}

// Resolver resolves host names, as [net.Resolver] does.
type Resolver interface {
	// LookupNetIP returns the addresses of host for network ip, ip4 or ip6.
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Boundary serves the egress socket of one container. It answers HTTP
// CONNECT requests, decides each by its destination, resolves names
// itself and dials only the addresses it checked.
type Boundary struct {
	cfg     BoundaryConfig
	ln      net.Listener
	slots   chan struct{}
	limiter *rate.Limiter
	log     *decisionLog
	// ctx ends when the boundary closes, which ends the requests it decides.
	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool
	wg     sync.WaitGroup
}

// New returns a boundary that serves the connections given to
// [Boundary.ServeConn].
func New(cfg BoundaryConfig) (*Boundary, error) {
	if cfg.Policy == nil {
		return nil, errors.New("the boundary needs a policy")
	}
	cfg.MaxTunnels = cmp.Or(cfg.MaxTunnels, DefaultMaxTunnels)
	cfg.IdleTimeout = cmp.Or(cfg.IdleTimeout, DefaultIdleTimeout)
	if cfg.Network == nil {
		cfg.Network = hostNetwork{}
	}
	if cfg.Resolver == nil {
		cfg.Resolver = net.DefaultResolver
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	b := &Boundary{
		cfg:     cfg,
		slots:   make(chan struct{}, cfg.MaxTunnels),
		limiter: rate.NewLimiter(tunnelRate, tunnelBurst),
		log:     &decisionLog{log: cfg.Log, sandbox: cfg.Sandbox},
		conns:   map[net.Conn]struct{}{},
	}
	b.ctx, b.cancel = context.WithCancel(context.Background())
	return b, nil
}

// Listen serves a new boundary on a Unix socket at path.
func Listen(path string, cfg BoundaryConfig) (*Boundary, error) {
	b, err := New(cfg)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listening for the sandbox's connections on %s: %w", path, err)
	}
	b.ln = ln
	b.wg.Go(b.serve)
	return b, nil
}

// Close stops the boundary and closes its open tunnels.
func (b *Boundary) Close() error {
	b.cancel()
	b.mu.Lock()
	b.closed = true
	var err error
	if b.ln != nil {
		err = b.ln.Close()
	}
	for c := range b.conns {
		if cerr := c.Close(); cerr != nil && !errors.Is(cerr, net.ErrClosed) {
			err = errors.Join(err, cerr)
		}
	}
	b.mu.Unlock()
	b.wg.Wait()
	b.log.flush()
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

func (b *Boundary) serve() {
	for {
		c, err := b.ln.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				b.cfg.Log.Error("egress socket failed", "sandbox", b.cfg.Sandbox, "error", err)
			}
			return
		}
		b.ServeConn(c)
	}
}

// ServeConn serves the requests of one connection of the container, and
// closes it.
func (b *Boundary) ServeConn(c net.Conn) {
	select {
	case b.slots <- struct{}{}:
	default:
		// The refusal comes before the head, as the wire profile says, so
		// a guest that writes first may see a reset.
		b.log.write(slog.LevelWarn, "egress blocked", "reason", "busy")
		writeRefusal(c, refuse(503, "busy"))
		closeConn(c)
		return
	}
	// The wait group grows under the lock that Close takes first, so that
	// Close waits for every connection it didn't refuse.
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		<-b.slots
		closeConn(c)
		return
	}
	b.conns[c] = struct{}{}
	b.wg.Add(1)
	b.mu.Unlock()
	go func() {
		defer b.wg.Done()
		defer func() {
			b.untrack(c)
			<-b.slots
		}()
		defer func() { logPanic(recover(), b.cfg.Log, b.cfg.Sandbox) }()
		b.handle(c)
	}()
}

// logPanic logs r, a recovered panic of the handling of one connection,
// which then closes, so that a bug refuses that connection and the others
// go on.
func logPanic(r any, log *slog.Logger, sandbox string) {
	if r != nil {
		log.Error("egress connection failed; refusing it", "sandbox", sandbox, "panic", r, "stack", string(debug.Stack()))
	}
}

func (b *Boundary) track(c net.Conn) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return false
	}
	b.conns[c] = struct{}{}
	return true
}

func (b *Boundary) untrack(c net.Conn) {
	b.mu.Lock()
	delete(b.conns, c)
	b.mu.Unlock()
	closeConn(c)
}

func closeConn(c net.Conn) {
	if err := c.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		slog.Debug("closing an egress connection", "error", err)
	}
}

// refusal is a refused request: its status and reason token, as the
// agents.net wire profile registers them.
type refusal struct {
	status int
	reason string
	// addrs are the resolved addresses the policy refused.
	addrs []netip.Addr
	// allow is the destination a rule needs to name to allow the request,
	// when it differs from the requested one.
	allow string
}

func (r *refusal) Error() string { return r.reason }

func refuse(status int, reason string) *refusal { return &refusal{status: status, reason: reason} }

// grant is an authorized request: the destination as requested, the rule
// that allowed it and the checked addresses to dial.
type grant struct {
	target string
	rule   string
	addrs  []netip.AddrPort
}

func (b *Boundary) handle(guest net.Conn) {
	ctx, cancel := context.WithTimeout(b.ctx, headTimeout)
	defer cancel()
	if err := b.limiter.Wait(ctx); err != nil {
		writeRefusal(guest, refuse(503, "busy"))
		return
	}
	if err := guest.SetDeadline(time.Now().Add(headTimeout)); err != nil {
		return
	}
	br := bufio.NewReaderSize(io.LimitReader(guest, maxHead), 4096)
	host, port, rerr := readConnect(br)
	if rerr != nil {
		b.log.write(slog.LevelWarn, "egress blocked", "reason", rerr.reason)
		writeRefusal(guest, rerr)
		return
	}
	target := net.JoinHostPort(host, strconv.Itoa(int(port)))
	ctx, cancel = context.WithTimeout(b.ctx, dialTimeout)
	defer cancel()
	g, rerr := b.authorize(ctx, host, port)
	if rerr != nil {
		args := []any{"destination", target, "reason", rerr.reason}
		if len(rerr.addrs) > 0 {
			args = append(args, "addresses", rerr.addrs)
		}
		b.log.write(slog.LevelWarn, "egress blocked", args...)
		switch {
		case rerr.status == 403:
			b.cfg.Blocks.add(Block{Destination: target, Allow: allowRule(rerr.allow, target)})
		case rerr.reason == "resolve-failed":
			b.cfg.Blocks.add(Block{Destination: target, Unresolved: true})
		}
		writeRefusal(guest, rerr)
		return
	}
	g.target = target
	upstream, addr, err := b.dial(ctx, g)
	if err != nil {
		b.log.write(slog.LevelWarn, "egress failed", "destination", target, "rule", g.rule, "reason", "dial-failed", "error", err)
		writeRefusal(guest, refuse(502, "dial-failed"))
		return
	}
	if !b.track(upstream) {
		closeConn(upstream)
		return
	}
	defer b.untrack(upstream)
	b.cfg.Blocks.allowed(target)
	b.log.write(slog.LevelDebug, "egress allowed", "destination", target, "address", addr, "rule", g.rule)
	if _, err := io.WriteString(guest, "HTTP/1.1 200 OK\r\n\r\n"); err != nil {
		return
	}
	if err := guest.SetDeadline(time.Time{}); err != nil {
		return
	}
	// Bytes the guest sent after the head are tunnel data.
	buffered, err := br.Peek(br.Buffered())
	if err != nil {
		return
	}
	if len(buffered) > 0 {
		if err := upstream.SetWriteDeadline(time.Now().Add(dialTimeout)); err != nil {
			return
		}
		if _, err := upstream.Write(buffered); err != nil {
			return
		}
		if err := upstream.SetWriteDeadline(time.Time{}); err != nil {
			return
		}
	}
	relay(guest, upstream, b.cfg.IdleTimeout)
}

// authorize decides a request for host and port. An allowed name may
// resolve to private addresses, though not to this host or its link-local
// and loopback addresses. Every other address must be allowed by a rule,
// or be public with the internet mode.
func (b *Boundary) authorize(ctx context.Context, host string, port uint16) (grant, *refusal) {
	p := b.cfg.Policy()
	if addr, err := netip.ParseAddr(host); err == nil {
		ap := []netip.AddrPort{netip.AddrPortFrom(addr, port)}
		if r, ok := p.AddrRule(addr, port); ok {
			return grant{rule: r.String(), addrs: ap}, nil
		}
		if p.Mode == egress.ModeInternet && b.public(addr) {
			return grant{rule: egress.ModeInternet, addrs: ap}, nil
		}
		return grant{}, b.denied(p, host, "ip-not-on-allowlist")
	}
	r, named := p.NameRule(host, port)
	if host == egress.HostGateway {
		return b.authorizeHostGateway(ctx, p, port, r, named)
	}
	// Without the internet mode an unlisted name isn't even resolved, so
	// that the agent can't send data out in DNS queries.
	if !named && p.Mode != egress.ModeInternet {
		return grant{}, b.denied(p, host, "not-on-allowlist")
	}
	addrs, err := b.cfg.Resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return grant{}, refuse(502, "resolve-failed")
	}
	var g grant
	var refused []netip.Addr
	for _, a := range addrs {
		a = a.Unmap()
		ar, byAddr := p.AddrRule(a, port)
		switch {
		case named && (byAddr || !egress.Special(a) && !b.ownAddr(a)):
			g.rule = r.String()
		case byAddr:
			g.rule = cmp.Or(g.rule, ar.String())
		case !named && p.Mode == egress.ModeInternet && b.public(a):
			g.rule = cmp.Or(g.rule, egress.ModeInternet)
		default:
			refused = append(refused, a)
			continue
		}
		g.addrs = append(g.addrs, netip.AddrPortFrom(a, port))
	}
	if len(g.addrs) == 0 {
		rerr := refuse(403, "resolved-address-denied")
		rerr.addrs = refused
		rerr.allow = b.allowFor(refused, port)
		return grant{}, rerr
	}
	return g, nil
}

// authorizeHostGateway decides a request for [egress.HostGateway]. A rule
// that names it reaches the port on this host's loopback. The internet
// mode also reaches the ports that containers publish, as other containers
// do; the allowlist mode reaches only the ports of its rules.
func (b *Boundary) authorizeHostGateway(ctx context.Context, p egress.Policy, port uint16, r egress.Rule, named bool) (grant, *refusal) {
	hint := net.JoinHostPort(egress.HostGateway, strconv.Itoa(int(port)))
	if named {
		return grant{rule: r.String(), addrs: []netip.AddrPort{netip.AddrPortFrom(netip.AddrFrom4([4]byte{127, 0, 0, 1}), port)}}, nil
	}
	if p.Mode == egress.ModeInternet && b.cfg.HostGateway != nil {
		if ap, ok := b.cfg.HostGateway(ctx, port); ok {
			return grant{rule: "published-port", addrs: []netip.AddrPort{ap}}, nil
		}
	}
	rerr := b.denied(p, egress.HostGateway, "not-on-allowlist")
	rerr.allow = hint
	return grant{}, rerr
}

// allowRule returns the first of the rules that allow a refused request
// that --egress-allow takes: the one the refusal suggests, then the
// requested destination. It returns "" for neither, such as for an address
// on the tailnet, which only a name can allow.
func allowRule(suggested, target string) string {
	for _, r := range []string{suggested, target} {
		if r == "" {
			continue
		}
		if rule, err := egress.ParseRule(r); err == nil {
			return rule.String()
		}
	}
	return ""
}

// allowFor returns the rule that allows refused addresses of a name on
// port: host.docker.internal for an address of this host, or else the
// first address.
func (b *Boundary) allowFor(refused []netip.Addr, port uint16) string {
	for _, a := range refused {
		if a.IsLoopback() || b.ownAddr(a) {
			return net.JoinHostPort(egress.HostGateway, strconv.Itoa(int(port)))
		}
	}
	if len(refused) == 0 {
		return ""
	}
	return netip.AddrPortFrom(refused[0], port).String()
}

// denied refuses host with reason, or with port-not-allowed when a rule
// names host on another port.
func (b *Boundary) denied(p egress.Policy, host, reason string) *refusal {
	if p.NamesHost(host) {
		return refuse(403, "port-not-allowed")
	}
	return refuse(403, reason)
}

// public reports whether addr is public, isn't an address of this host,
// and is reached through a default route, so that networks this host
// routes elsewhere, such as the LAN, a VPN or a bridge, stay private.
func (b *Boundary) public(addr netip.Addr) bool {
	if !egress.Public(addr) || b.ownAddr(addr) {
		return false
	}
	ok, err := b.cfg.Network.DefaultRoute(addr)
	if err != nil {
		b.cfg.Log.Warn("finding the route to an address; refusing it", "address", addr, "error", err)
		return false
	}
	return ok
}

// ownAddr reports whether addr is an address of this host.
func (b *Boundary) ownAddr(addr netip.Addr) bool {
	own, err := b.cfg.Network.OwnAddr(addr)
	if err != nil {
		b.cfg.Log.Warn("listing this host's addresses; refusing connections that need them", "error", err)
		return true
	}
	return own
}

// dial connects to the first of the checked addresses that answers.
func (b *Boundary) dial(ctx context.Context, g grant) (net.Conn, netip.AddrPort, error) {
	var d net.Dialer
	var errs []error
	for _, ap := range g.addrs {
		c, err := d.DialContext(ctx, "tcp", ap.String())
		if err == nil {
			return c, ap, nil
		}
		errs = append(errs, err)
		if ctx.Err() != nil {
			break
		}
	}
	return nil, netip.AddrPort{}, errors.Join(errs...)
}

// writeRefusal answers a refused request. Its error is the guest's to see.
func writeRefusal(c net.Conn, r *refusal) {
	text := map[int]string{400: "Bad Request", 403: "Forbidden", 405: "Method Not Allowed", 431: "Request Header Fields Too Large", 502: "Bad Gateway", 503: "Service Unavailable", 505: "HTTP Version Not Supported"}[r.status]
	if err := c.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return
	}
	if _, err := fmt.Fprintf(c, "HTTP/1.1 %d %s\r\nProxy-Status: a2a-serve; error=%s; reason=%s\r\nContent-Length: 0\r\nConnection: close\r\n\r\n", r.status, text, errorType(r.reason), r.reason); err != nil {
		slog.Debug("answering a refused egress request", "error", err)
	}
}

// errorType maps a reason token to its RFC 9209 error type.
func errorType(reason string) string {
	switch reason {
	case "not-on-allowlist", "port-not-allowed":
		return "http_request_denied"
	case "ip-not-on-allowlist", "resolved-address-denied", "scoped-ip":
		return "destination_ip_prohibited"
	case "resolve-failed":
		return "dns_error"
	case "dial-failed":
		return "destination_unavailable"
	case "busy":
		return "connection_limit_reached"
	case "unsupported-version":
		return "http_protocol_error"
	}
	return "http_request_error"
}

// readConnect reads a CONNECT request head and returns its destination.
// It parses the head itself, since net/http drops the Host field before
// it can be compared with the target.
func readConnect(br *bufio.Reader) (string, uint16, *refusal) {
	line, err := readLine(br)
	if err != nil {
		return "", 0, err
	}
	fields := strings.Split(line, " ")
	if len(fields) != 3 || fields[0] == "" || fields[1] == "" {
		return "", 0, refuse(400, "malformed-request-line")
	}
	method, target, version := fields[0], fields[1], fields[2]
	if version != "HTTP/1.1" && version != "HTTP/1.0" {
		if strings.HasPrefix(version, "HTTP/") {
			return "", 0, refuse(505, "unsupported-version")
		}
		return "", 0, refuse(400, "malformed-request-line")
	}
	var hostField string
	hosts := 0
	for {
		line, err := readLine(br)
		if err != nil {
			return "", 0, err
		}
		if line == "" {
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok || name == "" || strings.ContainsAny(name, " \t") || strings.ContainsFunc(line, isControl) {
			return "", 0, refuse(400, "malformed-header")
		}
		if strings.EqualFold(name, "Host") {
			hosts++
			hostField = strings.TrimSpace(value)
		}
	}
	if hosts > 1 {
		return "", 0, refuse(400, "duplicate-host")
	}
	if method != "CONNECT" {
		return "", 0, refuse(405, "connect-only")
	}
	if version == "HTTP/1.1" && hosts == 0 {
		return "", 0, refuse(400, "missing-host")
	}
	host, port, rerr := parseAuthority(target)
	if rerr != nil {
		return "", 0, rerr
	}
	if hosts == 1 {
		h, p, rerr := parseAuthority(hostField)
		if rerr != nil || h != host || p != port {
			return "", 0, refuse(400, "authority-mismatch")
		}
	}
	return host, port, nil
}

func isControl(r rune) bool { return r < 0x20 && r != '\t' || r == 0x7f }

// readLine reads one line of the head without its line ending.
func readLine(br *bufio.Reader) (string, *refusal) {
	line, err := br.ReadString('\n')
	if errors.Is(err, io.EOF) && len(line) > 0 {
		return "", refuse(431, "head-too-large")
	}
	if err != nil {
		return "", refuse(400, "malformed-request-line")
	}
	return strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r"), nil
}

// parseAuthority parses host:port, with an IPv6 address in brackets, and
// returns the normalized host.
func parseAuthority(s string) (string, uint16, *refusal) {
	if strings.ContainsAny(s, "/?#@") {
		return "", 0, refuse(400, "malformed-target")
	}
	h, p, err := net.SplitHostPort(s)
	if err != nil || h == "" {
		return "", 0, refuse(400, "malformed-target")
	}
	port, err := egress.ParsePort(p)
	if err != nil {
		return "", 0, refuse(400, "malformed-port")
	}
	if strings.Contains(h, "%") {
		return "", 0, refuse(403, "scoped-ip")
	}
	if addr, err := netip.ParseAddr(h); err == nil {
		if addr.Is6() && !addr.Is4In6() && !strings.HasPrefix(s, "[") {
			return "", 0, refuse(400, "malformed-target")
		}
		return addr.Unmap().String(), port, nil
	}
	name, err := egress.NormalizeHost(h)
	if err != nil {
		return "", 0, refuse(400, "malformed-target")
	}
	return name, port, nil
}

// relay copies between the guest and upstream until both directions end,
// passing each end of input on as a half-close, or until neither carried
// data for idle.
func relay(guest, upstream net.Conn, idle time.Duration) {
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	done := make(chan struct{})
	var wg sync.WaitGroup
	copyHalf := func(dst, src net.Conn) {
		defer wg.Done()
		buf := make([]byte, 32<<10)
		for {
			n, err := src.Read(buf)
			if n > 0 {
				last.Store(time.Now().UnixNano())
				if _, werr := dst.Write(buf[:n]); werr != nil {
					closeConn(src)
					closeConn(dst)
					return
				}
			}
			if err != nil {
				if cw, ok := dst.(interface{ CloseWrite() error }); ok && errors.Is(err, io.EOF) {
					if err := cw.CloseWrite(); err == nil {
						return
					}
				}
				closeConn(src)
				closeConn(dst)
				return
			}
		}
	}
	wg.Add(2)
	go copyHalf(upstream, guest)
	go copyHalf(guest, upstream)
	go func() {
		t := time.NewTicker(min(idle/4, time.Minute))
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if time.Since(time.Unix(0, last.Load())) > idle {
					closeConn(guest)
					closeConn(upstream)
					return
				}
			}
		}
	}()
	wg.Wait()
	close(done)
}

// maxRecentBlocks bounds the destinations [Blocks] remembers.
const maxRecentBlocks = 5

// Blocks counts the requests that boundaries refuse by policy and keeps
// the latest destinations, so that status can show them. A destination
// stays until a connection to it succeeds.
type Blocks struct {
	mu     sync.Mutex
	count  int
	recent []Block
}

// Block is a refused destination and the --egress-allow rule that allows
// it, or a name that doesn't resolve.
type Block struct {
	Destination, Allow string
	Unresolved         bool
}

func (b *Blocks) add(blk Block) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.count++
	b.recent = slices.DeleteFunc(b.recent, func(x Block) bool { return x.Destination == blk.Destination })
	b.recent = slices.Insert(b.recent, 0, blk)
	if len(b.recent) > maxRecentBlocks {
		b.recent = b.recent[:maxRecentBlocks]
	}
}

// allowed forgets dest, which a connection reached.
func (b *Blocks) allowed(dest string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.recent = slices.DeleteFunc(b.recent, func(x Block) bool { return x.Destination == dest })
}

// Snapshot returns the number of refused requests and the latest refused
// destinations, newest first.
func (b *Blocks) Snapshot() (int, []Block) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.count, slices.Clone(b.recent)
}

// decisionLog writes the decisions of one boundary, at most maxLogLines a
// minute, so that an agent can't flood the server log. It counts the
// decisions over the limit and logs the count with the first decision of
// a later minute, or when the boundary closes.
type decisionLog struct {
	log     *slog.Logger
	sandbox string

	mu      sync.Mutex
	window  time.Time
	lines   int
	dropped int
}

func (d *decisionLog) write(level slog.Level, msg string, args ...any) {
	if !d.log.Enabled(context.Background(), level) {
		return
	}
	d.mu.Lock()
	dropped := 0
	if now := time.Now(); now.Sub(d.window) >= time.Minute {
		dropped = d.dropped
		d.window, d.lines, d.dropped = now, 0, 0
	}
	over := d.lines >= maxLogLines
	if over {
		d.dropped++
	} else {
		d.lines++
	}
	d.mu.Unlock()
	d.logDropped(dropped)
	if over {
		return
	}
	d.log.Log(context.Background(), level, msg, append([]any{"sandbox", d.sandbox}, args...)...)
}

// flush logs the count of the decisions over the limit.
func (d *decisionLog) flush() {
	d.mu.Lock()
	dropped := d.dropped
	d.dropped = 0
	d.mu.Unlock()
	d.logDropped(dropped)
}

func (d *decisionLog) logDropped(n int) {
	if n > 0 {
		d.log.Warn("egress decisions over the log limit", "sandbox", d.sandbox, "count", n, "limit_per_minute", maxLogLines)
	}
}
