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
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-serve/internal/egressrules"
)

// Limits of the requests of intercepted connections, per boundary.
const (
	// maxHTTPRequests bounds the open requests.
	maxHTTPRequests = 16
	// maxHTTPBody bounds a request body.
	maxHTTPBody = 64 << 20
	// httpHeaderTimeout bounds the wait for the host to answer, which a
	// model can take long to start.
	httpHeaderTimeout = 10 * time.Minute
	// httpTimeout bounds a whole request, a streamed answer included.
	httpTimeout = 30 * time.Minute
	// httpIdleTimeout closes a connection with no request for so long.
	httpIdleTimeout = 5 * time.Minute
	// writeStallTimeout closes a connection to which the boundary can't
	// write for so long, such as one whose container stopped reading.
	writeStallTimeout = time.Minute
)

// tlsHandshakeRecord is the first byte of a TLS connection.
const tlsHandshakeRecord = 0x16

// intercept serves the tunnel of an allowed request whose route is HTTP:
// it terminates the container's TLS, or reads its plain HTTP, and sends
// each request through the route's middleware, whose next transport
// reaches the checked addresses of g.
func (b *Boundary) intercept(guest net.Conn, br *bufio.Reader, g grant, route Route, host string, port uint16) {
	var cert *tls.Certificate
	if b.cfg.CA != nil && b.cfg.CA.Covers(host) {
		var err error
		if cert, err = b.cfg.CA.leaf(host); err != nil {
			b.log.write(slog.LevelError, "egress failed", "destination", g.target, "reason", "certificate-failed", "error", err)
			writeRefusal(guest, refuse(502, "certificate-failed"))
			return
		}
	}
	b.cfg.Blocks.allowed(g.target)
	b.log.write(slog.LevelDebug, "egress allowed", interceptArgs(g, route)...)
	if _, err := io.WriteString(guest, "HTTP/1.1 200 OK\r\n\r\n"); err != nil {
		return
	}
	buffered, err := br.Peek(br.Buffered())
	if err != nil {
		return
	}
	peek := bufio.NewReader(io.MultiReader(bytes.NewReader(bytes.Clone(buffered)), guest))
	if err := guest.SetReadDeadline(time.Now().Add(headTimeout)); err != nil {
		return
	}
	first, err := peek.Peek(1)
	if err != nil {
		return
	}
	if err := guest.SetReadDeadline(time.Time{}); err != nil {
		return
	}
	var conn net.Conn = &prefixConn{Conn: guest, r: peek, stall: writeStallTimeout}
	secure := first[0] == tlsHandshakeRecord
	if !secure && route.tlsOnly {
		// The route adds a secret, which mustn't travel in the clear.
		b.log.write(slog.LevelWarn, "egress failed", "destination", g.target, "reason", "tls-required")
		return
	}
	if secure {
		if cert == nil {
			// The CA can't vouch for this host, so the container's TLS
			// can't be terminated, and the connection mustn't pass
			// uninspected.
			b.log.write(slog.LevelWarn, "egress failed", "destination", g.target, "reason", "certificate-not-covered")
			return
		}
		tc := tls.Server(conn, &tls.Config{
			MinVersion: tls.VersionTLS12,
			NextProtos: []string{"h2", "http/1.1"},
			GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
				// A name other than the requested host would let the container
				// pick which certificate the boundary presents.
				if name, err := egressrules.NormalizeHost(hello.ServerName); hello.ServerName != "" && (err != nil || name != host) {
					return nil, errors.New("the TLS server name differs from the requested host")
				}
				return cert, nil
			},
		})
		ctx, cancel := context.WithTimeout(b.ctx, headTimeout)
		err = tc.HandshakeContext(ctx)
		cancel()
		if err != nil {
			b.log.write(slog.LevelWarn, "egress failed", "destination", g.target, "reason", "tls-failed", "error", err)
			return
		}
		conn = tc
	}
	upstream := b.upstreamTransport(g, route.checks(), host, port, secure)
	defer upstream.CloseIdleConnections()
	var next http.RoundTripper = upstream
	if !route.upgrades {
		next = refuseUpgrades{upstream}
	}
	if route.wrap != nil {
		next = route.wrap(next)
	}
	dest := destination{host: host, port: port, secure: secure, target: g.target, secrets: route.secrets}
	var handlers sync.WaitGroup
	ln := newOneConnListener(conn)
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handlers.Add(1)
			defer handlers.Done()
			// A handler that outlives its request's limit is stuck writing
			// to a stream whose container doesn't take the data, which no
			// context ends; closing the connection does.
			stuck := time.AfterFunc(httpTimeout+writeStallTimeout, func() { closeConn(guest) })
			defer stuck.Stop()
			b.forward(w, r, dest, next)
		}),
		ReadHeaderTimeout: headTimeout,
		IdleTimeout:       httpIdleTimeout,
		MaxHeaderBytes:    maxHead,
		HTTP2:             &http.HTTP2Config{MaxConcurrentStreams: maxHTTPRequests, WriteByteTimeout: writeStallTimeout},
		ErrorLog:          log.New(io.Discard, "", 0),
		BaseContext:       func(net.Listener) context.Context { return b.ctx },
		ConnState: func(_ net.Conn, s http.ConnState) {
			if s == http.StateClosed || s == http.StateHijacked {
				ln.closeOnce()
			}
		},
	}
	if err := srv.Serve(ln); err != nil && !errors.Is(err, net.ErrClosed) {
		b.log.write(slog.LevelWarn, "egress failed", "destination", g.target, "reason", "serve-failed", "error", err)
	}
	handlers.Wait()
}

// interceptArgs returns the log attributes of an intercepted connection.
func interceptArgs(g grant, route Route) []any {
	args := []any{"destination", g.target}
	if g.rule != "" {
		args = append(args, "rule", g.rule)
	}
	if len(route.secrets) > 0 {
		return append(args, "inject", strings.Join(route.secrets, ","))
	}
	return append(args, "route", "http")
}

// destination is the host of an intercepted connection.
type destination struct {
	host   string
	port   uint16
	secure bool
	// target is host:port, for the log, and secrets name the secrets that
	// its requests get.
	target  string
	secrets []string
}

// url returns the URL of the destination with no path.
func (d destination) url() *url.URL {
	u := &url.URL{Scheme: "http", Host: d.target}
	if d.secure {
		u.Scheme = "https"
	}
	if d.secure && d.port == egressrules.HTTPSPort || !d.secure && d.port == 80 {
		u.Host = d.host
		if strings.Contains(d.host, ":") {
			u.Host = "[" + d.host + "]"
		}
	}
	return u
}

// forward sends the request r of the container through next.
func (b *Boundary) forward(w http.ResponseWriter, r *http.Request, dest destination, next http.RoundTripper) {
	if host, port := splitAuthority(r.Host, dest.port); host != dest.host || port != dest.port {
		http.Error(w, "the request names another host than its connection", http.StatusMisdirectedRequest)
		return
	}
	if r.URL.Opaque != "" || !strings.HasPrefix(r.URL.Path, "/") {
		http.Error(w, "the request target must be a path", http.StatusBadRequest)
		return
	}
	select {
	case b.httpSlots <- struct{}{}:
		defer func() { <-b.httpSlots }()
	default:
		w.Header().Set("Proxy-Status", "a2a-serve; error=connection_limit_reached; reason=busy")
		w.Header().Set("Retry-After", "1")
		http.Error(w, "too many open requests from the sandbox", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), httpTimeout)
	defer cancel()
	logArgs := []any{"destination", dest.target}
	if len(dest.secrets) > 0 {
		logArgs = append(logArgs, "inject", strings.Join(dest.secrets, ","))
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			u := dest.url()
			pr.Out.URL.Scheme, pr.Out.URL.Host, pr.Out.Host = u.Scheme, u.Host, ""
		},
		Transport:     next,
		FlushInterval: -1,
		ErrorLog:      log.New(io.Discard, "", 0),
		ModifyResponse: func(resp *http.Response) error {
			b.log.write(slog.LevelDebug, "egress request", append(logArgs, "status", resp.StatusCode)...)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			status := http.StatusBadGateway
			var tooLarge *http.MaxBytesError
			if rerr, ok := errors.AsType[*refusal](err); ok {
				w.Header().Set("Proxy-Status", proxyStatus(rerr))
				w.WriteHeader(rerr.status)
				return
			}
			switch {
			case errors.As(err, &tooLarge):
				status = http.StatusRequestEntityTooLarge
			case errors.Is(err, context.DeadlineExceeded):
				status = http.StatusGatewayTimeout
			case errors.Is(err, context.Canceled):
				return
			}
			b.log.write(slog.LevelWarn, "egress request failed", "destination", dest.target, "status", status, "error", withoutURL(err))
			w.WriteHeader(status)
		},
	}
	// The total time bounds a slow body. A read deadline of the connection,
	// as the server's own routes set, would also end the response when the
	// upstream transport closes the body early.
	if r.ContentLength > maxHTTPBody {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	// An HTTP/1.1 server closes the rest of the request body once the
	// answer starts, which aborts the upstream request that still sends
	// it and cuts its answer off. HTTP/2 is always full duplex.
	if err := http.NewResponseController(w).EnableFullDuplex(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		b.log.write(slog.LevelWarn, "egress request may be cut off", "destination", dest.target, "error", err)
	}
	body := http.MaxBytesReader(w, r.Body, maxHTTPBody)
	// The proxy doesn't close the body, and when the host answered before
	// the end of the body, a Go 1.26 server that drains it after the
	// handler fails the next request of the connection.
	defer func() {
		if err := body.Close(); err != nil {
			slog.Debug("closing an egress request body", "error", err)
		}
	}()
	r.Body = body
	proxy.ServeHTTP(w, r.WithContext(ctx))
}

// refuseUpgrades answers a request to switch protocols, such as to a
// WebSocket, with 501, and sends the others through its transport.
type refuseUpgrades struct{ next http.RoundTripper }

func (t refuseUpgrades) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Header.Get("Upgrade") == "" {
		return t.next.RoundTrip(r)
	}
	if r.Body != nil {
		if err := r.Body.Close(); err != nil {
			slog.Debug("closing an egress request body", "error", err)
		}
	}
	text := "the sandbox can't switch protocols on this connection\n"
	return &http.Response{
		Status:     strconv.Itoa(http.StatusNotImplemented) + " " + http.StatusText(http.StatusNotImplemented),
		StatusCode: http.StatusNotImplemented,
		Proto:      "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: http.Header{
			"Content-Type":   {"text/plain; charset=utf-8"},
			"Proxy-Status":   {"a2a-serve; error=http_protocol_error; reason=upgrade-refused"},
			"Content-Length": {strconv.Itoa(len(text))},
		},
		Body:          io.NopCloser(strings.NewReader(text)),
		ContentLength: int64(len(text)),
		Request:       r,
	}, nil
}

// upstreamTransport returns the transport of the requests of one tunnel to
// host and port, with TLS when secure. It dials only the checked addresses
// of g, or without any, the addresses that rules allow at each dial.
func (b *Boundary) upstreamTransport(g grant, rules egressrules.Policy, host string, port uint16, secure bool) *http.Transport {
	t := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			ctx, cancel := context.WithTimeout(ctx, dialTimeout)
			defer cancel()
			// Parallel requests dial at once, so each checks into its own
			// grant.
			dg := g
			if len(dg.addrs) == 0 {
				checked, rerr := b.authorize(ctx, rules, host, port)
				if rerr != nil {
					b.log.write(slog.LevelWarn, "egress blocked", "destination", g.target, "reason", rerr.reason)
					b.block(g.target, rerr, false)
					return nil, rerr
				}
				checked.target = g.target
				dg = checked
			}
			c, _, err := b.dial(ctx, dg)
			return c, err
		},
		ResponseHeaderTimeout: httpHeaderTimeout,
		IdleConnTimeout:       httpIdleTimeout,
		MaxIdleConnsPerHost:   4,
		DisableCompression:    true,
	}
	if secure {
		t.TLSClientConfig = &tls.Config{ServerName: host, RootCAs: b.cfg.UpstreamRoots, MinVersion: tls.VersionTLS12}
		t.ForceAttemptHTTP2 = true
		t.TLSHandshakeTimeout = dialTimeout
	}
	return t
}

// withoutURL returns err without the URL of a failed request, which may
// carry a key in its query.
func withoutURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// splitAuthority returns the host and port of a Host field, with
// defaultPort when it has none.
func splitAuthority(authority string, defaultPort uint16) (string, uint16) {
	host, port := authority, defaultPort
	if h, p, err := net.SplitHostPort(authority); err == nil {
		n, err := strconv.ParseUint(p, 10, 16)
		if err != nil {
			return "", 0
		}
		host, port = h, uint16(n)
	}
	if addr, err := netip.ParseAddr(strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")); err == nil {
		return addr.Unmap().String(), port
	}
	name, err := egressrules.NormalizeHost(host)
	if err != nil {
		return "", 0
	}
	return name, port
}

// prefixConn reads from r, which holds what the boundary read past the
// request head before the rest of the connection, and fails a write that
// makes no progress for stall, unless stall is zero.
type prefixConn struct {
	net.Conn
	r     io.Reader
	stall time.Duration
}

func (c *prefixConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func (c *prefixConn) Write(p []byte) (int, error) {
	if c.stall > 0 {
		if err := c.Conn.SetWriteDeadline(time.Now().Add(c.stall)); err != nil {
			return 0, err
		}
	}
	return c.Conn.Write(p)
}

// oneConnListener hands out one connection, then blocks until it closes.
type oneConnListener struct {
	conn net.Conn
	mu   sync.Mutex
	done chan struct{}
	once sync.Once
}

func newOneConnListener(c net.Conn) *oneConnListener {
	return &oneConnListener{conn: c, done: make(chan struct{})}
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	c := l.conn
	l.conn = nil
	l.mu.Unlock()
	if c != nil {
		return c, nil
	}
	<-l.done
	return nil, net.ErrClosed
}

func (l *oneConnListener) closeOnce() { l.once.Do(func() { close(l.done) }) }

func (l *oneConnListener) Close() error {
	l.closeOnce()
	return nil
}

func (l *oneConnListener) Addr() net.Addr {
	return &net.UnixAddr{Name: "egress", Net: "unix"}
}
