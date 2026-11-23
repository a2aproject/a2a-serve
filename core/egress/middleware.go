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
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/http/httpguts"

	"github.com/a2aproject/a2a-serve/internal/egressrules"
)

const middlewareHostKey = "egress-middleware.invalid"

// MiddlewareEntry routes intercepted HTTP requests for Host to URL.
type MiddlewareEntry struct {
	// Host is a host name, with :port for a port other than 443.
	Host string
	// URL is the middleware endpoint: http://host:port[/path] or unix:///path.
	URL string
}

// MiddlewareConfig configures a [MiddlewareRouter].
type MiddlewareConfig struct {
	// Entries are the hosts whose requests are forwarded to middleware endpoints.
	Entries []MiddlewareEntry
	// Next decides connections to hosts not named in Entries. Nil refuses them.
	Next Router
	// Task returns the task that the process of key works on, or "" when
	// it doesn't know one. Nil sends no task.
	Task func(key string) string
}

type middlewareTarget struct {
	host      string
	port      uint16
	urlHost   string
	basePath  string
	transport *http.Transport
}

// MiddlewareRouter is a [Router] that intercepts connections to named hosts,
// terminates their TLS, and forwards each HTTP request in origin form to the
// host's configured middleware endpoint with [ProxyHeaderContext] and
// [ProxyHeaderTask]. Other connections go to [MiddlewareConfig.Next].
type MiddlewareRouter struct {
	targets []middlewareTarget
	next    Router
	task    func(key string) string
}

// NewMiddlewareRouter validates cfg and returns a [MiddlewareRouter].
func NewMiddlewareRouter(cfg MiddlewareConfig) (*MiddlewareRouter, error) {
	if len(cfg.Entries) == 0 {
		return nil, errors.New("egress middleware needs at least one entry")
	}
	m := &MiddlewareRouter{next: cfg.Next, task: cfg.Task}
	for _, e := range cfg.Entries {
		host, port, err := egressrules.ParseInjectTarget(e.Host)
		if err != nil {
			return nil, fmt.Errorf("invalid egress middleware host: the host %w", err)
		}
		if slices.ContainsFunc(m.targets, func(t middlewareTarget) bool { return t.host == host && t.port == port }) {
			return nil, fmt.Errorf("two egress middlewares for %s; give each host one", net.JoinHostPort(host, strconv.Itoa(int(port))))
		}
		network, addr, basePath, err := parseMiddlewareURL(e.URL)
		if err != nil {
			return nil, fmt.Errorf("invalid egress middleware for %s: %w", net.JoinHostPort(host, strconv.Itoa(int(port))), err)
		}
		urlHost := addr
		if network == "unix" {
			urlHost = middlewareHostKey
		}
		dialer := &net.Dialer{Timeout: proxyDecisionTimeout}
		tr := &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, network, addr)
			},
			MaxIdleConns:           proxyIdleConns,
			MaxIdleConnsPerHost:    proxyIdleConns,
			IdleConnTimeout:        90 * time.Second,
			ResponseHeaderTimeout:  proxyHeaderTimeout,
			MaxResponseHeaderBytes: maxProxyResponseHead,
			DisableCompression:     true,
		}
		m.targets = append(m.targets, middlewareTarget{
			host:      host,
			port:      port,
			urlHost:   urlHost,
			basePath:  basePath,
			transport: tr,
		})
	}
	return m, nil
}

func parseMiddlewareURL(u string) (network, addr, basePath string, err error) {
	invalid := func(why string) error {
		return fmt.Errorf("invalid middleware URL: %s; want http://host:port[/path] or unix:///path", why)
	}
	if path, ok := strings.CutPrefix(u, "unix://"); ok {
		if !filepath.IsAbs(path) {
			return "", "", "", invalid("the socket path isn't absolute")
		}
		return "unix", filepath.Clean(path), "", nil
	}
	parsed, err := url.Parse(u)
	if err != nil || parsed.Scheme != "http" || parsed.Host == "" {
		return "", "", "", invalid("the URL isn't http or unix")
	}
	switch {
	case parsed.User != nil:
		return "", "", "", invalid("the URL can't carry credentials")
	case parsed.RawQuery != "", parsed.Fragment != "":
		return "", "", "", invalid("the URL can't have a query or fragment")
	case parsed.Port() == "":
		return "", "", "", invalid("the URL has no port")
	}
	base := strings.TrimSuffix(parsed.EscapedPath(), "/")
	return "tcp", parsed.Host, base, nil
}

// Route intercepts connections to configured middleware hosts and delegates
// all other connections to Next.
func (m *MiddlewareRouter) Route(ctx context.Context, f Flow) (Route, error) {
	for i := range m.targets {
		t := &m.targets[i]
		if t.host != f.Host || t.port != f.Port {
			continue
		}
		if f.Key != "" && !httpguts.ValidHeaderFieldValue(f.Key) {
			return Route{}, errors.New("the flow's key can't travel in a header")
		}
		return HTTP(m.forward(f.Key, t)), nil
	}
	if m.next == nil {
		return Refuse(http.StatusForbidden, "refused"), nil
	}
	return m.next.Route(ctx, f)
}

// Hosts implements [InterceptHosts], returning the middleware hosts together
// with the hosts that Next intercepts.
func (m *MiddlewareRouter) Hosts() []string {
	var hosts []string
	for _, t := range m.targets {
		if !slices.Contains(hosts, t.host) {
			hosts = append(hosts, t.host)
		}
	}
	if ih, ok := m.next.(InterceptHosts); ok {
		for _, h := range ih.Hosts() {
			if !slices.Contains(hosts, h) {
				hosts = append(hosts, h)
			}
		}
	}
	return hosts
}

func (m *MiddlewareRouter) unconstrained() bool {
	if m.next == nil {
		return false
	}
	if u, ok := m.next.(interface{ unconstrained() bool }); ok && u.unconstrained() {
		return true
	}
	_, ok := m.next.(InterceptHosts)
	return !ok
}

func (m *MiddlewareRouter) forward(key string, target *middlewareTarget) func(http.RoundTripper) http.RoundTripper {
	return func(next http.RoundTripper) http.RoundTripper {
		return roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.Header.Get("Upgrade") != "" {
				return next.RoundTrip(r)
			}
			out := r.Clone(r.Context())
			out.RequestURI = ""
			stripSidecarHeaders(out.Header)
			if key != "" {
				out.Header.Set(ProxyHeaderContext, key)
			}
			if m.task != nil {
				if task := m.task(key); task != "" && httpguts.ValidHeaderFieldValue(task) {
					out.Header.Set(ProxyHeaderTask, task)
				}
			}
			escapedPath := r.URL.EscapedPath()
			if escapedPath == "" {
				escapedPath = "/"
			}
			fullEscaped := target.basePath + escapedPath
			decoded, err := url.PathUnescape(fullEscaped)
			if err != nil {
				decoded = target.basePath + r.URL.Path
			}
			out.Host = r.URL.Host
			out.URL = &url.URL{
				Scheme:   "http",
				Host:     target.urlHost,
				Path:     decoded,
				RawPath:  fullEscaped,
				RawQuery: r.URL.RawQuery,
			}
			if out.URL.EscapedPath() == fullEscaped {
				out.URL.RawPath = ""
			}
			return target.transport.RoundTrip(out)
		})
	}
}

// CloseIdleConnections closes idle connections to every middleware endpoint.
func (m *MiddlewareRouter) CloseIdleConnections() {
	for _, t := range m.targets {
		t.transport.CloseIdleConnections()
	}
}
