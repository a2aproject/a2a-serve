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
	"crypto/tls"
	"errors"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-serve/internal/egress"
)

// Limits of the requests to the hosts that get headers, per boundary.
const (
	// maxInjectRequests bounds the open requests.
	maxInjectRequests = 16
	// maxInjectBody bounds a request body.
	maxInjectBody = 64 << 20
	// injectHeaderTimeout bounds the wait for the host to answer, which
	// a model can take long to start.
	injectHeaderTimeout = 10 * time.Minute
	// injectTimeout bounds a whole request, a streamed answer included.
	injectTimeout = 30 * time.Minute
	// injectIdleTimeout closes a connection with no request for so long.
	injectIdleTimeout = 5 * time.Minute
	// writeStallTimeout closes a connection to which the boundary can't
	// write for so long, such as one whose container stopped reading.
	writeStallTimeout = time.Minute
)

// Injection is a header that a boundary adds to the container's HTTPS
// requests to Host on Port. The boundary answers the container's TLS
// connections to that host with a certificate of its [CA], adds the
// header, and sends the request on to the host over TLS that it checks.
type Injection struct {
	Host string
	Port uint16
	// Header is set to Value.
	Header, Value string
	// Model is set for the key of a model provider. The boundary removes
	// the credential headers and the query parameter QueryKey that the
	// container sent, and sets Header only on the requests of Endpoints,
	// so that the container can't pick how the key reaches the host.
	Model     bool
	QueryKey  string
	Endpoints []string
	// Secrets name the secrets in Value, for the log.
	Secrets []string
}

// injection returns the injection of host on port.
func (b *Boundary) injection(host string, port uint16) (Injection, bool) {
	for _, inj := range b.cfg.Inject {
		if inj.Host == host && inj.Port == port {
			return inj, true
		}
	}
	return Injection{}, false
}

// intercept serves the tunnel of an allowed request to the host of inj:
// it terminates the container's TLS and forwards each request to the
// checked addresses of g with the header added.
func (b *Boundary) intercept(guest net.Conn, br *bufio.Reader, g grant, inj Injection) {
	cert, err := b.cfg.CA.leaf(inj.Host)
	if err != nil {
		b.log.write(slog.LevelError, "egress failed", "destination", g.target, "reason", "certificate-failed", "error", err)
		writeRefusal(guest, refuse(502, "certificate-failed"))
		return
	}
	b.cfg.Blocks.allowed(g.target)
	b.log.write(slog.LevelDebug, "egress allowed", "destination", g.target, "rule", g.rule, "inject", strings.Join(inj.Secrets, ","))
	if _, err := io.WriteString(guest, "HTTP/1.1 200 OK\r\n\r\n"); err != nil {
		return
	}
	buffered, err := br.Peek(br.Buffered())
	if err != nil {
		return
	}
	conn := &prefixConn{Conn: guest, r: io.MultiReader(bytes.NewReader(bytes.Clone(buffered)), guest), stall: writeStallTimeout}
	tc := tls.Server(conn, &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"h2", "http/1.1"},
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			// A name other than the requested host would let the container
			// pick which certificate the boundary presents.
			if name, err := egress.NormalizeHost(hello.ServerName); hello.ServerName != "" && (err != nil || name != inj.Host) {
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
	upstream := b.upstreamTransport(g, inj.Host)
	defer upstream.CloseIdleConnections()
	var handlers sync.WaitGroup
	ln := newOneConnListener(tc)
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handlers.Add(1)
			defer handlers.Done()
			// A handler that outlives its request's limit is stuck writing
			// to a stream whose container doesn't take the data, which no
			// context ends; closing the connection does.
			stuck := time.AfterFunc(injectTimeout+writeStallTimeout, func() { closeConn(guest) })
			defer stuck.Stop()
			b.forward(w, r, inj, g.target, upstream)
		}),
		ReadHeaderTimeout: headTimeout,
		IdleTimeout:       injectIdleTimeout,
		MaxHeaderBytes:    maxHead,
		HTTP2:             &http.HTTP2Config{MaxConcurrentStreams: maxInjectRequests, WriteByteTimeout: writeStallTimeout},
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

// forward sends the request r of the container to the host of inj.
func (b *Boundary) forward(w http.ResponseWriter, r *http.Request, inj Injection, target string, upstream http.RoundTripper) {
	if host, port := splitAuthority(r.Host, inj.Port); host != inj.Host || port != inj.Port {
		http.Error(w, "the request names another host than its connection", http.StatusMisdirectedRequest)
		return
	}
	if r.URL.Opaque != "" || !strings.HasPrefix(r.URL.Path, "/") {
		http.Error(w, "the request target must be a path", http.StatusBadRequest)
		return
	}
	select {
	case b.injectSlots <- struct{}{}:
		defer func() { <-b.injectSlots }()
	default:
		w.Header().Set("Proxy-Status", "a2a-serve; error=connection_limit_reached; reason=busy")
		w.Header().Set("Retry-After", "1")
		http.Error(w, "too many open requests from the sandbox", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), injectTimeout)
	defer cancel()
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host, pr.Out.Host = "https", target, ""
			if inj.Port == egress.HTTPSPort {
				pr.Out.URL.Host = inj.Host
			}
			inj.apply(pr.Out)
		},
		Transport:     upstream,
		FlushInterval: -1,
		ErrorLog:      log.New(io.Discard, "", 0),
		ModifyResponse: func(resp *http.Response) error {
			b.log.write(slog.LevelDebug, "egress request", "destination", target, "inject", strings.Join(inj.Secrets, ","), "status", resp.StatusCode)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			status := http.StatusBadGateway
			var tooLarge *http.MaxBytesError
			switch {
			case errors.As(err, &tooLarge):
				status = http.StatusRequestEntityTooLarge
			case errors.Is(err, context.DeadlineExceeded):
				status = http.StatusGatewayTimeout
			case errors.Is(err, context.Canceled):
				return
			}
			b.log.write(slog.LevelWarn, "egress request failed", "destination", target, "status", status, "error", withoutURL(err))
			w.WriteHeader(status)
		},
	}
	// The total time bounds a slow body. A read deadline of the connection,
	// as the server's own routes set, would also end the response when the
	// upstream transport closes the body early.
	if r.ContentLength > maxInjectBody {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	// An HTTP/1.1 server closes the rest of the request body once the
	// answer starts, which aborts the upstream request that still sends
	// it and cuts its answer off. HTTP/2 is always full duplex.
	if err := http.NewResponseController(w).EnableFullDuplex(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		b.log.write(slog.LevelWarn, "egress request may be cut off", "destination", target, "error", err)
	}
	body := http.MaxBytesReader(w, r.Body, maxInjectBody)
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

// apply puts the header of inj on the outgoing request r.
func (inj Injection) apply(r *http.Request) {
	if !inj.Model {
		r.Header.Del(inj.Header)
		r.Header.Set(inj.Header, inj.Value)
		return
	}
	for _, name := range egress.CredentialHeaders {
		r.Header.Del(name)
	}
	if inj.QueryKey != "" && r.URL.Query().Has(inj.QueryKey) {
		q := r.URL.Query()
		q.Del(inj.QueryKey)
		r.URL.RawQuery = q.Encode()
	}
	if r.URL.RawPath == "" && plainPath(r.URL.Path) && egress.MatchEndpoint(inj.Endpoints, r.Method, r.URL.Path) {
		r.Header.Set(inj.Header, inj.Value)
	}
}

// upstreamTransport returns the transport of the requests of one tunnel to
// host, which dials only the checked addresses of g.
func (b *Boundary) upstreamTransport(g grant, host string) *http.Transport {
	return &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			ctx, cancel := context.WithTimeout(ctx, dialTimeout)
			defer cancel()
			c, _, err := b.dial(ctx, g)
			return c, err
		},
		TLSClientConfig:       &tls.Config{ServerName: host, RootCAs: b.cfg.UpstreamRoots, MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   dialTimeout,
		ResponseHeaderTimeout: injectHeaderTimeout,
		IdleConnTimeout:       injectIdleTimeout,
		MaxIdleConnsPerHost:   4,
		DisableCompression:    true,
	}
}

// plainPath reports whether p is a clean path of the characters of API
// paths, so that a host that reads escapes, dot segments or parameters in
// it can't take it for another endpoint than the one it matched.
func plainPath(p string) bool {
	if path.Clean(p) != p || strings.Contains(p, "/.") {
		return false
	}
	return !strings.ContainsFunc(p, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("/-_.:~@+", r))
	})
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
	name, err := egress.NormalizeHost(host)
	if err != nil {
		return "", 0
	}
	return name, port
}

// prefixConn reads what the boundary read past the request head before
// the rest of the connection, and fails a write that makes no progress for
// stall.
type prefixConn struct {
	net.Conn
	r     io.Reader
	stall time.Duration
}

func (c *prefixConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func (c *prefixConn) Write(p []byte) (int, error) {
	if err := c.Conn.SetWriteDeadline(time.Now().Add(c.stall)); err != nil {
		return 0, err
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
