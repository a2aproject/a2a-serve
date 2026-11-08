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

package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestUserIdentities(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	now := time.Now()
	s.now = func() time.Time { return now }
	for _, u := range []struct {
		id, identity string
		age          time.Duration
	}{
		{id: "pair:ben", identity: `{"pairing":"ben"}`},
		{id: "pair:ben", identity: `{"pairing":"ben","network":"public"}`},
		{id: "email:ann@example.com", identity: `{"email":"ann@example.com"}`},
		{id: "email:old@example.com", identity: `{}`, age: UserIdentityRetention + time.Hour},
	} {
		s.now = func() time.Time { return now.Add(-u.age) }
		if err := s.SaveUserIdentity(ctx, u.id, u.identity); err != nil {
			t.Fatalf("SaveUserIdentity() error = %v", err)
		}
	}
	s.now = func() time.Time { return now }
	if got, err := s.UserIdentity(ctx, "pair:ben"); err != nil || got != `{"pairing":"ben","network":"public"}` {
		t.Fatalf("UserIdentity() = %q, %v, want the last one saved", got, err)
	}

	if _, err := s.EnsurePairing(ctx, Pairing{Name: "ben", Role: "authenticated"}); err != nil {
		t.Fatalf("EnsurePairing() error = %v", err)
	}
	if err := s.DeletePairing(ctx, "ben", PairingUser{Principal: "pair:ben", AgentKey: "user:pair:ben"}); err != nil {
		t.Fatalf("DeletePairing() error = %v", err)
	}
	if err := s.Prune(ctx); err != nil {
		t.Fatalf("Prune() error = %v", err)
	}

	for id, want := range map[string]error{"pair:ben": ErrNotFound, "email:old@example.com": ErrNotFound, "email:ann@example.com": nil} {
		if _, err := s.UserIdentity(ctx, id); !errors.Is(err, want) {
			t.Fatalf("UserIdentity(%s) error = %v, want %v", id, err, want)
		}
	}
}
