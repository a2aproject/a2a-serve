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

package pairings

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/messenger"
	"github.com/a2aproject/a2a-serve/internal/pairapi"
	"github.com/a2aproject/a2a-serve/internal/store"
)

const base = "https://coder.example.com"

func chatVia(identity string) Via {
	return Via{Chat: &messenger.Inbound{Messenger: "telegram", Identity: identity, Authentic: true}}
}

func pending(t *testing.T, s *Service, code string) bool {
	t.Helper()
	_, err := s.deps.Store.TakeCode(context.Background(), auth.NormalizeCode(code), store.AnyCode)
	return err == nil
}

func TestRedeem(t *testing.T) {
	t.Parallel()
	ignoreToken := cmpopts.IgnoreFields(Redemption{}, "Token")
	tests := []struct {
		name string
		// code returns the code to redeem.
		code        func(t *testing.T, s *Service) string
		via         Via
		want        *Redemption
		wantErr     error
		wantKind    errKind
		wantPending bool
	}{
		{
			name: "caller code",
			code: func(t *testing.T, s *Service) string {
				return invite(t, s, InviteRequest{Name: "anna"}).Code
			},
			via:  Via{Client: "1.2.3.4", URL: base, Kind: pairapi.PairingDevice},
			want: &Redemption{Kind: pairapi.KindCaller, Pairing: "anna", Principal: auth.PairPrincipal("anna"), URL: base},
		},
		{
			name: "node code",
			code: func(t *testing.T, s *Service) string {
				return invite(t, s, InviteRequest{Name: "grok", Webhook: "https://example.com/hook"}).Code
			},
			via:  Via{Client: "1.2.3.4", URL: base},
			want: &Redemption{Kind: pairapi.KindNode, Pairing: "grok", Principal: auth.NodePrincipal("grok"), URL: base + "/n/grok", Node: "grok"},
		},
		{
			name:        "local code over HTTP",
			code:        localCode,
			via:         Via{Client: "1.2.3.4", URL: base},
			wantErr:     ErrLocalCode,
			wantPending: true,
		},
		{
			name: "unknown kind",
			code: func(t *testing.T, s *Service) string {
				return invite(t, s, InviteRequest{Name: "anna"}).Code
			},
			via:         Via{Client: "1.2.3.4", URL: base, Kind: pairapi.PairingWebhook},
			wantKind:    Invalid,
			wantPending: true,
		},
		{
			name:    "wrong code",
			code:    func(*testing.T, *Service) string { return "WRONG-CODE" },
			via:     Via{Client: "1.2.3.4", URL: base},
			wantErr: ErrInvalidCode,
		},
		{
			name: "chat link",
			code: localCode,
			via:  chatVia("telegram:9"),
			want: &Redemption{Pairing: LocalOwner, Principal: auth.OwnerID, Role: auth.RoleOwner},
		},
		{
			name: "node code in chat",
			code: func(t *testing.T, s *Service) string {
				return invite(t, s, InviteRequest{Name: "grok", Webhook: "https://example.com/hook"}).Code
			},
			via:         chatVia("telegram:9"),
			wantErr:     ErrInvalidCode,
			wantPending: true,
		},
		{
			name:        "unverified chat sender",
			code:        localCode,
			via:         Via{Chat: &messenger.Inbound{Messenger: "telegram", Identity: "telegram:9"}},
			wantErr:     ErrUnverifiedSender,
			wantPending: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newService(t)
			code := tc.code(t, s)

			got, err := s.Redeem(context.Background(), code, tc.via)
			switch {
			case tc.wantErr != nil && !errors.Is(err, tc.wantErr):
				t.Fatalf("Redeem() error = %v, want %v", err, tc.wantErr)
			case tc.wantKind != "" && kindOf(err) != tc.wantKind:
				t.Fatalf("Redeem() error = %v, want %s", err, tc.wantKind)
			case tc.wantErr == nil && tc.wantKind == "" && err != nil:
				t.Fatalf("Redeem() error = %v", err)
			}
			if diff := cmp.Diff(tc.want, got, ignoreToken); diff != "" {
				t.Fatalf("Redeem() wrong result (-want +got) diff = %s", diff)
			}
			if got := pending(t, s, code); got != tc.wantPending {
				t.Fatalf("code pending after Redeem() = %v, want %v", got, tc.wantPending)
			}
		})
	}
}

func localCode(t *testing.T, s *Service) string {
	t.Helper()
	code, _, err := auth.MintCode(context.Background(), s.deps.Store, LocalOwner)
	if err != nil {
		t.Fatalf("MintCode() error = %v", err)
	}
	return code
}

func TestRedeemDoesNotRecreateRemovedNode(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newService(t)
	code := invite(t, s, InviteRequest{Name: "grok", Webhook: "https://example.com/hook"}).Code
	if _, err := s.Remove(ctx, "grok"); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}

	_, err := s.Redeem(ctx, code, Via{URL: base})

	if !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("Redeem() error = %v, want %v", err, ErrInvalidCode)
	}
	if _, err := s.deps.Store.Node(ctx, "grok"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Store.Node() error = %v, want %v", err, store.ErrNotFound)
	}
}

func TestRedeemIssuesWorkingCredentials(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newService(t)

	caller, err := s.Redeem(ctx, invite(t, s, InviteRequest{Name: "anna"}).Code, Via{URL: base, Kind: pairapi.PairingDevice})
	if err != nil {
		t.Fatalf("Redeem(caller code) error = %v", err)
	}
	tok, err := s.deps.Store.LookupToken(ctx, caller.Token)
	if err != nil || tok.Pairing != "anna" || tok.URL != base {
		t.Fatalf("LookupToken() = %+v, %v, want a token of anna at %s", tok, err, base)
	}
	if p, err := s.deps.Store.Pairing(ctx, "anna"); err != nil || p.Kind != pairapi.PairingDevice {
		t.Fatalf("Pairing(anna) = %+v, %v, want kind %s", p, err, pairapi.PairingDevice)
	}

	node, err := s.Redeem(ctx, invite(t, s, InviteRequest{Name: "grok", Webhook: "https://example.com/hook"}).Code, Via{URL: base})
	if err != nil {
		t.Fatalf("Redeem(node code) error = %v", err)
	}
	if got, ok, err := auth.NodeOfToken(ctx, s.deps.Store, node.Token); err != nil || !ok || got.ID != "grok" {
		t.Fatalf("NodeOfToken() = %+v, %v, %v, want node grok", got, ok, err)
	}

	in := &messenger.Inbound{Messenger: "telegram", Identity: "telegram:9", Conv: "dm9", Direct: true, Authentic: true}
	if _, err := s.Redeem(ctx, invite(t, s, InviteRequest{Name: "ben"}).Code, Via{Chat: in}); err != nil {
		t.Fatalf("Redeem(chat) error = %v", err)
	}
	if l, err := s.deps.Store.LinkOf(ctx, "telegram", "telegram:9"); err != nil || l.Pairing != "ben" || l.Conv != "dm9" {
		t.Fatalf("LinkOf() = %+v, %v, want a link to ben in dm9", l, err)
	}
}

func TestRedeemLimitsFailuresPerClient(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		via   Via
		other Via
	}{
		{name: "HTTP", via: Via{Client: "1.2.3.4", URL: base}, other: Via{Client: "5.6.7.8", URL: base}},
		{name: "chat", via: chatVia("telegram:1"), other: chatVia("telegram:2")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			s := newService(t)
			for range maxFailures - 1 {
				if _, err := s.Redeem(ctx, "WRONG-CODE", tc.via); !errors.Is(err, ErrInvalidCode) {
					t.Fatalf("Redeem(wrong code) error = %v, want %v", err, ErrInvalidCode)
				}
			}
			if _, err := s.Redeem(ctx, invite(t, s, InviteRequest{Name: "anna"}).Code, tc.via); err != nil {
				t.Fatalf("Redeem() of a valid code below the limit error = %v", err)
			}
			if _, err := s.Redeem(ctx, "WRONG-CODE", tc.via); !errors.Is(err, ErrInvalidCode) {
				t.Fatalf("Redeem(wrong code) error = %v, want %v", err, ErrInvalidCode)
			}

			if _, err := s.Redeem(ctx, invite(t, s, InviteRequest{Name: "ben"}).Code, tc.via); err != nil {
				t.Fatalf("Redeem() of a valid code over the limit error = %v", err)
			}
			if _, err := s.Redeem(ctx, "WRONG-CODE", tc.via); !errors.Is(err, ErrInvalidCode) {
				t.Fatalf("Redeem(wrong code) over the limit error = %v, want %v", err, ErrInvalidCode)
			}
			code := invite(t, s, InviteRequest{Name: "dora"}).Code
			var limited *RateLimitError
			if _, err := s.Redeem(ctx, code, tc.via); !errors.As(err, &limited) {
				t.Fatalf("Redeem() over the limit during the backoff error = %v, want a rate limit", err)
			}
			if limited.Wait > backoffMax {
				t.Fatalf("RateLimitError.Wait over the limit = %v, want at most %v", limited.Wait, backoffMax)
			}
			if !pending(t, s, code) {
				t.Fatalf("code pending after a rate-limited Redeem() = false, want true")
			}
			if _, err := s.Redeem(ctx, invite(t, s, InviteRequest{Name: "carl"}).Code, tc.other); err != nil {
				t.Fatalf("Redeem() by another client error = %v", err)
			}
		})
	}
}

func TestRedeemBacksOffClientsThatShareAnAddress(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newService(t)
	if _, err := s.Redeem(ctx, "WRONG-CODE", Via{URL: base}); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("Redeem(wrong code) error = %v, want %v", err, ErrInvalidCode)
	}
	var limited *RateLimitError
	if _, err := s.Redeem(ctx, invite(t, s, InviteRequest{Name: "anna"}).Code, Via{URL: base}); !errors.As(err, &limited) {
		t.Fatalf("Redeem() during the backoff error = %v, want a rate limit", err)
	}
	if limited.Wait <= 0 {
		t.Fatalf("RateLimitError.Wait = %v, want > 0", limited.Wait)
	}
	if _, err := s.Redeem(ctx, invite(t, s, InviteRequest{Name: "ben"}).Code, Via{Client: "1.2.3.4", URL: base}); err != nil {
		t.Fatalf("Redeem() by an identified client error = %v", err)
	}
}

func TestRedeemCountsConcurrentAttempts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newService(t)
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		attempts int
	)
	for range 5 * maxFailures {
		wg.Go(func() {
			_, err := s.Redeem(ctx, "WRONG-CODE", Via{Client: "1.2.3.4", URL: base})
			var limited *RateLimitError
			if errors.As(err, &limited) {
				return
			}
			mu.Lock()
			attempts++
			mu.Unlock()
		})
	}
	wg.Wait()
	// One attempt over the client's limit goes through the shared backoff.
	if want := maxFailures + 1; attempts != want {
		t.Fatalf("Redeem() tried %d codes at once, want %d", attempts, want)
	}
}

func TestLinkCode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		text   string
		want   string
		wantOK bool
	}{
		{text: "link K7Q2-9XF4", want: "K7Q2-9XF4", wantOK: true},
		{text: "join K7Q2-9XF4", want: "K7Q2-9XF4", wantOK: true},
		{text: "/join k7q29xf4", want: "k7q29xf4", wantOK: true},
		{text: "/start K7Q29XF4", want: "K7Q29XF4", wantOK: true},
		{text: "/link@my_bot k7q2-9xf4", want: "k7q2-9xf4", wantOK: true},
		{text: "please link K7Q2-9XF4 now"},
		{text: "/start"},
	}
	for _, tc := range tests {
		got, ok := LinkCode(tc.text)
		if ok != tc.wantOK || got != tc.want {
			t.Errorf("LinkCode(%q) = %q, %v, want %q, %v", tc.text, got, ok, tc.want, tc.wantOK)
		}
	}
}

type redeemingUsers struct {
	s    *Service
	code string
	err  error
}

func (u *redeemingUsers) Stop(ctx context.Context, principals ...string) (func(), error) {
	_, u.err = u.s.Redeem(ctx, u.code, Via{URL: base})
	return func() {}, nil
}

func TestRedeemRefusesPairingBeingRemoved(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	users := &redeemingUsers{}
	s := newService(t, withUsers(users, nil))
	users.s, users.code = s, invite(t, s, InviteRequest{Name: "anna"}).Code

	if _, err := s.Remove(ctx, "anna"); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}

	if !errors.Is(users.err, ErrInvalidCode) {
		t.Fatalf("Redeem() while anna is removed error = %v, want %v", users.err, ErrInvalidCode)
	}
	if _, err := s.Redeem(ctx, users.code, Via{Client: "9.9.9.9", URL: base}); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("Redeem() after anna is removed error = %v, want %v", err, ErrInvalidCode)
	}
}

func TestRateLimitErrorMessage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		wait time.Duration
		want string
	}{
		{wait: 300 * time.Millisecond, want: "too many invalid pairing codes; try again in 1 second"},
		{wait: 1500 * time.Millisecond, want: "too many invalid pairing codes; try again in 2 seconds"},
	}
	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			t.Parallel()
			if got := (&RateLimitError{Wait: tc.wait}).Error(); got != tc.want {
				t.Fatalf("RateLimitError.Error() = %q, want %q", got, tc.want)
			}
		})
	}
}
