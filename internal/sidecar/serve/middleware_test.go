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

package serve

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/core/egress"
)

func TestResolveEgressMiddleware(t *testing.T) {
	t.Parallel()
	env := map[string]string{
		"OPENAI_KEY": "sk-real",
		"USER_ENV":   "from-secret",
	}
	cfg := Config{
		Agent:     "opencode",
		Workspace: "memory",
		Sandbox:   SandboxDocker,
		Egress:    "allowlist",
		Env:       []string{"GEMINI_API_KEY=custom"},
		Secrets:   []string{"USER_ENV"},
		ModelKeys: []string{"openai=OPENAI_KEY"},
		EgressMiddleware: []string{
			"api.anthropic.com=http://127.0.0.1:9000/anthropic",
			"generativelanguage.googleapis.com=http://127.0.0.1:9000/gemini",
			"custom.internal=unix:///run/host/custom.sock",
		},
		Getenv: func(name string) string { return env[name] },
	}

	got, err := cfg.resolve(context.Background())
	if err != nil {
		t.Fatalf("Config.resolve() error = %v", err)
	}

	wantCreds := []egress.Credential{
		egress.ModelKey{Provider: "openai", Key: "sk-real"},
	}
	if diff := cmp.Diff(wantCreds, got.credentials); diff != "" {
		t.Fatalf("Config.resolve() credentials wrong result (-want +got) diff = %s", diff)
	}

	wantMW := []egress.MiddlewareEntry{
		{Host: "api.anthropic.com", URL: "http://127.0.0.1:9000/anthropic"},
		{Host: "generativelanguage.googleapis.com", URL: "http://127.0.0.1:9000/gemini"},
		{Host: "custom.internal", URL: "unix:///run/host/custom.sock"},
	}
	if diff := cmp.Diff(wantMW, got.egressMiddleware); diff != "" {
		t.Fatalf("Config.resolve() egressMiddleware wrong result (-want +got) diff = %s", diff)
	}

	wantEnv := []string{
		"ANTHROPIC_API_KEY=sk-ant-api03-a2a-serve-placeholder",
		"GEMINI_API_KEY=custom",
		"USER_ENV=from-secret",
	}
	if diff := cmp.Diff(wantEnv, got.env); diff != "" {
		t.Fatalf("Config.resolve() env wrong result (-want +got) diff = %s", diff)
	}
}

func TestNewBoundaryComposesMiddlewareAndModelKeys(t *testing.T) {
	t.Parallel()
	var gotReq string
	mwSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotReq = fmt.Sprintf("%s %s host=%s ctx=%s task=%s",
			r.Method, r.RequestURI, r.Host,
			r.Header.Get(egress.ProxyHeaderContext),
			r.Header.Get(egress.ProxyHeaderTask))
		if _, err := io.WriteString(w, "ok-from-mw"); err != nil {
			t.Errorf("writing middleware answer error = %v", err)
		}
	}))
	t.Cleanup(mwSrv.Close)

	cfg := Config{
		Agent:            "opencode",
		Workspace:        "memory",
		Sandbox:          SandboxDocker,
		Egress:           "allowlist",
		ModelKeys:        []string{"openai=OPENAI_KEY"},
		EgressMiddleware: []string{"api.anthropic.com=" + mwSrv.URL + "/v2"},
		Getenv:           func(name string) string { return map[string]string{"OPENAI_KEY": "sk-real"}[name] },
	}
	set, err := cfg.resolve(context.Background())
	if err != nil {
		t.Fatalf("Config.resolve() error = %v", err)
	}

	tr := newTurns()
	endTurn := tr.turn(context.Background(), &a2asrv.ExecutorContext{ContextID: "ctx-42", TaskID: "task-99"})
	defer endTurn()

	b, proxy, mw, err := newBoundary(set, slog.New(slog.DiscardHandler), tr, nil)
	if err != nil {
		t.Fatalf("newBoundary() error = %v", err)
	}
	if proxy != nil {
		t.Fatalf("newBoundary() proxy = %v, want nil", proxy)
	}
	if mw == nil {
		t.Fatalf("newBoundary() middleware = nil, want non-nil")
	}
	t.Cleanup(func() {
		mw.CloseIdleConnections()
		if err := b.Close(); err != nil {
			t.Errorf("Boundary.Close() error = %v", err)
		}
	})

	ep, err := b.Open("test", "ctx-42", set.credentials...)
	if err != nil {
		t.Fatalf("Boundary.Open() error = %v", err)
	}
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

	block, _ := pem.Decode(ep.CA())
	if block == nil {
		t.Fatalf("Endpoint.CA() = nil, want a CA certificate")
	}
	caCert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("x509.ParseCertificate() error = %v", err)
	}
	wantDomains := []string{"api.openai.com", "api.anthropic.com"}
	if diff := cmp.Diff(wantDomains, caCert.PermittedDNSDomains); diff != "" {
		t.Fatalf("Endpoint.CA() PermittedDNSDomains wrong result (-want +got) diff = %s", diff)
	}

	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ep.CA())
	addr := ln.Addr().String()
	httpClient := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, target string) (net.Conn, error) {
				var d net.Dialer
				c, err := d.DialContext(ctx, "tcp", addr)
				if err != nil {
					return nil, err
				}
				if _, err := fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
					return nil, errors.Join(err, c.Close())
				}
				br := bufio.NewReader(c)
				resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
				if err != nil {
					return nil, errors.Join(err, c.Close())
				}
				if resp.StatusCode != http.StatusOK {
					return nil, errors.Join(fmt.Errorf("CONNECT %s status = %d", target, resp.StatusCode), c.Close())
				}
				return &bufferedConn{Conn: c, r: br}, nil
			},
			TLSClientConfig: &tls.Config{RootCAs: roots},
		},
	}

	resp, err := httpClient.Get("https://api.anthropic.com/v1/models?limit=5")
	if err != nil {
		t.Fatalf("GET https://api.anthropic.com/v1/models error = %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	if cerr := resp.Body.Close(); cerr != nil {
		t.Errorf("closing response body error = %v", cerr)
	}
	if err != nil || resp.StatusCode != http.StatusOK || string(body) != "ok-from-mw" {
		t.Fatalf("GET https://api.anthropic.com/v1/models = %d %q, %v, want 200 %q", resp.StatusCode, string(body), err, "ok-from-mw")
	}
	wantReq := "GET /v2/v1/models?limit=5 host=api.anthropic.com ctx=ctx-42 task=task-99"
	if gotReq != wantReq {
		t.Fatalf("middleware request = %q, want %q", gotReq, wantReq)
	}

	if _, err := httpClient.Get("https://blocked.test/"); err == nil {
		t.Fatalf("GET https://blocked.test/ error = nil, want refusal")
	}
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }
