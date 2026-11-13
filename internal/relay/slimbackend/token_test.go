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
	"strings"
	"testing"
	"time"
)

var testNow = time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)

const testRelay = "https://relay.example.com"

var testAud = GateAudience(testRelay)

// sign returns an EdDSA JWT with header and claims, signed with key.
func sign(t *testing.T, key ed25519.PrivateKey, header, claims map[string]any) string {
	t.Helper()
	enc := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	in := enc(header) + "." + enc(claims)
	return in + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, []byte(in)))
}

// memberToken returns a valid token of the agent named agent at testNow.
func memberToken(t *testing.T, key ed25519.PrivateKey, agent string) string {
	t.Helper()
	return memberTokenAt(t, key, agent, testNow)
}

func memberTokenAt(t *testing.T, key ed25519.PrivateKey, agent string, now time.Time) string {
	t.Helper()
	return sign(t, key, map[string]any{"alg": "EdDSA", "kid": SLIMName(agent)}, map[string]any{
		"sub": SLIMName(agent), "aud": []string{testAud}, "iat": now.Unix(), "exp": now.Add(10 * time.Minute).Unix(),
	})
}

func TestVerifyToken(t *testing.T) {
	t.Parallel()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	_, other, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	coder := SLIMName("coder")
	header := map[string]any{"alg": "EdDSA", "kid": coder}
	claims := func(change func(map[string]any)) map[string]any {
		c := map[string]any{"sub": coder, "aud": testAud, "iat": testNow.Unix(), "exp": testNow.Add(10 * time.Minute).Unix()}
		if change != nil {
			change(c)
		}
		return c
	}
	tests := []struct {
		name  string
		token string
		want  string
	}{
		{name: "valid", token: sign(t, priv, header, claims(nil)), want: "coder"},
		{name: "audience list of the gate alone", token: sign(t, priv, header, claims(func(c map[string]any) { c["aud"] = []string{testAud} })), want: "coder"},
		{name: "identity token", token: sign(t, priv, header, claims(func(c map[string]any) { c["aud"] = testRelay }))},
		{name: "gate and identity audiences", token: sign(t, priv, header, claims(func(c map[string]any) { c["aud"] = []string{testAud, testRelay} }))},
		{name: "another relay", token: sign(t, priv, header, claims(func(c map[string]any) { c["aud"] = GateAudience("https://other.example.com") }))},
		{name: "subject isn't the key's", token: sign(t, priv, header, claims(func(c map[string]any) { c["sub"] = SLIMName("reviewer") }))},
		{name: "expired", token: sign(t, priv, header, claims(func(c map[string]any) { c["exp"] = testNow.Add(-time.Second).Unix() }))},
		{name: "lasts too long", token: sign(t, priv, header, claims(func(c map[string]any) { c["exp"] = testNow.Add(2 * time.Hour).Unix() }))},
		{name: "issued in the future", token: sign(t, priv, header, claims(func(c map[string]any) { c["iat"] = testNow.Add(time.Hour).Unix() }))},
		{name: "unknown member", token: sign(t, priv, map[string]any{"alg": "EdDSA", "kid": SLIMName("stranger")}, claims(func(c map[string]any) { c["sub"] = SLIMName("stranger") }))},
		{name: "another key", token: sign(t, other, header, claims(nil))},
		{name: "another algorithm", token: sign(t, priv, map[string]any{"alg": "none", "kid": coder}, claims(nil))},
		{name: "too long", token: strings.Repeat("a", maxTokenSize+1)},
		{name: "not a JWT", token: "garbage"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := verifyToken(tt.token, testAud, testNow, func(slimName string) (string, ed25519.PublicKey, bool) {
				if slimName != coder {
					return "", nil, false
				}
				return "coder", pub, true
			})
			if tt.want == "" {
				if err == nil {
					t.Fatalf("verifyToken() = %q, want an error", got)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("verifyToken() = %q, %v, want %q", got, err, tt.want)
			}
		})
	}
}
