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

package oauth_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/oauth"
	"github.com/a2aproject/a2a-serve/internal/googleauth"
	"github.com/a2aproject/a2a-serve/internal/googleauth/googletest"
	"github.com/a2aproject/a2a-serve/internal/network"
	"github.com/a2aproject/a2a-serve/internal/store"
)

const (
	sharedClientID = "shared-app"
	verifier       = "verifier-0123456789-0123456789-0123456789"
)

func noLimit(h http.Handler) http.Handler { return h }

type fakeTokens struct {
	mu     sync.Mutex
	tokens map[string]store.Token
}

func (f *fakeTokens) PutToken(_ context.Context, token string, t store.Token, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens[token] = t
	return nil
}

func (f *fakeTokens) emails() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, t := range f.tokens {
		out = append(out, t.Email)
	}
	return out
}

type testOAuth struct {
	url    string
	tokens *fakeTokens
	hc     *http.Client
}

func startOAuth(t *testing.T, g *googletest.Google, cfg *agentdir.Config, shared string) *testOAuth {
	t.Helper()
	tokens := &fakeTokens{tokens: map[string]store.Token{}}
	o := oauth.New(oauth.Deps{
		Google:         g.Endpoints(),
		SharedClientID: shared,
		Tokens:         tokens,
		Runtime:        agentdir.NewRuntime(agentdir.Dir{}, cfg, agentdir.Secrets{"GOOGLE_CLIENT_SECRET": g.Secret}),
	})
	mux := http.NewServeMux()
	var ts *httptest.Server
	o.Register(mux, func(*http.Request) string { return ts.URL }, noLimit)
	ts = httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	hc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &testOAuth{url: ts.URL, tokens: tokens, hc: hc}
}

func (o *testOAuth) get(t *testing.T, u string) *http.Response {
	t.Helper()
	resp, err := o.hc.Get(u)
	if err != nil {
		t.Fatalf("GET %s error = %v", u, err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	return resp
}

func (o *testOAuth) postToken(t *testing.T, form url.Values) (int, map[string]string) {
	t.Helper()
	resp, err := o.hc.PostForm(o.url+oauth.TokenPath, form)
	if err != nil {
		t.Fatalf("POST %s error = %v", oauth.TokenPath, err)
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

// signIn follows the sign-in of a native client through the fake Google and
// returns the query of the redirect to the client.
func (o *testOAuth) signIn(t *testing.T, redirect string) url.Values {
	t.Helper()
	q := url.Values{
		"response_type": {"code"}, "client_id": {"a2a-cli"}, "redirect_uri": {redirect}, "state": {"s1"},
		"code_challenge": {googleauth.Challenge(verifier)}, "code_challenge_method": {"S256"},
	}
	next := o.url + oauth.AuthorizePath + "?" + q.Encode()
	for range 3 {
		resp := o.get(t, next)
		if resp.StatusCode != http.StatusFound {
			t.Fatalf("GET %s = %s, want a redirect", next, resp.Status)
		}
		loc, err := resp.Location()
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(loc.String(), redirect) {
			return loc.Query()
		}
		next = loc.String()
	}
	t.Fatalf("the sign-in didn't reach %s", redirect)
	return nil
}

func ownerAppConfig(allow ...string) *agentdir.Config {
	return &agentdir.Config{
		Policy: agentdir.PolicyConfig{AllowEmails: allow},
		Auth:   agentdir.AuthConfig{GoogleClientID: "owner-app", GoogleClientSecret: "secret:GOOGLE_CLIENT_SECRET"},
	}
}

func TestServerAuthorizationCode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		account    googletest.Account
		verifier   string
		wantError  string
		wantStatus int
		wantEmails []string
	}{
		{name: "allowed email", account: googletest.Account{Email: "Mom@example.org", Verified: true}, verifier: verifier, wantStatus: http.StatusOK, wantEmails: []string{"mom@example.org"}},
		{name: "wrong code verifier", account: googletest.Account{Email: "mom@example.org", Verified: true}, verifier: "wrong", wantStatus: http.StatusBadRequest},
		{name: "email not allowed", account: googletest.Account{Email: "eve@example.net", Verified: true}, wantError: "access_denied"},
		{name: "unverified email", account: googletest.Account{Email: "mom@example.org"}, wantError: "access_denied"},
		{name: "personal account with an allowed domain", account: googletest.Account{Email: "ben@example.com", Verified: true}, wantError: "access_denied"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := googletest.New(t, tc.account)
			o := startOAuth(t, g, ownerAppConfig("mom@example.org", "*@example.com"), "")
			client := httptest.NewServer(http.NotFoundHandler())
			t.Cleanup(client.Close)
			redirect := client.URL + "/callback"

			got := o.signIn(t, redirect)
			if got.Get("state") != "s1" || got.Get("error") != tc.wantError {
				t.Fatalf("client redirect = %v, want state s1 and error %q", got, tc.wantError)
			}
			if tc.wantError != "" {
				if got.Get("code") != "" {
					t.Fatalf("client redirect = %v, want no code", got)
				}
				return
			}
			form := url.Values{"grant_type": {"authorization_code"}, "code": {got.Get("code")}, "redirect_uri": {redirect}, "client_id": {"a2a-cli"}, "code_verifier": {tc.verifier}}
			status, tok := o.postToken(t, form)
			if status != tc.wantStatus {
				t.Fatalf("token = %d %v, want %d", status, tok, tc.wantStatus)
			}
			if diff := cmp.Diff(tc.wantEmails, o.tokens.emails(), cmpopts.EquateEmpty()); diff != "" {
				t.Fatalf("issued tokens for emails wrong (-want +got) diff = %s", diff)
			}
		})
	}
}

// authorizeQuery is a valid authorization request of a native client.
func authorizeQuery() url.Values {
	return url.Values{
		"response_type": {"code"}, "client_id": {"a2a-cli"}, "redirect_uri": {"http://127.0.0.1:9999/callback"}, "state": {"s1"},
		"code_challenge": {googleauth.Challenge(verifier)}, "code_challenge_method": {"S256"},
	}
}

func TestServerAuthorizeRefusesLongFields(t *testing.T) {
	t.Parallel()
	g := googletest.New(t, googletest.Account{})
	o := startOAuth(t, g, ownerAppConfig("mom@example.org"), "")
	long := strings.Repeat("a", oauth.MaxParamLen+1)
	tests := []struct {
		name  string
		field string
		value string
		want  int
	}{
		{name: "valid request", want: http.StatusFound},
		{name: "long client_id", field: "client_id", value: long, want: http.StatusBadRequest},
		{name: "long state", field: "state", value: long, want: http.StatusBadRequest},
		{name: "long code_challenge", field: "code_challenge", value: long, want: http.StatusBadRequest},
		{name: "long redirect_uri", field: "redirect_uri", value: "http://127.0.0.1:9999/" + long, want: http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q := authorizeQuery()
			if tc.field != "" {
				q.Set(tc.field, tc.value)
			}
			if got := o.get(t, o.url+oauth.AuthorizePath+"?"+q.Encode()).StatusCode; got != tc.want {
				t.Fatalf("GET %s with %s status = %d, want %d", oauth.AuthorizePath, tc.name, got, tc.want)
			}
		})
	}
}

func TestServerAuthorizeState(t *testing.T) {
	t.Parallel()
	g := googletest.New(t, googletest.Account{})
	cfg := ownerAppConfig("mom@example.org")
	newMux := func() *http.ServeMux {
		o := oauth.New(oauth.Deps{Google: g.Endpoints(), Runtime: agentdir.NewRuntime(agentdir.Dir{}, cfg, nil)})
		mux := http.NewServeMux()
		o.Register(mux, func(*http.Request) string { return "https://coder.example.com" }, noLimit)
		return mux
	}
	mux, other := newMux(), newMux()
	authorize := func(mux *http.ServeMux) string {
		t.Helper()
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, oauth.AuthorizePath+"?"+authorizeQuery().Encode(), nil))
		loc, err := url.Parse(w.Header().Get("Location"))
		if w.Code != http.StatusFound || err != nil {
			t.Fatalf("GET %s = %d, %v, want a redirect to Google", oauth.AuthorizePath, w.Code, err)
		}
		return loc.Query().Get("state")
	}
	first := authorize(mux)
	for range 1000 {
		authorize(mux)
	}
	tampered := []byte(first)
	tampered[len(tampered)/3] ^= 1
	tests := []struct {
		name  string
		state string
		want  int
	}{
		{name: "first of many sign-ins", state: first, want: http.StatusFound},
		{name: "tampered state", state: string(tampered), want: http.StatusBadRequest},
		{name: "state of another server", state: authorize(other), want: http.StatusBadRequest},
		{name: "random state", state: "0123456789abcdef", want: http.StatusBadRequest},
		{name: "no state", want: http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, oauth.CallbackPath+"?"+url.Values{"state": {tc.state}, "error": {"access_denied"}}.Encode(), nil))
			if w.Code != tc.want {
				t.Fatalf("GET %s status = %d, want %d", oauth.CallbackPath, w.Code, tc.want)
			}
		})
	}
}

func TestServerTokenBodyLimit(t *testing.T) {
	t.Parallel()
	g := googletest.New(t, googletest.Account{})
	o := startOAuth(t, g, ownerAppConfig("mom@example.org"), "")
	tests := []struct {
		name    string
		padding int
		want    int
	}{
		{name: "small body", padding: 1 << 10, want: http.StatusBadRequest},
		{name: "body over the limit", padding: oauth.MaxTokenBody, want: http.StatusRequestEntityTooLarge},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			form := url.Values{"grant_type": {"authorization_code"}, "code": {"unknown"}, "padding": {strings.Repeat("a", tc.padding)}}
			resp, err := o.hc.PostForm(o.url+oauth.TokenPath, form)
			if err != nil {
				t.Fatalf("POST %s error = %v", oauth.TokenPath, err)
			}
			if err := resp.Body.Close(); err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != tc.want {
				t.Fatalf("POST %s status = %d, want %d", oauth.TokenPath, resp.StatusCode, tc.want)
			}
		})
	}
}

func TestServerTokenExchange(t *testing.T) {
	t.Parallel()
	g := googletest.New(t, googletest.Account{Email: "ben@example.com", Verified: true, HostedDomain: "example.com"})
	o := startOAuth(t, g, ownerAppConfig("*@example.com"), sharedClientID)
	tests := []struct {
		name       string
		token      string
		wantStatus int
	}{
		{name: "bound to this agent", token: g.IDToken(sharedClientID, googleauth.URLBinding(o.url)), wantStatus: http.StatusOK},
		{name: "bound to another agent", token: g.IDToken(sharedClientID, googleauth.URLBinding("https://other.example.com")), wantStatus: http.StatusBadRequest},
		{name: "another app", token: g.IDToken("someone-else", googleauth.URLBinding(o.url)), wantStatus: http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			form := url.Values{"grant_type": {oauth.GrantTokenExchange}, "subject_token": {tc.token}, "subject_token_type": {oauth.TokenTypeIDToken}}
			status, out := o.postToken(t, form)
			if status != tc.wantStatus {
				t.Fatalf("token exchange = %d %v, want %d", status, out, tc.wantStatus)
			}
		})
	}
}

func TestServerRoutes(t *testing.T) {
	t.Parallel()
	g := googletest.New(t, googletest.Account{})
	tests := []struct {
		name string
		cfg  *agentdir.Config
		path string
		want int
	}{
		{name: "metadata on", cfg: ownerAppConfig("mom@example.org"), path: oauth.AuthServerPath, want: http.StatusOK},
		{name: "resource on", cfg: ownerAppConfig("mom@example.org"), path: oauth.ProtectedResourcePath, want: http.StatusOK},
		{name: "metadata off", cfg: ownerAppConfig(), path: oauth.AuthServerPath, want: http.StatusNotFound},
		{name: "resource off", cfg: ownerAppConfig(), path: oauth.ProtectedResourcePath, want: http.StatusNotFound},
		{name: "authorize off", cfg: ownerAppConfig(), path: oauth.AuthorizePath, want: http.StatusNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			o := startOAuth(t, g, tc.cfg, sharedClientID)
			if got := o.get(t, o.url+tc.path).StatusCode; got != tc.want {
				t.Fatalf("GET %s = %d, want %d", tc.path, got, tc.want)
			}
		})
	}
}

func TestServerMetadata(t *testing.T) {
	t.Parallel()
	g := googletest.New(t, googletest.Account{})
	tests := []struct {
		name       string
		cfg        *agentdir.Config
		shared     string
		wantGrants []string
	}{
		{name: "shared app", cfg: &agentdir.Config{Policy: agentdir.PolicyConfig{AllowEmails: []string{"a@b.c"}}}, shared: sharedClientID, wantGrants: []string{oauth.GrantTokenExchange}},
		{name: "owner's app", cfg: ownerAppConfig("a@b.c"), wantGrants: []string{"authorization_code"}},
		{name: "owner's app and shared app", cfg: ownerAppConfig("a@b.c"), shared: sharedClientID, wantGrants: []string{oauth.GrantTokenExchange, "authorization_code"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			o := startOAuth(t, g, tc.cfg, tc.shared)
			resp, err := http.Get(o.url + oauth.AuthServerPath)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := resp.Body.Close(); err != nil {
					t.Errorf("closing: %v", err)
				}
			}()
			var meta struct {
				Grants   []string `json:"grant_types_supported"`
				Endpoint string   `json:"authorization_endpoint"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.wantGrants, meta.Grants); diff != "" {
				t.Fatalf("GET %s wrong grants (-want +got) diff = %s", oauth.AuthServerPath, diff)
			}
			if got, want := meta.Endpoint != "", tc.cfg.Auth.GoogleClientID != ""; got != want {
				t.Fatalf("GET %s advertises an authorization endpoint = %v, want %v", oauth.AuthServerPath, got, want)
			}
		})
	}
}

func TestServerCardScheme(t *testing.T) {
	t.Parallel()
	const base = "https://coder.example.com"
	allowEmail := []string{"mom@example.org"}
	tests := []struct {
		name   string
		cfg    *agentdir.Config
		shared string
		want   a2a.SecurityScheme
	}{
		{
			name: "shared app", cfg: &agentdir.Config{Policy: agentdir.PolicyConfig{AllowEmails: allowEmail}}, shared: sharedClientID,
			want: a2a.OpenIDConnectSecurityScheme{OpenIDConnectURL: googleauth.OpenIDConfiguration},
		},
		{
			name: "owner's app", cfg: ownerAppConfig(allowEmail...), shared: sharedClientID,
			want: a2a.OAuth2SecurityScheme{
				Oauth2MetadataURL: base + oauth.AuthServerPath,
				Flows:             a2a.AuthorizationCodeOAuthFlow{AuthorizationURL: base + oauth.AuthorizePath, TokenURL: base + oauth.TokenPath, Scopes: map[string]string{}, PKCERequired: true},
			},
		},
		{name: "no allowed emails", cfg: ownerAppConfig(), shared: sharedClientID},
		{name: "no shared app", cfg: &agentdir.Config{Policy: agentdir.PolicyConfig{AllowEmails: allowEmail}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			o := oauth.New(oauth.Deps{Google: googleauth.Google, SharedClientID: tc.shared, Runtime: agentdir.NewRuntime(agentdir.Dir{}, tc.cfg, nil)})
			got := o.CardScheme(base)
			if diff := cmp.Diff(tc.want, got, cmpopts.IgnoreFields(a2a.OpenIDConnectSecurityScheme{}, "Description"), cmpopts.IgnoreFields(a2a.OAuth2SecurityScheme{}, "Description")); diff != "" {
				t.Fatalf("Server.CardScheme() wrong scheme (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestServerChallengeHeader(t *testing.T) {
	t.Parallel()
	const base = "https://coder.example.com"
	tests := []struct {
		name string
		cfg  *agentdir.Config
		kind string
		want string
	}{
		{name: "public with sign-in", cfg: ownerAppConfig("mom@example.org"), kind: network.ListenerPublic, want: `Bearer realm="a2a", resource_metadata="` + base + oauth.ProtectedResourcePath + `"`},
		{name: "public without sign-in", cfg: ownerAppConfig(), kind: network.ListenerPublic, want: `Bearer realm="a2a"`},
		{name: "private with sign-in", cfg: ownerAppConfig("mom@example.org"), kind: network.ListenerPrivate, want: `Bearer realm="a2a"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			o := oauth.New(oauth.Deps{Google: googleauth.Google, Runtime: agentdir.NewRuntime(agentdir.Dir{}, tc.cfg, nil)})
			if got := o.ChallengeHeader(tc.kind, base); got != tc.want {
				t.Fatalf("Server.ChallengeHeader(%q) = %q, want %q", tc.kind, got, tc.want)
			}
		})
	}
}
