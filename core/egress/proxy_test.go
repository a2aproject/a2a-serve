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

package egress_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/core/egress"
	"github.com/a2aproject/a2a-serve/internal/testutil"
)

// fakeProxy is an egress proxy that refuses refuse.test, tunnels
// tunnel.test to an echo server, and intercepts other hosts. It answers
// intercepted requests with what it saw, and streams /sse in two events,
// the second once next receives.
type fakeProxy struct {
	t    *testing.T
	echo string
	// afterEOF answers each connection once it has read all of it.
	afterEOF string
	next     chan struct{}

	mu   sync.Mutex
	seen []string
}

func newFakeProxy(t *testing.T) *fakeProxy {
	t.Helper()
	return &fakeProxy{t: t, echo: echoServer(t), afterEOF: afterEOFServer(t), next: make(chan struct{})}
}

func (p *fakeProxy) record(s string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seen = append(p.seen, s)
}

func (p *fakeProxy) requests() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.seen...)
}

func (p *fakeProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		p.record(r.Method + " " + r.RequestURI + " context=" + r.Header.Get(egress.ProxyHeaderContext) + " task=" + r.Header.Get(egress.ProxyHeaderTask) + " host=" + r.Host + spoofed(r.Header))
		if r.URL.Path == "/sse" {
			p.stream(w)
			return
		}
		if _, err := io.WriteString(w, "answered "+r.RequestURI); err != nil {
			p.t.Errorf("writing the answer error = %v", err)
		}
		return
	}
	p.record("CONNECT " + r.Host + " context=" + r.Header.Get(egress.ProxyHeaderContext) + " intercept=" + r.Header.Get(egress.ProxyHeaderIntercept))
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		http.Error(w, "bad target", http.StatusBadRequest)
		return
	}
	switch host {
	case "refuse.test":
		w.Header().Set("Connection", "close")
		http.Error(w, "no\x00 calls\r\n  to refuse.test today", http.StatusForbidden)
	case "tunnel.test", "eof.test":
		target := p.echo
		if host == "eof.test" {
			target = p.afterEOF
		}
		upstream, err := net.Dial("tcp", target)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		c, brw := hijack(p.t, w)
		if _, err := io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
			p.t.Errorf("answering CONNECT error = %v", err)
		}
		go func() {
			if _, err := io.Copy(upstream, brw); err != nil {
				p.t.Logf("tunnel: %v", err)
			}
			if err := upstream.(*net.TCPConn).CloseWrite(); err != nil {
				p.t.Logf("tunnel: %v", err)
			}
		}()
		if _, err := io.Copy(c, upstream); err != nil {
			p.t.Logf("tunnel: %v", err)
		}
		closeLog(p.t, upstream)
		closeLog(p.t, c)
	default:
		c, _ := hijack(p.t, w)
		if _, err := io.WriteString(c, "HTTP/1.1 204 No Content\r\n"+egress.ProxyHeaderIntercept+": accept\r\n\r\n"); err != nil {
			p.t.Errorf("answering CONNECT error = %v", err)
		}
		closeLog(p.t, c)
	}
}

// spoofed returns the names of the headers in h that a proxy can read as
// A2A-Run-* headers, besides the ones a2a-run sets.
func spoofed(h http.Header) string {
	var out []string
	for name := range h {
		norm := strings.ToLower(strings.ReplaceAll(name, "_", "-"))
		if strings.HasPrefix(norm, "a2a-run-") && name != "A2a-Run-Context" && name != "A2a-Run-Task" {
			out = append(out, name)
		}
	}
	if len(out) == 0 {
		return ""
	}
	slices.Sort(out)
	return " spoofed=" + strings.Join(out, ",")
}

func (p *fakeProxy) stream(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	for i, ev := range []string{"data: one\n\n", "data: two\n\n"} {
		if i > 0 {
			select {
			case <-p.next:
			case <-time.After(10 * time.Second):
				p.t.Errorf("the client didn't read the first event")
				return
			}
		}
		if _, err := io.WriteString(w, ev); err != nil {
			p.t.Errorf("writing an event error = %v", err)
			return
		}
		http.NewResponseController(w).Flush() //nolint:errcheck // a failed flush shows as a missing event
	}
}

// afterEOFServer answers each connection with what it read, once the
// connection's input ends, and returns its address.
func afterEOFServer(t *testing.T) string {
	t.Helper()
	ln := listenTCP(t)
	t.Cleanup(func() { closeLog(t, ln) })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer closeLog(t, c)
				got, err := io.ReadAll(c)
				if err != nil {
					return
				}
				if _, err := io.WriteString(c, "after EOF: "+string(got)); err != nil {
					t.Logf("answering after EOF: %v", err)
				}
			}()
		}
	}()
	return ln.Addr().String()
}

func hijack(t *testing.T, w http.ResponseWriter) (net.Conn, *bufio.ReadWriter) {
	t.Helper()
	c, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		t.Fatalf("Hijack() error = %v", err)
	}
	return c, brw
}

func closeLog(t *testing.T, c io.Closer) {
	if err := c.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Logf("closing: %v", err)
	}
}

// serveProxy serves h on ln and returns the proxy URL.
func serveProxy(t *testing.T, ln net.Listener, h http.Handler) string {
	t.Helper()
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("serving the proxy error = %v", err)
		}
	}()
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Errorf("closing the proxy error = %v", err)
		}
	})
	if ln.Addr().Network() == "unix" {
		return "unix://" + ln.Addr().String()
	}
	return "http://" + ln.Addr().String()
}

func listenTCP(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

// proxyEndpoint serves an endpoint, of key, whose router is the proxy at
// url, and returns its address and the CA its clients trust.
func proxyEndpoint(t *testing.T, cfg egress.ProxyConfig, timeout time.Duration, key string) (string, []byte) {
	t.Helper()
	r, err := egress.NewProxyRouterWithTimeout(cfg, timeout)
	if err != nil {
		t.Fatalf("egress.NewProxyRouter() error = %v", err)
	}
	t.Cleanup(r.CloseIdleConnections)
	ep, addr := openEndpoint(t, newRouterBoundary(t, r), key)
	return addr, ep.CA()
}

func TestProxyRouterRoutesByTheProxyAnswer(t *testing.T) {
	t.Parallel()
	p := newFakeProxy(t)
	sock := filepath.Join(testutil.ShortTempDir(t), "proxy.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	for name, url := range map[string]string{"tcp": serveProxy(t, listenTCP(t), p), "unix": serveProxy(t, ln, p)} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			addr, _ := proxyEndpoint(t, egress.ProxyConfig{URL: url}, 5*time.Second, "ctx-"+name)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			status, proxyStatus, c, err := tunnel(ctx, addr, "refuse.test:443")
			if err != nil {
				t.Fatalf("CONNECT refuse.test error = %v", err)
			}
			closeLog(t, c)
			if want := `a2a-serve; error=http_request_denied; reason="no calls to refuse.test today"`; status != http.StatusForbidden || proxyStatus != want {
				t.Fatalf("CONNECT refuse.test = %d %q, want 403 %q", status, proxyStatus, want)
			}

			status, _, c, err = tunnel(ctx, addr, "tunnel.test:22")
			if err != nil || status != http.StatusOK {
				t.Fatalf("CONNECT tunnel.test = %d, %v, want 200", status, err)
			}
			defer closeLog(t, c)
			if _, err := io.WriteString(c, "ping"); err != nil {
				t.Fatal(err)
			}
			got := make([]byte, 4)
			if _, err := io.ReadFull(c, got); err != nil || string(got) != "ping" {
				t.Fatalf("tunnel through the proxy read %q, %v, want ping", got, err)
			}
		})
	}
	t.Cleanup(func() {
		want := map[string]bool{
			"CONNECT refuse.test:443 context=ctx-tcp intercept=offered":  true,
			"CONNECT tunnel.test:22 context=ctx-tcp intercept=offered":   true,
			"CONNECT refuse.test:443 context=ctx-unix intercept=offered": true,
			"CONNECT tunnel.test:22 context=ctx-unix intercept=offered":  true,
		}
		got := map[string]bool{}
		for _, s := range p.requests() {
			got[s] = true
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("proxy requests wrong result (-want +got) diff = %s", diff)
		}
	})
}

func TestProxyRouterForwardsInterceptedRequests(t *testing.T) {
	t.Parallel()
	p := newFakeProxy(t)
	url := serveProxy(t, listenTCP(t), p)
	addr, ca := proxyEndpoint(t, egress.ProxyConfig{URL: url, Task: func(key string) string { return "task-of-" + key }}, 5*time.Second, "ctx-1")
	c := client(t, addr, ca)

	status, body, err := get(t, c, "https://api.example.test/v1/models?page=2", http.Header{"A2a-Run-Context": {"forged"}, "A2A_Run_Context": {"victim"}, "a2a_run-task": {"victim"}, "A2a-Run-Other": {"x"}})
	if err != nil || status != http.StatusOK {
		t.Fatalf("GET https://api.example.test = %d, %v, want 200", status, err)
	}
	if want := "answered https://api.example.test/v1/models?page=2"; body != want {
		t.Fatalf("GET https://api.example.test body = %q, want %q", body, want)
	}
	status, body, err = get(t, c, "http://plain.test/index.html", nil)
	if err != nil || status != http.StatusOK || body != "answered http://plain.test/index.html" {
		t.Fatalf("GET http://plain.test = %d %q, %v, want 200 and the proxy's answer", status, body, err)
	}

	want := []string{
		"CONNECT api.example.test:443 context=ctx-1 intercept=offered",
		"GET https://api.example.test/v1/models?page=2 context=ctx-1 task=task-of-ctx-1 host=api.example.test",
		"GET http://plain.test/index.html context=ctx-1 task=task-of-ctx-1 host=plain.test",
	}
	if diff := cmp.Diff(want, p.requests()); diff != "" {
		t.Fatalf("proxy requests wrong result (-want +got) diff = %s", diff)
	}
}

func TestProxyRouterTunnelsPassHalfCloses(t *testing.T) {
	t.Parallel()
	p := newFakeProxy(t)
	addr, _ := proxyEndpoint(t, egress.ProxyConfig{URL: serveProxy(t, listenTCP(t), p)}, 5*time.Second, "ctx-1")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	status, _, c, err := tunnel(ctx, addr, "eof.test:7")
	if err != nil || status != http.StatusOK {
		t.Fatalf("CONNECT eof.test = %d, %v, want 200", status, err)
	}
	defer closeLog(t, c)
	if _, err := io.WriteString(c, "ping"); err != nil {
		t.Fatal(err)
	}
	if err := c.(*bufferedConn).Conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if err := c.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(c)
	if err != nil || string(got) != "after EOF: ping" {
		t.Fatalf("answer after the client's half-close = %q, %v, want %q", got, err, "after EOF: ping")
	}
}

func TestProxyRouterStreamsAnswers(t *testing.T) {
	t.Parallel()
	p := newFakeProxy(t)
	addr, ca := proxyEndpoint(t, egress.ProxyConfig{URL: serveProxy(t, listenTCP(t), p)}, 5*time.Second, "ctx-1")
	c := client(t, addr, ca)

	resp, err := c.Post("https://api.anthropic.com/sse", "application/json", strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatalf("POST https://api.anthropic.com/sse error = %v", err)
	}
	defer closeLog(t, resp.Body)
	br := bufio.NewReader(resp.Body)
	first, err := br.ReadString('\n')
	if err != nil || first != "data: one\n" {
		t.Fatalf("first event = %q, %v, want data: one", first, err)
	}
	close(p.next)
	rest, err := io.ReadAll(br)
	if err != nil || string(rest) != "\ndata: two\n\n" {
		t.Fatalf("rest of the stream = %q, %v, want the second event", rest, err)
	}
}

// rawProxy accepts connections and hands each to answer.
func rawProxy(t *testing.T, answer func(c net.Conn)) string {
	t.Helper()
	ln := listenTCP(t)
	var wg sync.WaitGroup
	t.Cleanup(func() {
		closeLog(t, ln)
		wg.Wait()
	})
	wg.Go(func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Go(func() {
				defer closeLog(t, c)
				if _, err := http.ReadRequest(bufio.NewReader(c)); err != nil {
					return
				}
				answer(c)
			})
		}
	})
	return "http://" + ln.Addr().String()
}

func TestProxyRouterSurvivesMisbehavingProxies(t *testing.T) {
	t.Parallel()
	const timeout = 500 * time.Millisecond
	write := func(s string) func(net.Conn) {
		return func(c net.Conn) {
			if _, err := io.WriteString(c, s); err != nil {
				t.Logf("writing: %v", err)
			}
		}
	}
	tests := []struct {
		name       string
		answer     func(net.Conn)
		wantStatus int
		wantProxy  string
	}{
		{name: "slow head", answer: func(net.Conn) { time.Sleep(3 * timeout) }, wantStatus: http.StatusBadGateway, wantProxy: "a2a-serve; error=destination_unavailable; reason=router-failed"},
		{name: "huge head", answer: write("HTTP/1.1 200 OK\r\nX-Big: " + strings.Repeat("a", 64<<10) + "\r\n\r\n"), wantStatus: http.StatusBadGateway, wantProxy: "a2a-serve; error=destination_unavailable; reason=router-failed"},
		{name: "garbage", answer: write("SSH-2.0-OpenSSH\r\n\r\n"), wantStatus: http.StatusBadGateway, wantProxy: "a2a-serve; error=destination_unavailable; reason=router-failed"},
		{name: "closed", answer: func(net.Conn) {}, wantStatus: http.StatusBadGateway, wantProxy: "a2a-serve; error=destination_unavailable; reason=router-failed"},
		{name: "redirect", answer: write("HTTP/1.1 302 Found\r\nLocation: /\r\n\r\n"), wantStatus: http.StatusBadGateway, wantProxy: "a2a-serve; error=destination_unavailable; reason=router-failed"},
		{name: "huge refusal", answer: write("HTTP/1.1 451 Unavailable\r\nConnection: close\r\n\r\n" + strings.Repeat("x", 1<<20)), wantStatus: http.StatusUnavailableForLegalReasons, wantProxy: `a2a-serve; error=http_request_error; reason=` + strings.Repeat("x", 255)},
		{name: "slow refusal body", answer: func(c net.Conn) {
			write("HTTP/1.1 403 Forbidden\r\nContent-Length: 100\r\n\r\nnot")(c)
			time.Sleep(3 * timeout)
		}, wantStatus: http.StatusForbidden, wantProxy: `a2a-serve; error=http_request_denied; reason=not`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			addr, _ := proxyEndpoint(t, egress.ProxyConfig{URL: rawProxy(t, tc.answer)}, timeout, "ctx-1")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			start := time.Now()

			status, proxyStatus, c, err := tunnel(ctx, addr, "example.test:443")
			if err != nil {
				t.Fatalf("CONNECT error = %v", err)
			}
			closeLog(t, c)

			if status != tc.wantStatus || proxyStatus != tc.wantProxy {
				t.Fatalf("CONNECT = %d %q, want %d %q", status, proxyStatus, tc.wantStatus, tc.wantProxy)
			}
			if elapsed := time.Since(start); elapsed > 2*timeout+time.Second {
				t.Fatalf("CONNECT took %v, want at most about the decision timeout %v", elapsed, timeout)
			}
		})
	}
}

func TestProxyRouterRefusesWhenTheProxyIsDown(t *testing.T) {
	t.Parallel()
	ln := listenTCP(t)
	url := "http://" + ln.Addr().String()
	closeLog(t, ln)
	addr, _ := proxyEndpoint(t, egress.ProxyConfig{URL: url}, time.Second, "ctx-1")

	if got := connect(t, addr, "example.test:443"); got != http.StatusBadGateway {
		t.Fatalf("CONNECT with the proxy down = %d, want %d", got, http.StatusBadGateway)
	}
}

func TestNewProxyRouterChecksTheURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		url     string
		wantErr string
	}{
		{url: "http://127.0.0.1:9000"},
		{url: "http://localhost:9000/"},
		{url: "unix:///run/host/egress.sock"},
		{url: "unix://run/egress.sock", wantErr: "the socket path isn't absolute"},
		{url: "https://127.0.0.1:9000", wantErr: "the URL isn't http or unix"},
		{url: "127.0.0.1:9000", wantErr: "the URL isn't http or unix"},
		{url: "http://user:secret@127.0.0.1:9000", wantErr: "the URL can't carry credentials"},
		{url: "http://127.0.0.1:9000/proxy", wantErr: "the URL has a path"},
		{url: "http://127.0.0.1", wantErr: "the URL has no port"},
	}
	for _, tc := range tests {
		t.Run(tc.url, func(t *testing.T) {
			t.Parallel()
			_, err := egress.NewProxyRouter(egress.ProxyConfig{URL: tc.url})
			if tc.wantErr == "" && err != nil || tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("egress.NewProxyRouter(%q) error = %v, want %q", tc.url, err, tc.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), "secret") {
				t.Fatalf("egress.NewProxyRouter(%q) error = %v, want it without the credentials", tc.url, fmt.Sprint(err))
			}
		})
	}
}
