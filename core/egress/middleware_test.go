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
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/core/egress"
	"github.com/a2aproject/a2a-serve/internal/testutil"
)

type fakeMiddleware struct {
	t    *testing.T
	next chan struct{}

	mu   sync.Mutex
	seen []string
}

func newFakeMiddleware(t *testing.T) *fakeMiddleware {
	t.Helper()
	return &fakeMiddleware{t: t, next: make(chan struct{})}
}

func (m *fakeMiddleware) record(s string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seen = append(m.seen, s)
}

func (m *fakeMiddleware) requests() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.seen...)
}

func (m *fakeMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		m.t.Errorf("reading the request body error = %v", err)
		return
	}
	m.record(fmt.Sprintf("%s %s host=%s context=%s task=%s body=%s%s",
		r.Method, r.RequestURI, r.Host,
		r.Header.Get(egress.ProxyHeaderContext),
		r.Header.Get(egress.ProxyHeaderTask),
		string(body), spoofed(r.Header)))
	if strings.HasSuffix(r.URL.Path, "/sse") {
		w.Header().Set("Content-Type", "text/event-stream")
		for i, ev := range []string{"data: one\n\n", "data: two\n\n"} {
			if i > 0 {
				select {
				case <-m.next:
				case <-time.After(10 * time.Second):
					m.t.Errorf("the client didn't read the first event")
					return
				}
			}
			if _, err := io.WriteString(w, ev); err != nil {
				m.t.Errorf("writing an event error = %v", err)
				return
			}
			http.NewResponseController(w).Flush() //nolint:errcheck // a failed flush shows as a missing event
		}
		return
	}
	if _, err := io.WriteString(w, "middleware "+r.RequestURI); err != nil {
		m.t.Errorf("writing the answer error = %v", err)
	}
}

func parseCACert(t *testing.T, pemBytes []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		t.Fatalf("pem.Decode() = nil, want a CA certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("x509.ParseCertificate() error = %v", err)
	}
	return cert
}

func TestNewMiddlewareRouterValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		entries []egress.MiddlewareEntry
		wantErr string
	}{
		{
			name:    "valid http without path",
			entries: []egress.MiddlewareEntry{{Host: "api.anthropic.com", URL: "http://127.0.0.1:9000"}},
		},
		{
			name:    "valid http with path and custom port",
			entries: []egress.MiddlewareEntry{{Host: "api.example.com:8443", URL: "http://127.0.0.1:9000/anthropic/"}},
		},
		{
			name:    "valid unix",
			entries: []egress.MiddlewareEntry{{Host: "api.anthropic.com", URL: "unix:///run/host/middleware.sock"}},
		},
		{
			name:    "empty entries",
			wantErr: "at least one entry",
		},
		{
			name:    "wildcard host",
			entries: []egress.MiddlewareEntry{{Host: "*.example.com", URL: "http://127.0.0.1:9000"}},
			wantErr: "invalid egress middleware host",
		},
		{
			name:    "ip host",
			entries: []egress.MiddlewareEntry{{Host: "127.0.0.1", URL: "http://127.0.0.1:9000"}},
			wantErr: "invalid egress middleware host",
		},
		{
			name: "duplicate host default port",
			entries: []egress.MiddlewareEntry{
				{Host: "api.anthropic.com", URL: "http://127.0.0.1:9000"},
				{Host: "API.ANTHROPIC.COM:443", URL: "http://127.0.0.1:9001"},
			},
			wantErr: "two egress middlewares for api.anthropic.com:443",
		},
		{
			name:    "relative unix socket",
			entries: []egress.MiddlewareEntry{{Host: "api.anthropic.com", URL: "unix://run/mw.sock"}},
			wantErr: "the socket path isn't absolute",
		},
		{
			name:    "https url rejected",
			entries: []egress.MiddlewareEntry{{Host: "api.anthropic.com", URL: "https://127.0.0.1:9000"}},
			wantErr: "the URL isn't http or unix",
		},
		{
			name:    "credentials rejected without leaking secret",
			entries: []egress.MiddlewareEntry{{Host: "api.anthropic.com", URL: "http://user:secret@127.0.0.1:9000"}},
			wantErr: "the URL can't carry credentials",
		},
		{
			name:    "query rejected",
			entries: []egress.MiddlewareEntry{{Host: "api.anthropic.com", URL: "http://127.0.0.1:9000/v1?token=secret"}},
			wantErr: "the URL can't have a query or fragment",
		},
		{
			name:    "missing port",
			entries: []egress.MiddlewareEntry{{Host: "api.anthropic.com", URL: "http://127.0.0.1"}},
			wantErr: "the URL has no port",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := egress.NewMiddlewareRouter(egress.MiddlewareConfig{Entries: tc.entries})
			if tc.wantErr == "" && err != nil || tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("egress.NewMiddlewareRouter() error = %v, want %q", err, tc.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), "secret") {
				t.Fatalf("egress.NewMiddlewareRouter() error = %v, want it without secrets", err)
			}
		})
	}
}

func TestMiddlewareRouterForwardsRequests(t *testing.T) {
	t.Parallel()
	mw := newFakeMiddleware(t)
	tcpURL := serveProxy(t, listenTCP(t), mw)
	sock := filepath.Join(testutil.ShortTempDir(t), "mw.sock")
	unixLn, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("net.Listen(unix) error = %v", err)
	}
	unixURL := serveProxy(t, unixLn, mw)

	credUpstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.WriteString(w, r.Header.Get("Authorization")); err != nil {
			t.Errorf("writing upstream answer error = %v", err)
		}
	}))
	t.Cleanup(credUpstream.Close)
	_, credPort, err := net.SplitHostPort(credUpstream.Listener.Addr().String())
	if err != nil {
		t.Fatalf("net.SplitHostPort() error = %v", err)
	}
	credHost := "example.com:" + credPort
	roots := x509.NewCertPool()
	roots.AddCert(credUpstream.Certificate())

	cred := egress.Header{Host: credHost, Name: "Authorization", Value: "Bearer secret"}
	policyRouter, err := (egress.Policy{Mode: egress.ModeAllowlist, Allow: []string{"127.0.0.1"}}).Router(cred)
	if err != nil {
		t.Fatalf("Policy.Router() error = %v", err)
	}
	mr, err := egress.NewMiddlewareRouter(egress.MiddlewareConfig{
		Entries: []egress.MiddlewareEntry{
			{Host: "api.anthropic.com", URL: tcpURL},
			{Host: "api.prefixed.test:8443", URL: tcpURL + "/proxy"},
			{Host: "api.unix.test", URL: unixURL},
		},
		Next: policyRouter,
		Task: func(key string) string { return "task-of-" + key },
	})
	if err != nil {
		t.Fatalf("egress.NewMiddlewareRouter() error = %v", err)
	}
	t.Cleanup(mr.CloseIdleConnections)

	b, err := egress.NewWithHosts(egress.Config{Router: mr, Log: slog.New(slog.DiscardHandler)}, roots)
	if err != nil {
		t.Fatalf("egress.NewWithHosts() error = %v", err)
	}
	t.Cleanup(func() {
		if err := b.Close(); err != nil {
			t.Errorf("Boundary.Close() error = %v", err)
		}
	})
	ep, addr := openEndpoint(t, b, "ctx-1")
	c := client(t, addr, ep.CA())

	spoofHeaders := http.Header{
		"A2a-Sidecar-Context": {"forged"},
		"A2A_Sidecar_Context": {"victim"},
		"a2a_sidecar-task":    {"victim"},
		"A2a-Sidecar-Other":   {"x"},
	}

	req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages?stream=false", strings.NewReader(`{"hi":1}`))
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}
	req.Header = spoofHeaders.Clone()
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("POST api.anthropic.com error = %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	closeLog(t, resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK || string(body) != "middleware /v1/messages?stream=false" {
		t.Fatalf("POST api.anthropic.com = %d %q, %v, want 200 %q", resp.StatusCode, string(body), err, "middleware /v1/messages?stream=false")
	}

	status, gotBody, err := get(t, c, "https://api.prefixed.test:8443/v1/models?page=2", spoofHeaders)
	if err != nil || status != http.StatusOK || gotBody != "middleware /proxy/v1/models?page=2" {
		t.Fatalf("GET api.prefixed.test:8443 = %d %q, %v, want 200 %q", status, gotBody, err, "middleware /proxy/v1/models?page=2")
	}

	status, gotBody, err = get(t, c, "https://api.unix.test/v1/ping", nil)
	if err != nil || status != http.StatusOK || gotBody != "middleware /v1/ping" {
		t.Fatalf("GET api.unix.test = %d %q, %v, want 200 %q", status, gotBody, err, "middleware /v1/ping")
	}

	status, gotBody, err = get(t, c, "https://"+credHost+"/repos", http.Header{"Authorization": {"Bearer placeholder"}})
	if err != nil || status != http.StatusOK || gotBody != "Bearer secret" {
		t.Fatalf("GET https://%s/repos = %d %q, %v, want 200 %q", credHost, status, gotBody, err, "Bearer secret")
	}

	if got := connect(t, addr, "denied.test:443"); got != http.StatusForbidden {
		t.Fatalf("CONNECT denied.test:443 status = %d, want %d", got, http.StatusForbidden)
	}

	want := []string{
		`POST /v1/messages?stream=false host=api.anthropic.com context=ctx-1 task=task-of-ctx-1 body={"hi":1}`,
		`GET /proxy/v1/models?page=2 host=api.prefixed.test:8443 context=ctx-1 task=task-of-ctx-1 body=`,
		`GET /v1/ping host=api.unix.test context=ctx-1 task=task-of-ctx-1 body=`,
	}
	if diff := cmp.Diff(want, mw.requests()); diff != "" {
		t.Fatalf("middleware requests wrong result (-want +got) diff = %s", diff)
	}
}

func TestMiddlewareRouterStreamsAnswers(t *testing.T) {
	t.Parallel()
	mw := newFakeMiddleware(t)
	mr, err := egress.NewMiddlewareRouter(egress.MiddlewareConfig{
		Entries: []egress.MiddlewareEntry{{Host: "api.anthropic.com", URL: serveProxy(t, listenTCP(t), mw)}},
	})
	if err != nil {
		t.Fatalf("egress.NewMiddlewareRouter() error = %v", err)
	}
	t.Cleanup(mr.CloseIdleConnections)
	ep, addr := openEndpoint(t, newRouterBoundary(t, mr), "ctx-1")
	c := client(t, addr, ep.CA())

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
	close(mw.next)
	rest, err := io.ReadAll(br)
	if err != nil || string(rest) != "\ndata: two\n\n" {
		t.Fatalf("rest of the stream = %q, %v, want the second event", rest, err)
	}
}

func TestMiddlewareRouterRefusesUpgrade(t *testing.T) {
	t.Parallel()
	mw := newFakeMiddleware(t)
	mr, err := egress.NewMiddlewareRouter(egress.MiddlewareConfig{
		Entries: []egress.MiddlewareEntry{{Host: "api.anthropic.com", URL: serveProxy(t, listenTCP(t), mw)}},
	})
	if err != nil {
		t.Fatalf("egress.NewMiddlewareRouter() error = %v", err)
	}
	t.Cleanup(mr.CloseIdleConnections)
	ep, addr := openEndpoint(t, newRouterBoundary(t, mr), "ctx-1")
	c := client(t, addr, ep.CA())

	status, _, err := get(t, c, "https://api.anthropic.com/ws", http.Header{"Connection": {"Upgrade"}, "Upgrade": {"websocket"}})
	if err != nil || status != http.StatusNotImplemented {
		t.Fatalf("GET upgrade = %d, %v, want %d", status, err, http.StatusNotImplemented)
	}
	if len(mw.requests()) != 0 {
		t.Fatalf("middleware saw %v on upgrade, want none", mw.requests())
	}
}

func TestMiddlewareRouterCAConstraints(t *testing.T) {
	t.Parallel()
	policyRouter, err := (egress.Policy{Mode: egress.ModeAllowlist}).Router(egress.Header{
		Host:  "api.github.com",
		Name:  "Authorization",
		Value: "Bearer secret",
	})
	if err != nil {
		t.Fatalf("Policy.Router() error = %v", err)
	}
	unconstrainedNext := egress.RouterFunc(func(context.Context, egress.Flow) (egress.Route, error) {
		return egress.Direct(), nil
	})
	tests := []struct {
		name        string
		next        egress.Router
		wantDomains []string
	}{
		{
			name:        "nil next constrains CA to middleware hosts",
			next:        nil,
			wantDomains: []string{"api.anthropic.com"},
		},
		{
			name:        "policy next constrains CA to middleware and credential hosts",
			next:        policyRouter,
			wantDomains: []string{"api.anthropic.com", "api.github.com"},
		},
		{
			name:        "unconstrained next leaves CA unconstrained",
			next:        unconstrainedNext,
			wantDomains: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mr, err := egress.NewMiddlewareRouter(egress.MiddlewareConfig{
				Entries: []egress.MiddlewareEntry{{Host: "api.anthropic.com", URL: "http://127.0.0.1:9000"}},
				Next:    tc.next,
			})
			if err != nil {
				t.Fatalf("egress.NewMiddlewareRouter() error = %v", err)
			}
			t.Cleanup(mr.CloseIdleConnections)
			ep, _ := openEndpoint(t, newRouterBoundary(t, mr), "ctx-1")
			cert := parseCACert(t, ep.CA())
			if diff := cmp.Diff(tc.wantDomains, cert.PermittedDNSDomains); diff != "" {
				t.Fatalf("Endpoint.CA() PermittedDNSDomains wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}
