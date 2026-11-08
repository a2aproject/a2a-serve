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

// Package egress guards the requests the server sends to addresses that
// other parties pick, such as the interfaces on a contact's card and the
// redirects of a webhook.
package egress

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ErrPrivateAddress is the error of a connection from a public origin to a
// private address.
var ErrPrivateAddress = errors.New("a public URL can't lead to a private address")

// ErrInsecureRedirect is the error of a redirect from https to http.
var ErrInsecureRedirect = errors.New("refusing a redirect from https to http")

// ErrResponseTooLarge is the error of reading a response over the limit.
var ErrResponseTooLarge = errors.New("response too large")

var nonPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	// Shared address space, where Tailscale puts tailnet addresses.
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
	// NAT64 and 6to4 addresses embed an IPv4 address, which may be private.
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2002::/16"),
	// IPv4-compatible, IETF protocol, documentation, deprecated site-local
	// and SRv6 addresses.
	netip.MustParsePrefix("::/96"),
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("3fff::/20"),
	netip.MustParsePrefix("5f00::/16"),
	netip.MustParsePrefix("fec0::/10"),
}

// Special reports whether ip belongs to this host or its link, or to no
// host at all: loopback, unspecified, link-local, as cloud metadata
// services are, multicast or reserved. A rule that allows a name never
// reaches it.
func Special(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip == netip.AddrFrom4([4]byte{255, 255, 255, 255}) ||
		netip.MustParsePrefix("0.0.0.0/8").Contains(ip) || netip.MustParsePrefix("240.0.0.0/4").Contains(ip)
}

// Public reports whether ip is a public unicast address: not loopback,
// private, link-local or on the tailnet.
func Public(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return false
	}
	for _, p := range nonPublic {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

// Transport returns a transport for requests that start at origin, a URL
// the owner configured. Once its first connection to the origin reaches a
// public address, it connects to public addresses only, so that a card or
// a redirect can't lead it into a private network. An origin on a private
// network, such as the tailnet or the LAN, may lead anywhere.
//
// It checks the address each connection dials, so a name that resolves
// differently later can't get around it. Connections through a proxy from
// the environment go unchecked, as the proxy picks the address.
func Transport(origin string) *http.Transport {
	return newTransport(origin, func(ap netip.AddrPort) bool { return Public(ap.Addr()) })
}

func newTransport(origin string, public func(netip.AddrPort) bool) *http.Transport {
	g := &guard{public: public, proxies: map[string]bool{}}
	if u, err := url.Parse(origin); err == nil {
		g.origin = net.JoinHostPort(u.Hostname(), cmp.Or(u.Port(), defaultPorts[u.Scheme]))
	}
	return &http.Transport{
		Proxy:                 g.proxy,
		DialContext:           g.dial,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

var defaultPorts = map[string]string{"http": "80", "https": "443", "socks5": "1080", "socks5h": "1080"}

// guard decides which addresses the connections of one transport may reach.
type guard struct {
	// origin is the host and port of the origin.
	origin string
	public func(netip.AddrPort) bool

	mu sync.Mutex
	// decided is set by the first connection to the origin; until then
	// the origin counts as public.
	decided bool
	// open is set when the origin is on a private network.
	open    bool
	proxies map[string]bool
}

func (g *guard) proxy(r *http.Request) (*url.URL, error) {
	u, err := http.ProxyFromEnvironment(r)
	if u == nil || err != nil {
		return u, err
	}
	g.mu.Lock()
	g.proxies[net.JoinHostPort(u.Hostname(), cmp.Or(u.Port(), defaultPorts[u.Scheme], "80"))] = true
	g.mu.Unlock()
	return u, nil
}

func (g *guard) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	viaProxy := g.proxies[addr]
	first := !g.decided && strings.EqualFold(addr, g.origin)
	g.mu.Unlock()
	d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	if !viaProxy && !first {
		d.Control = func(_, address string, _ syscall.RawConn) error { return g.check(host, address) }
	}
	conn, err := d.DialContext(ctx, network, addr)
	if err != nil || !first {
		return conn, err
	}
	// The address the first connection to the origin reached decides, so
	// that a name with a closed private address next to its public one
	// can't open the guard.
	if err := g.decide(host, conn.RemoteAddr().String()); err != nil {
		return nil, errors.Join(err, conn.Close())
	}
	return conn, nil
}

func (g *guard) decide(host, address string) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return err
	}
	g.mu.Lock()
	if !g.decided {
		g.decided, g.open = true, !g.public(ap)
	}
	g.mu.Unlock()
	return g.check(host, address)
}

func (g *guard) check(host, address string) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return err
	}
	g.mu.Lock()
	open := g.open
	g.mu.Unlock()
	if open || g.public(ap) {
		return nil
	}
	return fmt.Errorf("%w: %s is at %s", ErrPrivateAddress, host, ap.Addr())
}

// CheckRedirect is the redirect policy of [http.Client], which also refuses
// a redirect from https to http.
func CheckRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if via[len(via)-1].URL.Scheme == "https" && req.URL.Scheme != "https" {
		return ErrInsecureRedirect
	}
	return nil
}

// LimitResponses returns rt with response bodies limited to n bytes. Event
// streams aren't limited, because they last as long as their task.
func LimitResponses(rt http.RoundTripper, n int64) http.RoundTripper {
	return &limitTransport{base: rt, n: n}
}

type limitTransport struct {
	base http.RoundTripper
	n    int64
}

func (t *limitTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	if mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err == nil && mt == "text/event-stream" {
		return resp, nil
	}
	tooLarge := fmt.Errorf("%w: over %d bytes", ErrResponseTooLarge, t.n)
	if resp.ContentLength > t.n {
		return nil, errors.Join(tooLarge, resp.Body.Close())
	}
	resp.Body = &limitedBody{ReadCloser: resp.Body, left: t.n, err: tooLarge}
	return resp, nil
}

type limitedBody struct {
	io.ReadCloser
	left int64
	err  error
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		var probe [1]byte
		n, err := b.ReadCloser.Read(probe[:])
		if n > 0 {
			return 0, b.err
		}
		return 0, err
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.ReadCloser.Read(p)
	b.left -= int64(n)
	return n, err
}
