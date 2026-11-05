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

package front_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentmesh"
	"github.com/a2aproject/a2a-serve/internal/relay/front"
)

var info = agentmesh.Info{A2AServe: "v1.0.0", AgentMesh: "v0.2.0"}

// upstream fakes agentmesh-one: it answers with the method and path it
// got, and echoes an upgraded connection.
func upstream(t *testing.T) *url.URL {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") == "websocket" {
			conn, rw, err := http.NewResponseController(w).Hijack()
			if err != nil {
				t.Errorf("Hijack() error = %v", err)
				return
			}
			defer func() {
				if err := conn.Close(); err != nil {
					t.Logf("closing: %v", err)
				}
			}()
			if _, err := rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"); err != nil {
				return
			}
			if err := rw.Flush(); err != nil {
				return
			}
			if _, err := io.Copy(conn, rw); err != nil {
				return
			}
			return
		}
		if _, err := fmt.Fprintf(w, "mesh %s %s from %s", r.Method, r.URL.Path, r.Header.Get("X-Forwarded-For")); err != nil {
			t.Errorf("writing: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func newFront(t *testing.T) (*front.Front, *httptest.Server) {
	t.Helper()
	control := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := fmt.Fprintf(w, "control %s", r.URL.Path); err != nil {
			t.Errorf("writing: %v", err)
		}
	})
	f := front.New(info, control)
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func do(t *testing.T, method, rawURL string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatalf("%s %s error = %v", method, rawURL, err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Error(err)
		}
	}()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

func TestFrontRoutes(t *testing.T) {
	t.Parallel()
	f, srv := newFront(t)
	f.Open(upstream(t))
	tests := []struct {
		method, path string
		wantStatus   int
		wantPrefix   string
	}{
		{method: "GET", path: "/info", wantStatus: 200, wantPrefix: "mesh GET /info from 127.0.0.1"},
		{method: "POST", path: "/register", wantStatus: 200, wantPrefix: "mesh POST /register"},
		{method: "GET", path: "/policies", wantStatus: 200, wantPrefix: "mesh GET /policies"},
		{method: "GET", path: "/ingress/12D3/a2a/coder", wantStatus: 200, wantPrefix: "mesh GET /ingress/12D3/a2a/coder"},
		{method: "GET", path: "/a2a-serve/control/status", wantStatus: 200, wantPrefix: "control /status"},
		{method: "POST", path: "/policies", wantStatus: 404},
		{method: "PUT", path: "/policies", wantStatus: 404},
		{method: "GET", path: "/admin/status", wantStatus: 404},
		{method: "POST", path: "/admin/bootstrap-tokens", wantStatus: 404},
		{method: "GET", path: "/admin", wantStatus: 404},
		{method: "GET", path: "/console/", wantStatus: 404},
		{method: "GET", path: "/metrics", wantStatus: 404},
		{method: "GET", path: "/x/../admin/status", wantStatus: 404},
		{method: "GET", path: "//admin/status", wantStatus: 404},
		{method: "GET", path: "/console/%2e%2e/admin/status", wantStatus: 404},
		{method: "GET", path: "/info/%2E%2E/console/", wantStatus: 404},
		{method: "GET", path: "/x/%2e%2e/info", wantStatus: 200, wantPrefix: "mesh GET /info from"},
		{method: "GET", path: "/ingress/12D3/a2a/coder/", wantStatus: 200, wantPrefix: "mesh GET /ingress/12D3/a2a/coder/ from"},
		{method: "CONNECT", path: "/admin/nodes/../../x", wantStatus: 405},
		{method: "TRACE", path: "/info", wantStatus: 405},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			t.Parallel()
			status, body := do(t, tt.method, srv.URL+tt.path)
			if status != tt.wantStatus || !strings.HasPrefix(body, tt.wantPrefix) {
				t.Fatalf("%s %s = %d %q, want %d %q…", tt.method, tt.path, status, body, tt.wantStatus, tt.wantPrefix)
			}
		})
	}
}

func TestFrontReportsVersions(t *testing.T) {
	t.Parallel()
	_, srv := newFront(t)

	status, body := do(t, http.MethodGet, srv.URL+agentmesh.InfoPath)

	if status != http.StatusOK {
		t.Fatalf("GET %s status = %d, want %d", agentmesh.InfoPath, status, http.StatusOK)
	}
	var got agentmesh.Info
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if diff := cmp.Diff(info, got); diff != "" {
		t.Fatalf("GET %s wrong result (-want +got) diff = %s", agentmesh.InfoPath, diff)
	}
}

func TestFrontAnswers503UntilOpenAndAfterClose(t *testing.T) {
	t.Parallel()
	f, srv := newFront(t)

	if status, _ := do(t, http.MethodGet, srv.URL+"/info"); status != http.StatusServiceUnavailable {
		t.Fatalf("GET /info before Open() status = %d, want %d", status, http.StatusServiceUnavailable)
	}
	f.Open(upstream(t))
	if status, _ := do(t, http.MethodGet, srv.URL+"/info"); status != http.StatusOK {
		t.Fatalf("GET /info after Open() status = %d, want %d", status, http.StatusOK)
	}
	f.Close()
	if status, _ := do(t, http.MethodGet, srv.URL+"/info"); status != http.StatusServiceUnavailable {
		t.Fatalf("GET /info after Close() status = %d, want %d", status, http.StatusServiceUnavailable)
	}
}

func TestFrontProxiesWebSockets(t *testing.T) {
	t.Parallel()
	f, srv := newFront(t)
	f.Open(upstream(t))
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(srv.URL, "http://"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}

	if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost: relay\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("reading the upgrade response: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade status = %d, want %d", resp.StatusCode, http.StatusSwitchingProtocols)
	}
	if _, err := io.WriteString(conn, "ping"); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(br, got); err != nil {
		t.Fatalf("reading the echo: %v", err)
	}
	if string(got) != "ping" {
		t.Fatalf("echo = %q, want %q", got, "ping")
	}
}
