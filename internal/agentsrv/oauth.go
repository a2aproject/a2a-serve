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
	"crypto/subtle"
	"errors"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/cryptoutil"
	"github.com/a2aproject/a2a-serve/internal/googleauth"
	"github.com/a2aproject/a2a-serve/internal/jsonhttp"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// The agent is its own OAuth authorization server: people sign in at Google,
// and the agent issues its own access tokens, which work only at this agent.
const (
	protectedResourcePath = "/.well-known/oauth-protected-resource"
	authServerPath        = "/.well-known/oauth-authorization-server"
	authorizePath         = "/oauth/authorize"
	callbackPath          = "/oauth/callback"
	tokenPath             = "/oauth/token"

	grantTokenExchange = "urn:ietf:params:oauth:grant-type:token-exchange"
	tokenTypeIDToken   = "urn:ietf:params:oauth:token-type:id_token"

	oauthPendingTTL = 10 * time.Minute
)

// oauthServer signs people in with Google, limited to the allowlist.
type oauthServer struct {
	s        *Server
	google   googleauth.Endpoints
	verifier *googleauth.Verifier
	hc       *http.Client

	mu sync.Mutex
	// pending are sign-ins sent to Google, by the state sent there.
	pending map[string]pendingSignIn
	// codes are authorization codes issued to clients.
	codes map[string]issuedCode
}

type pendingSignIn struct {
	issuer         string
	clientRedirect string
	clientState    string
	clientID       string
	challenge      string
	googleVerifier string
	nonce          string
	expires        time.Time
}

type issuedCode struct {
	issuer    string
	email     string
	clientID  string
	redirect  string
	challenge string
	expires   time.Time
}

func newOAuthServer(s *Server, google googleauth.Endpoints) *oauthServer {
	hc := &http.Client{Timeout: 30 * time.Second}
	return &oauthServer{
		s: s, google: google, hc: hc,
		verifier: &googleauth.Verifier{Endpoints: google, HTTP: hc},
		pending:  map[string]pendingSignIn{}, codes: map[string]issuedCode{},
	}
}

func (o *oauthServer) enabled() bool { return len(o.s.config().Auth.AllowEmails) > 0 }

func (o *oauthServer) register(mux *http.ServeMux, base func(*http.Request) string) {
	mux.HandleFunc("GET "+protectedResourcePath, o.on(func(w http.ResponseWriter, r *http.Request) {
		b := base(r)
		jsonhttp.Write(w, http.StatusOK, map[string]any{
			"resource":                 b,
			"authorization_servers":    []string{b},
			"bearer_methods_supported": []string{"header"},
		})
	}))
	mux.HandleFunc("GET "+authServerPath, o.on(func(w http.ResponseWriter, r *http.Request) {
		b := base(r)
		grants := []string{grantTokenExchange}
		meta := map[string]any{
			"issuer":                                b,
			"token_endpoint":                        b + tokenPath,
			"token_endpoint_auth_methods_supported": []string{"none"},
			"code_challenge_methods_supported":      []string{"S256"},
		}
		if o.s.config().Auth.GoogleClientID != "" {
			grants = append(grants, "authorization_code")
			meta["authorization_endpoint"] = b + authorizePath
			meta["response_types_supported"] = []string{"code"}
		}
		meta["grant_types_supported"] = grants
		jsonhttp.Write(w, http.StatusOK, meta)
	}))
	mux.HandleFunc("GET "+authorizePath, o.on(func(w http.ResponseWriter, r *http.Request) { o.authorize(w, r, base(r)) }))
	mux.HandleFunc("GET "+callbackPath, o.on(o.callback))
	mux.HandleFunc("POST "+tokenPath, o.on(func(w http.ResponseWriter, r *http.Request) { o.token(w, r, base(r)) }))
}

// on serves h while sign-in is on, and 404 otherwise.
func (o *oauthServer) on(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !o.enabled() {
			jsonhttp.Error(w, http.StatusNotFound, "this agent doesn't let people sign in with Google")
			return
		}
		h(w, r)
	}
}

// challengeHeader is the WWW-Authenticate header of a 401, which points
// clients to the agent's auth metadata when sign-in is on.
func (o *oauthServer) challengeHeader(base string) string {
	if !o.enabled() {
		return `Bearer realm="a2a"`
	}
	return `Bearer realm="a2a", resource_metadata="` + base + protectedResourcePath + `"`
}

// loopbackRedirect reports whether a client's redirect URI is on loopback,
// where native apps such as the a2a CLI receive the code (RFC 8252).
func loopbackRedirect(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.User != nil {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func oauthError(w http.ResponseWriter, status int, code, desc string) {
	jsonhttp.Write(w, status, map[string]string{"error": code, "error_description": desc})
}

// authorize sends a client's sign-in to Google with the owner's app.
func (o *oauthServer) authorize(w http.ResponseWriter, r *http.Request, base string) {
	cfg := o.s.config()
	q := r.URL.Query()
	if cfg.Auth.GoogleClientID == "" {
		oauthError(w, http.StatusBadRequest, "unsupported_response_type", "this agent signs people in with the A2A project's shared Google app; use a client that supports it")
		return
	}
	redirect := q.Get("redirect_uri")
	if !loopbackRedirect(redirect) {
		oauthError(w, http.StatusBadRequest, "invalid_request", "redirect_uri must be an http loopback address")
		return
	}
	if q.Get("response_type") != "code" || q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256" || q.Get("client_id") == "" {
		oauthError(w, http.StatusBadRequest, "invalid_request", "want response_type=code, a client_id and a PKCE S256 code_challenge")
		return
	}
	p := pendingSignIn{
		issuer: base, clientRedirect: redirect, clientState: q.Get("state"), clientID: q.Get("client_id"),
		challenge: q.Get("code_challenge"), googleVerifier: cryptoutil.RandomString(64, cryptoutil.Alphanumeric),
		nonce: cryptoutil.RandomString(32, cryptoutil.Alphanumeric), expires: time.Now().Add(oauthPendingTTL),
	}
	state := cryptoutil.RandomString(32, cryptoutil.Alphanumeric)
	o.mu.Lock()
	o.prune()
	o.pending[state] = p
	o.mu.Unlock()
	http.Redirect(w, r, o.google.AuthCodeURL(cfg.Auth.GoogleClientID, base+callbackPath, state, p.nonce, googleauth.Challenge(p.googleVerifier)), http.StatusFound)
}

// prune drops expired sign-ins and codes. o.mu is held.
func (o *oauthServer) prune() {
	now := time.Now()
	for k, p := range o.pending {
		if now.After(p.expires) {
			delete(o.pending, k)
		}
	}
	for k, c := range o.codes {
		if now.After(c.expires) {
			delete(o.codes, k)
		}
	}
}

// callback takes the account back from Google, checks it against the
// allowlist and sends the client an authorization code.
func (o *oauthServer) callback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	o.mu.Lock()
	p, ok := o.pending[q.Get("state")]
	delete(o.pending, q.Get("state"))
	o.mu.Unlock()
	if !ok || time.Now().After(p.expires) {
		http.Error(w, "This sign-in expired. Start again from your client.", http.StatusBadRequest)
		return
	}
	if e := q.Get("error"); e != "" {
		o.redirectClient(w, r, p, url.Values{"error": {"access_denied"}, "error_description": {"Google sign-in: " + e}})
		return
	}
	cfg := o.s.config()
	secret, err := o.s.secrets.Resolve(cfg.Auth.GoogleClientSecret)
	if err != nil {
		slog.Error("signing in with Google", "error", err)
		http.Error(w, "The agent's Google app isn't set up: "+err.Error(), http.StatusInternalServerError)
		return
	}
	idToken, err := o.google.Exchange(r.Context(), o.hc, cfg.Auth.GoogleClientID, secret, q.Get("code"), p.issuer+callbackPath, p.googleVerifier)
	if err != nil {
		o.redirectClient(w, r, p, url.Values{"error": {"access_denied"}, "error_description": {err.Error()}})
		return
	}
	claims, err := o.verifier.Verify(r.Context(), idToken, []string{cfg.Auth.GoogleClientID})
	if err == nil && subtle.ConstantTimeCompare([]byte(claims.Nonce), []byte(p.nonce)) != 1 {
		err = errNonce
	}
	if err != nil {
		o.redirectClient(w, r, p, url.Values{"error": {"access_denied"}, "error_description": {err.Error()}})
		return
	}
	if !googleauth.Allowed(cfg.Auth.AllowEmails, claims) {
		slog.Warn("refused a Google sign-in", "email", claims.Email)
		o.redirectClient(w, r, p, url.Values{"error": {"access_denied"}, "error_description": {claims.Email + " may not call this agent; ask its owner to allow it"}})
		return
	}
	code := cryptoutil.RandomString(32, cryptoutil.Alphanumeric)
	o.mu.Lock()
	o.codes[code] = issuedCode{issuer: p.issuer, email: strings.ToLower(claims.Email), clientID: p.clientID, redirect: p.clientRedirect, challenge: p.challenge, expires: time.Now().Add(oauthPendingTTL)}
	o.mu.Unlock()
	o.redirectClient(w, r, p, url.Values{"code": {code}})
}

var errNonce = errors.New("the Google sign-in doesn't belong to this request")

func (o *oauthServer) redirectClient(w http.ResponseWriter, r *http.Request, p pendingSignIn, v url.Values) {
	u, err := url.Parse(p.clientRedirect)
	if err != nil {
		http.Error(w, "Invalid redirect.", http.StatusBadRequest)
		return
	}
	q := u.Query()
	maps.Copy(q, v)
	if p.clientState != "" {
		q.Set("state", p.clientState)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

// token issues the agent's access token for an authorization code, or for
// a Google ID token of the shared app bound to this agent's URL.
func (o *oauthServer) token(w http.ResponseWriter, r *http.Request, base string) {
	switch r.FormValue("grant_type") {
	case "authorization_code":
		o.mu.Lock()
		c, ok := o.codes[r.FormValue("code")]
		delete(o.codes, r.FormValue("code"))
		o.mu.Unlock()
		switch {
		case !ok || time.Now().After(c.expires):
			oauthError(w, http.StatusBadRequest, "invalid_grant", "unknown or expired code")
		case c.redirect != r.FormValue("redirect_uri") || c.clientID != r.FormValue("client_id") || c.issuer != base:
			oauthError(w, http.StatusBadRequest, "invalid_grant", "the code was issued for another client")
		case googleauth.Challenge(r.FormValue("code_verifier")) != c.challenge:
			oauthError(w, http.StatusBadRequest, "invalid_grant", "wrong code_verifier")
		default:
			o.issue(w, r, base, c.email)
		}
	case grantTokenExchange:
		o.exchange(w, r, base)
	default:
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "want authorization_code or token exchange")
	}
}

// exchange trades a Google ID token of the shared app for the agent's own
// token. The token's nonce must bind it to this agent's URL, so another
// agent of the same app can't use a caller's token here.
func (o *oauthServer) exchange(w http.ResponseWriter, r *http.Request, base string) {
	if googleauth.SharedClientID == "" {
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "the shared Google app isn't available yet; the agent's owner can register their own with --google-client-id")
		return
	}
	if r.FormValue("subject_token_type") != tokenTypeIDToken {
		oauthError(w, http.StatusBadRequest, "invalid_request", "want subject_token_type "+tokenTypeIDToken)
		return
	}
	claims, err := o.verifier.Verify(r.Context(), r.FormValue("subject_token"), []string{googleauth.SharedClientID})
	if err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_grant", err.Error())
		return
	}
	if subtle.ConstantTimeCompare([]byte(claims.Nonce), []byte(googleauth.URLBinding(base))) != 1 {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the Google token was requested for another agent")
		return
	}
	if !googleauth.Allowed(o.s.config().Auth.AllowEmails, claims) {
		oauthError(w, http.StatusForbidden, "access_denied", claims.Email+" may not call this agent; ask its owner to allow it")
		return
	}
	o.issue(w, r, base, strings.ToLower(claims.Email))
}

// issue stores a new access token for email and returns it. The token is a
// pairing token of the principal google:<email>, so `pair rm` revokes it,
// and the allowlist is checked on every use.
func (o *oauthServer) issue(w http.ResponseWriter, r *http.Request, base, email string) {
	ctx := r.Context()
	id := auth.GooglePrefix + email
	if _, err := o.s.store.EnsureUser(ctx, id, []string{auth.RoleMember}); err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	tok := auth.NewToken()
	if err := o.s.store.PutToken(ctx, tok, store.Token{Name: id, Principal: id, Roles: []string{auth.RoleMember}, URL: base}); err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	slog.Info("signed in with Google", "email", email)
	w.Header().Set("Cache-Control", "no-store")
	jsonhttp.Write(w, http.StatusOK, map[string]any{"access_token": tok, "token_type": "Bearer", "issued_token_type": "urn:ietf:params:oauth:token-type:access_token"})
}
