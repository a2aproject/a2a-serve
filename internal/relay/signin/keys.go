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
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
)

// keys are the issuer's keys. They derive from the relay's admin token, so
// a relay whose state doesn't last keeps them, and a new admin token signs
// everyone out.
type keys struct {
	sign ed25519.PrivateKey
	kid  string
	seal cipher.AEAD
}

func deriveKeys(secret string) (*keys, error) {
	seed, err := hkdf.Key(sha256.New, []byte(secret), nil, "a2a-serve relay sign-in: ID token key", ed25519.SeedSize)
	if err != nil {
		return nil, err
	}
	sealKey, err := hkdf.Key(sha256.New, []byte(secret), nil, "a2a-serve relay sign-in: refresh token key", 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(sealKey)
	if err != nil {
		return nil, err
	}
	seal, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	sign := ed25519.NewKeyFromSeed(seed)
	sum := sha256.Sum256(sign.Public().(ed25519.PublicKey))
	return &keys{sign: sign, kid: base64.RawURLEncoding.EncodeToString(sum[:12]), seal: seal}, nil
}

// jwks is the JSON Web Key Set of the ID token key.
func (k *keys) jwks() map[string]any {
	return map[string]any{"keys": []map[string]string{{
		"kty": "OKP", "crv": "Ed25519", "alg": "EdDSA", "use": "sig", "kid": k.kid,
		"x": base64.RawURLEncoding.EncodeToString(k.sign.Public().(ed25519.PublicKey)),
	}}}
}

// idToken signs claims as a JWT.
func (k *keys) idToken(claims map[string]any) (string, error) {
	header, err := json.Marshal(map[string]string{"alg": "EdDSA", "kid": k.kid, "typ": "JWT"})
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signed := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	return signed + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(k.sign, []byte(signed))), nil
}

// refreshPrefix starts the refresh tokens the issuer hands out.
const refreshPrefix = "a2rr."

// session is what a refresh token holds: the account, and Google's
// refresh token, which lets the issuer check the account again.
type session struct {
	Subject string `json:"sub"`
	Email   string `json:"email"`
	Google  string `json:"google"`
}

var errBadRefreshToken = errors.New("unknown refresh token")

// sealSession returns the refresh token of s, bound to the issuer url.
func (k *keys) sealSession(s session, url string) (string, error) {
	plain, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, k.seal.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return refreshPrefix + base64.RawURLEncoding.EncodeToString(k.seal.Seal(nonce, nonce, plain, []byte(url))), nil
}

// openSession returns the session of a refresh token from sealSession.
func (k *keys) openSession(token, url string) (session, error) {
	raw, ok := strings.CutPrefix(token, refreshPrefix)
	if !ok {
		return session{}, errBadRefreshToken
	}
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(data) < k.seal.NonceSize() {
		return session{}, errBadRefreshToken
	}
	plain, err := k.seal.Open(nil, data[:k.seal.NonceSize()], data[k.seal.NonceSize():], []byte(url))
	if err != nil {
		return session{}, errBadRefreshToken
	}
	var s session
	if err := json.Unmarshal(plain, &s); err != nil {
		return session{}, errBadRefreshToken
	}
	return s, nil
}
