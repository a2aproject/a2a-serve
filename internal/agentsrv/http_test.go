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

package agentsrv

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/oauth"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/googleauth"
	"github.com/a2aproject/a2a-serve/internal/httpserver"
	"github.com/a2aproject/a2a-serve/internal/jsonhttp"
	"github.com/a2aproject/a2a-serve/internal/network"
)

// listTasksRPC is a JSON-RPC request any caller may send.
const listTasksRPC = `{"jsonrpc":"2.0","id":1,"method":"ListTasks","params":{}}`

// statusOf sends a request without following redirects and returns its status.
func statusOf(t *testing.T, method, u, body string, header http.Header) int {
	t.Helper()
	req, err := http.NewRequest(method, u, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	maps.Copy(req.Header, header)
	if host := header.Get("Host"); host != "" {
		req.Host = host
	}
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatalf("%s %s error = %v", method, u, err)
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode
}

func TestBrowserRequestChecks(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	addNode(t, ts, "grok", 0)
	bearer := "Bearer " + ts.pair("me", auth.RoleOwner)
	tests := []struct {
		name   string
		method string
		path   string
		header http.Header
		want   int
	}{
		{name: "JSON-RPC as JSON", method: http.MethodPost, path: "/", header: http.Header{"Content-Type": {"application/json"}}, want: http.StatusOK},
		{name: "JSON-RPC as A2A JSON", method: http.MethodPost, path: "/", header: http.Header{"Content-Type": {"application/a2a+json; charset=utf-8"}}, want: http.StatusOK},
		{name: "JSON-RPC as text with A2A-Version", method: http.MethodPost, path: "/", header: http.Header{"Content-Type": {"text/plain"}, "A2a-Version": {"1.0"}}, want: http.StatusOK},
		{name: "JSON-RPC as text", method: http.MethodPost, path: "/", header: http.Header{"Content-Type": {"text/plain"}}, want: http.StatusUnsupportedMediaType},
		{name: "JSON-RPC without a content type", method: http.MethodPost, path: "/", want: http.StatusUnsupportedMediaType},
		{name: "REST as a form", method: http.MethodPost, path: "/message:send", header: http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}, want: http.StatusUnsupportedMediaType},
		{name: "pairing as text", method: http.MethodPost, path: "/pair", header: http.Header{"Content-Type": {"text/plain"}}, want: http.StatusUnsupportedMediaType},
		{name: "node as text", method: http.MethodPost, path: "/n/grok", header: http.Header{"Content-Type": {"text/plain"}}, want: http.StatusUnsupportedMediaType},
		{name: "mailbox as text", method: http.MethodPost, path: "/mailbox/api/tasks/t1/events", header: http.Header{"Content-Type": {"text/plain"}}, want: http.StatusUnsupportedMediaType},
		{name: "same-origin JSON-RPC", method: http.MethodPost, path: "/", header: http.Header{"Content-Type": {"application/json"}, "Sec-Fetch-Site": {"same-origin"}}, want: http.StatusOK},
		{name: "cross-site JSON-RPC", method: http.MethodPost, path: "/", header: http.Header{"Content-Type": {"application/json"}, "Sec-Fetch-Site": {"cross-site"}}, want: http.StatusForbidden},
		{name: "cross-origin JSON-RPC without fetch metadata", method: http.MethodPost, path: "/", header: http.Header{"Content-Type": {"application/json"}, "Origin": {"https://evil.example"}}, want: http.StatusForbidden},
		{name: "cross-site question form", method: http.MethodPost, path: "/hitl/unknown", header: http.Header{"Content-Type": {"application/x-www-form-urlencoded"}, "Sec-Fetch-Site": {"cross-site"}}, want: http.StatusForbidden},
		{name: "cross-site card", method: http.MethodGet, path: "/.well-known/agent-card.json", header: http.Header{"Sec-Fetch-Site": {"cross-site"}}, want: http.StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			header := tc.header.Clone()
			if header == nil {
				header = http.Header{}
			}
			header.Set("Authorization", bearer)
			if got := statusOf(t, tc.method, ts.localURL()+tc.path, listTasksRPC, header); got != tc.want {
				t.Fatalf("%s %s status = %d, want %d", tc.method, tc.path, got, tc.want)
			}
		})
	}
}

func TestCardCORS(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	addNode(t, ts, "grok", 0)
	bearer := "Bearer " + ts.pair("me", auth.RoleOwner)
	preflight := http.Header{"Access-Control-Request-Method": {"GET"}, "Access-Control-Request-Headers": {"a2a-version"}}
	tests := []struct {
		name       string
		method     string
		path       string
		header     http.Header
		wantStatus int
		want       http.Header
	}{
		{name: "card", method: http.MethodGet, path: "/.well-known/agent-card.json", wantStatus: http.StatusOK, want: http.Header{"Access-Control-Allow-Origin": {"*"}}},
		{name: "card at the root", method: http.MethodGet, path: "/", wantStatus: http.StatusOK, want: http.Header{"Access-Control-Allow-Origin": {"*"}}},
		{name: "node card", method: http.MethodGet, path: "/n/grok/.well-known/agent-card.json", header: http.Header{"Authorization": {bearer}}, wantStatus: http.StatusOK, want: http.Header{"Access-Control-Allow-Origin": {"*"}}},
		{
			name: "card preflight", method: http.MethodOptions, path: "/.well-known/agent-card.json", header: preflight, wantStatus: http.StatusNoContent,
			want: http.Header{"Access-Control-Allow-Origin": {"*"}, "Access-Control-Allow-Methods": {"GET, OPTIONS"}, "Access-Control-Allow-Headers": {"*"}, "Access-Control-Max-Age": {"86400"}},
		},
		{
			name: "legacy card preflight", method: http.MethodOptions, path: "/.well-known/agent.json", header: preflight, wantStatus: http.StatusNoContent,
			want: http.Header{"Access-Control-Allow-Origin": {"*"}, "Access-Control-Allow-Methods": {"GET, OPTIONS"}, "Access-Control-Allow-Headers": {"*"}, "Access-Control-Max-Age": {"86400"}},
		},
		{
			name: "node card preflight", method: http.MethodOptions, path: "/n/grok/.well-known/agent-card.json", header: preflight, wantStatus: http.StatusNoContent,
			want: http.Header{"Access-Control-Allow-Origin": {"*"}, "Access-Control-Allow-Methods": {"GET, OPTIONS"}, "Access-Control-Allow-Headers": {"*"}, "Access-Control-Max-Age": {"86400"}},
		},
	}
	corsHeaders := []string{"Access-Control-Allow-Origin", "Access-Control-Allow-Credentials", "Access-Control-Allow-Methods", "Access-Control-Allow-Headers", "Access-Control-Max-Age"}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req, err := http.NewRequest(tc.method, ts.localURL()+tc.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			maps.Copy(req.Header, tc.header)
			req.Header.Set("Origin", "https://inspector.example")
			resp, err := testHTTP.Do(req)
			if err != nil {
				t.Fatalf("%s %s error = %v", tc.method, tc.path, err)
			}
			if _, err := io.Copy(io.Discard, resp.Body); err != nil {
				t.Fatal(err)
			}
			if err := resp.Body.Close(); err != nil {
				t.Fatal(err)
			}

			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("%s %s status = %d, want %d", tc.method, tc.path, resp.StatusCode, tc.wantStatus)
			}
			got := http.Header{}
			for _, k := range corsHeaders {
				if v := resp.Header.Values(k); v != nil {
					got[k] = v
				}
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("%s %s CORS headers wrong result (-want +got) diff = %s", tc.method, tc.path, diff)
			}
		})
	}
}

func TestPrivateNetworkAccess(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		provider *fakeProvider
		access   string
		want     int
		wantBody string
	}{
		{name: "everyone on the private network", provider: &fakeProvider{}, want: http.StatusOK},
		{name: "closed private network", provider: &fakeProvider{}, access: agentdir.AccessPaired, want: http.StatusForbidden, wantBody: "this agent doesn't accept the machine peer on the private network"},
		{name: "caller the network doesn't identify", provider: &fakeProvider{unidentified: true}, want: http.StatusUnauthorized, wantBody: "this agent needs a token"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.provider.net, tc.provider.name = &fakeNet{agents: map[string]string{}}, "coder"
			ts := startServer(t, serverOpts{private: tc.provider, configure: func(c *agentdir.Config) { c.Policy.PrivateAllowIncoming = tc.access }})
			_, private, _ := ts.srv.URLs()
			resp, err := testHTTP.Post(private+"/", "application/json", strings.NewReader(listTasksRPC))
			if err != nil {
				t.Fatalf("POST private error = %v", err)
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if err := resp.Body.Close(); err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != tc.want || !strings.Contains(string(body), tc.wantBody) {
				t.Fatalf("POST private = %d %s, want %d with %q", resp.StatusCode, body, tc.want, tc.wantBody)
			}
		})
	}
}

func TestInspectorOnPrivateListener(t *testing.T) {
	t.Parallel()
	tests := []struct {
		provider string
		want     int
	}{
		{provider: network.ProviderTailscale, want: http.StatusSeeOther},
		{provider: network.ProviderRelay, want: http.StatusNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.provider, func(t *testing.T) {
			t.Parallel()
			p := &fakeProvider{net: &fakeNet{agents: map[string]string{}}, name: "coder", kind: tc.provider}
			ts := startServer(t, serverOpts{private: p})
			_, private, _ := ts.srv.URLs()
			link, err := controlapi.NewClient(ts.dir).InspectorLogin(context.Background())
			if err != nil {
				t.Fatalf("InspectorLogin() error = %v", err)
			}
			if got := statusOf(t, http.MethodGet, private+link.Path, "", nil); got != tc.want {
				t.Fatalf("GET the login link on the private URL of %s = %d, want %d", tc.provider, got, tc.want)
			}
		})
	}
}

func TestUnauthorizedHint(t *testing.T) {
	t.Parallel()
	tests := []struct {
		kind      string
		wantFirst string
	}{
		{kind: network.ListenerLocal, wantFirst: "credential helper"},
		{kind: network.ListenerPrivate, wantFirst: "pairing code"},
	}
	for _, tc := range tests {
		t.Run(tc.kind, func(t *testing.T) {
			t.Parallel()
			got := unauthorizedHint(tc.kind)
			first, _, _ := strings.Cut(got, ";")
			if !strings.Contains(first, tc.wantFirst) || !strings.Contains(got, "credential helper") || !strings.Contains(got, "`a2a-serve pair join <agent-url> <code>`") {
				t.Fatalf("unauthorizedHint(%q) = %q, want both cases, %q first", tc.kind, got, tc.wantFirst)
			}
		})
	}
}

func TestCardSignInScheme(t *testing.T) {
	t.Parallel()
	const base = "https://coder.example.com"
	allowEmail := agentdir.PolicyConfig{AllowEmails: []string{"mom@example.org"}}
	tests := []struct {
		name   string
		kind   string
		policy agentdir.PolicyConfig
		want   bool
	}{
		{name: "public with allowed emails", kind: network.ListenerPublic, policy: allowEmail, want: true},
		{name: "no allowed emails", kind: network.ListenerPublic},
		{name: "private listener", kind: network.ListenerPrivate, policy: agentdir.PolicyConfig{AllowEmails: allowEmail.AllowEmails, PrivateAllowIncoming: agentdir.AccessPaired}},
		{name: "public open to anyone", kind: network.ListenerPublic, policy: agentdir.PolicyConfig{AllowEmails: allowEmail.AllowEmails, PublicAllowIncoming: agentdir.AccessAnyone}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rt := agentdir.NewRuntime(agentdir.Dir{}, &agentdir.Config{Name: "coder", Harness: "opencode", Policy: tc.policy}, nil)
			s := &Server{private: &fakeProvider{}, rt: rt, oauth: oauth.New(oauth.Deps{Google: googleauth.Google, SharedClientID: "shared-app", Runtime: rt})}
			card := s.card(tc.kind, base)
			if _, got := card.SecuritySchemes["google"]; got != tc.want {
				t.Fatalf("Server.card(%q) advertises the google scheme = %v, want %v", tc.kind, got, tc.want)
			}
			if !tc.want {
				return
			}
			wantReqs := a2a.SecurityRequirementsOptions{{"pairing": {}}, {"google": {}}}
			if diff := cmp.Diff(wantReqs, card.SecurityRequirements); diff != "" {
				t.Fatalf("Server.card(%q) wrong security requirements (-want +got) diff = %s", tc.kind, diff)
			}
		})
	}
}

// postOversized declares a body over jsonhttp.MaxBody to u without sending
// it and returns the status of the response.
func postOversized(t *testing.T, u, authorization string) int {
	t.Helper()
	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", parsed.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Errorf("closing the connection: %v", err)
		}
	}()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	req := fmt.Sprintf("POST %s HTTP/1.1\r\nHost: %s\r\nAuthorization: %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n", parsed.RequestURI(), parsed.Host, authorization, jsonhttp.MaxBody+1)
	if _, err := io.WriteString(conn, req); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("POST %s error = %v, want a response", u, err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode
}

func TestOversizedA2ABody(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	addNode(t, ts, "grok", 0)
	bearer := "Bearer " + ts.pair("me", auth.RoleOwner)
	for _, path := range []string{"/", "/message:send", "/n/grok", "/n/grok/message:send"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			if got := postOversized(t, ts.localURL()+path, bearer); got != http.StatusRequestEntityTooLarge {
				t.Fatalf("POST %s status = %d, want %d", path, got, http.StatusRequestEntityTooLarge)
			}
		})
	}
}

func TestPairBodyLimit(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	tests := []struct {
		name    string
		padding int
		want    int
	}{
		{name: "small body", padding: 1 << 10, want: http.StatusForbidden},
		{name: "body over the limit", padding: 4 << 10, want: http.StatusRequestEntityTooLarge},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			body := `{"code":"WRONGCODE","padding":"` + strings.Repeat("a", tc.padding) + `"}`
			if got := statusOf(t, http.MethodPost, ts.localURL()+"/pair", body, http.Header{"Content-Type": {"application/json"}}); got != tc.want {
				t.Fatalf("POST /pair status = %d, want %d", got, tc.want)
			}
		})
	}
}

// stallPair starts a pairing request on a new connection to u that sends
// only the start of its body, and keeps it open until the test ends.
func stallPair(t *testing.T, u string) net.Conn {
	t.Helper()
	return stallPost(t, u, "/pair", nil)
}

// stallPost starts a JSON POST of path with header on a new connection to
// u that sends only the start of its body, and keeps it open until the
// test ends.
func stallPost(t *testing.T, u, path string, header http.Header) net.Conn {
	t.Helper()
	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", parsed.Host)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := conn.Close(); err != nil {
			t.Errorf("closing the connection: %v", err)
		}
	})
	var lines strings.Builder
	if err := header.Write(&lines); err != nil {
		t.Fatal(err)
	}
	req := fmt.Sprintf("POST %s HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: 100\r\n%s\r\n{", path, parsed.Host, lines.String())
	if _, err := io.WriteString(conn, req); err != nil {
		t.Fatal(err)
	}
	return conn
}

// stalledStatus returns the status of the response to a stalled request on
// conn, or 0 when none comes within wait.
func stalledStatus(t *testing.T, conn net.Conn, wait time.Duration) int {
	t.Helper()
	resp, _ := stalledResponse(t, conn, wait)
	if resp == nil {
		return 0
	}
	return resp.StatusCode
}

// stalledResponse returns the response to a stalled request on conn and its
// body, or nil when none comes within wait.
func stalledResponse(t *testing.T, conn net.Conn, wait time.Duration) (*http.Response, string) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(wait)); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return nil, ""
	}
	if err != nil {
		t.Fatalf("reading the response to a stalled request: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the response body to a stalled request: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	return resp, string(body)
}

func TestUnauthenticatedClientLimit(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{timings: func(tm *Timings) { tm.PairReadTimeout = time.Hour }})
	bearer := "Bearer " + ts.pair("me", auth.RoleOwner)
	for range httpserver.MaxUnauthenticatedPerClient {
		stallPair(t, ts.localURL())
	}
	// Until all stalled requests reach the server, more may get in too.
	refused := 0
	for deadline := time.Now().Add(time.Minute); refused == 0 && time.Now().Before(deadline); {
		refused = stalledStatus(t, stallPair(t, ts.localURL()), time.Second)
	}

	hitl := statusOf(t, http.MethodGet, ts.localURL()+"/hitl/unknown", "", nil)
	download := statusOf(t, http.MethodGet, ts.localURL()+"/download/a2a-serve", "", nil)
	owner := statusOf(t, http.MethodPost, ts.localURL()+"/", listTasksRPC, http.Header{"Content-Type": {"application/json"}, "Authorization": {bearer}})

	if refused != http.StatusTooManyRequests {
		t.Fatalf("POST /pair over the client's stalled requests status = %d, want %d", refused, http.StatusTooManyRequests)
	}
	if hitl != http.StatusTooManyRequests {
		t.Fatalf("GET /hitl/unknown while the client's pairing requests stall status = %d, want %d", hitl, http.StatusTooManyRequests)
	}
	if download != http.StatusTooManyRequests {
		t.Fatalf("GET /download/a2a-serve while the client's pairing requests stall status = %d, want %d", download, http.StatusTooManyRequests)
	}
	if owner != http.StatusOK {
		t.Fatalf("POST / of the owner while the client's pairing requests stall status = %d, want %d", owner, http.StatusOK)
	}
}

func TestMembersAndGuestsLeaveRequestsToTheOwner(t *testing.T) {
	t.Parallel()
	ts := startPublicServer(t, nil)
	jsonType := http.Header{"Content-Type": {"application/json"}}
	owner := http.Header{"Authorization": {"Bearer " + ts.pair("me", auth.RoleOwner)}}
	member := http.Header{"Authorization": {"Bearer " + ts.pair("bob", auth.RoleMember)}}
	tests := []struct {
		name    string
		url     string
		caller  http.Header
		held    int
		through map[string]http.Header
	}{
		{name: "members", url: ts.localURL(), caller: member, held: httpserver.MaxMemberRequests, through: map[string]http.Header{"owner": owner}},
		{name: "guests", url: ts.publicURL(), caller: http.Header{}, held: httpserver.MaxGuestRequests, through: map[string]http.Header{"owner": owner, "member": member}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for range tc.held {
				stallPost(t, tc.url, "/", tc.caller)
			}
			// Until all stalled requests reach the server, more may get in too.
			var refused *http.Response
			var body string
			for deadline := time.Now().Add(time.Minute); refused == nil && time.Now().Before(deadline); {
				refused, body = stalledResponse(t, stallPost(t, tc.url, "/", tc.caller), time.Second)
			}

			if refused == nil || refused.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("POST / over the stalled requests of %s = %v, want status %d", tc.name, refused, http.StatusServiceUnavailable)
			}
			if got := refused.Header.Get("Retry-After"); got != "1" {
				t.Fatalf("POST / over the stalled requests of %s Retry-After = %q, want %q", tc.name, got, "1")
			}
			var got rpcErrorResponse
			if err := json.Unmarshal([]byte(body), &got); err != nil {
				t.Fatalf("POST / over the stalled requests of %s body = %s, want JSON: %v", tc.name, body, err)
			}
			want := rpcError{Code: -32603, Message: httpserver.BusyMessage}
			if diff := cmp.Diff(want, got.Error, cmpopts.IgnoreFields(rpcError{}, "Data")); diff != "" {
				t.Fatalf("POST / over the stalled requests of %s wrong error (-want +got) diff = %s", tc.name, diff)
			}
			for who, caller := range tc.through {
				header := caller.Clone()
				maps.Copy(header, jsonType)
				if got := statusOf(t, http.MethodPost, tc.url+"/", listTasksRPC, header); got != http.StatusOK {
					t.Fatalf("POST / of the %s while the requests of %s stall status = %d, want %d", who, tc.name, got, http.StatusOK)
				}
			}
		})
	}
}

func TestOversizedHeader(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})

	got := statusOf(t, http.MethodGet, ts.localURL()+"/healthz", "", http.Header{"X-Padding": {strings.Repeat("a", 512<<10)}})

	if got != http.StatusRequestHeaderFieldsTooLarge {
		t.Fatalf("GET /healthz with a large header status = %d, want %d", got, http.StatusRequestHeaderFieldsTooLarge)
	}
}

func TestPairingThroughTunnel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// peerPerConn identifies each connection through the relay as its
		// own peer; otherwise the relay identifies nobody.
		peerPerConn bool
		wantStatus  int
	}{
		{name: "anonymous callers share a backoff", wantStatus: http.StatusTooManyRequests},
		{name: "identified callers are limited apart", peerPerConn: true, wantStatus: http.StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			relay := &fakeProvider{net: &fakeNet{agents: map[string]string{}}, name: "coder", kind: network.ProviderRelay, unidentified: !tc.peerPerConn, peerPerConn: tc.peerPerConn}
			start := time.Now()
			ts := startServer(t, serverOpts{public: relay, now: func() time.Time { return start }})
			code, err := controlapi.NewClient(ts.dir).Pair(context.Background(), controlapi.PairCodeRequest{Name: "anna"})
			if err != nil {
				t.Fatalf("Pair() error = %v", err)
			}
			pair := func(u, code string) int {
				return statusOf(t, http.MethodPost, u+"/pair", `{"code":"`+code+`"}`, http.Header{"Content-Type": {"application/json"}, "Connection": {"close"}})
			}
			if got := pair(ts.publicURL(), "WRONGCODE"); got != http.StatusForbidden {
				t.Fatalf("pair with a wrong code status = %d, want %d", got, http.StatusForbidden)
			}
			if got := pair(ts.publicURL(), code.Code); got != tc.wantStatus {
				t.Fatalf("pair through the tunnel after a wrong code status = %d, want %d", got, tc.wantStatus)
			}
			if tc.wantStatus == http.StatusOK {
				return
			}
			if got := pair(ts.localURL(), code.Code); got != http.StatusOK {
				t.Fatalf("pair on the local listener status = %d, want %d", got, http.StatusOK)
			}
		})
	}
}
