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
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/googleauth"
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
		{name: "network that admits by sign-in", provider: &fakeProvider{signIn: true}, want: http.StatusForbidden, wantBody: "this agent doesn't accept the machine peer on the private network"},
		{name: "network that admits by sign-in, opened", provider: &fakeProvider{signIn: true}, access: agentdir.AccessEveryone, want: http.StatusOK},
		{name: "caller the network doesn't identify", provider: &fakeProvider{unidentified: true}, want: http.StatusUnauthorized, wantBody: "this agent needs a token"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.provider.net, tc.provider.name = &fakeNet{agents: map[string]string{}}, "coder"
			ts := startServer(t, serverOpts{private: tc.provider, configure: func(c *agentdir.Config) { c.Policy.PrivateAccess = tc.access }})
			_, private, _ := ts.srv.URLs()
			resp, err := http.Post(private+"/", "application/json", strings.NewReader(listTasksRPC))
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

func TestCardDescription(t *testing.T) {
	t.Parallel()
	tests := []struct {
		harness string
		want    string
	}{
		{harness: "acp", want: "coder is an acp coding agent reached through `a2a-serve`."},
		{harness: "opencode", want: "coder is an opencode coding agent reached through `a2a-serve`."},
		{harness: "claude", want: "coder is a claude coding agent reached through `a2a-serve`."},
	}
	for _, tc := range tests {
		t.Run(tc.harness, func(t *testing.T) {
			t.Parallel()
			s := &Server{}
			s.rt.Store(&runtimeConfig{cfg: &agentdir.Config{Name: "coder", Harness: tc.harness}})
			if got := s.card(network.ListenerPrivate, "http://127.0.0.1:9000").Description; got != tc.want {
				t.Fatalf("Server.card().Description = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCardPairingScheme(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		kind   string
		policy agentdir.PolicyConfig
		signIn bool
		want   bool
	}{
		{name: "local", kind: network.ListenerLocal, want: true},
		{name: "private open to everyone", kind: network.ListenerPrivate},
		{name: "private for pairings", kind: network.ListenerPrivate, policy: agentdir.PolicyConfig{PrivateAccess: agentdir.AccessPaired}, want: true},
		{name: "private network that admits by sign-in", kind: network.ListenerPrivate, signIn: true, want: true},
		{name: "public for pairings", kind: network.ListenerPublic, want: true},
		{name: "public open to anyone", kind: network.ListenerPublic, policy: agentdir.PolicyConfig{PublicAccess: agentdir.AccessAnyone}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := &Server{private: &fakeProvider{signIn: tc.signIn}}
			s.oauth = newOAuthServer(s, googleauth.Google, "")
			s.rt.Store(&runtimeConfig{cfg: &agentdir.Config{Name: "coder", Harness: "opencode", Policy: tc.policy}})
			card := s.card(tc.kind, "http://127.0.0.1:9000")
			_, got := card.SecuritySchemes["pairing"]
			if got != tc.want || (len(card.SecurityRequirements) > 0) != tc.want {
				t.Fatalf("Server.card(%q) advertises the pairing scheme = %v, want %v", tc.kind, got, tc.want)
			}
		})
	}
}

func TestCardSignInScheme(t *testing.T) {
	t.Parallel()
	const base = "https://coder.example.com"
	allowEmail := agentdir.PolicyConfig{AllowEmails: []string{"mom@example.org"}}
	ownerApp := agentdir.AuthConfig{GoogleClientID: "owner-app", GoogleClientSecret: "secret"}
	tests := []struct {
		name           string
		kind           string
		policy         agentdir.PolicyConfig
		auth           agentdir.AuthConfig
		sharedClientID string
		want           a2a.SecurityScheme
	}{
		{
			name: "shared app", kind: network.ListenerPublic, policy: allowEmail, sharedClientID: "shared-app",
			want: a2a.OpenIDConnectSecurityScheme{OpenIDConnectURL: googleauth.OpenIDConfiguration},
		},
		{
			name: "owner's app", kind: network.ListenerPublic, policy: allowEmail, auth: ownerApp, sharedClientID: "shared-app",
			want: a2a.OAuth2SecurityScheme{
				Oauth2MetadataURL: base + authServerPath,
				Flows:             a2a.AuthorizationCodeOAuthFlow{AuthorizationURL: base + authorizePath, TokenURL: base + tokenPath, Scopes: map[string]string{}, PKCERequired: true},
			},
		},
		{name: "no allowed emails", kind: network.ListenerPublic, sharedClientID: "shared-app"},
		{name: "no shared app", kind: network.ListenerPublic, policy: allowEmail},
		{name: "private listener", kind: network.ListenerPrivate, policy: agentdir.PolicyConfig{AllowEmails: allowEmail.AllowEmails, PrivateAccess: agentdir.AccessPaired}, sharedClientID: "shared-app"},
		{name: "public open to anyone", kind: network.ListenerPublic, policy: agentdir.PolicyConfig{AllowEmails: allowEmail.AllowEmails, PublicAccess: agentdir.AccessAnyone}, sharedClientID: "shared-app"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := &Server{private: &fakeProvider{}}
			s.oauth = newOAuthServer(s, googleauth.Google, tc.sharedClientID)
			s.rt.Store(&runtimeConfig{cfg: &agentdir.Config{Name: "coder", Harness: "opencode", Policy: tc.policy, Auth: tc.auth}})
			card := s.card(tc.kind, base)
			got := card.SecuritySchemes["google"]
			if diff := cmp.Diff(tc.want, got, cmpopts.IgnoreFields(a2a.OpenIDConnectSecurityScheme{}, "Description"), cmpopts.IgnoreFields(a2a.OAuth2SecurityScheme{}, "Description")); diff != "" {
				t.Fatalf("Server.card(%q) wrong google scheme (-want +got) diff = %s", tc.kind, diff)
			}
			if tc.want == nil {
				return
			}
			wantReqs := a2a.SecurityRequirementsOptions{{"pairing": {}}, {"google": {}}}
			if diff := cmp.Diff(wantReqs, card.SecurityRequirements); diff != "" {
				t.Fatalf("Server.card(%q) wrong security requirements (-want +got) diff = %s", tc.kind, diff)
			}
		})
	}
}

func TestOversizedA2ABody(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	bearer := "Bearer " + ts.pair("me", auth.RoleOwner)
	u, err := url.Parse(ts.localURL())
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/", "/message:send"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			conn, err := net.Dial("tcp", u.Host)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := conn.Close(); err != nil {
					t.Errorf("closing the connection: %v", err)
				}
			}()
			req := fmt.Sprintf("POST %s HTTP/1.1\r\nHost: %s\r\nAuthorization: %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n", path, u.Host, bearer, jsonhttp.MaxBody+1)
			if _, err := io.WriteString(conn, req); err != nil {
				t.Fatal(err)
			}
			resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := resp.Body.Close(); err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusRequestEntityTooLarge {
				t.Fatalf("POST %s status = %d, want %d", path, resp.StatusCode, http.StatusRequestEntityTooLarge)
			}
		})
	}
}

func TestPairingBackoffThroughTunnel(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{public: publicNet()})
	ctx := context.Background()
	code, err := controlapi.NewClient(ts.dir).Pair(ctx, controlapi.PairCodeRequest{Name: "anna"})
	if err != nil {
		t.Fatalf("Pair() error = %v", err)
	}
	pair := func(u, code string) int {
		return statusOf(t, http.MethodPost, u+"/pair", `{"code":"`+code+`"}`, http.Header{"Content-Type": {"application/json"}})
	}
	if got := pair(ts.publicURL(), "WRONGCODE"); got != http.StatusForbidden {
		t.Fatalf("pair with a wrong code status = %d, want %d", got, http.StatusForbidden)
	}
	if got := pair(ts.publicURL(), code.Code); got != http.StatusTooManyRequests {
		t.Fatalf("pair through the tunnel during backoff status = %d, want %d", got, http.StatusTooManyRequests)
	}
	if got := pair(ts.localURL(), code.Code); got != http.StatusOK {
		t.Fatalf("pair on the local listener status = %d, want %d", got, http.StatusOK)
	}
}
