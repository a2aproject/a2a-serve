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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/egress"
	"github.com/a2aproject/a2a-serve/internal/testutil"
)

// fakeNetwork is a host with the addresses and links of local, whose
// routes to every other address are default routes.
type fakeNetwork struct{ local []netip.Prefix }

func (n fakeNetwork) OwnAddr(addr netip.Addr) (bool, error) {
	for _, p := range n.local {
		if p.Addr() == addr {
			return true, nil
		}
	}
	return false, nil
}

func (n fakeNetwork) DefaultRoute(addr netip.Addr) (bool, error) {
	for _, p := range n.local {
		if p.Contains(addr) {
			return false, nil
		}
	}
	return true, nil
}

// fakeResolver resolves the names it holds.
type fakeResolver map[string][]netip.Addr

func (r fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if addrs, ok := r[host]; ok {
		return addrs, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

func addrs(s ...string) []netip.Addr {
	out := make([]netip.Addr, len(s))
	for i, a := range s {
		out[i] = netip.MustParseAddr(a)
	}
	return out
}

func mustPolicy(t *testing.T, mode string, allow ...string) egress.Policy {
	t.Helper()
	p, err := egress.NewPolicy(mode, allow)
	if err != nil {
		t.Fatalf("NewPolicy() error = %v", err)
	}
	return p
}

func TestAuthorize(t *testing.T) {
	t.Parallel()
	resolver := fakeResolver{
		"example.com":   addrs("93.184.216.34", "2606:2800:220:1::1"),
		"mixed.example": addrs("10.1.1.1", "93.184.216.35"),
		"rebind.test":   addrs("192.168.1.10"),
		"tailnet.test":  addrs("100.101.102.103"),
		"me.test":       addrs("203.0.113.7"),
		"nas.lan":       addrs("192.168.1.20"),
		"loop.test":     addrs("127.0.0.1"),
		"meta.corp":     addrs("169.254.169.254"),
		"self.corp":     addrs("203.0.113.7"),
		"zero.corp":     addrs("0.0.0.0", "10.2.2.2"),
	}
	gateway := func(_ context.Context, port uint16) (netip.AddrPort, bool) {
		return netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), port), port == 3000
	}
	tests := []struct {
		name   string
		policy egress.Policy
		host   string
		port   uint16
		want   grant
		reason string
	}{
		{name: "public name", policy: mustPolicy(t, ""), host: "example.com", port: 443,
			want: grant{rule: "internet", addrs: []netip.AddrPort{netip.MustParseAddrPort("93.184.216.34:443"), netip.MustParseAddrPort("[2606:2800:220:1::1]:443")}}},
		{name: "public literal", policy: mustPolicy(t, ""), host: "93.184.216.34", port: 80,
			want: grant{rule: "internet", addrs: []netip.AddrPort{netip.MustParseAddrPort("93.184.216.34:80")}}},
		{name: "private addresses of a public name are dropped", policy: mustPolicy(t, ""), host: "mixed.example", port: 443,
			want: grant{rule: "internet", addrs: []netip.AddrPort{netip.MustParseAddrPort("93.184.216.35:443")}}},
		{name: "name rebound to the LAN", policy: mustPolicy(t, ""), host: "rebind.test", port: 443, reason: "resolved-address-denied"},
		{name: "name on the tailnet", policy: mustPolicy(t, ""), host: "tailnet.test", port: 443, reason: "resolved-address-denied"},
		{name: "name of loopback", policy: mustPolicy(t, ""), host: "loop.test", port: 80, reason: "resolved-address-denied"},
		{name: "address of this host", policy: mustPolicy(t, ""), host: "me.test", port: 22, reason: "resolved-address-denied"},
		{name: "LAN literal", policy: mustPolicy(t, ""), host: "192.168.1.1", port: 80, reason: "ip-not-on-allowlist"},
		{name: "tailnet literal", policy: mustPolicy(t, ""), host: "100.100.100.100", port: 80, reason: "ip-not-on-allowlist"},
		{name: "tailnet IPv6 literal", policy: mustPolicy(t, ""), host: "fd7a:115c:a1e0::1", port: 80, reason: "ip-not-on-allowlist"},
		{name: "loopback literal", policy: mustPolicy(t, ""), host: "127.0.0.1", port: 80, reason: "ip-not-on-allowlist"},
		{name: "metadata service", policy: mustPolicy(t, ""), host: "169.254.169.254", port: 80, reason: "ip-not-on-allowlist"},
		{name: "unknown name", policy: mustPolicy(t, ""), host: "missing.test", port: 443, reason: "resolve-failed"},
		{name: "neighbor on this host's link", policy: mustPolicy(t, ""), host: "203.0.113.9", port: 80, reason: "ip-not-on-allowlist"},
		{name: "global IPv6 address on this host's link", policy: mustPolicy(t, ""), host: "2001:470:1::9", port: 80, reason: "ip-not-on-allowlist"},
		{name: "allowed name can't reach loopback", policy: mustPolicy(t, "", "loop.test"), host: "loop.test", port: 80, reason: "resolved-address-denied"},
		{name: "allowed suffix can't reach a metadata service", policy: mustPolicy(t, egress.ModeAllowlist, "*.corp"), host: "meta.corp", port: 80, reason: "resolved-address-denied"},
		{name: "allowed suffix can't reach this host", policy: mustPolicy(t, egress.ModeAllowlist, "*.corp"), host: "self.corp", port: 22, reason: "resolved-address-denied"},
		{name: "allowed suffix skips the unspecified address", policy: mustPolicy(t, egress.ModeAllowlist, "*.corp"), host: "zero.corp", port: 22,
			want: grant{rule: "*.corp", addrs: []netip.AddrPort{netip.MustParseAddrPort("10.2.2.2:22")}}},
		{name: "allowed name reaches loopback an address rule allows", policy: mustPolicy(t, "", "loop.test", "127.0.0.1:80"), host: "loop.test", port: 80,
			want: grant{rule: "loop.test", addrs: []netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:80")}}},
		{name: "allowed name reaches its private address", policy: mustPolicy(t, "", "nas.lan"), host: "nas.lan", port: 22,
			want: grant{rule: "nas.lan", addrs: []netip.AddrPort{netip.MustParseAddrPort("192.168.1.20:22")}}},
		{name: "allowed range", policy: mustPolicy(t, "", "192.168.1.0/24:22"), host: "192.168.1.30", port: 22,
			want: grant{rule: "192.168.1.0/24:22", addrs: []netip.AddrPort{netip.MustParseAddrPort("192.168.1.30:22")}}},
		{name: "allowed range on another port", policy: mustPolicy(t, "", "192.168.1.0/24:22"), host: "192.168.1.30", port: 80, reason: "port-not-allowed"},
		{name: "allowed range admits a resolved address", policy: mustPolicy(t, "", "192.168.1.0/24"), host: "rebind.test", port: 80,
			want: grant{rule: "192.168.1.0/24", addrs: []netip.AddrPort{netip.MustParseAddrPort("192.168.1.10:80")}}},
		{name: "allowlist refuses the internet", policy: mustPolicy(t, egress.ModeAllowlist, "nas.lan"), host: "example.com", port: 443, reason: "not-on-allowlist"},
		{name: "allowlist doesn't resolve unlisted names for address rules", policy: mustPolicy(t, egress.ModeAllowlist, "192.168.1.0/24"), host: "rebind.test", port: 80, reason: "not-on-allowlist"},
		{name: "allowlist refuses a public literal", policy: mustPolicy(t, egress.ModeAllowlist), host: "93.184.216.34", port: 443, reason: "ip-not-on-allowlist"},
		{name: "allowlist allows a listed name", policy: mustPolicy(t, egress.ModeAllowlist, "*.example.com:443"), host: "api.example.com", port: 443, reason: "resolve-failed"},
		{name: "allowlist refuses a listed name on another port", policy: mustPolicy(t, egress.ModeAllowlist, "nas.lan:22"), host: "nas.lan", port: 80, reason: "port-not-allowed"},
		{name: "host gateway on a published port", policy: mustPolicy(t, ""), host: egress.HostGateway, port: 3000,
			want: grant{rule: "published-port", addrs: []netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:3000")}}},
		{name: "host gateway on a port no container publishes", policy: mustPolicy(t, ""), host: egress.HostGateway, port: 22, reason: "not-on-allowlist"},
		{name: "a rule opens a host port", policy: mustPolicy(t, "", egress.HostGateway+":11434"), host: egress.HostGateway, port: 11434,
			want: grant{rule: egress.HostGateway + ":11434", addrs: []netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:11434")}}},
		{name: "allowlist refuses the host gateway", policy: mustPolicy(t, egress.ModeAllowlist), host: egress.HostGateway, port: 3000, reason: "not-on-allowlist"},
		{name: "a rule for another port keeps published ports", policy: mustPolicy(t, "", egress.HostGateway+":11434"), host: egress.HostGateway, port: 3000,
			want: grant{rule: "published-port", addrs: []netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:3000")}}},
		{name: "a rule for another port doesn't open an unpublished one", policy: mustPolicy(t, "", egress.HostGateway+":11434"), host: egress.HostGateway, port: 22, reason: "port-not-allowed"},
		{name: "allowlist limits the host gateway to its rules", policy: mustPolicy(t, egress.ModeAllowlist, egress.HostGateway+":11434"), host: egress.HostGateway, port: 3000, reason: "port-not-allowed"},
		{name: "allowlist allows a listed host gateway", policy: mustPolicy(t, egress.ModeAllowlist, egress.HostGateway), host: egress.HostGateway, port: 3000,
			want: grant{rule: egress.HostGateway, addrs: []netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:3000")}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := &Boundary{cfg: BoundaryConfig{
				Policy:      func() egress.Policy { return tc.policy },
				HostGateway: gateway,
				Resolver:    resolver,
				Network:     fakeNetwork{local: []netip.Prefix{netip.MustParsePrefix("203.0.113.7/24"), netip.MustParsePrefix("2001:470:1::5/64")}},
				Log:         slog.New(slog.DiscardHandler),
			}}
			got, rerr := b.authorize(context.Background(), tc.host, tc.port)
			reason := ""
			if rerr != nil {
				reason = rerr.reason
			}
			if reason != tc.reason {
				t.Fatalf("authorize(%s, %d) refusal = %q, want %q", tc.host, tc.port, reason, tc.reason)
			}
			if diff := cmp.Diff(tc.want, got, cmp.AllowUnexported(grant{}), cmp.Comparer(func(a, b netip.AddrPort) bool { return a == b })); diff != "" {
				t.Fatalf("authorize(%s, %d) wrong result (-want +got) diff = %s", tc.host, tc.port, diff)
			}
		})
	}
}

func TestReadConnect(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		head     string
		wantHost string
		wantPort uint16
		reason   string
	}{
		{name: "name", head: "CONNECT Example.COM.:443 HTTP/1.1\r\nHost: example.com:443\r\nUser-Agent: x\r\n\r\n", wantHost: "example.com", wantPort: 443},
		{name: "IPv6", head: "CONNECT [2001:db8::1]:22 HTTP/1.1\r\nHost: [2001:db8::1]:22\r\n\r\n", wantHost: "2001:db8::1", wantPort: 22},
		{name: "HTTP/1.0 without Host", head: "CONNECT 192.0.2.1:80 HTTP/1.0\r\n\r\n", wantHost: "192.0.2.1", wantPort: 80},
		{name: "GET", head: "GET http://example.com/ HTTP/1.1\r\nHost: example.com\r\n\r\n", reason: "connect-only"},
		{name: "missing Host", head: "CONNECT example.com:443 HTTP/1.1\r\n\r\n", reason: "missing-host"},
		{name: "duplicate Host", head: "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\nHost: example.com:443\r\n\r\n", reason: "duplicate-host"},
		{name: "Host names another destination", head: "CONNECT example.com:443 HTTP/1.1\r\nHost: other.com:443\r\n\r\n", reason: "authority-mismatch"},
		{name: "no port", head: "CONNECT example.com HTTP/1.1\r\nHost: example.com\r\n\r\n", reason: "malformed-target"},
		{name: "port zero", head: "CONNECT example.com:0 HTTP/1.1\r\nHost: example.com:0\r\n\r\n", reason: "malformed-port"},
		{name: "userinfo", head: "CONNECT u@example.com:443 HTTP/1.1\r\nHost: u@example.com:443\r\n\r\n", reason: "malformed-target"},
		{name: "zone", head: "CONNECT [fe80::1%eth0]:22 HTTP/1.1\r\nHost: [fe80::1%eth0]:22\r\n\r\n", reason: "scoped-ip"},
		{name: "bad name", head: "CONNECT a..b:443 HTTP/1.1\r\nHost: a..b:443\r\n\r\n", reason: "malformed-target"},
		{name: "space before colon", head: "CONNECT example.com:443 HTTP/1.1\r\nHost : example.com:443\r\n\r\n", reason: "malformed-header"},
		{name: "HTTP/2 preface", head: "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n", reason: "unsupported-version"},
		{name: "extra spaces", head: "CONNECT  example.com:443 HTTP/1.1\r\n\r\n", reason: "malformed-request-line"},
		{name: "head too large", head: "CONNECT example.com:443 HTTP/1.1\r\nX: " + strings.Repeat("a", maxHead), reason: "head-too-large"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			br := bufio.NewReader(io.LimitReader(strings.NewReader(tc.head), maxHead))
			host, port, rerr := readConnect(br)
			reason := ""
			if rerr != nil {
				reason = rerr.reason
			}
			if reason != tc.reason || host != tc.wantHost || port != tc.wantPort {
				t.Fatalf("readConnect() = %q, %d, %q, want %q, %d, %q", host, port, reason, tc.wantHost, tc.wantPort, tc.reason)
			}
		})
	}
}

// logRecords collects the records of a logger.
type logRecords struct {
	mu      sync.Mutex
	records []string
}

func (l *logRecords) Enabled(context.Context, slog.Level) bool { return true }
func (l *logRecords) WithAttrs([]slog.Attr) slog.Handler       { return l }
func (l *logRecords) WithGroup(string) slog.Handler            { return l }
func (l *logRecords) Handle(_ context.Context, r slog.Record) error {
	var sb strings.Builder
	sb.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		if a.Key != "sandbox" && a.Key != "error" && a.Key != "address" {
			fmt.Fprintf(&sb, " %s=%s", a.Key, a.Value)
		}
		return true
	})
	l.mu.Lock()
	l.records = append(l.records, sb.String())
	l.mu.Unlock()
	return nil
}

func (l *logRecords) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.records...)
}

// echoServer listens on loopback and echoes what each connection sends.
func echoServer(t *testing.T) netip.AddrPort {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := ln.Close(); err != nil {
			t.Errorf("closing the echo server error = %v", err)
		}
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				if _, err := io.Copy(c, c); err != nil {
					t.Logf("echo: %v", err)
				}
				if err := c.Close(); err != nil {
					t.Logf("closing an echo connection: %v", err)
				}
			}()
		}
	}()
	return netip.MustParseAddrPort(ln.Addr().String())
}

func listen(t *testing.T, cfg BoundaryConfig) (*Boundary, string) {
	t.Helper()
	dir := testutil.ShortTempDir(t)
	sock := filepath.Join(dir, SocketName)
	b, err := Listen(sock, cfg)
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	t.Cleanup(func() {
		if err := b.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	return b, sock
}

// connect sends a CONNECT for target on the socket and returns the
// connection and the response.
func connect(t *testing.T, sock, target string) (net.Conn, *bufio.Reader, *http.Response) {
	t.Helper()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dialing the boundary error = %v", err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("closing the connection to the boundary error = %v", err)
		}
	})
	if _, err := fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("reading the response error = %v", err)
	}
	return c, br, resp
}

func TestBoundaryTunnels(t *testing.T) {
	t.Parallel()
	echo := echoServer(t)
	logs := &logRecords{}
	blocks := &Blocks{}
	_, sock := listen(t, BoundaryConfig{
		Sandbox:  "test",
		Policy:   func() egress.Policy { return mustPolicy(t, "", "127.0.0.1:"+strconv.Itoa(int(echo.Port()))) },
		Resolver: fakeResolver{"echo.test": addrs("127.0.0.2")},
		Log:      slog.New(logs),
		Blocks:   blocks,
	})

	c, br, resp := connect(t, sock, echo.String())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT %s status = %d, want 200", echo, resp.StatusCode)
	}
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	if err := c.(*net.UnixConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(br)
	if err != nil {
		t.Fatalf("reading the tunnel error = %v", err)
	}
	if string(got) != "ping" {
		t.Fatalf("tunnel echoed %q, want %q", got, "ping")
	}

	_, _, resp = connect(t, sock, "echo.test:"+strconv.Itoa(int(echo.Port())))
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("CONNECT echo.test status = %d, want 403", resp.StatusCode)
	}
	if got, want := resp.Header.Get("Proxy-Status"), "a2a-serve; error=destination_ip_prohibited; reason=resolved-address-denied"; got != want {
		t.Fatalf("Proxy-Status = %q, want %q", got, want)
	}
	_, _, resp = connect(t, sock, "192.168.1.1:22")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("CONNECT 192.168.1.1:22 status = %d, want 403", resp.StatusCode)
	}

	port := strconv.Itoa(int(echo.Port()))
	want := []string{
		"egress allowed destination=127.0.0.1:" + port + " rule=127.0.0.1:" + port,
		"egress blocked destination=echo.test:" + port + " reason=resolved-address-denied addresses=[127.0.0.2]",
		"egress blocked destination=192.168.1.1:22 reason=ip-not-on-allowlist",
	}
	if diff := cmp.Diff(want, logs.all()); diff != "" {
		t.Fatalf("log records wrong result (-want +got) diff = %s", diff)
	}
	n, recent := blocks.Snapshot()
	wantRecent := []Block{{Destination: "192.168.1.1:22", Allow: "192.168.1.1:22"}, {Destination: "echo.test:" + port, Allow: "host.docker.internal:" + port}}
	if diff := cmp.Diff(wantRecent, recent); n != 2 || diff != "" {
		t.Fatalf("Blocks.Snapshot() = %d, %v, want 2 (-want +got) diff = %s", n, recent, diff)
	}
}

func TestBoundaryAppliesChangedPolicy(t *testing.T) {
	t.Parallel()
	echo := echoServer(t)
	var mu sync.Mutex
	policy := mustPolicy(t, "")
	_, sock := listen(t, BoundaryConfig{
		Policy: func() egress.Policy {
			mu.Lock()
			defer mu.Unlock()
			return policy
		},
		Log: slog.New(slog.DiscardHandler),
	})
	if _, _, resp := connect(t, sock, echo.String()); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("CONNECT %s status = %d, want 403", echo, resp.StatusCode)
	}
	mu.Lock()
	policy = mustPolicy(t, "", "127.0.0.1")
	mu.Unlock()
	if _, _, resp := connect(t, sock, echo.String()); resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT %s after allowing it status = %d, want 200", echo, resp.StatusCode)
	}
}

func TestBoundaryLimitsTunnels(t *testing.T) {
	t.Parallel()
	echo := echoServer(t)
	_, sock := listen(t, BoundaryConfig{
		Policy:     func() egress.Policy { return mustPolicy(t, "", "127.0.0.1") },
		Log:        slog.New(slog.DiscardHandler),
		MaxTunnels: 1,
	})
	if _, _, resp := connect(t, sock, echo.String()); resp.StatusCode != http.StatusOK {
		t.Fatalf("first CONNECT status = %d, want 200", resp.StatusCode)
	}
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dialing the boundary error = %v", err)
	}
	defer func() {
		if err := c.Close(); err != nil {
			t.Errorf("closing the connection error = %v", err)
		}
	}()
	resp, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("reading the response to a connection over the limit error = %v", err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(resp.Header.Get("Proxy-Status"), "reason=busy") {
		t.Fatalf("connection over the limit = %d %q, want 503 busy", resp.StatusCode, resp.Header.Get("Proxy-Status"))
	}
}

func TestBoundaryCloseEndsTunnels(t *testing.T) {
	t.Parallel()
	echo := echoServer(t)
	b, sock := listen(t, BoundaryConfig{
		Policy: func() egress.Policy { return mustPolicy(t, "", "127.0.0.1") },
		Log:    slog.New(slog.DiscardHandler),
	})
	c, br, resp := connect(t, sock, echo.String())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d, want 200", resp.StatusCode)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := c.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := br.ReadByte(); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("reading a tunnel after Close() error = %v, want the tunnel closed", err)
	}
	if _, err := net.Dial("unix", sock); err == nil {
		t.Fatalf("dialing the boundary after Close() error = nil, want an error")
	}
}

func TestBoundaryIdleTimeout(t *testing.T) {
	t.Parallel()
	echo := echoServer(t)
	_, sock := listen(t, BoundaryConfig{
		Policy:      func() egress.Policy { return mustPolicy(t, "", "127.0.0.1") },
		Log:         slog.New(slog.DiscardHandler),
		IdleTimeout: 200 * time.Millisecond,
	})
	c, br, resp := connect(t, sock, echo.String())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d, want 200", resp.StatusCode)
	}
	if err := c.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, br); err != nil {
		t.Fatalf("reading an idle tunnel error = %v, want it closed", err)
	}
}

func TestDecisionLogLimitsLines(t *testing.T) {
	t.Parallel()
	logs := &logRecords{}
	d := &decisionLog{log: slog.New(logs), sandbox: "test"}
	for range maxLogLines + 5 {
		d.write(slog.LevelWarn, "egress blocked", "reason", "busy")
	}
	d.flush()
	got := logs.all()
	if len(got) != maxLogLines+1 {
		t.Fatalf("decisionLog wrote %d records, want %d", len(got), maxLogLines+1)
	}
	if want := "egress decisions over the log limit count=5 limit_per_minute=600"; got[len(got)-1] != want {
		t.Fatalf("decisionLog last record = %q, want %q", got[len(got)-1], want)
	}
}

func TestBoundaryCloseEndsTunnelsToSilentUpstreams(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		accepted <- c
	}()
	t.Cleanup(func() {
		if err := ln.Close(); err != nil {
			t.Errorf("closing the upstream listener error = %v", err)
		}
	})
	b, sock := listen(t, BoundaryConfig{
		Policy: func() egress.Policy { return mustPolicy(t, "", "127.0.0.1") },
		Log:    slog.New(slog.DiscardHandler),
	})
	c, _, resp := connect(t, sock, ln.Addr().String())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d, want 200", resp.StatusCode)
	}
	upstream := <-accepted
	defer func() {
		if err := upstream.Close(); err != nil {
			t.Errorf("closing the upstream error = %v", err)
		}
	}()
	if err := c.(*net.UnixConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	closed := make(chan error, 1)
	go func() { closed <- b.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Close() didn't return with a half-closed guest and a silent upstream")
	}
	if err := upstream.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(upstream); err != nil {
		t.Fatalf("reading the upstream after Close() error = %v, want EOF", err)
	}
}

func TestAllowRuleParses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		suggested, target, want string
	}{
		{target: "192.168.1.1:22", want: "192.168.1.1:22"},
		{target: "100.100.100.100:80"},
		{suggested: "100.101.102.103:443", target: "nas.example.ts.net:443", want: "nas.example.ts.net:443"},
		{suggested: "169.254.169.254:80", target: "meta.corp:80", want: "169.254.169.254:80"},
		{suggested: "host.docker.internal:3000", target: "me.test:3000", want: "host.docker.internal:3000"},
		{target: "[fd7a:115c:a1e0::1]:22", want: "[fd7a:115c:a1e0::1]:22"},
	}
	for _, tc := range tests {
		got := allowRule(tc.suggested, tc.target)
		if got != tc.want {
			t.Errorf("allowRule(%q, %q) = %q, want %q", tc.suggested, tc.target, got, tc.want)
		}
		if got == "" {
			continue
		}
		if _, err := egress.ParseRule(got); err != nil {
			t.Errorf("allowRule(%q, %q) = %q, which ParseRule refuses: %v", tc.suggested, tc.target, got, err)
		}
	}
}

func TestBlocksSuggestOnlyRulesThatParse(t *testing.T) {
	t.Parallel()
	blocks := &Blocks{}
	_, sock := listen(t, BoundaryConfig{
		Policy:   func() egress.Policy { return mustPolicy(t, "") },
		Resolver: fakeResolver{"nas.example.ts.net": addrs("100.101.102.103"), "nas.lan": addrs("192.168.1.20"), "meta.corp": addrs("169.254.169.254")},
		Log:      slog.New(slog.DiscardHandler),
		Blocks:   blocks,
	})
	for _, target := range []string{"100.100.100.100:80", "nas.example.ts.net:443", "nas.lan:22", "meta.corp:80", "10.1.2.3:5432", "[fd7a:115c:a1e0::1]:22"} {
		if _, _, resp := connect(t, sock, target); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("CONNECT %s status = %d, want 403", target, resp.StatusCode)
		}
	}
	_, recent := blocks.Snapshot()
	for _, b := range recent {
		if b.Allow == "" {
			continue
		}
		if _, err := egress.ParseRule(b.Allow); err != nil {
			t.Errorf("the rule suggested for %s is %q, which ParseRule refuses: %v", b.Destination, b.Allow, err)
		}
	}
}

// panickingNetwork panics on every route lookup.
type panickingNetwork struct{ fakeNetwork }

func (panickingNetwork) DefaultRoute(netip.Addr) (bool, error) { panic("a bug in the route lookup") }

func TestBoundarySurvivesAPanic(t *testing.T) {
	t.Parallel()
	echo := echoServer(t)
	logs := &logRecords{}
	_, sock := listen(t, BoundaryConfig{
		Policy:  func() egress.Policy { return mustPolicy(t, "", "127.0.0.1") },
		Network: panickingNetwork{},
		Log:     slog.New(logs),
	})
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := c.Close(); err != nil {
			t.Errorf("closing the connection error = %v", err)
		}
	}()
	if _, err := fmt.Fprint(c, "CONNECT 93.184.216.34:80 HTTP/1.1\r\nHost: 93.184.216.34:80\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	if err := c.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := http.ReadResponse(bufio.NewReader(c), nil); err == nil {
		t.Fatalf("CONNECT whose decision panics got a response, want the connection closed")
	}
	if _, _, resp := connect(t, sock, echo.String()); resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT after a panic status = %d, want 200", resp.StatusCode)
	}
	if !slices.ContainsFunc(logs.all(), func(r string) bool { return strings.HasPrefix(r, "egress connection failed; refusing it") }) {
		t.Fatalf("log records = %q, want the panic logged", logs.all())
	}
}
