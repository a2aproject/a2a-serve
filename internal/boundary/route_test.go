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

package boundary

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/egressrules"
)

// fixedRouter routes every connection with r.
func fixedRouter(r Route) Router {
	return func(context.Context, string, uint16) (Route, error) { return r, nil }
}

func TestRoutesKeepTheBounds(t *testing.T) {
	t.Parallel()
	echo := echoServer(t)
	dial := Dial(func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", echo.String())
	})
	serve := Serve(func(c net.Conn) {
		if _, err := io.Copy(c, c); err != nil {
			t.Logf("serving: %v", err)
		}
	})
	tests := []struct {
		name  string
		route Route
	}{
		{name: "dial", route: dial},
		{name: "serve", route: serve},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, sock := listen(t, BoundaryConfig{Router: fixedRouter(tc.route), Log: slog.New(slog.DiscardHandler), MaxTunnels: 1, IdleTimeout: 200 * time.Millisecond})
			c, br, resp := connect(t, sock, "any.test:22")
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("CONNECT status = %d, want 200", resp.StatusCode)
			}
			over, err := net.Dial("unix", sock)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := over.Close(); err != nil {
					t.Errorf("closing the connection error = %v", err)
				}
			}()
			busy, err := http.ReadResponse(bufio.NewReader(over), &http.Request{Method: http.MethodConnect})
			if err != nil || busy.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("connection over the tunnel limit = %v, %v, want 503", busy, err)
			}
			if err := c.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			if _, err := br.ReadByte(); err == nil || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("reading an idle tunnel error = %v, want it closed by the idle timeout", err)
			}
		})
	}
}

func TestDirectRoutesKeepTheAddressChecks(t *testing.T) {
	t.Parallel()
	resolver := fakeResolver{"public.test": addrs("93.184.216.34"), "lan.test": addrs("192.168.1.20"), "loop.test": addrs("127.0.0.1")}
	tests := []struct {
		name   string
		route  Route
		host   string
		want   []netip.AddrPort
		reason string
	}{
		{name: "public name", route: Direct(), host: "public.test", want: []netip.AddrPort{netip.MustParseAddrPort("93.184.216.34:443")}},
		{name: "LAN name", route: Direct(), host: "lan.test", reason: "resolved-address-denied"},
		{name: "loopback literal", route: Direct(), host: "127.0.0.1", reason: "ip-not-on-allowlist"},
		{name: "redirect to loopback", route: Redirect("loop.test", 80), host: "public.test", reason: "resolved-address-denied"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := &Boundary{cfg: BoundaryConfig{Resolver: resolver, Network: fakeNetwork{}, Log: slog.New(slog.DiscardHandler)}}
			host, port := tc.host, uint16(443)
			if tc.route.kind == routeRedirect {
				host, port = tc.route.host, tc.route.port
			}
			g, rerr := b.authorize(context.Background(), tc.route.checks(), host, port)
			reason := ""
			if rerr != nil {
				reason = rerr.reason
			}
			if reason != tc.reason {
				t.Fatalf("authorize(%s) refusal = %q, want %q", host, reason, tc.reason)
			}
			if diff := cmp.Diff(tc.want, g.addrs, cmp.Comparer(func(a, b netip.AddrPort) bool { return a == b })); diff != "" {
				t.Fatalf("authorize(%s) wrong result (-want +got) diff = %s", host, diff)
			}
		})
	}
}

func TestHTTPRouteFailsClosedForHostsTheCADoesNotCover(t *testing.T) {
	t.Parallel()
	ca, err := NewCA([]string{injectHost})
	if err != nil {
		t.Fatal(err)
	}
	answered := HTTP(func(http.RoundTripper) http.RoundTripper {
		return roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody, Request: r}, nil
		})
	})
	_, sock := listen(t, BoundaryConfig{Router: fixedRouter(answered), CA: ca, Log: slog.New(slog.DiscardHandler)})
	tests := []struct {
		host    string
		wantErr bool
	}{
		{host: injectHost},
		{host: "other.test", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.host, func(t *testing.T) {
			t.Parallel()
			c, err := dialTunnel(context.Background(), sock, net.JoinHostPort(tc.host, "443"))
			if err != nil {
				t.Fatalf("CONNECT %s error = %v", tc.host, err)
			}
			tlsConn := tls.Client(c, &tls.Config{ServerName: tc.host, InsecureSkipVerify: true})
			defer func() {
				if err := tlsConn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
					t.Logf("closing the connection: %v", err)
				}
			}()
			if err := tlsConn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			if err := tlsConn.Handshake(); (err != nil) != tc.wantErr {
				t.Fatalf("TLS handshake with %s error = %v, want error %v", tc.host, err, tc.wantErr)
			}
		})
	}
}

func TestCredentialRoutesRefusePlainHTTP(t *testing.T) {
	t.Parallel()
	port, roots := injectUpstream(t)
	inj := Injection{Host: injectHost, Port: port, Header: "Authorization", Value: "Bearer real"}
	ca, err := NewCA([]string{injectHost})
	if err != nil {
		t.Fatal(err)
	}
	_, sock := listen(t, BoundaryConfig{
		Router:        PolicyRouter(func() egressrules.Policy { return mustPolicy(t, "", injectHost, "127.0.0.1") }, []Injection{inj}),
		Resolver:      fakeResolver{injectHost: addrs("127.0.0.1")},
		Log:           slog.New(slog.DiscardHandler),
		CA:            ca,
		UpstreamRoots: roots,
	})
	c, err := dialTunnel(context.Background(), sock, net.JoinHostPort(injectHost, strconv.Itoa(int(port))))
	if err != nil {
		t.Fatalf("CONNECT error = %v", err)
	}
	defer func() {
		if err := c.Close(); err != nil {
			t.Errorf("closing the connection error = %v", err)
		}
	}()
	if _, err := io.WriteString(c, "GET / HTTP/1.1\r\nHost: "+injectHost+"\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	if err := c.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if resp, err := http.ReadResponse(bufio.NewReader(c), nil); err == nil {
		t.Fatalf("a plain HTTP request to a host that gets a credential got %d, want the connection closed", resp.StatusCode)
	}
}
