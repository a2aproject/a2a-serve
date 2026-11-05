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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
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

// jwksTransport counts the requests it sends. With hold set, every request
// after the first signals entered and waits until hold is closed.
type jwksTransport struct {
	fetches atomic.Int32
	hold    chan struct{}
	entered chan struct{}
}

func (tr *jwksTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if tr.fetches.Add(1) > 1 && tr.hold != nil {
		tr.entered <- struct{}{}
		<-tr.hold
	}
	return http.DefaultTransport.RoundTrip(r)
}

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// withKid returns tok with its key ID replaced by kid.
func withKid(t *testing.T, tok, kid string) string {
	t.Helper()
	header, err := json.Marshal(map[string]string{"alg": "RS256", "kid": kid, "typ": "JWT"})
	if err != nil {
		t.Fatal(err)
	}
	_, rest, _ := strings.Cut(tok, ".")
	return base64.RawURLEncoding.EncodeToString(header) + "." + rest
}

func TestVerifyRateLimitsFetchesForUnknownKeys(t *testing.T) {
	t.Parallel()
	g := googletest.New(t, googletest.Account{Email: "mom@example.com", Verified: true})
	tr := &jwksTransport{}
	c := &clock{now: time.Now()}
	v := &googleauth.Verifier{Endpoints: g.Endpoints(), HTTP: &http.Client{Transport: tr}, Now: c.Now}
	ctx := context.Background()
	tok := g.IDToken("app", "n1")
	if _, err := v.Verify(ctx, tok, []string{"app"}); err != nil {
		t.Fatalf("Verifier.Verify() error = %v", err)
	}

	for i := range 5 {
		if _, err := v.Verify(ctx, withKid(t, tok, fmt.Sprintf("unknown-%d", i)), []string{"app"}); err == nil {
			t.Fatalf("Verifier.Verify() of an unknown key error = nil, want error")
		}
	}
	if got := tr.fetches.Load(); got != 1 {
		t.Fatalf("JWKS fetches for unknown keys within the refetch interval = %d, want 1", got)
	}

	c.advance(googleauth.KeyRefetchInterval)
	if _, err := v.Verify(ctx, withKid(t, tok, "unknown-later"), []string{"app"}); err == nil {
		t.Fatalf("Verifier.Verify() of an unknown key error = nil, want error")
	}
	if got := tr.fetches.Load(); got != 2 {
		t.Fatalf("JWKS fetches for an unknown key after the refetch interval = %d, want 2", got)
	}
}

func TestVerifyKnownKeyDuringFetch(t *testing.T) {
	t.Parallel()
	g := googletest.New(t, googletest.Account{Email: "mom@example.com", Verified: true})
	tr := &jwksTransport{hold: make(chan struct{}), entered: make(chan struct{})}
	c := &clock{now: time.Now()}
	v := &googleauth.Verifier{Endpoints: g.Endpoints(), HTTP: &http.Client{Transport: tr}, Now: c.Now}
	ctx := context.Background()
	tok := g.IDToken("app", "n1")
	if _, err := v.Verify(ctx, tok, []string{"app"}); err != nil {
		t.Fatalf("Verifier.Verify() error = %v", err)
	}
	c.advance(googleauth.KeyRefetchInterval)
	fetching := make(chan error, 1)
	go func() {
		_, err := v.Verify(ctx, withKid(t, tok, "unknown"), []string{"app"})
		fetching <- err
	}()
	<-tr.entered
	defer func() {
		close(tr.hold)
		if err := <-fetching; err == nil {
			t.Errorf("Verifier.Verify() of an unknown key error = nil, want error")
		}
	}()

	done := make(chan error, 1)
	go func() {
		_, err := v.Verify(ctx, tok, []string{"app"})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Verifier.Verify() during a key fetch error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Verifier.Verify() with a known key waited for the fetch of an unknown one")
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
}
