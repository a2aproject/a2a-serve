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
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/sandbox/egress"
)

// fakeInterceptor decides connections by host from actions, answers the
// requests to answer itself, and sets the header X-Other on the
// others. It records what it saw.
type fakeInterceptor struct {
	actions map[string]ConnectAction
	answer  string

	mu       sync.Mutex
	connects []string
	requests []string
}

func (f *fakeInterceptor) Connect(_ context.Context, host string, port uint16) ConnectAction {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connects = append(f.connects, net.JoinHostPort(host, strconv.Itoa(int(port))))
	return f.actions[host]
}

func (f *fakeInterceptor) Request(_ context.Context, r *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.requests = append(f.requests, r.Method+" "+r.URL.String()+" "+string(body))
	f.mu.Unlock()
	if r.URL.Hostname() == f.answer {
		return &http.Response{StatusCode: http.StatusTeapot, Header: http.Header{"X-Answer": {"virtual"}}, Body: io.NopCloser(strings.NewReader("from code"))}, nil
	}
	r.Header.Set("X-Other", "1")
	return nil, nil
}

func (f *fakeInterceptor) seen() ([]string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.connects...), append([]string(nil), f.requests...)
}

func TestBoundaryInterceptorDecidesConnections(t *testing.T) {
	t.Parallel()
	echo := echoServer(t)
	port := strconv.Itoa(int(echo.Port()))
	tests := []struct {
		name   string
		action ConnectAction
		allow  []string
		want   int
	}{
		{name: "deny overrides a rule", action: ConnectDeny, allow: []string{"127.0.0.1"}, want: http.StatusForbidden},
		{name: "policy decides", action: ConnectPolicy, allow: []string{"127.0.0.1"}, want: http.StatusOK},
		{name: "allow still refuses loopback", action: ConnectAllow, want: http.StatusForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ic := &fakeInterceptor{actions: map[string]ConnectAction{"127.0.0.1": tc.action}}
			_, sock := listen(t, BoundaryConfig{
				Policy:      func() egress.Policy { return mustPolicy(t, egress.ModeAllowlist, tc.allow...) },
				Log:         slog.New(&logRecords{}),
				Interceptor: ic,
			})
			_, _, resp := connect(t, sock, echo.String())
			if resp.StatusCode != tc.want {
				t.Fatalf("CONNECT %s status = %d, want %d", echo, resp.StatusCode, tc.want)
			}
			connects, _ := ic.seen()
			if diff := cmp.Diff([]string{"127.0.0.1:" + port}, connects); diff != "" {
				t.Fatalf("Interceptor.Connect() calls wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestBoundaryInterceptorInspectsRequests(t *testing.T) {
	t.Parallel()
	upstreamPort, roots := injectUpstream(t)
	ca, err := NewCA([]string{injectHost, "tools.internal"})
	if err != nil {
		t.Fatalf("NewCA() error = %v", err)
	}
	ic := &fakeInterceptor{
		actions: map[string]ConnectAction{injectHost: ConnectInspect, "tools.internal": ConnectInspect},
		answer:  "tools.internal",
	}
	_, sock := listen(t, BoundaryConfig{
		Policy:        func() egress.Policy { return mustPolicy(t, egress.ModeAllowlist, "127.0.0.1") },
		Resolver:      fakeResolver{injectHost: addrs("127.0.0.1")},
		Log:           slog.New(&logRecords{}),
		CA:            ca,
		UpstreamRoots: roots,
		Interceptor:   ic,
	})
	trusted := x509.NewCertPool()
	trusted.AppendCertsFromPEM(ca.PEM())
	tr := &http.Transport{
		DialContext:     func(ctx context.Context, _, addr string) (net.Conn, error) { return dialTunnel(ctx, sock, addr) },
		TLSClientConfig: &tls.Config{RootCAs: trusted},
	}
	t.Cleanup(tr.CloseIdleConnections)
	client := &http.Client{Transport: tr, Timeout: 10 * time.Second}

	resp, err := client.Post("https://tools.internal/call", "text/plain", strings.NewReader("hi"))
	if err != nil {
		t.Fatalf("Client.Post(tools.internal) error = %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusTeapot || string(body) != "from code" || resp.Header.Get("X-Answer") != "virtual" {
		t.Fatalf("Client.Post(tools.internal) = %d %q %v, want 418 %q", resp.StatusCode, body, resp.Header, "from code")
	}

	url := "https://" + injectHost + ":" + strconv.Itoa(int(upstreamPort)) + "/v1/models?x=1"
	status, _, got := send(t, client, http.MethodPost, url, http.Header{})
	if status != http.StatusOK {
		t.Fatalf("POST %s status = %d, want 200", url, status)
	}
	if got.Headers["X-Other"] != "1" || got.Path != "/v1/models" {
		t.Fatalf("upstream saw %+v, want X-Other: 1 on /v1/models", got)
	}
	_, requests := ic.seen()
	want := []string{"POST https://tools.internal/call hi", "POST " + url + " {}"}
	if diff := cmp.Diff(want, requests); diff != "" {
		t.Fatalf("Interceptor.Request() calls wrong result (-want +got) diff = %s", diff)
	}
}

func TestBoundaryInterceptorInspectsPlainHTTP(t *testing.T) {
	t.Parallel()
	ic := &fakeInterceptor{actions: map[string]ConnectAction{"tools.internal": ConnectInspect}, answer: "tools.internal"}
	_, sock := listen(t, BoundaryConfig{
		Policy:      func() egress.Policy { return mustPolicy(t, egress.ModeAllowlist) },
		Log:         slog.New(&logRecords{}),
		Interceptor: ic,
	})
	tr := &http.Transport{DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) { return dialTunnel(ctx, sock, addr) }}
	t.Cleanup(tr.CloseIdleConnections)
	resp, err := (&http.Client{Transport: tr, Timeout: 10 * time.Second}).Get("http://tools.internal/mcp")
	if err != nil {
		t.Fatalf("Client.Get() error = %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusTeapot {
		t.Fatalf("Client.Get() status = %d, want 418", resp.StatusCode)
	}
}
