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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/googleauth"
	"github.com/a2aproject/a2a-serve/internal/googleauth/googletest"
)

const sharedClientID = "shared-app"

// publicNet is a public URL on loopback.
func publicNet() *fakeProvider {
	return &fakeProvider{net: &fakeNet{agents: map[string]string{}}, name: "coder"}
}

func startSignInServer(t *testing.T, g *googletest.Google, policy agentdir.PolicyConfig) *testServer {
	t.Helper()
	endpoints := g.Endpoints()
	return startServer(t, serverOpts{
		google:         &endpoints,
		sharedClientID: sharedClientID,
		secrets:        agentdir.Secrets{"GOOGLE_CLIENT_SECRET": g.Secret},
		public:         publicNet(),
		configure: func(c *agentdir.Config) {
			c.Policy = policy
			c.Auth = agentdir.AuthConfig{GoogleClientID: "owner-app", GoogleClientSecret: "secret:GOOGLE_CLIENT_SECRET"}
		},
	})
}

func getJSON(t *testing.T, u string, out any) {
	t.Helper()
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("GET %s error = %v", u, err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("closing: %v", err)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %s", u, resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("decoding %s: %v", u, err)
	}
}

func postToken(t *testing.T, ts *testServer, form url.Values) (int, map[string]string) {
	t.Helper()
	resp, err := http.PostForm(ts.publicURL()+tokenPath, form)
	if err != nil {
		t.Fatalf("POST %s error = %v", tokenPath, err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("closing: %v", err)
		}
	}()
	out := map[string]string{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decoding the token response: %v", err)
	}
	return resp.StatusCode, out
}

// signIn runs the sign-in of a native client and returns the parameters its
// loopback redirect got.
func signIn(t *testing.T, ts *testServer, verifier string) (url.Values, string) {
	t.Helper()
	var got url.Values
	client := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r.URL.Query() }))
	t.Cleanup(client.Close)
	redirect := client.URL + "/callback"
	var meta map[string]any
	getJSON(t, ts.publicURL()+authServerPath, &meta)
	q := url.Values{
		"response_type": {"code"}, "client_id": {"a2a-cli"}, "redirect_uri": {redirect}, "state": {"s1"},
		"code_challenge": {googleauth.Challenge(verifier)}, "code_challenge_method": {"S256"},
	}
	resp, err := http.Get(meta["authorization_endpoint"].(string) + "?" + q.Encode())
	if err != nil {
		t.Fatalf("GET authorize error = %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatalf("the sign-in didn't reach the client's redirect (status %s)", resp.Status)
	}
	return got, redirect
}

func TestGoogleSignIn(t *testing.T) {
	t.Parallel()
	g := googletest.New(t, googletest.Account{Email: "Mom@example.org", Verified: true})
	ts := startSignInServer(t, g, agentdir.PolicyConfig{AllowEmails: []string{"mom@example.org", "*@example.com"}})
	ctx := context.Background()

	resp, err := http.Post(ts.publicURL()+"/", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	wantHeader := `resource_metadata="` + ts.publicURL() + protectedResourcePath + `"`
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(resp.Header.Get("WWW-Authenticate"), wantHeader) {
		t.Fatalf("request without a token = %s %q, want 401 pointing to %s", resp.Status, resp.Header.Get("WWW-Authenticate"), protectedResourcePath)
	}
	var resource map[string]any
	getJSON(t, ts.publicURL()+protectedResourcePath, &resource)
	if resource["resource"] != ts.publicURL() {
		t.Fatalf("protected resource metadata = %v, want the agent's URL", resource)
	}

	got, redirect := signIn(t, ts, "verifier-0123456789-0123456789-0123456789")
	if got.Get("state") != "s1" || got.Get("code") == "" {
		t.Fatalf("client redirect = %v, want a code and the client's state", got)
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {got.Get("code")}, "redirect_uri": {redirect}, "client_id": {"a2a-cli"}, "code_verifier": {"wrong"}}
	if status, _ := postToken(t, ts, form); status != http.StatusBadRequest {
		t.Fatalf("token with a wrong verifier = %d, want %d", status, http.StatusBadRequest)
	}
	got, redirect = signIn(t, ts, "verifier-0123456789-0123456789-0123456789")
	form = url.Values{"grant_type": {"authorization_code"}, "code": {got.Get("code")}, "redirect_uri": {redirect}, "client_id": {"a2a-cli"}, "code_verifier": {"verifier-0123456789-0123456789-0123456789"}}
	status, tok := postToken(t, ts, form)
	if status != http.StatusOK || !strings.HasPrefix(tok["access_token"], "a2p_") {
		t.Fatalf("token = %d %v, want an access token", status, tok)
	}

	c := ts.client(tok["access_token"])
	task := send(t, c, userText("hello", "", ""))
	wantResult(t, task, a2a.TaskStateCompleted, "Hello from fakeacp.")

	if err := ts.srv.updateConfig(func(c *agentdir.Config) error {
		c.Policy.AllowEmails = []string{"*@example.com"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SendMessage(ctx, &a2a.SendMessageRequest{Message: userText("hello", "", "")}); err == nil {
		t.Fatalf("SendMessage() after removing the email from the allowlist error = nil, want error")
	}
}

func TestGoogleSignInRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		account googletest.Account
	}{
		{name: "email not allowed", account: googletest.Account{Email: "eve@example.net", Verified: true}},
		{name: "personal account with an allowed domain", account: googletest.Account{Email: "ben@example.com", Verified: true}},
		{name: "unverified email", account: googletest.Account{Email: "mom@example.org"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := googletest.New(t, tc.account)
			ts := startSignInServer(t, g, agentdir.PolicyConfig{AllowEmails: []string{"mom@example.org", "*@example.com"}})
			got, _ := signIn(t, ts, "verifier-0123456789-0123456789-0123456789")
			if got.Get("error") != "access_denied" || got.Get("code") != "" {
				t.Fatalf("client redirect = %v, want access_denied", got)
			}
		})
	}
}

func TestGoogleTokenExchange(t *testing.T) {
	t.Parallel()
	g := googletest.New(t, googletest.Account{Email: "ben@example.com", Verified: true, HostedDomain: "example.com"})
	ts := startSignInServer(t, g, agentdir.PolicyConfig{AllowEmails: []string{"*@example.com"}})
	exchange := func(idToken string) (int, map[string]string) {
		return postToken(t, ts, url.Values{"grant_type": {grantTokenExchange}, "subject_token": {idToken}, "subject_token_type": {tokenTypeIDToken}})
	}
	tests := []struct {
		name       string
		token      string
		wantStatus int
	}{
		{name: "bound to this agent", token: g.IDToken(sharedClientID, googleauth.URLBinding(ts.publicURL())), wantStatus: http.StatusOK},
		{name: "bound to another agent", token: g.IDToken(sharedClientID, googleauth.URLBinding("https://other.example.com")), wantStatus: http.StatusBadRequest},
		{name: "another app", token: g.IDToken("someone-else", googleauth.URLBinding(ts.publicURL())), wantStatus: http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status, out := exchange(tc.token)
			if status != tc.wantStatus {
				t.Fatalf("token exchange = %d %v, want %d", status, out, tc.wantStatus)
			}
		})
	}
}

func TestSignInOff(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{public: publicNet()})
	if got := statusOf(t, http.MethodGet, ts.publicURL()+authServerPath, "", nil); got != http.StatusNotFound {
		t.Fatalf("GET %s without an allowlist = %d, want 404", authServerPath, got)
	}
}

func TestSignInOnlyOnPublicURL(t *testing.T) {
	t.Parallel()
	g := googletest.New(t, googletest.Account{Email: "mom@example.org", Verified: true})
	endpoints := g.Endpoints()
	ts := startServer(t, serverOpts{
		google:  &endpoints,
		private: &fakeProvider{net: &fakeNet{agents: map[string]string{}}, name: "coder", unidentified: true},
		public:  publicNet(),
		configure: func(c *agentdir.Config) {
			c.Policy.AllowEmails = []string{"mom@example.org"}
		},
	})
	_, private, public := ts.srv.URLs()
	tests := []struct {
		name string
		url  string
		want int
	}{
		{name: "public", url: public, want: http.StatusOK},
		{name: "local", url: ts.localURL(), want: http.StatusUnauthorized},
		{name: "private", url: private, want: http.StatusUnauthorized},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := statusOf(t, http.MethodGet, tc.url+authServerPath, "", nil); got != tc.want {
				t.Fatalf("GET %s on the %s URL = %d, want %d", authServerPath, tc.name, got, tc.want)
			}
			resp, err := http.Post(tc.url+"/", "application/json", strings.NewReader(listTasksRPC))
			if err != nil {
				t.Fatal(err)
			}
			if err := resp.Body.Close(); err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(resp.Header.Get("WWW-Authenticate"), "resource_metadata"); got != (tc.name == "public") {
				t.Fatalf("401 on the %s URL points to sign-in = %v, want %v", tc.name, got, tc.name == "public")
			}
		})
	}
}

func TestSignInForOwnerAndAllowedMembers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		policy agentdir.PolicyConfig
	}{
		{name: "owner", policy: agentdir.PolicyConfig{Owner: []string{"email:mom@example.org"}}},
		{name: "allowed", policy: agentdir.PolicyConfig{Allow: []string{"email:mom@example.org"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := googletest.New(t, googletest.Account{Email: "mom@example.org", Verified: true})
			ts := startSignInServer(t, g, tc.policy)
			if got, _ := signIn(t, ts, "verifier-0123456789-0123456789-0123456789"); got.Get("code") == "" {
				t.Fatalf("client redirect = %v, want a code", got)
			}
		})
	}
}
