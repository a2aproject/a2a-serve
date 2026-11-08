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

// Package modelproxy forwards the model API requests of an agent in a
// sandbox to the model providers and adds the API keys, so that the keys
// stay out of the sandbox.
package modelproxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/a2aproject/a2a-serve/internal/httpserver"
	"github.com/a2aproject/a2a-serve/internal/jsonhttp"
)

// PathPrefix starts the paths of the model routes, followed by the name of
// the provider and the path of its API. The handles of the agents routes
// never contain "_".
const PathPrefix = "/_model/"

// Provider is a model API that the proxy forwards to.
type Provider struct {
	// Name is the ID of the provider in opencode, such as google.
	Name string
	// Keys are the environment variables that hold its API key, in the
	// order the proxy picks the key in.
	Keys []string
	// Endpoints are the inference endpoints that the proxy forwards, as
	// "METHOD path". A path that ends in "/" matches the paths below it.
	// Other endpoints of the API, such as files and batches, can read data
	// that other apps stored with the key.
	Endpoints []string
	// SDKBase is the path that its SDKs take as the base URL, such as /v1.
	SDKBase string
	// BaseURLEnv is the environment variable that its SDKs read the base URL
	// from, with the path EnvBase. Empty for none.
	BaseURLEnv string
	EnvBase    string

	upstream string
	// setKey puts the key on a request to the provider.
	setKey func(h http.Header, key string)
}

// Providers lists the providers the proxy forwards to. GOOGLE_API_KEY isn't
// a key of google, since other Google APIs use it too.
var Providers = []Provider{
	{
		Name: "anthropic", Keys: []string{"ANTHROPIC_API_KEY"},
		Endpoints: []string{"POST /v1/messages", "POST /v1/messages/count_tokens", "GET /v1/models", "GET /v1/models/"},
		SDKBase:   "/v1", BaseURLEnv: "ANTHROPIC_BASE_URL",
		upstream: "https://api.anthropic.com", setKey: header("X-Api-Key", ""),
	},
	{
		Name: "google", Keys: []string{"GOOGLE_GENERATIVE_AI_API_KEY", "GEMINI_API_KEY"},
		Endpoints: []string{"POST /v1beta/models/", "GET /v1beta/models", "GET /v1beta/models/", "POST /v1/models/", "GET /v1/models", "GET /v1/models/"},
		SDKBase:   "/v1beta", BaseURLEnv: "GOOGLE_GEMINI_BASE_URL",
		upstream: "https://generativelanguage.googleapis.com", setKey: header("X-Goog-Api-Key", ""),
	},
	{
		Name: "openai", Keys: []string{"OPENAI_API_KEY"},
		Endpoints: []string{"POST /v1/responses", "POST /v1/chat/completions", "POST /v1/completions", "POST /v1/embeddings", "GET /v1/models", "GET /v1/models/"},
		SDKBase:   "/v1", BaseURLEnv: "OPENAI_BASE_URL", EnvBase: "/v1",
		upstream: "https://api.openai.com", setKey: header("Authorization", "Bearer "),
	},
	{
		Name: "openrouter", Keys: []string{"OPENROUTER_API_KEY"},
		Endpoints: []string{"POST /api/v1/chat/completions", "POST /api/v1/completions", "POST /api/v1/embeddings", "GET /api/v1/models", "GET /api/v1/models/"},
		SDKBase:   "/api/v1",
		upstream:  "https://openrouter.ai", setKey: header("Authorization", "Bearer "),
	},
}

// forwards reports whether the proxy forwards a request with method to the
// path p of the provider.
func (pr Provider) forwards(method, p string) bool {
	for _, e := range pr.Endpoints {
		m, prefix, _ := strings.Cut(e, " ")
		if m != method {
			continue
		}
		if p == prefix || (strings.HasSuffix(prefix, "/") && strings.HasPrefix(p, prefix) && len(p) > len(prefix)) {
			return true
		}
	}
	return false
}

func header(name, prefix string) func(http.Header, string) {
	return func(h http.Header, key string) { h.Set(name, prefix+key) }
}

// upstreamOverride replaces the upstream of every provider in e2e builds.
var upstreamOverride string

// ProviderOfKey returns the provider whose API key the environment
// variable name holds.
func ProviderOfKey(name string) (Provider, bool) {
	for _, p := range Providers {
		if slices.Contains(p.Keys, name) {
			return p, true
		}
	}
	return Provider{}, false
}

func providerNamed(name string) (Provider, bool) {
	for _, p := range Providers {
		if p.Name == name {
			return p, true
		}
	}
	return Provider{}, false
}

// credentialHeaders are the request headers that carry API keys. The proxy
// drops them, and the key the agent sent in the query.
var credentialHeaders = []string{"Authorization", "Proxy-Authorization", "X-Api-Key", "X-Goog-Api-Key", "Cookie"}

// Credential returns the API key a request carries in any of the ways the
// SDKs of the providers send it, or "".
func Credential(r *http.Request) string {
	if scheme, tok, ok := strings.Cut(strings.TrimSpace(r.Header.Get("Authorization")), " "); ok && strings.EqualFold(scheme, "Bearer") {
		return strings.TrimSpace(tok)
	}
	for _, h := range []string{"X-Api-Key", "X-Goog-Api-Key"} {
		if v := r.Header.Get(h); v != "" {
			return v
		}
	}
	return r.URL.Query().Get("key")
}

// Limits of the requests of one agent process.
const (
	// maxPerProcess bounds the open requests of one process, and
	// maxConcurrent those of all, which leaves each of the at most 12
	// processes of an agent its share.
	maxPerProcess = 16
	maxConcurrent = 12 * maxPerProcess
	// responseHeaderTimeout bounds the wait for the provider to answer,
	// which reasoning models without streaming take minutes for.
	responseHeaderTimeout = 10 * time.Minute
	// requestTimeout bounds a whole request, the streamed answer included.
	requestTimeout = 30 * time.Minute
)

// Options configure a [Proxy].
type Options struct {
	// Key returns the API key of provider for the agent process, or false
	// when the process has none.
	Key func(process string, provider Provider) (string, bool)
	// Process returns the agent process a request comes from, or false when
	// the request proves none.
	Process func(*http.Request) (string, bool)
	// Transport sends the requests to the providers. Nil uses a clone of
	// [http.DefaultTransport].
	Transport http.RoundTripper
}

// Proxy serves the model routes at [PathPrefix].
type Proxy struct {
	opts Options
	// forward bounds the requests and forwards them.
	forward http.Handler
}

// New returns a proxy configured by opts.
func New(opts Options) *Proxy {
	if opts.Transport == nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.ResponseHeaderTimeout = responseHeaderTimeout
		opts.Transport = t
	}
	limiter := httpserver.NewKeyedLimiter(maxConcurrent, maxPerProcess, func(r *http.Request) string {
		return routeOf(r).process
	})
	rp := &httputil.ReverseProxy{
		Rewrite:       rewrite,
		Transport:     opts.Transport,
		FlushInterval: -1,
		ErrorHandler:  proxyError,
	}
	return &Proxy{opts: opts, forward: limiter.Handler(jsonhttp.LimitBody(rp))}
}

// route is a request the proxy forwards.
type route struct {
	process  string
	provider Provider
	path     string
	key      string
}

type routeKey struct{}

func routeOf(r *http.Request) *route { return r.Context().Value(routeKey{}).(*route) }

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	process, ok := p.opts.Process(r)
	if !ok {
		jsonhttp.Error(w, http.StatusUnauthorized, "agent key required")
		return
	}
	name, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, PathPrefix), "/")
	provider, ok := providerNamed(name)
	if !ok || !strings.HasPrefix(r.URL.Path, PathPrefix) {
		jsonhttp.Error(w, http.StatusNotFound, "unknown model provider")
		return
	}
	rest = "/" + rest
	if path.Clean(rest) != rest || !provider.forwards(r.Method, rest) {
		jsonhttp.Error(w, http.StatusNotFound, fmt.Sprintf("the server forwards only the inference endpoints of %s; %s %s isn't one", provider.Name, r.Method, rest))
		return
	}
	key, ok := p.opts.Key(process, provider)
	if !ok {
		jsonhttp.Error(w, http.StatusNotFound, fmt.Sprintf("the server has no %s for this agent; run `a2a-serve config set --secret %s --restart`", provider.Keys[0], provider.Keys[0]))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	ctx = context.WithValue(ctx, routeKey{}, &route{process: process, provider: provider, path: rest, key: key})
	p.forward.ServeHTTP(w, r.WithContext(ctx))
}

// rewrite sends the request to the provider with its key in place of the
// credentials the agent sent.
func rewrite(pr *httputil.ProxyRequest) {
	rt := routeOf(pr.In)
	upstream := rt.provider.upstream
	if upstreamOverride != "" {
		upstream = upstreamOverride
	}
	u, err := url.Parse(upstream)
	if err != nil {
		panic(err)
	}
	q := pr.In.URL.Query()
	q.Del("key")
	pr.Out.URL = &url.URL{Scheme: u.Scheme, Host: u.Host, Path: strings.TrimSuffix(u.Path, "/") + rt.path, RawQuery: q.Encode()}
	pr.Out.Host = ""
	for _, h := range credentialHeaders {
		pr.Out.Header.Del(h)
	}
	rt.provider.setKey(pr.Out.Header, rt.key)
}

// proxyError answers a request that didn't reach the provider, or whose
// answer stopped. The URL of the request carries no key, since rewrite
// drops it.
func proxyError(w http.ResponseWriter, r *http.Request, err error) {
	rt := routeOf(r)
	if errors.Is(r.Context().Err(), context.Canceled) {
		return
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		jsonhttp.Error(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	code, msg := http.StatusBadGateway, "the server couldn't reach "+rt.provider.Name+"; try again"
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		code, msg = http.StatusGatewayTimeout, rt.provider.Name+" didn't answer in time; try again"
	}
	slog.Warn("forwarding a model request", "provider", rt.provider.Name, "path", rt.path, "error", err)
	jsonhttp.Error(w, code, msg)
}
