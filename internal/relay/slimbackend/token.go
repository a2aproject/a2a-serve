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

package slimbackend

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"
)

// Bounds of the tokens members present. SLIM's signer refreshes a token
// before it expires; one that lasts longer than maxTokenLife was minted to
// outlive a removal.
const (
	maxTokenSize = 4096
	maxTokenLife = time.Hour
	clockSkew    = time.Minute
)

// errToken is returned for a token that doesn't prove membership. It says
// no more, so that a caller learns nothing about the relay's members.
var errToken = errors.New("the token doesn't belong to a member of the relay")

type tokenHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
}

type tokenClaims struct {
	Sub string   `json:"sub"`
	Aud audience `json:"aud"`
	Exp int64    `json:"exp"`
	Iat int64    `json:"iat"`
}

// audience is the aud claim, a string or a list of them.
type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*a = audience{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*a = many
	return nil
}

// GateAudience returns the audience of the tokens members present to the
// gate of the relay at relayURL. It differs from the audience of the
// identity tokens agents send each other through the node, so that a member
// can't replay another's identity token at the gate.
func GateAudience(relayURL string) string {
	return strings.TrimSuffix(relayURL, "/") + "/slim"
}

// verifyToken checks that token is an EdDSA JWT for aud alone that the
// member named by its kid signed with its key, and returns that member's
// agent. key returns the key of an active member by SLIM name.
func verifyToken(token, aud string, now time.Time, key func(slimName string) (agent string, k ed25519.PublicKey, ok bool)) (string, error) {
	if len(token) > maxTokenSize {
		return "", errToken
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", errToken
	}
	var h tokenHeader
	if err := decodePart(parts[0], &h); err != nil || h.Alg != "EdDSA" {
		return "", errToken
	}
	var c tokenClaims
	if err := decodePart(parts[1], &c); err != nil {
		return "", errToken
	}
	if h.Kid == "" || c.Sub != h.Kid || !slices.Equal(c.Aud, audience{aud}) {
		return "", errToken
	}
	exp, iat := time.Unix(c.Exp, 0), time.Unix(c.Iat, 0)
	if !now.Before(exp) || iat.After(now.Add(clockSkew)) || exp.Sub(now) > maxTokenLife+clockSkew {
		return "", errToken
	}
	agent, k, ok := key(h.Kid)
	if !ok {
		return "", errToken
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !ed25519.Verify(k, []byte(parts[0]+"."+parts[1]), sig) {
		return "", errToken
	}
	return agent, nil
}

func decodePart(s string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
