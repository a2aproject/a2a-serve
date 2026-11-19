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
// over streamable HTTP, which may also answer the hooks of a2a-run.
package hosttools

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"
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

// Proxy returns a transport that sends each request to the endpoint, with
// the endpoint's scheme, host and query in place of the request's, so that
// a client given only [Endpoint.Path] never sees the query. The request
// keeps its path.
func (e Endpoint) Proxy() http.RoundTripper {
	t := e.Transport()
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		r.URL.Scheme, r.URL.Host, r.Host, r.URL.RawQuery = e.url.Scheme, e.url.Host, e.url.Host, e.url.RawQuery
		return t.RoundTrip(r)
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// errorf returns an error about the endpoint e.
func (e Endpoint) errorf(format string, args ...any) error {
	return fmt.Errorf("the tools endpoint %s: %w", e, fmt.Errorf(format, args...))
}
