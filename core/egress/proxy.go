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

package egress

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/net/http/httpguts"
)

// Headers and values of the egress proxy protocol, which [ProxyRouter]
// speaks. Names are case-insensitive, as all HTTP header names are.
const (
	// ProxyHeaderContext carries [Flow.Key], which a2a-run sets to the A2A
	// context ID, on the CONNECT request and on every forwarded request.
	ProxyHeaderContext = "A2A-Run-Context"
	// ProxyHeaderTask carries the task that the process works on, on
	// forwarded requests, when [ProxyConfig.Task] knows it.
	ProxyHeaderTask = "A2A-Run-Task"
	// ProxyHeaderIntercept is ProxyInterceptOffered on the CONNECT
	// request, and ProxyInterceptAccept on a 204 answer that asks for the
	// connection's requests.
	ProxyHeaderIntercept  = "A2A-Run-Intercept"
	ProxyInterceptOffered = "offered"
	ProxyInterceptAccept  = "accept"
)

// Bounds of the answers of an egress proxy.
const (
	// proxyDecisionTimeout bounds dialing the proxy and reading its answer
	// to a CONNECT request.
	proxyDecisionTimeout = 10 * time.Second
	// maxProxyHead bounds the head of an answer to a CONNECT request, and
	// maxProxyResponseHead that of an answer to a forwarded request.
	maxProxyHead         = 16 << 10
	maxProxyResponseHead = 64 << 10
	// maxProxyRefusal bounds the body of a refusal that is read, and
	// maxProxyReason the reason the client sees.
	maxProxyRefusal = 4 << 10
	maxProxyReason  = 256
	// proxyHeaderTimeout bounds the wait for the head of an answer to a
	// forwarded request, which a model can take long to start.
	proxyHeaderTimeout = 10 * time.Minute
	// proxyIdleConns bounds the idle connections kept to the proxy.
	proxyIdleConns = 32
)

// proxyHostKey names the proxy in the URLs of forwarded requests, whose
// connections always reach the proxy.
const proxyHostKey = "egress-proxy.invalid"

// ProxyConfig configures a [ProxyRouter].
type ProxyConfig struct {
	// URL is the proxy: http://host:port, or unix:///path for a Unix
	// socket.
	URL string
	// Task returns the task that the process of key works on, or "" when
	// it doesn't know one. Nil sends no task.
	Task func(key string) string
}

// ProxyRouter is a [Router] that lets an HTTP proxy decide each
// connection. It sends the proxy CONNECT host:port with the flow's key and
// an offer to intercept, and routes by the answer:
//
//   - 2xx tunnels the connection through the proxy.
//   - 204 with A2A-Run-Intercept: accept terminates the client's TLS and
//     sends each of its requests to the proxy in absolute form, such as
//     POST https://api.anthropic.com/v1/messages, with the key. Answers
//     stream back as they arrive.
//   - 4xx and 5xx refuse the connection, with the body as the reason.
//
// Connections to port 80 skip the CONNECT request, and their plain HTTP
// requests go to the proxy in absolute form. A proxy that ignores the
// headers tunnels or refuses, as any HTTP proxy does.
//
// It doesn't implement [InterceptHosts], so its boundary's CA signs any
// host.
type ProxyRouter struct {
	dial      func(ctx context.Context) (net.Conn, error)
	task      func(key string) string
	transport *http.Transport
	timeout   time.Duration
}

// NewProxyRouter returns the router of the proxy cfg.URL.
func NewProxyRouter(cfg ProxyConfig) (*ProxyRouter, error) {
	return newProxyRouter(cfg, proxyDecisionTimeout)
}

func newProxyRouter(cfg ProxyConfig, timeout time.Duration) (*ProxyRouter, error) {
	network, addr, err := parseProxyURL(cfg.URL)
	if err != nil {
		return nil, err
	}
	p := &ProxyRouter{task: cfg.Task, timeout: timeout}
	p.dial = func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		return d.DialContext(ctx, network, addr)
	}
	p.transport = &http.Transport{
		DialContext:            func(ctx context.Context, _, _ string) (net.Conn, error) { return p.dial(ctx) },
		MaxIdleConns:           proxyIdleConns,
		MaxIdleConnsPerHost:    proxyIdleConns,
		IdleConnTimeout:        90 * time.Second,
		ResponseHeaderTimeout:  proxyHeaderTimeout,
		MaxResponseHeaderBytes: maxProxyResponseHead,
		DisableCompression:     true,
	}
	return p, nil
}

// parseProxyURL returns the network and the address of the proxy at u.
func parseProxyURL(u string) (network, addr string, err error) {
	invalid := func(why string) error {
		return fmt.Errorf("invalid egress proxy URL: %s; want http://host:port or unix:///path", why)
	}
	if path, ok := strings.CutPrefix(u, "unix://"); ok {
		if !filepath.IsAbs(path) {
			return "", "", invalid("the socket path isn't absolute")
		}
		return "unix", filepath.Clean(path), nil
	}
	parsed, err := url.Parse(u)
	if err != nil || parsed.Scheme != "http" || parsed.Host == "" {
		return "", "", invalid("the URL isn't http or unix")
	}
	switch {
	case parsed.User != nil:
		return "", "", invalid("the URL can't carry credentials")
	case parsed.Path != "" && parsed.Path != "/", parsed.RawQuery != "", parsed.Fragment != "":
		return "", "", invalid("the URL has a path")
	case parsed.Port() == "":
		return "", "", invalid("the URL has no port")
	}
	return "tcp", parsed.Host, nil
}

// Route asks the proxy about f.
func (p *ProxyRouter) Route(ctx context.Context, f Flow) (Route, error) {
	if f.Key != "" && !httpguts.ValidHeaderFieldValue(f.Key) {
		return Route{}, errors.New("the flow's key can't travel in a header")
	}
	if f.Port == 80 {
		return HTTP(p.forward(f.Key)), nil
	}
	target := net.JoinHostPort(f.Host, strconv.Itoa(int(f.Port)))
	conn, err := p.dial(ctx)
	if err != nil {
		return Route{}, fmt.Errorf("dialing the egress proxy: %w", err)
	}
	route, keep, err := p.decide(ctx, conn, target, f.Key)
	if !keep {
		if cerr := conn.Close(); cerr != nil && err == nil {
			return Route{}, fmt.Errorf("closing the connection to the egress proxy: %w", cerr)
		}
	}
	return route, err
}

// decide sends the CONNECT request for target on conn and routes by the
// answer. keep is set when the route took conn.
func (p *ProxyRouter) decide(ctx context.Context, conn net.Conn, target, key string) (_ Route, keep bool, _ error) {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	stop := context.AfterFunc(ctx, func() {
		// A deadline in the past ends the reads and writes in progress.
		if err := conn.SetDeadline(time.Unix(1, 0)); err != nil {
			return
		}
	})
	defer stop()
	if d, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(d); err != nil {
			return Route{}, false, err
		}
	}
	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: target},
		Host:   target,
		Header: http.Header{ProxyHeaderIntercept: {ProxyInterceptOffered}},
	}
	if key != "" {
		req.Header.Set(ProxyHeaderContext, key)
	}
	if err := req.Write(conn); err != nil {
		return Route{}, false, fmt.Errorf("asking the egress proxy about %s: %w", target, proxyError(err))
	}
	head := &limitReader{r: conn, n: maxProxyHead}
	br := bufio.NewReader(head)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		return Route{}, false, fmt.Errorf("reading the egress proxy's answer about %s: %w", target, proxyError(err))
	}
	switch {
	case resp.StatusCode == http.StatusNoContent && strings.EqualFold(resp.Header.Get(ProxyHeaderIntercept), ProxyInterceptAccept):
		return HTTP(p.forward(key)), false, nil
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		if err := conn.SetDeadline(time.Time{}); err != nil {
			return Route{}, false, err
		}
		if !stop() {
			return Route{}, false, fmt.Errorf("asking the egress proxy about %s: %w", target, ctx.Err())
		}
		head.n = -1
		return p.tunnel(&readerConn{Conn: conn, r: br}), true, nil
	case resp.StatusCode >= 400 && resp.StatusCode < 600:
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxProxyRefusal))
		reason := refusalReason(body)
		if err != nil && len(body) == 0 {
			reason = "refused by the egress proxy, whose reason broke off"
		}
		return Refuse(resp.StatusCode, reason), false, nil
	}
	return Route{}, false, fmt.Errorf("the egress proxy answered CONNECT %s with status %d", target, resp.StatusCode)
}

// tunnel returns the route through the tunnel c, which closes when the
// boundary doesn't take it in time.
func (p *ProxyRouter) tunnel(c net.Conn) Route {
	var taken atomic.Bool
	timer := time.AfterFunc(p.timeout, func() {
		if taken.CompareAndSwap(false, true) {
			if err := c.Close(); err != nil {
				return
			}
		}
	})
	return Dial(func(context.Context) (net.Conn, error) {
		if !taken.CompareAndSwap(false, true) {
			return nil, errors.New("the tunnel through the egress proxy expired")
		}
		timer.Stop()
		return c, nil
	})
}

// forward returns the middleware that sends each request to the proxy in
// absolute form, with the key and the current task of its process.
func (p *ProxyRouter) forward(key string) func(http.RoundTripper) http.RoundTripper {
	return func(next http.RoundTripper) http.RoundTripper {
		return roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.Header.Get("Upgrade") != "" {
				return next.RoundTrip(r)
			}
			out := r.Clone(r.Context())
			out.RequestURI = ""
			for name := range out.Header {
				// The client mustn't speak for another context, in any
				// spelling that a proxy may read as one of these headers:
				// CGI-style stacks read _ as -.
				if strings.HasPrefix(strings.ToLower(strings.ReplaceAll(name, "_", "-")), "a2a-run-") {
					delete(out.Header, name)
				}
			}
			if key != "" {
				out.Header.Set(ProxyHeaderContext, key)
			}
			if p.task != nil {
				if task := p.task(key); task != "" && httpguts.ValidHeaderFieldValue(task) {
					out.Header.Set(ProxyHeaderTask, task)
				}
			}
			path := r.URL.EscapedPath()
			if path == "" {
				path = "/"
			}
			// The transport writes an opaque URL as the request target,
			// which gives the absolute form on a connection to the proxy.
			out.Host = r.URL.Host
			out.URL = &url.URL{Scheme: "http", Host: proxyHostKey, Opaque: r.URL.Scheme + "://" + r.URL.Host + path, RawQuery: r.URL.RawQuery}
			return p.transport.RoundTrip(out)
		})
	}
}

// CloseIdleConnections closes the idle connections to the proxy.
func (p *ProxyRouter) CloseIdleConnections() { p.transport.CloseIdleConnections() }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// refusalReason returns the printable text of body on one line, at most
// maxProxyReason bytes long.
func refusalReason(body []byte) string {
	var sb strings.Builder
	space := false
	for _, c := range string(body) {
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			space = sb.Len() > 0
			continue
		case c < 0x20 || c >= 0x7f:
			continue
		}
		if sb.Len()+2 > maxProxyReason {
			break
		}
		if space {
			sb.WriteByte(' ')
			space = false
		}
		sb.WriteRune(c)
	}
	if sb.Len() == 0 {
		return "refused by the egress proxy"
	}
	return sb.String()
}

// proxyError returns err without the text of a malformed answer, which
// the log mustn't repeat.
func proxyError(err error) error {
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return errors.New("no answer in time")
	}
	if ne, ok := errors.AsType[net.Error](err); ok {
		return ne
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return errors.New("the answer ended early or was too large")
	}
	return errors.New("the answer isn't HTTP")
}

// limitReader reads at most n bytes of r, or all of r when n is negative.
type limitReader struct {
	r io.Reader
	n int64
}

func (l *limitReader) Read(p []byte) (int, error) {
	if l.n < 0 {
		return l.r.Read(p)
	}
	if l.n == 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > l.n {
		p = p[:l.n]
	}
	n, err := l.r.Read(p)
	l.n -= int64(n)
	return n, err
}

// readerConn reads from r, which holds what was read past an answer's
// head before the rest of the connection.
type readerConn struct {
	net.Conn
	r io.Reader
}

func (c *readerConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// CloseWrite passes a half-close on to the proxy, which may still answer.
func (c *readerConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return errors.ErrUnsupported
}
