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

package signin_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/googleauth/googletest"
	"github.com/a2aproject/a2a-serve/internal/jsonhttp"
	"github.com/a2aproject/a2a-serve/internal/relay/signin"
)

const googleApp = "relay-app.apps.googleusercontent.com"

var anna = googletest.Account{Subject: "anna-id", Email: "Anna@Example.com", Verified: true, HostedDomain: "example.com"}

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type relay struct {
	url    string
	google *googletest.Google
	clock  *clock
	hc     *http.Client
	// issuer serves the relay; tests swap it to restart the relay.
	mu     sync.Mutex
	issuer *signin.Issuer
}

type relayOptions struct {
	allow  []string
	secret string
}

func newRelay(t *testing.T, account googletest.Account, o relayOptions) *relay {
	t.Helper()
	r := &relay{google: googletest.New(t, account), clock: &clock{now: time.Now()}}
	srv := httptest.NewServer(http.StripPrefix(signin.Path, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		iss := r.issuer
		r.mu.Unlock()
		iss.ServeHTTP(w, req)
	})))
	t.Cleanup(srv.Close)
	r.url, r.hc = srv.URL, srv.Client()
	r.restart(t, o)
	return r
}

// restart replaces the relay's issuer, as a relay that starts again with
// other settings.
func (r *relay) restart(t *testing.T, o relayOptions) {
	t.Helper()
	if o.secret == "" {
		o.secret = strings.Repeat("s", 64)
	}
	if o.allow == nil {
		o.allow = []string{"*@example.com"}
	}
	iss, err := signin.New(signin.Options{
		URL: r.url, Secret: o.secret, AllowEmails: o.allow,
		ClientID: googleApp, ClientSecret: r.google.Secret, Google: r.google.Endpoints(),
		HTTP: r.hc, Now: r.clock.Now,
	})
	if err != nil {
		t.Fatalf("signin.New() error = %v", err)
	}
	r.mu.Lock()
	r.issuer = iss
	r.mu.Unlock()
}

type tokenResponse struct {
	IDToken      string `json:"id_token"`
	RefreshToken string `json:"refresh_token"`
	Error        string `json:"error"`
	Desc         string `json:"error_description"`
}

func (r *relay) post(t *testing.T, path string, form url.Values) (int, tokenResponse) {
	t.Helper()
	resp, err := r.hc.PostForm(signin.URL(r.url)+path, form)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(t, resp)
	var out tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decoding %s: %v", path, err)
	}
	return resp.StatusCode, out
}

type deviceResponse struct {
	DeviceCode string `json:"device_code"`
	UserCode   string `json:"user_code"`
	Complete   string `json:"verification_uri_complete"`
}

func (r *relay) startSignIn(t *testing.T) deviceResponse {
	t.Helper()
	resp, err := r.hc.PostForm(signin.URL(r.url)+"/device/authorize", url.Values{"client_id": {signin.ClientID}, "scope": {"openid email"}})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /device/authorize status = %d, want 200", resp.StatusCode)
	}
	var d deviceResponse
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		t.Fatal(err)
	}
	return d
}

func (r *relay) poll(t *testing.T, deviceCode string) (int, tokenResponse) {
	t.Helper()
	r.clock.advance(signin.PollInterval)
	return r.post(t, "/token", url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}, "device_code": {deviceCode}, "client_id": {signin.ClientID}})
}

func (r *relay) refresh(t *testing.T, refreshToken string) (int, tokenResponse) {
	t.Helper()
	return r.post(t, "/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refreshToken}, "client_id": {signin.ClientID}})
}

// browse follows link in a browser that signs in at Google at once, and
// returns the page it ends on.
func (r *relay) browse(t *testing.T, link string) (int, string) {
	t.Helper()
	resp, err := r.hc.Get(link)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(t, resp)
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

// leave follows link to the redirect to Google's sign-in, and returns
// where it redirects.
func (r *relay) leave(t *testing.T, link string) string {
	t.Helper()
	hc := *r.hc
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := hc.Get(link)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(t, resp)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("GET %s status = %d, want 302", link, resp.StatusCode)
	}
	return resp.Header.Get("Location")
}

var approvalToken = regexp.MustCompile(`name="token" value="([^"]+)"`)

// answer answers the approval form on page.
func (r *relay) answer(t *testing.T, page, answer string) string {
	t.Helper()
	m := approvalToken.FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("no approval form on the page:\n%s", page)
	}
	resp, err := r.hc.PostForm(signin.URL(r.url)+"/approve", url.Values{"token": {m[1]}, "answer": {answer}})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(t, resp)
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// signIn runs a sign-in the person approves and returns its tokens.
func (r *relay) signIn(t *testing.T) tokenResponse {
	t.Helper()
	d := r.startSignIn(t)
	status, page := r.browse(t, d.Complete)
	if status != http.StatusOK || !strings.Contains(page, d.UserCode) {
		t.Fatalf("sign-in page status = %d, want 200 with the code %s:\n%s", status, d.UserCode, page)
	}
	r.answer(t, page, "approve")
	status, tok := r.poll(t, d.DeviceCode)
	if status != http.StatusOK {
		t.Fatalf("POST /token status = %d (%s: %s), want 200", status, tok.Error, tok.Desc)
	}
	return tok
}

func closeBody(t *testing.T, resp *http.Response) {
	t.Helper()
	if err := resp.Body.Close(); err != nil {
		t.Errorf("closing a response: %v", err)
	}
}

type idClaims struct {
	Subject  string `json:"sub"`
	Email    string `json:"email"`
	Verified bool   `json:"email_verified"`
	Domain   string `json:"hd"`
}

// verify checks idToken as Agent Mesh does, with go-oidc and the issuer's
// discovery document.
func (r *relay) verify(t *testing.T, idToken string) idClaims {
	t.Helper()
	ctx := oidc.ClientContext(context.Background(), r.hc)
	p, err := oidc.NewProvider(ctx, signin.URL(r.url))
	if err != nil {
		t.Fatalf("oidc.NewProvider() error = %v", err)
	}
	tok, err := p.Verifier(&oidc.Config{ClientID: signin.ClientID}).Verify(ctx, idToken)
	if err != nil {
		t.Fatalf("IDTokenVerifier.Verify() error = %v", err)
	}
	var c idClaims
	if err := tok.Claims(&c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestSignInIssuesTokensAgentMeshAccepts(t *testing.T) {
	t.Parallel()
	r := newRelay(t, anna, relayOptions{})

	tok := r.signIn(t)

	want := idClaims{Subject: "anna-id", Email: "anna@example.com", Verified: true, Domain: "example.com"}
	if diff := cmp.Diff(want, r.verify(t, tok.IDToken)); diff != "" {
		t.Fatalf("ID token claims wrong result (-want +got) diff = %s", diff)
	}
	if tok.RefreshToken == "" {
		t.Fatalf("POST /token returned no refresh token")
	}
	status, renewed := r.refresh(t, tok.RefreshToken)
	if status != http.StatusOK {
		t.Fatalf("refresh status = %d (%s: %s), want 200", status, renewed.Error, renewed.Desc)
	}
	if diff := cmp.Diff(want, r.verify(t, renewed.IDToken)); diff != "" {
		t.Fatalf("renewed ID token claims wrong result (-want +got) diff = %s", diff)
	}
}

func TestSignInKeepsWorkingAfterARestart(t *testing.T) {
	t.Parallel()
	r := newRelay(t, anna, relayOptions{})
	tok := r.signIn(t)

	r.restart(t, relayOptions{})

	r.verify(t, tok.IDToken)
	if status, renewed := r.refresh(t, tok.RefreshToken); status != http.StatusOK {
		t.Fatalf("refresh after a restart status = %d (%s: %s), want 200", status, renewed.Error, renewed.Desc)
	}
}

func TestRefreshIsRefused(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		change func(t *testing.T, r *relay)
	}{
		{name: "email no longer allowed", change: func(t *testing.T, r *relay) { r.restart(t, relayOptions{allow: []string{"ben@example.com"}}) }},
		{name: "new admin token", change: func(t *testing.T, r *relay) { r.restart(t, relayOptions{secret: strings.Repeat("n", 64)}) }},
		{name: "Google access revoked", change: func(t *testing.T, r *relay) { r.google.RevokeRefreshTokens() }},
		{name: "account left the Workspace", change: func(t *testing.T, r *relay) {
			r.google.SetAccount(googletest.Account{Subject: anna.Subject, Email: anna.Email, Verified: true})
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRelay(t, anna, relayOptions{})
			tok := r.signIn(t)

			tc.change(t, r)

			status, got := r.refresh(t, tok.RefreshToken)
			if status != http.StatusBadRequest || got.Error != "invalid_grant" {
				t.Fatalf("refresh = %d %q, want 400 invalid_grant", status, got.Error)
			}
		})
	}
}

func TestSignInRefusesAccountsNotAllowed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		account googletest.Account
	}{
		{name: "personal account of an allowed domain", account: googletest.Account{Subject: "x", Email: "eve@example.com", Verified: true}},
		{name: "other domain", account: googletest.Account{Subject: "x", Email: "eve@example.net", Verified: true, HostedDomain: "example.net"}},
		{name: "unverified email", account: googletest.Account{Subject: "x", Email: "eve@example.com", HostedDomain: "example.com"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRelay(t, tc.account, relayOptions{})
			d := r.startSignIn(t)

			if status, page := r.browse(t, d.Complete); status != http.StatusForbidden {
				t.Fatalf("sign-in page status = %d, want 403:\n%s", status, page)
			}
			if status, got := r.poll(t, d.DeviceCode); status != http.StatusBadRequest || got.Error != "authorization_pending" {
				t.Fatalf("POST /token after a refused account = %d %q, want 400 authorization_pending", status, got.Error)
			}

			r.google.SetAccount(anna)
			status, page := r.browse(t, d.Complete)
			if status != http.StatusOK {
				t.Fatalf("sign-in page with an allowed account status = %d, want 200:\n%s", status, page)
			}
			r.answer(t, page, "approve")
			if status, got := r.poll(t, d.DeviceCode); status != http.StatusOK {
				t.Fatalf("POST /token after signing in with an allowed account = %d %q, want 200", status, got.Error)
			}
		})
	}
}

func TestDeclinedSignIn(t *testing.T) {
	t.Parallel()
	r := newRelay(t, anna, relayOptions{})
	d := r.startSignIn(t)
	_, page := r.browse(t, d.Complete)

	r.answer(t, page, "decline")

	if status, got := r.poll(t, d.DeviceCode); status != http.StatusBadRequest || got.Error != "access_denied" {
		t.Fatalf("POST /token after declining = %d %q, want 400 access_denied", status, got.Error)
	}
}

func TestRefreshTokenIsUsedAtMostOnceInAnInterval(t *testing.T) {
	t.Parallel()
	r := newRelay(t, anna, relayOptions{})
	tok := r.signIn(t)
	if status, got := r.refresh(t, tok.RefreshToken); status != http.StatusOK {
		t.Fatalf("refresh = %d %q, want 200", status, got.Error)
	}

	if status, got := r.refresh(t, tok.RefreshToken); status != http.StatusServiceUnavailable || got.Error != "temporarily_unavailable" {
		t.Fatalf("refresh right after a refresh = %d %q, want 503 temporarily_unavailable", status, got.Error)
	}
	r.clock.advance(signin.MinRefreshInterval)
	if status, got := r.refresh(t, tok.RefreshToken); status != http.StatusOK {
		t.Fatalf("refresh after the interval = %d %q, want 200", status, got.Error)
	}
}

func TestRefusedRenewalsLeaveRoomForOthers(t *testing.T) {
	t.Parallel()
	r := newRelay(t, anna, relayOptions{})
	spender, other := r.signIn(t), r.signIn(t)
	if status, got := r.refresh(t, spender.RefreshToken); status != http.StatusOK {
		t.Fatalf("refresh = %d %q, want 200", status, got.Error)
	}
	for range 300 {
		if status, got := r.refresh(t, spender.RefreshToken); status != http.StatusServiceUnavailable {
			t.Fatalf("repeated refresh = %d %q, want 503", status, got.Error)
		}
	}

	if status, got := r.refresh(t, other.RefreshToken); status != http.StatusOK {
		t.Fatalf("refresh of another token = %d %q, want 200", status, got.Error)
	}
}

func TestDeviceFlowAnswers(t *testing.T) {
	t.Parallel()
	r := newRelay(t, anna, relayOptions{})
	d := r.startSignIn(t)
	form := func(code, client string) url.Values {
		return url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}, "device_code": {code}, "client_id": {client}}
	}

	if status, got := r.post(t, "/token", form(d.DeviceCode, signin.ClientID)); status != http.StatusBadRequest || got.Error != "slow_down" {
		t.Fatalf("POST /token at once = %d %q, want 400 slow_down", status, got.Error)
	}
	r.clock.advance(time.Minute)
	if status, got := r.post(t, "/token", form(d.DeviceCode, signin.ClientID)); status != http.StatusBadRequest || got.Error != "authorization_pending" {
		t.Fatalf("POST /token before sign-in = %d %q, want 400 authorization_pending", status, got.Error)
	}
	if status, got := r.post(t, "/token", form(d.DeviceCode, "other")); status != http.StatusUnauthorized || got.Error != "invalid_client" {
		t.Fatalf("POST /token of another client = %d %q, want 401 invalid_client", status, got.Error)
	}
	if status, got := r.post(t, "/token", form("unknown", signin.ClientID)); status != http.StatusBadRequest || got.Error != "expired_token" {
		t.Fatalf("POST /token of an unknown code = %d %q, want 400 expired_token", status, got.Error)
	}
	r.clock.advance(signin.DeviceTTL)
	if status, got := r.post(t, "/token", form(d.DeviceCode, signin.ClientID)); status != http.StatusBadRequest || got.Error != "expired_token" {
		t.Fatalf("POST /token after the link expired = %d %q, want 400 expired_token", status, got.Error)
	}
	if status, got := r.refresh(t, "a2rr.forged"); status != http.StatusBadRequest || got.Error != "invalid_grant" {
		t.Fatalf("refresh with a forged token = %d %q, want 400 invalid_grant", status, got.Error)
	}
}

func TestDevicePageTakesTheCodeAsTyped(t *testing.T) {
	t.Parallel()
	r := newRelay(t, anna, relayOptions{})
	d := r.startSignIn(t)
	typed := strings.ToLower(strings.ReplaceAll(d.UserCode, "-", " "))

	status, page := r.browse(t, signin.URL(r.url)+"/device?"+url.Values{"code": {typed}}.Encode())

	if status != http.StatusOK || !strings.Contains(page, d.UserCode) {
		t.Fatalf("sign-in page for %q status = %d, want 200 with %s:\n%s", typed, status, d.UserCode, page)
	}
}

func TestDevicePageSlowsDownGuessedCodes(t *testing.T) {
	t.Parallel()
	r := newRelay(t, anna, relayOptions{})
	d := r.startSignIn(t)
	wrong := signin.URL(r.url) + "/device?code=BCDF-GHJK"
	right := signin.URL(r.url) + "/device?" + url.Values{"code": {d.UserCode}}.Encode()

	if status, _ := r.browse(t, wrong); status != http.StatusNotFound {
		t.Fatalf("sign-in page of an unknown code status = %d, want 404", status)
	}
	if status, _ := r.browse(t, right); status != http.StatusTooManyRequests {
		t.Fatalf("sign-in page right after a wrong code status = %d, want 429", status)
	}
	if status, page := r.browse(t, d.Complete); status != http.StatusOK {
		t.Fatalf("sign-in page of the printed link during the backoff status = %d, want 200:\n%s", status, page)
	}
	r.clock.advance(1500 * time.Millisecond)
	if status, _ := r.browse(t, right); status != http.StatusOK {
		t.Fatalf("sign-in page of the right code after the backoff status = %d, want 200", status)
	}
	if status, _ := r.browse(t, wrong); status != http.StatusNotFound {
		t.Fatalf("sign-in page of an unknown code status = %d, want 404", status)
	}
	r.clock.advance(1500 * time.Millisecond)
	if status, _ := r.browse(t, wrong); status != http.StatusTooManyRequests {
		t.Fatalf("sign-in page of a second wrong code within the doubled backoff status = %d, want 429: a right code must not reset it", status)
	}
}

func TestASignInKeepsItsLastThreeApprovals(t *testing.T) {
	t.Parallel()
	r := newRelay(t, anna, relayOptions{})
	d := r.startSignIn(t)
	var pages []string
	for range 4 {
		_, page := r.browse(t, d.Complete)
		pages = append(pages, page)
	}

	if got := r.answer(t, pages[0], "approve"); !strings.Contains(got, "expired") {
		t.Fatalf("approving the oldest of four pages = %q, want it expired", got)
	}
	r.answer(t, pages[3], "approve")
	if status, got := r.poll(t, d.DeviceCode); status != http.StatusOK {
		t.Fatalf("POST /token after approving the newest page = %d %q, want 200", status, got.Error)
	}
}

func TestASignInKeepsItsLastThreeTripsToGoogle(t *testing.T) {
	t.Parallel()
	r := newRelay(t, anna, relayOptions{})
	d := r.startSignIn(t)
	var trips []string
	for range 4 {
		trips = append(trips, r.leave(t, d.Complete))
	}

	if _, got := r.browse(t, trips[0]); !strings.Contains(got, "expired") {
		t.Fatalf("returning from the oldest of four trips to Google = %q, want it expired", got)
	}
	if status, got := r.browse(t, trips[3]); status != http.StatusOK || !strings.Contains(got, d.UserCode) {
		t.Fatalf("returning from the newest trip to Google = %d %q, want 200 with the code %s", status, got, d.UserCode)
	}
}

func TestSignInDropsBodiesSentTooSlowly(t *testing.T) {
	t.Parallel()
	r := newRelay(t, anna, relayOptions{})
	u, err := url.Parse(r.url)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := conn.Close(); err != nil {
			t.Errorf("closing the connection: %v", err)
		}
	})
	req := fmt.Sprintf("POST %s/token HTTP/1.1\r\nHost: %s\r\nContent-Type: application/x-www-form-urlencoded\r\nContent-Length: 100\r\n\r\nclient_id=", signin.Path, u.Host)
	if _, err := io.WriteString(conn, req); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(3 * jsonhttp.UnauthenticatedReadTimeout)); err != nil {
		t.Fatal(err)
	}

	_, err = io.ReadAll(conn)

	if errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("reading the connection of a stalled body error = %v, want the server to close it", err)
	}
}
