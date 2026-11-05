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
	"crypto/subtle"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/a2aproject/a2a-serve/internal/cryptoutil"
	"github.com/a2aproject/a2a-serve/internal/googleauth"
	"github.com/a2aproject/a2a-serve/internal/jsonhttp"
	"github.com/a2aproject/a2a-serve/internal/webpage"
)

// userCodeAlphabet has no vowels, so user codes don't spell words, and no
// letters that look like digits.
const userCodeAlphabet = "BCDFGHJKLMNPQRSTVWXZ"

// device is a sign-in an agent started. tokens or denied is set when the
// person answered.
type device struct {
	userCode string
	// link is the secret of the printed link, which needs no limit on
	// guesses, unlike the user code people type.
	link     string
	expires  time.Time
	interval time.Duration
	nextPoll time.Time
	tokens   *tokens
	denied   string
}

type tokens struct {
	idToken, refreshToken string
}

// pendingSignIn is a sign-in sent to Google, by the state sent there.
type pendingSignIn struct {
	deviceCode string
	verifier   string
	nonce      string
	expires    time.Time
	seq        uint64
}

// approval is a sign-in back from Google that waits for the person to
// approve it, by the token of its approval form.
type approval struct {
	deviceCode string
	claims     googleauth.Claims
	google     string
	expires    time.Time
	seq        uint64
}

// authorize starts the sign-in of an agent (RFC 8628).
func (i *Issuer) authorize(w http.ResponseWriter, r *http.Request) {
	if name, long := longParam(r, "client_id", "scope", "audience"); long {
		oauthError(w, http.StatusBadRequest, "invalid_request", name+" is too long")
		return
	}
	if r.FormValue("client_id") != ClientID {
		oauthError(w, http.StatusUnauthorized, "invalid_client", "unknown client_id; want "+ClientID)
		return
	}
	if !i.starts.Allow() {
		oauthError(w, http.StatusServiceUnavailable, "slow_down", "too many sign-ins start at once; try again in a minute")
		return
	}
	now := i.opts.Now()
	deviceCode := cryptoutil.RandomString(43, cryptoutil.Alphanumeric)
	i.mu.Lock()
	i.prune(now)
	if len(i.devices) >= MaxPending {
		i.mu.Unlock()
		oauthError(w, http.StatusServiceUnavailable, "slow_down", "too many sign-ins are in progress; try again in a few minutes")
		return
	}
	userCode := i.newUserCode()
	link := cryptoutil.RandomString(32, cryptoutil.Alphanumeric)
	i.devices[deviceCode] = &device{userCode: userCode, link: link, expires: now.Add(DeviceTTL), interval: PollInterval, nextPoll: now.Add(PollInterval)}
	i.byUser[userCode] = deviceCode
	i.byLink[link] = deviceCode
	i.mu.Unlock()
	page := i.url + devicePath
	w.Header().Set("Cache-Control", "no-store")
	jsonhttp.Write(w, http.StatusOK, map[string]any{
		"device_code":               deviceCode,
		"user_code":                 userCode,
		"verification_uri":          page,
		"verification_uri_complete": page + "?" + url.Values{"link": {link}}.Encode(),
		"expires_in":                int(DeviceTTL / time.Second),
		"interval":                  int(PollInterval / time.Second),
	})
}

// newUserCode returns an unused user code such as BCDF-GHJK. i.mu is held.
func (i *Issuer) newUserCode() string {
	for {
		c := cryptoutil.RandomString(8, userCodeAlphabet)
		c = c[:4] + "-" + c[4:]
		if _, ok := i.byUser[c]; !ok {
			return c
		}
	}
}

// normalizeUserCode returns a user code as the person typed it in the
// form newUserCode makes.
func normalizeUserCode(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		if strings.ContainsRune(userCodeAlphabet, r) {
			b.WriteRune(r)
		}
	}
	c := b.String()
	if len(c) != 8 {
		return ""
	}
	return c[:4] + "-" + c[4:]
}

// forget drops the sign-in deviceCode. i.mu is held.
func (i *Issuer) forget(deviceCode string, d *device) {
	delete(i.devices, deviceCode)
	delete(i.byUser, d.userCode)
	delete(i.byLink, d.link)
}

// prune drops what expired. i.mu is held.
func (i *Issuer) prune(now time.Time) {
	for k, d := range i.devices {
		if now.After(d.expires) {
			i.forget(k, d)
		}
	}
	for k, p := range i.signins {
		if now.After(p.expires) {
			delete(i.signins, k)
		}
	}
	for k, a := range i.approvals {
		if now.After(a.expires) {
			delete(i.approvals, k)
		}
	}
}

// devicePage sends the sign-in of a printed link, or of a code the person
// typed, to Google's sign-in, and asks for the code otherwise.
func (i *Issuer) devicePage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	now := i.opts.Now()
	var deviceCode string
	switch {
	case q.Get("link") != "":
		i.mu.Lock()
		i.prune(now)
		dc, ok := i.byLink[q.Get("link")]
		i.mu.Unlock()
		if !ok {
			i.page(w, http.StatusNotFound, pageData{Ask: true, Message: "This link expired. Type the code the agent printed, or start the agent again for a new link."})
			return
		}
		deviceCode = dc
	case q.Get("code") != "":
		// Wrong codes share one backoff, since people behind the relay's
		// proxy share an address. A code that matches doesn't reset it, or
		// a sign-in of one's own would let guessing go on.
		if wait := i.lookups.Wait(); wait > 0 {
			i.page(w, http.StatusTooManyRequests, pageData{Ask: true, Message: "Too many wrong codes. Try again in a few seconds."})
			return
		}
		i.mu.Lock()
		i.prune(now)
		dc, ok := i.byUser[normalizeUserCode(q.Get("code"))]
		i.mu.Unlock()
		if !ok {
			i.lookups.Fail()
			i.page(w, http.StatusNotFound, pageData{Ask: true, Message: "This code is unknown or expired. Check it, or start the agent again for a new one."})
			return
		}
		deviceCode = dc
	default:
		i.page(w, http.StatusOK, pageData{Ask: true})
		return
	}
	i.startGoogleSignIn(w, r, deviceCode)
}

// startGoogleSignIn sends the person to Google's sign-in for the sign-in
// deviceCode.
func (i *Issuer) startGoogleSignIn(w http.ResponseWriter, r *http.Request, deviceCode string) {
	p := &pendingSignIn{
		deviceCode: deviceCode,
		verifier:   cryptoutil.RandomString(64, cryptoutil.Alphanumeric),
		nonce:      cryptoutil.RandomString(32, cryptoutil.Alphanumeric),
	}
	state := cryptoutil.RandomString(32, cryptoutil.Alphanumeric)
	i.mu.Lock()
	d, ok := i.devices[deviceCode]
	if !ok {
		i.mu.Unlock()
		i.page(w, http.StatusNotFound, pageData{Ask: true, Message: "This sign-in expired. Start the agent again for a new link."})
		return
	}
	p.expires = d.expires
	p.seq = i.nextSeq()
	i.dropExtraSignIns(deviceCode)
	i.signins[state] = p
	i.mu.Unlock()
	webpage.SetSecurityHeaders(w)
	http.Redirect(w, r, i.opts.Google.OfflineAuthCodeURL(i.opts.ClientID, CallbackURL(i.opts.URL), state, p.nonce, googleauth.Challenge(p.verifier)), http.StatusFound)
}

// maxSignInsPerDevice bounds the trips to Google a sign-in has open, so
// that all of them stay below MaxPending times it.
const maxSignInsPerDevice = 3

// nextSeq orders the trips to Google and approvals of a sign-in, which all
// share its expiry. i.mu is held.
func (i *Issuer) nextSeq() uint64 {
	i.seq++
	return i.seq
}

// dropExtraSignIns makes room for one more trip to Google of deviceCode by
// dropping its oldest. i.mu is held.
func (i *Issuer) dropExtraSignIns(deviceCode string) {
	var oldest string
	n := 0
	for k, p := range i.signins {
		if p.deviceCode != deviceCode {
			continue
		}
		n++
		if oldest == "" || p.seq < i.signins[oldest].seq {
			oldest = k
		}
	}
	if n >= maxSignInsPerDevice {
		delete(i.signins, oldest)
	}
}

var errNonce = errors.New("the Google sign-in doesn't belong to this request")

// callback takes the account back from Google, checks it against the
// allowlist, and asks the person to approve the agent's sign-in.
func (i *Issuer) callback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	now := i.opts.Now()
	i.mu.Lock()
	p, ok := i.signins[q.Get("state")]
	delete(i.signins, q.Get("state"))
	i.mu.Unlock()
	if !ok || now.After(p.expires) {
		i.page(w, http.StatusBadRequest, pageData{Message: "This sign-in expired. Open the link the agent printed again."})
		return
	}
	if e := q.Get("error"); e != "" {
		i.page(w, http.StatusBadRequest, pageData{Message: "Google sign-in didn't finish (" + e + "). Open the link the agent printed to try again."})
		return
	}
	t, err := i.opts.Google.ExchangeTokens(r.Context(), i.opts.HTTP, i.opts.ClientID, i.opts.ClientSecret, q.Get("code"), CallbackURL(i.opts.URL), p.verifier)
	if err != nil {
		slog.Warn("relay sign-in: exchanging Google's code", "error", err)
		i.page(w, http.StatusBadGateway, pageData{Message: "Google refused the sign-in. Open the link the agent printed to try again."})
		return
	}
	claims, err := i.verifier.Verify(r.Context(), t.IDToken, []string{i.opts.ClientID})
	if err == nil && subtle.ConstantTimeCompare([]byte(claims.Nonce), []byte(p.nonce)) != 1 {
		err = errNonce
	}
	if err != nil {
		slog.Warn("relay sign-in: verifying Google's ID token", "error", err)
		i.page(w, http.StatusBadGateway, pageData{Message: "The Google sign-in couldn't be verified. Open the link the agent printed to try again."})
		return
	}
	if !i.allowed(claims) {
		email := strings.ToLower(claims.Email)
		slog.Warn("relay sign-in refused", "email", email)
		i.page(w, http.StatusForbidden, pageData{Message: email + " may not join this relay. Open the link the agent printed again to sign in with another account, or ask the relay's operator to allow this one."})
		return
	}
	if t.RefreshToken == "" {
		i.page(w, http.StatusBadGateway, pageData{Message: "Google didn't let the relay keep the sign-in. Open the link the agent printed to try again."})
		return
	}
	token := cryptoutil.RandomString(32, cryptoutil.Alphanumeric)
	i.mu.Lock()
	d, ok := i.devices[p.deviceCode]
	if ok {
		i.dropExtraApprovals(p.deviceCode)
		i.approvals[token] = &approval{deviceCode: p.deviceCode, claims: claims, google: t.RefreshToken, expires: d.expires, seq: i.nextSeq()}
	}
	i.mu.Unlock()
	if !ok {
		i.page(w, http.StatusBadRequest, pageData{Message: "This sign-in expired. Start the agent again for a new link."})
		return
	}
	i.page(w, http.StatusOK, pageData{Confirm: true, Token: token, Email: strings.ToLower(claims.Email), Code: d.userCode, Relay: i.opts.URL})
}

// dropExtraApprovals makes room for one more approval of deviceCode by
// dropping its oldest. i.mu is held.
func (i *Issuer) dropExtraApprovals(deviceCode string) {
	var oldest string
	n := 0
	for k, a := range i.approvals {
		if a.deviceCode != deviceCode {
			continue
		}
		n++
		if oldest == "" || a.seq < i.approvals[oldest].seq {
			oldest = k
		}
	}
	if n >= maxSignInsPerDevice {
		delete(i.approvals, oldest)
	}
}

// approve takes the person's answer to the agent's sign-in.
func (i *Issuer) approve(w http.ResponseWriter, r *http.Request) {
	i.mu.Lock()
	a, ok := i.approvals[r.FormValue("token")]
	delete(i.approvals, r.FormValue("token"))
	i.mu.Unlock()
	if !ok || i.opts.Now().After(a.expires) {
		i.page(w, http.StatusBadRequest, pageData{Message: "This sign-in expired. Open the link the agent printed again."})
		return
	}
	if r.FormValue("answer") != "approve" {
		i.answer(a.deviceCode, nil, "the person declined the sign-in")
		i.page(w, http.StatusOK, pageData{Message: "Declined. The agent didn't join the relay."})
		return
	}
	idToken, err := i.issue(a.claims)
	if err != nil {
		slog.Error("relay sign-in: issuing an ID token", "error", err)
		i.page(w, http.StatusInternalServerError, pageData{Message: "The relay couldn't finish the sign-in. Open the link the agent printed to try again."})
		return
	}
	refresh, err := i.keys.sealSession(session{Subject: a.claims.Subject, Email: strings.ToLower(a.claims.Email), Google: a.google}, i.url)
	if err != nil {
		slog.Error("relay sign-in: sealing a refresh token", "error", err)
		i.page(w, http.StatusInternalServerError, pageData{Message: "The relay couldn't finish the sign-in. Open the link the agent printed to try again."})
		return
	}
	if !i.answer(a.deviceCode, &tokens{idToken: idToken, refreshToken: refresh}, "") {
		i.page(w, http.StatusBadRequest, pageData{Message: "This sign-in expired. Start the agent again for a new link."})
		return
	}
	slog.Info("relay sign-in", "email", strings.ToLower(a.claims.Email))
	i.page(w, http.StatusOK, pageData{Message: "Signed in as " + strings.ToLower(a.claims.Email) + ". The agent joins the relay now; you can close this tab."})
}

// answer records the answer to the sign-in deviceCode, and reports whether
// it is still in progress.
func (i *Issuer) answer(deviceCode string, t *tokens, denied string) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	d, ok := i.devices[deviceCode]
	if !ok || i.opts.Now().After(d.expires) {
		return false
	}
	d.tokens, d.denied = t, denied
	return true
}

type pageData struct {
	Ask, Confirm bool
	Message      string
	Token        string
	Email, Code  string
	Relay        string
}

var page = template.Must(template.New("signin").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Relay sign-in</title>
<style>body{font-family:system-ui,sans-serif;max-width:40rem;margin:2rem auto;padding:0 1rem}
code{font-size:1.25rem;background:#f4f4f4;padding:.25rem .5rem;border-radius:.25rem}
input,button{font-size:1rem;margin:.25rem;padding:.5rem 1rem}</style></head>
<body><h2>Relay sign-in</h2>
{{if .Message}}<p>{{.Message}}</p>{{end}}
{{if .Ask}}<form method="get"><label>Code the agent printed: <input name="code" autocomplete="off" autofocus></label><button>Continue</button></form>{{end}}
{{if .Confirm}}<p>Sign an agent in to the relay {{.Relay}} as <b>{{.Email}}</b>?</p>
<p>Approve only if you started this agent and it printed the code <code>{{.Code}}</code>.</p>
<form method="post" action="approve"><input type="hidden" name="token" value="{{.Token}}">
<button name="answer" value="approve">Approve</button><button name="answer" value="decline">Decline</button></form>{{end}}
</body></html>`))

func (i *Issuer) page(w http.ResponseWriter, status int, d pageData) {
	webpage.SetSecurityHeaders(w)
	// The code form leads to Google's sign-in through a redirect, which
	// browsers check against form-action too.
	if u, err := url.Parse(i.opts.Google.AuthURL); err == nil && u.Host != "" {
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self' "+u.Scheme+"://"+u.Host+"; frame-ancestors 'none'")
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := page.Execute(w, d); err != nil {
		slog.Debug("writing the sign-in page", "error", err)
	}
}
