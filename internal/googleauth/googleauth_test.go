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

package googleauth_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/googleauth"
	"github.com/a2aproject/a2a-serve/internal/googleauth/googletest"
)

func TestVerify(t *testing.T) {
	t.Parallel()
	g := googletest.New(t, googletest.Account{Email: "mom@example.com", Verified: true, HostedDomain: "example.com"})
	v := &googleauth.Verifier{Endpoints: g.Endpoints()}
	ctx := context.Background()

	got, err := v.Verify(ctx, g.IDToken("app", "n1"), []string{"other", "app"})
	if err != nil {
		t.Fatalf("Verifier.Verify() error = %v", err)
	}
	want := googleauth.Claims{Email: "mom@example.com", EmailVerified: true, HostedDomain: "example.com", Nonce: "n1", Audience: "app"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("Verifier.Verify() wrong result (-want +got) diff = %s", diff)
	}

	tok := g.IDToken("app", "n1")
	parts := strings.Split(tok, ".")
	tests := []struct {
		name      string
		token     string
		audiences []string
		now       time.Time
	}{
		{name: "other app", token: tok, audiences: []string{"other"}},
		{name: "changed claims", token: parts[0] + "." + parts[1] + "x." + parts[2], audiences: []string{"app"}},
		{name: "not a token", token: "abc", audiences: []string{"app"}},
		{name: "expired", token: tok, audiences: []string{"app"}, now: time.Now().Add(2 * time.Hour)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := &googleauth.Verifier{Endpoints: g.Endpoints()}
			if !tc.now.IsZero() {
				v.Now = func() time.Time { return tc.now }
			}
			if _, err := v.Verify(ctx, tc.token, tc.audiences); err == nil {
				t.Fatalf("Verifier.Verify() error = nil, want error")
			}
		})
	}
}

func TestAllowed(t *testing.T) {
	t.Parallel()
	list := []string{"Mom@Example.org", "*@example.com"}
	tests := []struct {
		name   string
		claims googleauth.Claims
		want   bool
	}{
		{name: "listed email", claims: googleauth.Claims{Email: "mom@example.org", EmailVerified: true}, want: true},
		{name: "workspace account of a listed domain", claims: googleauth.Claims{Email: "ben@example.com", EmailVerified: true, HostedDomain: "example.com"}, want: true},
		{name: "personal account with a listed domain", claims: googleauth.Claims{Email: "ben@example.com", EmailVerified: true}},
		{name: "unverified email", claims: googleauth.Claims{Email: "mom@example.org"}},
		{name: "other email", claims: googleauth.Claims{Email: "eve@example.net", EmailVerified: true}},
		{name: "subdomain", claims: googleauth.Claims{Email: "ben@sub.example.com", EmailVerified: true, HostedDomain: "sub.example.com"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := googleauth.Allowed(list, tc.claims); got != tc.want {
				t.Fatalf("Allowed(%v, %+v) = %v, want %v", list, tc.claims, got, tc.want)
			}
		})
	}
	if !googleauth.AllowedEmail(list, "ben@example.com") || googleauth.AllowedEmail(list, "eve@example.net") {
		t.Fatalf("AllowedEmail() disagrees with the allowlist %v", list)
	}
}
