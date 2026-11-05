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

// Package googleauth signs people in with Google: it builds the sign-in
// request, trades the code for an ID token, verifies ID tokens, and matches
// their email against an allowlist.
package googleauth

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

// Endpoints are Google's OAuth endpoints. Tests replace them.
type Endpoints struct {
	AuthURL  string
	TokenURL string
	JWKSURL  string
	Issuers  []string
}

// Google are the endpoints of Google's OAuth.
var Google = Endpoints{
	AuthURL:  "https://accounts.google.com/o/oauth2/v2/auth",
	TokenURL: "https://oauth2.googleapis.com/token",
	JWKSURL:  "https://www.googleapis.com/oauth2/v3/certs",
	Issuers:  []string{"https://accounts.google.com", "accounts.google.com"},
}

// OpenIDConfiguration is the OpenID Connect discovery URL of Google.
const OpenIDConfiguration = "https://accounts.google.com/.well-known/openid-configuration"

// SharedClientID is the client ID of the Google app the A2A project owns,
// which a2a CLI signs in with for agents whose owner registered none. Empty
// until the app is registered, which turns the token exchange off.
const SharedClientID = ""

// AuthCodeURL returns the URL of Google's sign-in page for a request with
// PKCE, asking for the email of the account.
func (e Endpoints) AuthCodeURL(clientID, redirectURI, state, nonce, challenge string) string {
	return e.authCodeURL(clientID, redirectURI, state, nonce, challenge, false)
}

// OfflineAuthCodeURL is [Endpoints.AuthCodeURL] that also asks for a refresh
// token, so the app can check the account again later. Google sends one only
// when the person consents, so the page always asks.
func (e Endpoints) OfflineAuthCodeURL(clientID, redirectURI, state, nonce, challenge string) string {
	return e.authCodeURL(clientID, redirectURI, state, nonce, challenge, true)
}

func (e Endpoints) authCodeURL(clientID, redirectURI, state, nonce, challenge string, offline bool) string {
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"scope":                 {"openid email"},
		"state":                 {state},
		"nonce":                 {nonce},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"prompt":                {"select_account"},
	}
	if offline {
		q.Set("access_type", "offline")
		q.Set("prompt", "consent select_account")
	}
	return e.AuthURL + "?" + q.Encode()
}

// Tokens are what Google returns for a sign-in.
type Tokens struct {
	IDToken string
	// RefreshToken is set for a sign-in from [Endpoints.OfflineAuthCodeURL].
	RefreshToken string
}

// ErrInvalidGrant means Google refused a code or a refresh token: it was
// used, it expired, or the person revoked the app's access.
var ErrInvalidGrant = errors.New("google refused the grant")

// Exchange trades an authorization code for the ID token of the account.
func (e Endpoints) Exchange(ctx context.Context, hc *http.Client, clientID, clientSecret, code, redirectURI, verifier string) (string, error) {
	t, err := e.ExchangeTokens(ctx, hc, clientID, clientSecret, code, redirectURI, verifier)
	return t.IDToken, err
}

// ExchangeTokens trades an authorization code for the tokens of the account.
func (e Endpoints) ExchangeTokens(ctx context.Context, hc *http.Client, clientID, clientSecret, code, redirectURI, verifier string) (Tokens, error) {
	return e.token(ctx, hc, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
	})
}

// Refresh trades a refresh token for a new ID token of the account. It
// returns [ErrInvalidGrant] when the account no longer lets the app in.
func (e Endpoints) Refresh(ctx context.Context, hc *http.Client, clientID, clientSecret, refreshToken string) (string, error) {
	t, err := e.token(ctx, hc, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
	})
	return t.IDToken, err
}

func (e Endpoints) token(ctx context.Context, hc *http.Client, form url.Values) (Tokens, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return Tokens{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := hc.Do(req)
	if err != nil {
		return Tokens{}, err
	}
	defer closeBody(resp)
	var out struct {
		IDToken      string `json:"id_token"`
		RefreshToken string `json:"refresh_token"`
		Error        string `json:"error"`
		Desc         string `json:"error_description"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return Tokens{}, fmt.Errorf("reading Google's token response: %w", err)
	}
	if out.Error == "invalid_grant" {
		return Tokens{}, fmt.Errorf("%w: %s", ErrInvalidGrant, out.Desc)
	}
	if resp.StatusCode != http.StatusOK || out.IDToken == "" {
		return Tokens{}, fmt.Errorf("google refused the sign-in: %s %s", out.Error, out.Desc)
	}
	return Tokens{IDToken: out.IDToken, RefreshToken: out.RefreshToken}, nil
}

// Challenge returns the S256 PKCE challenge of verifier.
func Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// URLBinding is the nonce a client puts in a sign-in request with the
// shared app for the agent at agentURL. Google ties a token to the app which
// requested it, so without it an agent could reuse a caller's token at
// another agent of the same app.
func URLBinding(agentURL string) string {
	return Challenge(strings.TrimSuffix(agentURL, "/"))
}

// Claims are the verified claims of an ID token.
type Claims struct {
	// Subject is the account's ID, which stays when its email changes.
	Subject       string `json:"sub"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	// HostedDomain is the Google Workspace domain of the account, if any.
	HostedDomain string `json:"hd"`
	Nonce        string `json:"nonce"`
	Audience     string `json:"-"`
}

// Allowed reports whether the allowlist admits the account: an entry is an
// email, or *@domain, which matches only accounts of that Google Workspace
// domain, since anyone can create a personal account with any address.
func Allowed(list []string, c Claims) bool {
	if !c.EmailVerified || c.Email == "" {
		return false
	}
	email := strings.ToLower(c.Email)
	for _, entry := range list {
		entry = strings.ToLower(strings.TrimSpace(entry))
		if domain, ok := strings.CutPrefix(entry, "*@"); ok {
			if c.HostedDomain != "" && strings.EqualFold(c.HostedDomain, domain) && strings.HasSuffix(email, "@"+domain) {
				return true
			}
			continue
		}
		if entry == email {
			return true
		}
	}
	return false
}

// keysTTL is how long Google's signing keys are cached.
const keysTTL = time.Hour

// KeyRefetchInterval is how long after a fetch of Google's signing keys a
// token with an unknown key may trigger another fetch.
const KeyRefetchInterval = time.Minute

// fetchTimeout bounds a fetch of Google's signing keys.
const fetchTimeout = 30 * time.Second

// Verifier verifies Google ID tokens.
type Verifier struct {
	Endpoints Endpoints
	HTTP      *http.Client
	// Now defaults to time.Now.
	Now func() time.Time

	mu      sync.Mutex
	keys    map[string]*rsa.PublicKey
	fetched time.Time
	// fetching is closed when the fetch of keys in progress ends.
	fetching chan struct{}
}

// Verify checks the signature, issuer, expiry and audience of an ID token,
// which must be one of audiences, and returns its claims.
func (v *Verifier) Verify(ctx context.Context, idToken string, audiences []string) (Claims, error) {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return Claims{}, errors.New("not an ID token")
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodeSegment(parts[0], &header); err != nil {
		return Claims{}, err
	}
	if header.Alg != "RS256" {
		return Claims{}, fmt.Errorf("unsupported ID token algorithm %q", header.Alg)
	}
	key, err := v.key(ctx, header.Kid)
	if err != nil {
		return Claims{}, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Claims{}, errors.New("invalid ID token signature")
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], sig); err != nil {
		return Claims{}, errors.New("invalid ID token signature")
	}
	var payload struct {
		Claims
		Issuer   string          `json:"iss"`
		Aud      json.RawMessage `json:"aud"`
		Expiry   float64         `json:"exp"`
		Verified json.RawMessage `json:"email_verified"`
	}
	if err := decodeSegment(parts[1], &payload); err != nil {
		return Claims{}, err
	}
	c := payload.Claims
	// Google sends email_verified as a boolean, and once sent it as a string.
	c.EmailVerified = string(payload.Verified) == "true" || string(payload.Verified) == `"true"`
	if !slices.Contains(v.Endpoints.Issuers, payload.Issuer) {
		return Claims{}, fmt.Errorf("ID token from unexpected issuer %q", payload.Issuer)
	}
	if now := v.now(); now.After(time.Unix(int64(payload.Expiry), 0).Add(time.Minute)) {
		return Claims{}, errors.New("the ID token expired")
	}
	var auds []string
	var one string
	if json.Unmarshal(payload.Aud, &one) == nil {
		auds = []string{one}
	} else if err := json.Unmarshal(payload.Aud, &auds); err != nil {
		return Claims{}, errors.New("invalid ID token audience")
	}
	for _, a := range auds {
		if a != "" && slices.Contains(audiences, a) {
			c.Audience = a
			return c, nil
		}
	}
	return Claims{}, errors.New("the ID token was issued to another app")
}

func (v *Verifier) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

// key returns Google's signing key kid. It fetches the keys when they are
// stale, or when they don't have kid and are older than KeyRefetchInterval.
// One fetch runs at a time without holding v.mu, so tokens with known keys
// don't wait for it.
func (v *Verifier) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	for {
		v.mu.Lock()
		k, ok := v.keys[kid]
		age := v.now().Sub(v.fetched)
		wait := v.fetching
		switch {
		case ok && age < keysTTL:
			v.mu.Unlock()
			return k, nil
		case !ok && age < KeyRefetchInterval:
			v.mu.Unlock()
			return nil, errors.New("the ID token is signed with an unknown key")
		case wait != nil:
			v.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		done := make(chan struct{})
		v.fetching = done
		v.mu.Unlock()
		keys, err := v.fetchKeys(ctx)
		v.mu.Lock()
		if err == nil {
			v.keys, v.fetched = keys, v.now()
		}
		v.fetching = nil
		v.mu.Unlock()
		close(done)
		if err != nil {
			return nil, fmt.Errorf("fetching Google's signing keys: %w", err)
		}
	}
}

// fetchKeys fetches Google's signing keys. Callers waiting for the keys
// share the fetch, so it doesn't end with the request which started it.
func (v *Verifier) fetchKeys(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.Endpoints.JWKSURL, nil)
	if err != nil {
		return nil, err
	}
	hc := v.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer closeBody(resp)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", v.Endpoints.JWKSURL, resp.Status)
	}
	var set struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&set); err != nil {
		return nil, err
	}
	out := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "RSA" {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(k.N)
		e, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil {
			continue
		}
		out[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	return out, nil
}

func decodeSegment(seg string, v any) error {
	data, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return errors.New("invalid ID token encoding")
	}
	if err := json.Unmarshal(data, v); err != nil {
		return errors.New("invalid ID token")
	}
	return nil
}

func closeBody(resp *http.Response) {
	if err := resp.Body.Close(); err != nil {
		slog.Debug("closing a response from Google", "error", err)
	}
}
