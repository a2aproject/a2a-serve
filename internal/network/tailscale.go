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
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/net/tsaddr"
	"tailscale.com/tailcfg"
	"tailscale.com/tsnet"
	"tailscale.com/util/dnsname"
)

// Tailscale is a [Provider] which joins the agent server to a tailnet as its
// own machine with tsnet: private network, verified identity, discovery and
// an optional public URL through Funnel, all in-process.
type Tailscale struct {
	// Hostname is the machine name: https://<hostname>.<tailnet>.ts.net.
	Hostname string
	// StateDir holds the node identity, so the URL survives restarts and moves.
	StateDir string
	// ControlURL is empty for Tailscale, or a Headscale URL.
	ControlURL string
	// AuthKey joins the tailnet without the interactive login.
	AuthKey string
	// Funnel makes ListenPublic available. Private and public then share
	// one listener on :443, split by whether a connection came through Funnel.
	Funnel bool
	// Logf receives messages for the user, such as the login URL.
	Logf func(format string, args ...any)

	mu  sync.Mutex
	srv *tsnet.Server
	// started is set once srv is up; readers check it without mu.
	started atomic.Bool
	private *splitListener
	public  *splitListener
	url     *url.URL
	cards   *CardCache
}

var _ Provider = (*Tailscale)(nil)

// Name implements [Provider].
func (t *Tailscale) Name() string { return ProviderTailscale }

func (t *Tailscale) start(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.started.Load() {
		return nil
	}
	logf := t.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	var login loginPrompt
	srv := &tsnet.Server{
		Hostname:   t.Hostname,
		Dir:        t.StateDir,
		ControlURL: t.ControlURL,
		AuthKey:    t.AuthKey,
		UserLogf: func(format string, args ...any) {
			msg := fmt.Sprintf(format, args...)
			if strings.Contains(msg, "login.") || strings.Contains(msg, "/a/") || strings.Contains(msg, "AuthURL") {
				if u := extractURL(msg); login.show(u) {
					logf("tailscale: open %s to add %q to your tailnet", u, t.Hostname)
				}
				return
			}
			slog.Debug("tailscale", "msg", msg)
		},
		Logf: func(format string, args ...any) { slog.Debug("tsnet", "msg", fmt.Sprintf(format, args...)) },
	}
	st, err := srv.Up(ctx)
	if err != nil {
		return errors.Join(fmt.Errorf("joining the tailnet: %w", err), srv.Close())
	}
	domains := srv.CertDomains()
	if len(domains) == 0 {
		return errors.Join(errors.New("tailscale: HTTPS certificates are disabled in this tailnet; enable HTTPS on the DNS page of the admin console (https://login.tailscale.com/admin/dns)"), srv.Close())
	}
	if name, renamed := assignedName(t.Hostname, domains[0]); renamed {
		where := "in the admin console"
		if t.ControlURL == "" {
			where = "at https://login.tailscale.com/admin/machines"
		}
		logf("tailscale: the name %q is taken on this tailnet, so this machine is %q; remove the old machine and rename this one %s", dnsname.SanitizeHostname(t.Hostname), name, where)
	}
	t.srv = srv
	t.url = &url.URL{Scheme: "https", Host: domains[0]}
	t.cards = newCardCache()
	slog.Info("joined tailnet", "host", domains[0], "ip", st.TailscaleIPs)
	t.started.Store(true)
	go t.fetchCert(ctx, domains[0], logf)
	if slog.Default().Enabled(ctx, slog.LevelDebug) {
		go t.logFilterRules(ctx)
	}
	return nil
}

// logFilterRules logs the packet filter control sends to the node whenever it
// changes. Funnel only works when it grants the ingress capability to the
// Funnel relays.
func (t *Tailscale) logFilterRules(ctx context.Context) {
	lc, err := t.srv.LocalClient()
	if err != nil {
		slog.Debug("tailscale packet filter", "error", err)
		return
	}
	watcher, err := lc.WatchIPNBus(ctx, ipn.NotifyInitialNetMap|ipn.NotifyRateLimit)
	if err != nil {
		slog.Debug("tailscale packet filter", "error", err)
		return
	}
	defer func() {
		if err := watcher.Close(); err != nil {
			slog.Debug("closing the tailscale bus watcher", "error", err)
		}
	}()
	var last string
	for {
		n, err := watcher.Next()
		if err != nil {
			return
		}
		if n.NetMap == nil {
			continue
		}
		rules, err := lc.DebugPacketFilterRules(ctx)
		if err != nil {
			slog.Debug("tailscale packet filter", "error", err)
			continue
		}
		b, err := json.Marshal(rules)
		if err != nil || string(b) == last {
			continue
		}
		last = string(b)
		slog.Debug("tailscale packet filter", "rules", last)
	}
}

// fetchCert gets the TLS certificate at startup: tsnet otherwise requests it
// on the first handshake, which delays the first request and hides issuance
// errors until then.
func (t *Tailscale) fetchCert(ctx context.Context, domain string, logf func(string, ...any)) {
	lc, err := t.srv.LocalClient()
	if err != nil {
		logf("! tailscale: getting the certificate for %s: %v", domain, err)
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	if _, _, err := lc.CertPair(ctx, domain); err != nil {
		if ctx.Err() != nil && !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return
		}
		logf("! tailscale: getting the certificate for %s: %v", domain, err)
		return
	}
	slog.Info("tailscale certificate ready", "domain", domain)
}

// ListenPrivate implements [Provider].
func (t *Tailscale) ListenPrivate(ctx context.Context) (net.Listener, *url.URL, error) {
	if err := t.start(ctx); err != nil {
		return nil, nil, err
	}
	if !t.Funnel {
		ln, err := t.srv.ListenTLS("tcp", ":443")
		if err != nil {
			return nil, nil, err
		}
		return ln, t.url, nil
	}
	if err := t.split(); err != nil {
		return nil, nil, err
	}
	t.private.claim()
	return t.private, t.url, nil
}

// ListenPublic implements [Provider].
func (t *Tailscale) ListenPublic(ctx context.Context) (net.Listener, *url.URL, error) {
	if !t.Funnel {
		return nil, nil, ErrUnsupported
	}
	if err := t.start(ctx); err != nil {
		return nil, nil, err
	}
	if err := t.split(); err != nil {
		return nil, nil, err
	}
	t.public.claim()
	return t.public, t.url, nil
}

// split opens one Funnel listener, which accepts both tailnet and internet
// connections, and routes each accepted connection by its origin.
func (t *Tailscale) split() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.private != nil {
		return nil
	}
	ln, err := t.srv.ListenFunnel("tcp", ":443")
	if err != nil {
		return fmt.Errorf("tailscale funnel: %w; allow Funnel for this machine in the tailnet policy (nodeAttrs \"funnel\") and enable HTTPS, see https://tailscale.com/kb/1223/funnel", err)
	}
	t.private = newSplitListener(ln.Addr(), ln.Close)
	t.public = newSplitListener(ln.Addr(), nil)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				if !errors.Is(err, net.ErrClosed) {
					slog.Warn("tailscale listener stopped", "error", err)
				}
				return
			}
			if isFunnel(c) {
				go t.public.deliver(c)
			} else {
				go t.private.deliver(c)
			}
		}
	}()
	return nil
}

func isFunnel(c net.Conn) bool {
	_, ok := funnelConn(c)
	return ok
}

func funnelConn(c net.Conn) (*ipn.FunnelConn, bool) {
	if tc, ok := c.(*tls.Conn); ok {
		c = tc.NetConn()
	}
	fc, ok := c.(*ipn.FunnelConn)
	return fc, ok
}

// FunnelSource returns the address of the client that opened a Funnel
// connection; its remote address is the Funnel relay's. An IPv6 address
// is cut to its /64, which one client usually holds whole.
func FunnelSource(c net.Conn) (string, bool) {
	fc, ok := funnelConn(c)
	if !ok || !fc.Src.IsValid() {
		return "", false
	}
	addr := fc.Src.Addr().Unmap()
	if addr.Is6() {
		return netip.PrefixFrom(addr, 64).Masked().String(), true
	}
	return addr.String(), true
}

// WhoIs implements [Provider].
func (t *Tailscale) WhoIs(ctx context.Context, remoteAddr string) (*Peer, error) {
	if !t.started.Load() {
		return nil, ErrNoIdentity
	}
	lc, err := t.srv.LocalClient()
	if err != nil {
		return nil, err
	}
	who, err := lc.WhoIs(ctx, remoteAddr)
	if err != nil || who.UserProfile == nil || who.Node == nil {
		return nil, ErrNoIdentity
	}
	st, err := lc.StatusWithoutPeers(ctx)
	if err != nil {
		return nil, err
	}
	n := tailnetNode{
		id: string(who.Node.StableID), machine: who.Node.ComputedName, tagged: who.Node.IsTagged(),
		login: who.UserProfile.LoginName, user: who.Node.User,
	}
	for _, a := range who.Node.Addresses {
		n.addrs = append(n.addrs, a.Addr())
	}
	local, err := localAddrs()
	return tailnetPeer(n, selfUser(st), local, err), nil
}

// Member implements [Provider].
func (t *Tailscale) Member(ctx context.Context, node string) (*Peer, error) {
	if !t.started.Load() {
		return nil, ErrNoIdentity
	}
	lc, err := t.srv.LocalClient()
	if err != nil {
		return nil, err
	}
	st, err := lc.Status(ctx)
	if err != nil {
		return nil, err
	}
	for _, ps := range st.Peer {
		if string(ps.ID) != node {
			continue
		}
		n := tailnetNode{
			id: node, machine: ps.HostName, tagged: ps.Tags != nil && ps.Tags.Len() > 0,
			login: st.User[ps.UserID].LoginName, user: ps.UserID, addrs: ps.TailscaleIPs,
		}
		local, err := localAddrs()
		return tailnetPeer(n, selfUser(st), local, err), nil
	}
	return nil, ErrNoIdentity
}

// tailnetNode is what the tailnet reports of a machine.
type tailnetNode struct {
	id, machine string
	tagged      bool
	login       string
	user        tailcfg.UserID
	addrs       []netip.Addr
}

// tailnetPeer identifies node to a server run by the tailnet user self, on
// a machine whose interfaces have the addresses local, or unknown ones when
// localErr is set.
func tailnetPeer(node tailnetNode, self tailcfg.UserID, local []netip.Addr, localErr error) *Peer {
	p := &Peer{Node: node.id, Machine: node.machine}
	// Tagged nodes belong to the tailnet, not to the user who tagged them.
	if node.tagged {
		return p
	}
	// The machine's own Tailscale node carries the sandbox's connections to
	// the tailnet, so it gets neither the owner nor the owner's email. Its
	// tailnet addresses are on a local interface; the server's tsnet node
	// has none.
	if localErr != nil {
		slog.Warn("treating a tailnet caller as this machine", "node", node.id, "error", localErr)
		return p
	}
	if slices.ContainsFunc(node.addrs, func(a netip.Addr) bool { return tsaddr.IsTailscaleIP(a) && slices.Contains(local, a) }) {
		return p
	}
	if IsEmail(node.login) {
		p.Email = node.login
	}
	p.Self = self != 0 && node.user == self
	return p
}

// localAddrs returns the addresses of this machine's network interfaces.
func localAddrs() ([]netip.Addr, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, fmt.Errorf("listing the addresses of this machine: %w", err)
	}
	var out []netip.Addr
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		if ip, ok := netip.AddrFromSlice(n.IP); ok {
			out = append(out, ip.Unmap())
		}
	}
	return out, nil
}

func selfUser(st *ipnstate.Status) tailcfg.UserID {
	if st.Self == nil {
		return 0
	}
	return st.Self.UserID
}

// Peers implements [Provider].
func (t *Tailscale) Peers(ctx context.Context) ([]PeerInfo, error) {
	if !t.started.Load() {
		return nil, ErrUnsupported
	}
	lc, err := t.srv.LocalClient()
	if err != nil {
		return nil, err
	}
	st, err := lc.Status(ctx)
	if err != nil {
		return nil, err
	}
	var (
		candidates []PeerInfo
		taken      []string
	)
	if st.Self != nil && st.Self.DNSName != "" {
		_, name := machineName(st.Self.DNSName)
		taken = append(taken, name)
	}
	for _, p := range st.Peer {
		if p.DNSName == "" {
			continue
		}
		host, name := machineName(p.DNSName)
		if !p.Online {
			taken = append(taken, name)
			continue
		}
		candidates = append(candidates, PeerInfo{Name: name, URL: "https://" + host, Online: true, Node: string(p.ID)})
	}
	return t.cards.ProbeAgents(ctx, t.HTTPClient(), candidates, taken), nil
}

// machineName returns the host of a MagicDNS name and the machine name in it.
func machineName(dnsName string) (host, name string) {
	host = strings.TrimSuffix(dnsName, ".")
	name, _, _ = strings.Cut(host, ".")
	return host, name
}

// HTTPClient implements [Provider]. tsnet runs in userspace, so calls to
// tailnet peers must go through its dialer, not the host network.
func (t *Tailscale) HTTPClient() *http.Client {
	if !t.started.Load() {
		return http.DefaultClient
	}
	return t.srv.HTTPClient()
}

// Close implements [Provider].
func (t *Tailscale) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	var errs []error
	if t.private != nil {
		errs = append(errs, t.private.Close(), t.public.Close())
	}
	if t.srv != nil {
		errs = append(errs, t.srv.Close())
	}
	return errors.Join(errs...)
}

// loginRepeat is how often tsnet's repeated login link is shown again.
const loginRepeat = 10

// loginPrompt throttles the login link, which tsnet logs on every status poll
// until the machine is approved: a new link is shown at once, a repeated one
// every loginRepeat times.
type loginPrompt struct {
	mu    sync.Mutex
	url   string
	count int
}

func (p *loginPrompt) show(link string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if link != p.url {
		p.url, p.count = link, 0
	}
	p.count++
	return p.count%loginRepeat == 1
}

// assignedName returns the machine name in the tailnet domain, and whether it
// differs from the requested hostname, as when Tailscale adds a suffix
// because another machine has the name.
func assignedName(hostname, domain string) (string, bool) {
	name, _, _ := strings.Cut(domain, ".")
	return name, name != dnsname.SanitizeHostname(hostname)
}

func extractURL(msg string) string {
	for f := range strings.FieldsSeq(msg) {
		if strings.HasPrefix(f, "https://") {
			return strings.TrimRight(f, ".,")
		}
	}
	return msg
}
