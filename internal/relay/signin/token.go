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

package signin

import (
	"crypto/sha256"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/a2aproject/a2a-serve/internal/googleauth"
	"github.com/a2aproject/a2a-serve/internal/jsonhttp"
)

// pollSlack is how much earlier than its interval an agent may poll, since
// the interval runs between responses on its side.
const pollSlack = time.Second

func (i *Issuer) token(w http.ResponseWriter, r *http.Request) {
	if name, long := longParam(r, "client_id", "device_code", "refresh_token"); long {
		oauthError(w, http.StatusBadRequest, "invalid_request", name+" is too long")
		return
	}
	if r.FormValue("client_id") != ClientID {
		oauthError(w, http.StatusUnauthorized, "invalid_client", "unknown client_id; want "+ClientID)
		return
	}
	switch r.FormValue("grant_type") {
	case grantDeviceCode:
		i.pollDevice(w, r.FormValue("device_code"))
	case "refresh_token":
		i.refresh(w, r)
	default:
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "want the device code or the refresh token grant")
	}
}

// pollDevice answers an agent that waits for its sign-in.
func (i *Issuer) pollDevice(w http.ResponseWriter, deviceCode string) {
	now := i.opts.Now()
	i.mu.Lock()
	d, ok := i.devices[deviceCode]
	if !ok || now.After(d.expires) {
		i.mu.Unlock()
		oauthError(w, http.StatusBadRequest, "expired_token", "the sign-in link expired; start the agent again for a new one")
		return
	}
	if now.Before(d.nextPoll.Add(-pollSlack)) {
		d.interval += 5 * time.Second
		d.nextPoll = now.Add(d.interval)
		i.mu.Unlock()
		oauthError(w, http.StatusBadRequest, "slow_down", "poll less often")
		return
	}
	d.nextPoll = now.Add(d.interval)
	t, denied := d.tokens, d.denied
	if t != nil || denied != "" {
		i.forget(deviceCode, d)
	}
	i.mu.Unlock()
	switch {
	case denied != "":
		oauthError(w, http.StatusBadRequest, "access_denied", denied)
	case t == nil:
		oauthError(w, http.StatusBadRequest, "authorization_pending", "waiting for the person to sign in")
	default:
		i.writeTokens(w, t.idToken, t.refreshToken)
	}
}

// refresh issues a new ID token for a refresh token. It asks Google about
// the account again, so an account that left its Workspace or revoked the
// relay's access is refused, and it checks the allowlist as it is now.
func (i *Issuer) refresh(w http.ResponseWriter, r *http.Request) {
	refresh := r.FormValue("refresh_token")
	s, err := i.keys.openSession(refresh, i.url)
	if err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "unknown refresh token; sign in again")
		return
	}
	if !i.mayRefresh(refresh) || !i.refreshes.Allow() {
		oauthError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "too many renewals; try again in a minute")
		return
	}
	idToken, err := i.opts.Google.Refresh(r.Context(), i.opts.HTTP, i.opts.ClientID, i.opts.ClientSecret, s.Google)
	if errors.Is(err, googleauth.ErrInvalidGrant) {
		slog.Info("relay sign-in: Google no longer lets the account in", "email", s.Email)
		oauthError(w, http.StatusBadRequest, "invalid_grant", "Google no longer lets "+s.Email+" in to the relay; sign in again")
		return
	}
	if err != nil {
		slog.Warn("relay sign-in: renewing with Google", "email", s.Email, "error", err)
		oauthError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "the relay couldn't reach Google; try again later")
		return
	}
	claims, err := i.verifier.Verify(r.Context(), idToken, []string{i.opts.ClientID})
	if err != nil {
		slog.Warn("relay sign-in: verifying Google's ID token", "email", s.Email, "error", err)
		oauthError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "the relay couldn't verify the account with Google; try again later")
		return
	}
	if claims.Subject != s.Subject {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the refresh token belongs to another account; sign in again")
		return
	}
	if !i.allowed(claims) {
		email := strings.ToLower(claims.Email)
		slog.Info("relay sign-in: renewal refused", "email", email)
		oauthError(w, http.StatusBadRequest, "invalid_grant", email+" may no longer join this relay")
		return
	}
	tok, err := i.issue(claims)
	if err != nil {
		slog.Error("relay sign-in: issuing an ID token", "error", err)
		oauthError(w, http.StatusInternalServerError, "server_error", "the relay couldn't issue an ID token")
		return
	}
	i.writeTokens(w, tok, refresh)
}

// MinRefreshInterval is how often one refresh token may be used. An agent
// renews once per [IDTokenTTL].
const MinRefreshInterval = 10 * time.Second

// mayRefresh records a use of refresh, and reports whether it is the first
// in MinRefreshInterval, so one holder can't spend the renewals of all.
func (i *Issuer) mayRefresh(refresh string) bool {
	sum := sha256.Sum256([]byte(refresh))
	key := string(sum[:])
	now := i.opts.Now()
	i.mu.Lock()
	defer i.mu.Unlock()
	if last, ok := i.lastRefresh[key]; ok && now.Sub(last) < MinRefreshInterval {
		return false
	}
	for k, t := range i.lastRefresh {
		if now.Sub(t) >= MinRefreshInterval {
			delete(i.lastRefresh, k)
		}
	}
	i.lastRefresh[key] = now
	return true
}

func (i *Issuer) writeTokens(w http.ResponseWriter, idToken, refreshToken string) {
	w.Header().Set("Cache-Control", "no-store")
	jsonhttp.Write(w, http.StatusOK, map[string]any{
		"id_token":      idToken,
		"refresh_token": refreshToken,
		"token_type":    "Bearer",
		"expires_in":    int(IDTokenTTL / time.Second),
	})
}
