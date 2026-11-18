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
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/core/egress"
)

func newBoundary(t *testing.T, policy func() egress.Policy, roots *x509.CertPool) *egress.Boundary {
	t.Helper()
	b, err := egress.NewWithHosts(egress.Config{Policy: policy, Log: slog.New(slog.DiscardHandler)}, roots)
	if err != nil {
		t.Fatalf("egress.New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := b.Close(); err != nil {
			t.Errorf("Boundary.Close() error = %v", err)
		}
	})
	return b
}

// serve serves ep on a TCP listener and returns its address.
func serve(t *testing.T, ep *egress.Endpoint) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- ep.Serve(ln) }()
	t.Cleanup(func() {
		if err := ep.Close(); err != nil {
			t.Errorf("Endpoint.Close() error = %v", err)
		}
		if err := <-done; !errors.Is(err, net.ErrClosed) {
			t.Errorf("Endpoint.Serve() error = %v, want %v", err, net.ErrClosed)
		}
	})
	return ln.Addr().String()
}

// connect sends a CONNECT for target to the proxy at addr and returns the
// status of the answer.
func connect(t *testing.T, addr, target string) int {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dialing the endpoint error = %v", err)
	}
	defer func() {
		if err := c.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("closing the connection error = %v", err)
		}
	}()
	if _, err := fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
		t.Fatalf("writing the CONNECT error = %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("reading the answer to CONNECT %s error = %v", target, err)
	}
	return resp.StatusCode
}

func upstream(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

func TestEndpointDecidesByPolicy(t *testing.T) {
	t.Parallel()
	allowed := upstream(t)
	b := newBoundary(t, func() egress.Policy {
		return egress.Policy{Mode: egress.ModeAllowlist, Allow: []string{"127.0.0.1"}}
	}, nil)
	ep, err := b.Open("test")
	if err != nil {
		t.Fatalf("Boundary.Open() error = %v", err)
	}
	addr := serve(t, ep)

	tests := []struct {
		target string
		want   int
	}{
		{target: allowed, want: http.StatusOK},
		{target: "denied.test:443", want: http.StatusForbidden},
		{target: "missing.test:443", want: http.StatusForbidden},
	}
	for _, tc := range tests {
		if got := connect(t, addr, tc.target); got != tc.want {
			t.Fatalf("CONNECT %s status = %d, want %d", tc.target, got, tc.want)
		}
	}
	want := egress.Status{
		Mode:    egress.ModeAllowlist,
		Refused: 2,
		Recent: []egress.Refusal{
			{Destination: "missing.test:443", Rule: "missing.test:443"},
			{Destination: "denied.test:443", Rule: "denied.test:443"},
		},
	}
	if diff := cmp.Diff(want, b.Status()); diff != "" {
		t.Fatalf("Boundary.Status() wrong result (-want +got) diff = %s", diff)
	}
}

func TestEndpointReportsUnresolvedNames(t *testing.T) {
	t.Parallel()
	b := newBoundary(t, func() egress.Policy { return egress.Policy{} }, nil)
	ep, err := b.Open("test")
	if err != nil {
		t.Fatalf("Boundary.Open() error = %v", err)
	}
	if got := connect(t, serve(t, ep), "missing.test:443"); got != http.StatusBadGateway {
		t.Fatalf("CONNECT missing.test:443 status = %d, want %d", got, http.StatusBadGateway)
	}
	want := egress.Status{Mode: egress.ModeInternet, Refused: 1, Recent: []egress.Refusal{{Destination: "missing.test:443", Unresolved: true}}}
	if diff := cmp.Diff(want, b.Status()); diff != "" {
		t.Fatalf("Boundary.Status() wrong result (-want +got) diff = %s", diff)
	}
}

func TestEndpointAppliesPolicyChanges(t *testing.T) {
	t.Parallel()
	target := upstream(t)
	var mu sync.Mutex
	policy := egress.Policy{Mode: egress.ModeAllowlist}
	b := newBoundary(t, func() egress.Policy {
		mu.Lock()
		defer mu.Unlock()
		return policy
	}, nil)
	ep, err := b.Open("test")
	if err != nil {
		t.Fatalf("Boundary.Open() error = %v", err)
	}
	addr := serve(t, ep)
	if got := connect(t, addr, target); got != http.StatusForbidden {
		t.Fatalf("CONNECT %s status = %d, want %d", target, got, http.StatusForbidden)
	}
	mu.Lock()
	policy.Allow = []string{"127.0.0.1"}
	mu.Unlock()
	if got := connect(t, addr, target); got != http.StatusOK {
		t.Fatalf("CONNECT %s after allowing it status = %d, want %d", target, got, http.StatusOK)
	}
	if got := b.Status().Recent; len(got) != 0 {
		t.Fatalf("Boundary.Status().Recent = %v, want none after the connection succeeded", got)
	}
}

func TestEndpointAddsHeader(t *testing.T) {
	t.Parallel()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.WriteString(w, r.Header.Get("Authorization")); err != nil {
			t.Errorf("writing the answer error = %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	// httptest's certificate names example.com, which the boundary of the
	// test resolves to 127.0.0.1.
	host := net.JoinHostPort("example.com", port)
	b := newBoundary(t, func() egress.Policy {
		return egress.Policy{Mode: egress.ModeAllowlist, Allow: []string{"127.0.0.1"}}
	}, roots)
	ep, err := b.Open("test", egress.Header{Host: host, Name: "Authorization", Value: "Bearer secret"})
	if err != nil {
		t.Fatalf("Boundary.Open() error = %v", err)
	}
	addr := serve(t, ep)
	trusted := x509.NewCertPool()
	if !trusted.AppendCertsFromPEM(ep.CA()) {
		t.Fatalf("Endpoint.CA() = %q, want a PEM certificate", ep.CA())
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		Proxy:           http.ProxyURL(&url.URL{Scheme: "http", Host: addr}),
		TLSClientConfig: &tls.Config{RootCAs: trusted},
	}}
	t.Cleanup(client.CloseIdleConnections)
	req, err := http.NewRequest(http.MethodGet, "https://"+host+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer placeholder")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Client.Do() error = %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("closing the body error = %v", err)
		}
	}()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the answer error = %v", err)
	}
	if got, want := string(body), "Bearer secret"; got != want {
		t.Fatalf("Authorization the host got = %q, want %q", got, want)
	}
	leaf := resp.TLS.PeerCertificates[0]
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: trusted, DNSName: "example.com"}); err != nil {
		t.Fatalf("verifying the boundary's certificate with Endpoint.CA() error = %v", err)
	}
}

func TestEndpointEnv(t *testing.T) {
	t.Parallel()
	b := newBoundary(t, func() egress.Policy { return egress.Policy{} }, nil)
	tests := []struct {
		name  string
		creds []egress.Credential
		want  []string
	}{
		{name: "no credentials"},
		{name: "header", creds: []egress.Credential{egress.Header{Host: "api.github.com", Name: "Authorization", Value: "Bearer t"}}},
		{name: "model keys", creds: []egress.Credential{
			egress.ModelKey{Provider: "anthropic", Key: "k1"},
			egress.ModelKey{Provider: "openai", Key: "k2"},
		}, want: []string{"ANTHROPIC_API_KEY=sk-ant-api03-a2a-serve-placeholder", "OPENAI_API_KEY=sk-proj-a2a-serve-placeholder"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ep, err := b.Open(tc.name, tc.creds...)
			if err != nil {
				t.Fatalf("Boundary.Open() error = %v", err)
			}
			t.Cleanup(func() {
				if err := ep.Close(); err != nil {
					t.Errorf("Endpoint.Close() error = %v", err)
				}
			})
			if diff := cmp.Diff(tc.want, ep.Env()); diff != "" {
				t.Fatalf("Endpoint.Env() wrong result (-want +got) diff = %s", diff)
			}
			if got, want := len(ep.CA()) > 0, len(tc.creds) > 0; got != want {
				t.Fatalf("Endpoint.CA() set = %v, want %v", got, want)
			}
		})
	}
}

func TestOpenRefusesInvalidCredentials(t *testing.T) {
	t.Parallel()
	b := newBoundary(t, func() egress.Policy { return egress.Policy{} }, nil)
	tests := []struct {
		name string
		cred egress.Credential
		want string
	}{
		{name: "unknown provider", cred: egress.ModelKey{Provider: "acme", Key: "k"}, want: "unknown model provider"},
		{name: "IP address", cred: egress.Header{Host: "10.0.0.1", Name: "X-Key", Value: "v"}, want: "IP address"},
		{name: "reserved header", cred: egress.Header{Host: "api.test", Name: "Host", Value: "v"}, want: "can't set the header Host"},
		{name: "line break", cred: egress.Header{Host: "api.test", Name: "X-Key", Value: "a\nb"}, want: "can't carry"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := b.Open(tc.name, tc.cred); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Boundary.Open(%+v) error = %v, want one that says %q", tc.cred, err, tc.want)
			}
		})
	}
}

func TestPolicyValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		policy  egress.Policy
		wantErr bool
	}{
		{name: "empty", policy: egress.Policy{}},
		{name: "allowlist", policy: egress.Policy{Mode: egress.ModeAllowlist, Allow: []string{"*.github.com:443", "192.168.1.0/24", "nas.lan:22"}}},
		{name: "unrestricted", policy: egress.Policy{Mode: egress.ModeUnrestricted}},
		{name: "unknown mode", policy: egress.Policy{Mode: "open"}, wantErr: true},
		{name: "bad rule", policy: egress.Policy{Allow: []string{"nas.lan:http"}}, wantErr: true},
		{name: "tailnet range", policy: egress.Policy{Allow: []string{"100.64.0.0/10"}}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := tc.policy.Validate(); (err != nil) != tc.wantErr {
				t.Fatalf("Policy.Validate() error = %v, want error %v", err, tc.wantErr)
			}
		})
	}
}

func TestNewRefusesInvalidPolicy(t *testing.T) {
	t.Parallel()
	if _, err := egress.New(egress.Config{Policy: func() egress.Policy { return egress.Policy{Mode: "open"} }}); err == nil {
		t.Fatal("egress.New() with an unknown mode error = nil, want one")
	}
	if _, err := egress.New(egress.Config{}); err == nil {
		t.Fatal("egress.New() without a policy error = nil, want one")
	}
}

func TestModesMatchTheRules(t *testing.T) {
	t.Parallel()
	got := []string{string(egress.ModeInternet), string(egress.ModeAllowlist), string(egress.ModeUnrestricted)}
	if diff := cmp.Diff(egress.EngineModes, got); diff != "" {
		t.Fatalf("egress modes wrong result (-want +got) diff = %s", diff)
	}
}
