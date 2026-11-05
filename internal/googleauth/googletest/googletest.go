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

// Package googletest is a fake of Google's OAuth for tests: its sign-in page
// signs in the account set on it at once.
package googletest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-serve/internal/googleauth"
)

const issuer = "https://accounts.google.test"

// Account is who the fake signs in.
type Account struct {
	// Subject defaults to 1234.
	Subject      string
	Email        string
	Verified     bool
	HostedDomain string
}

// Google is a fake of Google's OAuth.
type Google struct {
	t      *testing.T
	server *httptest.Server
	key    *rsa.PrivateKey
	// Secret is the client secret token requests must send.
	Secret string

	mu      sync.Mutex
	account Account
	codes   map[string]authCode
	// refresh maps the refresh tokens the fake issued to their app.
	refresh map[string]string
}

type authCode struct {
	clientID, nonce, challenge string
	offline                    bool
}

// New starts a fake which signs in account.
func New(t *testing.T, account Account) *Google {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	g := &Google{t: t, key: key, Secret: "client-secret", account: account, codes: map[string]authCode{}, refresh: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth", g.auth)
	mux.HandleFunc("POST /token", g.token)
	mux.HandleFunc("GET /jwks", g.jwks)
	g.server = httptest.NewServer(mux)
	t.Cleanup(g.server.Close)
	return g
}

// Endpoints returns the endpoints of the fake.
func (g *Google) Endpoints() googleauth.Endpoints {
	return googleauth.Endpoints{AuthURL: g.server.URL + "/auth", TokenURL: g.server.URL + "/token", JWKSURL: g.server.URL + "/jwks", Issuers: []string{issuer}}
}

// SetAccount changes who the fake signs in.
func (g *Google) SetAccount(a Account) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.account = a
}

func (g *Google) auth(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	code := rand.Text()
	g.mu.Lock()
	g.codes[code] = authCode{clientID: q.Get("client_id"), nonce: q.Get("nonce"), challenge: q.Get("code_challenge"), offline: q.Get("access_type") == "offline"}
	g.mu.Unlock()
	back, err := url.Parse(q.Get("redirect_uri"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	v := back.Query()
	v.Set("code", code)
	v.Set("state", q.Get("state"))
	back.RawQuery = v.Encode()
	http.Redirect(w, r, back.String(), http.StatusFound)
}

// RevokeRefreshTokens makes every refresh token the fake issued invalid, as
// when the person removes the app's access.
func (g *Google) RevokeRefreshTokens() {
	g.mu.Lock()
	defer g.mu.Unlock()
	clear(g.refresh)
}

func (g *Google) token(w http.ResponseWriter, r *http.Request) {
	if r.FormValue("grant_type") == "refresh_token" {
		g.mu.Lock()
		clientID, ok := g.refresh[r.FormValue("refresh_token")]
		g.mu.Unlock()
		if !ok || r.FormValue("client_secret") != g.Secret || r.FormValue("client_id") != clientID {
			g.invalidGrant(w)
			return
		}
		g.write(w, map[string]string{"id_token": g.IDToken(clientID, "")})
		return
	}
	g.mu.Lock()
	c, ok := g.codes[r.FormValue("code")]
	delete(g.codes, r.FormValue("code"))
	g.mu.Unlock()
	if !ok || r.FormValue("client_secret") != g.Secret || googleauth.Challenge(r.FormValue("code_verifier")) != c.challenge {
		g.invalidGrant(w)
		return
	}
	resp := map[string]string{"id_token": g.IDToken(c.clientID, c.nonce)}
	if c.offline {
		refresh := rand.Text()
		g.mu.Lock()
		g.refresh[refresh] = c.clientID
		g.mu.Unlock()
		resp["refresh_token"] = refresh
	}
	g.write(w, resp)
}

func (g *Google) invalidGrant(w http.ResponseWriter) {
	w.WriteHeader(http.StatusBadRequest)
	g.write(w, map[string]string{"error": "invalid_grant"})
}

func (g *Google) write(w http.ResponseWriter, v any) {
	if err := json.NewEncoder(w).Encode(v); err != nil {
		g.t.Errorf("writing a token response: %v", err)
	}
}

func (g *Google) jwks(w http.ResponseWriter, _ *http.Request) {
	pub := g.key.PublicKey
	set := map[string]any{"keys": []map[string]string{{
		"kid": "k1", "kty": "RSA", "alg": "RS256",
		"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}}}
	if err := json.NewEncoder(w).Encode(set); err != nil {
		g.t.Errorf("writing the keys: %v", err)
	}
}

// IDToken returns an ID token of the account for the app audience.
func (g *Google) IDToken(audience, nonce string) string {
	g.mu.Lock()
	a := g.account
	g.mu.Unlock()
	sub := a.Subject
	if sub == "" {
		sub = "1234"
	}
	claims := map[string]any{
		"iss": issuer, "aud": audience, "sub": sub, "email": a.Email, "email_verified": a.Verified,
		"nonce": nonce, "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
	}
	if a.HostedDomain != "" {
		claims["hd"] = a.HostedDomain
	}
	return g.sign(claims)
}

func (g *Google) sign(claims map[string]any) string {
	enc := func(v any) string {
		data, err := json.Marshal(v)
		if err != nil {
			g.t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(data)
	}
	signed := enc(map[string]string{"alg": "RS256", "kid": "k1", "typ": "JWT"}) + "." + enc(claims)
	sum := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, g.key, crypto.SHA256, sum[:])
	if err != nil {
		g.t.Fatal(err)
	}
	return signed + "." + base64.RawURLEncoding.EncodeToString(sig)
}
