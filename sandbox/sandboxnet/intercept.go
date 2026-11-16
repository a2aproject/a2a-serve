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
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"strconv"
	"sync"
	"time"

	"github.com/a2aproject/a2a-serve/sandbox/egress"
)

// MaxInterceptBody bounds the body of a request that an [Interceptor]
// gets.
const MaxInterceptBody = 16 << 20

// interceptHeadTimeout bounds the wait for the first bytes of an inspected
// tunnel, which tell TLS from plain HTTP, and connectTimeout the wait for
// the interceptor to decide a connection.
const (
	interceptHeadTimeout = 15 * time.Second
	connectTimeout       = 15 * time.Second
)

// ConnectAction is what an [Interceptor] does with a connection.
type ConnectAction int

const (
	// ConnectPolicy leaves the connection to the policy.
	ConnectPolicy ConnectAction = iota
	// ConnectAllow allows the connection to the public addresses of its
	// host, as the internet mode does. Private addresses, such as those of
	// the LAN or the tailnet, still need a rule of the policy.
	ConnectAllow
	// ConnectDeny refuses the connection.
	ConnectDeny
	// ConnectInspect accepts the connection without dialing the host, and
	// hands each HTTP request on it to [Interceptor.Request]. TLS gets a
	// certificate of the boundary's CA, which must cover the host.
	ConnectInspect
)

// Interceptor decides the connections of a sandbox in code, before the
// policy does, and the requests of the connections it inspects. Its
// methods run concurrently.
type Interceptor interface {
	// Connect decides a connection to host, a normalized name or an
	// address, on port. The sandbox waits for the answer, so it should
	// come within seconds.
	Connect(ctx context.Context, host string, port uint16) ConnectAction
	// Request decides a request of an inspected connection. r has an
	// absolute URL and a body of at most [MaxInterceptBody] bytes in
	// memory. It returns the response for the sandbox, or nil to forward r,
	// which it may change. The boundary sends r to the host of its URL as a
	// connection that [ConnectAllow] allowed, so changing the URL sends r
	// elsewhere. A new body needs a matching ContentLength.
	Request(ctx context.Context, r *http.Request) (*http.Response, error)
}

// connectAction asks the interceptor about host:port, if there is one.
func (b *Boundary) connectAction(host string, port uint16) ConnectAction {
	if b.cfg.Interceptor == nil {
		return ConnectPolicy
	}
	ctx, cancel := context.WithTimeout(b.ctx, connectTimeout)
	defer cancel()
	return b.cfg.Interceptor.Connect(ctx, host, port)
}

// inspect serves the requests of an inspected tunnel to host on port.
func (b *Boundary) inspect(guest net.Conn, br *bufio.Reader, host string, port uint16) {
	target := net.JoinHostPort(host, strconv.Itoa(int(port)))
	b.log.write(slog.LevelDebug, "egress inspected", "destination", target)
	upstream := b.interceptTransport()
	defer upstream.CloseIdleConnections()
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.inspected(w, r, host, port, target, upstream)
	})
	b.serveHTTP(guest, br, target, host, false, h)
}

// interceptTransport returns the transport of the requests that an
// interceptor forwards. It dials the host of each request as a connection
// that the interceptor allowed, and adds the headers of the injections.
func (b *Boundary) interceptTransport() *http.Transport {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			ctx, cancel := context.WithTimeout(ctx, dialTimeout)
			defer cancel()
			host, port := splitAuthority(addr, 0)
			if host == "" {
				return nil, refuse(400, "malformed-target")
			}
			g, rerr := b.authorize(ctx, host, port, true)
			if rerr != nil {
				return nil, rerr
			}
			c, _, err := b.dial(ctx, g)
			return c, err
		},
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   dialTimeout,
		ResponseHeaderTimeout: injectHeaderTimeout,
		IdleConnTimeout:       injectIdleTimeout,
		MaxIdleConnsPerHost:   4,
		DisableCompression:    true,
	}
	tr.TLSClientConfig = &tls.Config{RootCAs: b.cfg.UpstreamRoots, MinVersion: tls.VersionTLS12}
	return tr
}

// inspected hands the request r of an inspected tunnel to the interceptor,
// then answers it or forwards it.
func (b *Boundary) inspected(w http.ResponseWriter, r *http.Request, host string, port uint16, target string, upstream http.RoundTripper) {
	if h, p := splitAuthority(r.Host, port); h != host || p != port {
		http.Error(w, "the request names another host than its connection", http.StatusMisdirectedRequest)
		return
	}
	if r.URL.Opaque != "" || r.URL.Host != "" {
		http.Error(w, "the request target must be a path", http.StatusBadRequest)
		return
	}
	select {
	case b.injectSlots <- struct{}{}:
		defer func() { <-b.injectSlots }()
	default:
		w.Header().Set("Retry-After", "1")
		http.Error(w, "too many open requests from the sandbox", http.StatusServiceUnavailable)
		return
	}
	if r.ContentLength > MaxInterceptBody {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxInterceptBody))
	if err != nil {
		http.Error(w, "reading the request body: "+err.Error(), http.StatusRequestEntityTooLarge)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), injectTimeout)
	defer cancel()
	req := r.Clone(ctx)
	req.Body, req.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
	req.RequestURI = ""
	req.URL.Scheme, req.URL.Host = "http", r.Host
	if r.TLS != nil {
		req.URL.Scheme = "https"
	}
	resp, err := b.cfg.Interceptor.Request(ctx, req)
	if err != nil {
		b.log.write(slog.LevelWarn, "egress request failed", "destination", target, "reason", "interceptor-failed", "error", withoutURL(err))
		http.Error(w, "the sandbox's interceptor failed", http.StatusBadGateway)
		return
	}
	if resp != nil {
		b.log.write(slog.LevelDebug, "egress request answered", "destination", target, "method", req.Method, "status", resp.StatusCode)
		writeResponse(w, resp)
		return
	}
	if req.URL.Scheme != "https" && req.URL.Scheme != "http" || req.URL.Host == "" {
		http.Error(w, "the interceptor forwarded the request to an invalid URL", http.StatusBadGateway)
		return
	}
	dest := req.URL.Host
	destHost, destPort := splitAuthority(dest, defaultPort(req.URL.Scheme))
	inj, injected := b.injection(destHost, destPort)
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL = req.URL
			pr.Out.Host = ""
			if injected && req.URL.Scheme == "https" {
				inj.apply(pr.Out)
			}
		},
		Transport:     upstream,
		FlushInterval: -1,
		ErrorLog:      log.New(io.Discard, "", 0),
		ModifyResponse: func(resp *http.Response) error {
			b.log.write(slog.LevelDebug, "egress request forwarded", "destination", target, "to", dest, "method", req.Method, "status", resp.StatusCode)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			if errors.Is(err, context.Canceled) {
				return
			}
			status := http.StatusBadGateway
			if rerr, ok := errors.AsType[*refusal](err); ok {
				status = rerr.status
				w.Header().Set("Proxy-Status", fmt.Sprintf("a2a-serve; error=%s; reason=%s", errorType(rerr.reason), rerr.reason))
			}
			b.log.write(slog.LevelWarn, "egress request failed", "destination", target, "to", dest, "status", status, "error", withoutURL(err))
			w.WriteHeader(status)
		},
	}
	proxy.ServeHTTP(w, req)
}

// defaultPort returns the port of a URL of scheme without one.
func defaultPort(scheme string) uint16 {
	if scheme == "https" {
		return egress.HTTPSPort
	}
	return 80
}

// writeResponse writes resp, which an interceptor made, and closes its body.
func writeResponse(w http.ResponseWriter, resp *http.Response) {
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	status := resp.StatusCode
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	if resp.Body == nil {
		return
	}
	if _, err := io.Copy(w, resp.Body); err != nil {
		slog.Debug("writing an intercepted response", "error", err)
	}
	if err := resp.Body.Close(); err != nil {
		slog.Debug("closing an intercepted response", "error", err)
	}
}

// serveHTTP answers the CONNECT request of guest, then serves the HTTP
// requests of its tunnel to host with h: over TLS with a certificate of
// the CA when the guest starts TLS, or else in plain text unless
// requireTLS.
func (b *Boundary) serveHTTP(guest net.Conn, br *bufio.Reader, target, host string, requireTLS bool, h http.Handler) {
	b.cfg.Blocks.allowed(target)
	if _, err := io.WriteString(guest, "HTTP/1.1 200 OK\r\n\r\n"); err != nil {
		return
	}
	buffered, err := br.Peek(br.Buffered())
	if err != nil {
		return
	}
	in := bufio.NewReader(io.MultiReader(bytes.NewReader(bytes.Clone(buffered)), guest))
	if err := guest.SetReadDeadline(time.Now().Add(interceptHeadTimeout)); err != nil {
		return
	}
	first, err := in.Peek(1)
	if err != nil {
		return
	}
	if err := guest.SetReadDeadline(time.Time{}); err != nil {
		return
	}
	conn := net.Conn(&prefixConn{Conn: guest, r: in, stall: writeStallTimeout})
	if first[0] == tlsHandshakeRecord {
		tc, ok := b.terminateTLS(conn, target, host)
		if !ok {
			return
		}
		conn = tc
	} else if requireTLS {
		b.log.write(slog.LevelWarn, "egress failed", "destination", target, "reason", "tls-required")
		return
	}
	var handlers sync.WaitGroup
	ln := newOneConnListener(conn)
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handlers.Add(1)
			defer handlers.Done()
			// A handler that outlives its request's limit is stuck writing
			// to a stream whose container doesn't take the data, which no
			// context ends; closing the connection does.
			stuck := time.AfterFunc(injectTimeout+writeStallTimeout, func() { closeConn(guest) })
			defer stuck.Stop()
			h.ServeHTTP(w, r)
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
		b.log.write(slog.LevelWarn, "egress failed", "destination", target, "reason", "serve-failed", "error", err)
	}
	handlers.Wait()
}

// tlsHandshakeRecord is the first byte of a TLS connection.
const tlsHandshakeRecord = 0x16
