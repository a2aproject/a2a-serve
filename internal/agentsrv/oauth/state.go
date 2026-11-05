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

package oauth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"

	"github.com/a2aproject/a2a-serve/internal/cryptoutil"
)

// pendingSignIn is a sign-in sent to Google, which travels in the signed
// state of the request to Google and comes back in the callback.
type pendingSignIn struct {
	// ID derives the secrets of the sign-in that Google mustn't see in the
	// state: the PKCE verifier and the nonce.
	ID             string `json:"id"`
	Issuer         string `json:"iss"`
	ClientRedirect string `json:"redirect"`
	ClientState    string `json:"state,omitempty"`
	ClientID       string `json:"client"`
	Challenge      string `json:"challenge"`
	Expires        int64  `json:"exp"`
}

// stateSigner signs the state of sign-ins with a key that only this server
// process holds, so a restart ends the sign-ins in progress.
type stateSigner struct {
	key []byte
}

func newStateSigner() stateSigner {
	return stateSigner{key: []byte(cryptoutil.RandomString(43, cryptoutil.Alphanumeric))}
}

// newPending returns a sign-in with a new ID that expires after ttl.
func newPending(ttl time.Duration) pendingSignIn {
	return pendingSignIn{ID: cryptoutil.RandomString(22, cryptoutil.Alphanumeric), Expires: time.Now().Add(ttl).Unix()}
}

// seal returns the state that carries p: its JSON and the MAC of the JSON.
func (s stateSigner) seal(p pendingSignIn) (string, error) {
	body, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(body)
	return payload + "." + base64.RawURLEncoding.EncodeToString(s.mac("state", payload)), nil
}

// open returns the sign-in of a state that s sealed and that hasn't
// expired at now.
func (s stateSigner) open(state string, now time.Time) (pendingSignIn, bool) {
	payload, sig, ok := strings.Cut(state, ".")
	if !ok {
		return pendingSignIn{}, false
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, s.mac("state", payload)) {
		return pendingSignIn{}, false
	}
	body, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return pendingSignIn{}, false
	}
	var p pendingSignIn
	if err := json.Unmarshal(body, &p); err != nil || now.Unix() >= p.Expires {
		return pendingSignIn{}, false
	}
	return p, true
}

// googleVerifier is the PKCE verifier of p at Google.
func (s stateSigner) googleVerifier(p pendingSignIn) string {
	return base64.RawURLEncoding.EncodeToString(s.mac("google verifier", p.ID))
}

// nonce is the nonce of p's ID token.
func (s stateSigner) nonce(p pendingSignIn) string {
	return base64.RawURLEncoding.EncodeToString(s.mac("nonce", p.ID))
}

// mac is the HMAC-SHA256 of msg for the purpose label.
func (s stateSigner) mac(label, msg string) []byte {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(label + "\x00" + msg))
	return m.Sum(nil)
}
