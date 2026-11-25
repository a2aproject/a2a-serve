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

// Package hosttools talks to the tools endpoint of a host: an MCP server
// over streamable HTTP, which may also answer the hooks of a2a-serve-sidecar.
package hosttools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/net/http/httpguts"
)

// dialTimeout bounds a connection to the endpoint.
const dialTimeout = 10 * time.Second

// Endpoint is where the host serves its tools: an http or https URL, or a
// Unix socket that serves HTTP with the MCP endpoint at /.
type Endpoint struct {
	// socket is the path of a Unix socket, or "".
	socket string
	// url is the URL of the requests, with the host "tools" for a socket.
	url *url.URL
	// token, when non-empty, is sent in [HeaderToken] on every request.
	token string
}

// Parse parses an http://, https:// or unix:///path URL.
func Parse(v string) (Endpoint, error) {
	if path, ok := strings.CutPrefix(v, "unix://"); ok {
		if !filepath.IsAbs(path) {
			return Endpoint{}, errors.New("want unix:///absolute/path")
		}
		return Endpoint{socket: filepath.Clean(path), url: &url.URL{Scheme: "http", Host: "tools", Path: "/"}}, nil
	}
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Fragment != "" {
		return Endpoint{}, errors.New("want an http:// or https:// URL without user info, or unix:///path")
	}
	if u.Path == "" {
		u.Path = "/"
	}
	return Endpoint{url: u}, nil
}

// Unix reports whether the endpoint is a Unix socket.
func (e Endpoint) Unix() bool { return e.socket != "" }

// WithToken returns a copy of e that sends token in [HeaderToken] on every
// request.
func (e Endpoint) WithToken(token string) (Endpoint, error) {
	if token != "" && !httpguts.ValidHeaderFieldValue(token) {
		return Endpoint{}, errors.New("invalid tools endpoint token: has a character that a header can't carry")
	}
	e.token = token
	return e, nil
}

// Token returns the token that the endpoint's requests carry in
// [HeaderToken], or "".
func (e Endpoint) Token() string { return e.token }

// URL returns the URL of the endpoint's requests, which reaches it only
// through [Endpoint.Transport] for a Unix socket.
func (e Endpoint) URL() string { return e.url.String() }

// Path returns the path of the endpoint's requests, without their query.
func (e Endpoint) Path() string { return e.url.EscapedPath() }

// String describes the endpoint without the query of its URL, which may
// carry a secret.
func (e Endpoint) String() string {
	if e.socket != "" {
		return "unix://" + e.socket
	}
	return e.url.Scheme + "://" + e.url.Host + e.url.EscapedPath()
}

// Transport returns a transport whose connections reach the endpoint.
func (e Endpoint) Transport() *http.Transport {
	d := &net.Dialer{Timeout: dialTimeout}
	t := &http.Transport{
		DialContext:         d.DialContext,
		TLSHandshakeTimeout: dialTimeout,
		IdleConnTimeout:     time.Minute,
		MaxConnsPerHost:     maxConns,
		MaxIdleConnsPerHost: maxConns,
	}
	if e.socket != "" {
		t.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return d.DialContext(ctx, "unix", e.socket)
		}
	}
	return t
}

// Proxy forwards the agent's requests from the sandbox to the endpoint.
type Proxy struct {
	ep        Endpoint
	transport *http.Transport
}

// Proxy returns the proxy of the agent's requests from the sandbox, whose
// processes share its connections to the endpoint.
func (e Endpoint) Proxy() *Proxy { return &Proxy{ep: e, transport: e.Transport()} }

// For returns a transport for the requests of the agent process of the A2A
// context contextID; task returns the task that the process works on, or
// "", and may be nil. It refuses a request to another path than the
// endpoint's, or with a method that streamable HTTP doesn't use, without
// sending it. It sends the others to the endpoint with the endpoint's
// scheme, host and query in place of the request's, so that a client given
// only [Endpoint.Path] never sees the query. It drops the request's
// A2A-Sidecar- headers, sets HeaderOrigin to OriginSandbox, so that the host
// can tell the agent from a2a-serve-sidecar, and sets HeaderContext and
// HeaderTask to the process's context and task when it has them.
func (p *Proxy) For(contextID string, task func() string) http.RoundTripper {
	e := p.ep
	path := e.url.EscapedPath()
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.Method != http.MethodPost && r.Method != http.MethodGet && r.Method != http.MethodDelete:
			resp := refusal(r, http.StatusMethodNotAllowed, "the tools endpoint takes POST, GET and DELETE")
			resp.Header.Set("Allow", "POST, GET, DELETE")
			closeRequest(r)
			return resp, nil
		case r.URL.Opaque != "" || r.URL.EscapedPath() != path:
			closeRequest(r)
			return refusal(r, http.StatusForbidden, "the tools endpoint is at "+path), nil
		}
		r = r.Clone(r.Context())
		for name := range r.Header {
			// CGI-style stacks read _ as -.
			if strings.HasPrefix(strings.ToLower(strings.ReplaceAll(name, "_", "-")), "a2a-sidecar-") {
				delete(r.Header, name)
			}
		}
		r.Header.Set(HeaderOrigin, OriginSandbox)
		if contextID != "" && httpguts.ValidHeaderFieldValue(contextID) {
			r.Header.Set(HeaderContext, contextID)
			if task != nil {
				if t := task(); t != "" && httpguts.ValidHeaderFieldValue(t) {
					r.Header.Set(HeaderTask, t)
				}
			}
		}
		if e.token != "" {
			r.Header.Set(HeaderToken, e.token)
		}
		r.URL.Scheme, r.URL.Host, r.Host, r.URL.RawQuery = e.url.Scheme, e.url.Host, e.url.Host, e.url.RawQuery
		return p.transport.RoundTrip(r)
	})
}

// CloseIdleConnections closes the proxy's idle connections to the endpoint.
func (p *Proxy) CloseIdleConnections() { p.transport.CloseIdleConnections() }

// refusal returns an answer to r with status and the text msg.
func refusal(r *http.Request, status int, msg string) *http.Response {
	body := msg + "\n"
	return &http.Response{
		Status:        fmt.Sprintf("%d %s", status, http.StatusText(status)),
		StatusCode:    status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{"Content-Type": {"text/plain; charset=utf-8"}},
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       r,
	}
}

// closeRequest closes the body of a request that a transport doesn't send,
// as a transport must.
func closeRequest(r *http.Request) {
	if r.Body == nil {
		return
	}
	if err := r.Body.Close(); err != nil {
		slog.Debug("closing the body of a refused request to the tools endpoint", "error", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// errorf returns an error about the endpoint e.
func (e Endpoint) errorf(format string, args ...any) error {
	return fmt.Errorf("the tools endpoint %s: %w", e, fmt.Errorf(format, args...))
}
