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

// Package oauth is the agent's OAuth authorization server, which signs people
// in with Google.
package oauth

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/cryptoutil"
	"github.com/a2aproject/a2a-serve/internal/googleauth"
	"github.com/a2aproject/a2a-serve/internal/jsonhttp"
	"github.com/a2aproject/a2a-serve/internal/network"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// The agent is its own OAuth authorization server: people sign in at Google,
// and the agent issues its own access tokens, which work only at this agent.
const (
	ProtectedResourcePath = "/.well-known/oauth-protected-resource"
	AuthServerPath        = "/.well-known/oauth-authorization-server"
	AuthorizePath         = "/oauth/authorize"
	CallbackPath          = "/oauth/callback"
	TokenPath             = "/oauth/token"

	GrantTokenExchange = "urn:ietf:params:oauth:grant-type:token-exchange"
	TokenTypeIDToken   = "urn:ietf:params:oauth:token-type:id_token"

	oauthPendingTTL = 10 * time.Minute

	// MaxPendingSignIns bounds the sign-ins sent to Google and not back yet;
	// a new one beyond it drops the oldest.
	MaxPendingSignIns = 1000
	// MaxParamLen bounds each client parameter of an authorization request.
	MaxParamLen = 2048
)

// TokenStore keeps the access tokens the server issues.
type TokenStore interface {
	// PutToken stores a token by its value; kind is the pairing kind.
	PutToken(ctx context.Context, token string, t store.Token, kind string) error
}

// Deps are what the OAuth server reads and calls.
type Deps struct {
	Google googleauth.Endpoints
	// SharedClientID is the client ID of the shared app, or empty when
	// there is none.
	SharedClientID string
	Tokens         TokenStore
	Config         func() *agentdir.Config
	Secrets        func() agentdir.Secrets
}

// Server signs people in with Google at the public URL, limited to the
// emails the policy names.
type Server struct {
	deps     Deps
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
	domain    string
	clientID  string
	redirect  string
	challenge string
	expires   time.Time
}

// New returns an OAuth server with no sign-ins in progress.
func New(deps Deps) *Server {
	hc := &http.Client{Timeout: 30 * time.Second}
	return &Server{
		deps: deps, hc: hc,
		verifier: &googleauth.Verifier{Endpoints: deps.Google, HTTP: hc},
		pending:  map[string]pendingSignIn{}, codes: map[string]issuedCode{},
	}
}

func (o *Server) enabled() bool { return len(o.deps.Config().Policy.AllowEmails) > 0 }

// Register serves the sign-in routes on mux, where base is the URL the
// request reached the agent at.
func (o *Server) Register(mux *http.ServeMux, base func(*http.Request) string) {
	mux.HandleFunc("GET "+ProtectedResourcePath, o.on(func(w http.ResponseWriter, r *http.Request) {
		b := base(r)
		jsonhttp.Write(w, http.StatusOK, map[string]any{
			"resource":                 b,
			"authorization_servers":    []string{b},
			"bearer_methods_supported": []string{"header"},
		})
	}))
	mux.HandleFunc("GET "+AuthServerPath, o.on(func(w http.ResponseWriter, r *http.Request) {
		jsonhttp.Write(w, http.StatusOK, o.metadata(base(r)))
	}))
	mux.HandleFunc("GET "+AuthorizePath, o.on(func(w http.ResponseWriter, r *http.Request) { o.authorize(w, r, base(r)) }))
	mux.HandleFunc("GET "+CallbackPath, o.on(o.callback))
	mux.HandleFunc("POST "+TokenPath, o.on(func(w http.ResponseWriter, r *http.Request) { o.token(w, r, base(r)) }))
}

// signInFlow is how callers sign in with Google at the public listener.
type signInFlow int

const (
	signInOff signInFlow = iota
	// signInShared is Google's sign-in with the shared app, followed by the
	// token exchange at the agent.
	signInShared
	// signInOwnerApp is the authorization code flow against the agent, which
	// signs in at Google with the owner's app.
	signInOwnerApp
)

// flow returns the sign-in the agent offers, which the agent card and the
// authorization server metadata advertise.
func (o *Server) flow() signInFlow {
	switch {
	case !o.enabled():
		return signInOff
	case o.deps.Config().Auth.GoogleClientID != "":
		return signInOwnerApp
	case o.deps.SharedClientID != "":
		return signInShared
	}
	return signInOff
}

// metadata is the authorization server metadata at base (RFC 8414). The
// token exchange stays on with the owner's app, for clients that signed in
// with the shared one.
func (o *Server) metadata(base string) map[string]any {
	grants := []string{}
	if o.deps.SharedClientID != "" {
		grants = append(grants, GrantTokenExchange)
	}
	meta := map[string]any{
		"issuer":                                base,
		"token_endpoint":                        base + TokenPath,
		"token_endpoint_auth_methods_supported": []string{"none"},
		"code_challenge_methods_supported":      []string{"S256"},
	}
	if o.flow() == signInOwnerApp {
		grants = append(grants, "authorization_code")
		meta["authorization_endpoint"] = base + AuthorizePath
		meta["response_types_supported"] = []string{"code"}
	}
	meta["grant_types_supported"] = grants
	return meta
}

// CardScheme is the security scheme of the agent card for the sign-in at
// base, or nil when sign-in is off.
func (o *Server) CardScheme(base string) a2a.SecurityScheme {
	switch o.flow() {
	case signInShared:
		return a2a.OpenIDConnectSecurityScheme{
			Description:      "Google sign-in with the A2A project's shared app and the nonce URLBinding(agent URL), followed by the token exchange at " + base + TokenPath + ". Only emails the owner allows may sign in.",
			OpenIDConnectURL: googleauth.OpenIDConfiguration,
		}
	case signInOwnerApp:
		return a2a.OAuth2SecurityScheme{
			Description:       "Google sign-in through the agent. Only emails the owner allows may sign in.",
			Oauth2MetadataURL: base + AuthServerPath,
			Flows: a2a.AuthorizationCodeOAuthFlow{
				AuthorizationURL: base + AuthorizePath,
				TokenURL:         base + TokenPath,
				Scopes:           map[string]string{},
				PKCERequired:     true,
			},
		}
	}
	return nil
}

// on serves h while sign-in is on, and 404 otherwise.
func (o *Server) on(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !o.enabled() {
			jsonhttp.Error(w, http.StatusNotFound, "this agent doesn't let people sign in with Google")
			return
		}
		h(w, r)
	}
}

// ChallengeHeader is the WWW-Authenticate header of a 401 on the listener
// kind, which points clients to the agent's auth metadata when sign-in is
// on there.
func (o *Server) ChallengeHeader(kind, base string) string {
	if kind != network.ListenerPublic || !o.enabled() {
		return `Bearer realm="a2a"`
	}
	return `Bearer realm="a2a", resource_metadata="` + base + ProtectedResourcePath + `"`
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
func (o *Server) authorize(w http.ResponseWriter, r *http.Request, base string) {
	cfg := o.deps.Config()
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
	for _, name := range []string{"redirect_uri", "client_id", "state", "code_challenge"} {
		if len(q.Get(name)) > MaxParamLen {
			oauthError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("%s is longer than %d bytes", name, MaxParamLen))
			return
		}
	}
	p := pendingSignIn{
		issuer: base, clientRedirect: redirect, clientState: q.Get("state"), clientID: q.Get("client_id"),
		challenge: q.Get("code_challenge"), googleVerifier: cryptoutil.RandomString(64, cryptoutil.Alphanumeric),
		nonce: cryptoutil.RandomString(32, cryptoutil.Alphanumeric), expires: time.Now().Add(oauthPendingTTL),
	}
	state := cryptoutil.RandomString(32, cryptoutil.Alphanumeric)
	o.mu.Lock()
	o.prune()
	if len(o.pending) >= MaxPendingSignIns {
		o.dropOldestPending()
	}
	o.pending[state] = p
	o.mu.Unlock()
	http.Redirect(w, r, o.deps.Google.AuthCodeURL(cfg.Auth.GoogleClientID, base+CallbackPath, state, p.nonce, googleauth.Challenge(p.googleVerifier)), http.StatusFound)
}

// prune drops expired sign-ins and codes. o.mu is held.
func (o *Server) prune() {
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

// dropOldestPending drops the sign-in which expires first. o.mu is held.
func (o *Server) dropOldestPending() {
	var oldest string
	var expires time.Time
	for k, p := range o.pending {
		if oldest == "" || p.expires.Before(expires) {
			oldest, expires = k, p.expires
		}
	}
	delete(o.pending, oldest)
}

// callback takes the account back from Google, checks it against the
// allowlist and sends the client an authorization code.
func (o *Server) callback(w http.ResponseWriter, r *http.Request) {
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
	cfg := o.deps.Config()
	secret, err := o.deps.Secrets().Resolve(cfg.Auth.GoogleClientSecret)
	if err != nil {
		slog.Error("signing in with Google", "error", err)
		http.Error(w, "The agent's Google app isn't set up: "+err.Error(), http.StatusInternalServerError)
		return
	}
	idToken, err := o.deps.Google.Exchange(r.Context(), o.hc, cfg.Auth.GoogleClientID, secret, q.Get("code"), p.issuer+CallbackPath, p.googleVerifier)
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
	if !googleauth.Allowed(cfg.Policy.AllowEmails, claims) {
		slog.Warn("refused a Google sign-in", "email", claims.Email)
		o.redirectClient(w, r, p, url.Values{"error": {"access_denied"}, "error_description": {claims.Email + " may not call this agent; ask its owner to allow it"}})
		return
	}
	code := cryptoutil.RandomString(32, cryptoutil.Alphanumeric)
	o.mu.Lock()
	o.codes[code] = issuedCode{issuer: p.issuer, email: strings.ToLower(claims.Email), domain: workspaceDomain(claims), clientID: p.clientID, redirect: p.clientRedirect, challenge: p.challenge, expires: time.Now().Add(oauthPendingTTL)}
	o.mu.Unlock()
	o.redirectClient(w, r, p, url.Values{"code": {code}})
}

var errNonce = errors.New("the Google sign-in doesn't belong to this request")

func (o *Server) redirectClient(w http.ResponseWriter, r *http.Request, p pendingSignIn, v url.Values) {
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
func (o *Server) token(w http.ResponseWriter, r *http.Request, base string) {
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
			o.issue(w, r, base, c.email, c.domain)
		}
	case GrantTokenExchange:
		o.exchange(w, r, base)
	default:
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "want authorization_code or token exchange")
	}
}

// exchange trades a Google ID token of the shared app for the agent's own
// token. The token's nonce must bind it to this agent's URL, so another
// agent of the same app can't use a caller's token here.
func (o *Server) exchange(w http.ResponseWriter, r *http.Request, base string) {
	if o.deps.SharedClientID == "" {
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "the shared Google app isn't available yet; the agent's owner can register their own with --google-client-id")
		return
	}
	if r.FormValue("subject_token_type") != TokenTypeIDToken {
		oauthError(w, http.StatusBadRequest, "invalid_request", "want subject_token_type "+TokenTypeIDToken)
		return
	}
	claims, err := o.verifier.Verify(r.Context(), r.FormValue("subject_token"), []string{o.deps.SharedClientID})
	if err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_grant", err.Error())
		return
	}
	if subtle.ConstantTimeCompare([]byte(claims.Nonce), []byte(googleauth.URLBinding(base))) != 1 {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the Google token was requested for another agent")
		return
	}
	if !googleauth.Allowed(o.deps.Config().Policy.AllowEmails, claims) {
		oauthError(w, http.StatusForbidden, "access_denied", claims.Email+" may not call this agent; ask its owner to allow it")
		return
	}
	o.issue(w, r, base, strings.ToLower(claims.Email), workspaceDomain(claims))
}

// workspaceDomain is the Google Workspace domain of a signed-in email, when
// it is the email's own domain, which domain patterns of --allow-email
// match.
func workspaceDomain(c googleauth.Claims) string {
	if d := auth.DomainOf(c.Email); c.HostedDomain != "" && strings.EqualFold(c.HostedDomain, d) {
		return d
	}
	return ""
}

// issue stores a new access token for email and returns it. The token
// carries the email, and the policy decides each request it makes.
func (o *Server) issue(w http.ResponseWriter, r *http.Request, base, email, domain string) {
	ctx := r.Context()
	tok := auth.NewToken()
	if err := o.deps.Tokens.PutToken(ctx, tok, store.Token{Email: email, Domain: domain, URL: base}, ""); err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	slog.Info("signed in with Google", "email", email)
	w.Header().Set("Cache-Control", "no-store")
	jsonhttp.Write(w, http.StatusOK, map[string]any{"access_token": tok, "token_type": "Bearer", "issued_token_type": "urn:ietf:params:oauth:token-type:access_token"})
}
