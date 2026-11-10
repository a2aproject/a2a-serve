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
	"testing"
)

func TestVoiceReplies(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	for _, step := range []struct {
		user string
		on   bool
	}{
		{"pair:anna", true},
		{"pair:anna", true},
		{"pair:bob", true},
		{"pair:bob", false},
	} {
		if err := s.SetVoiceReplies(ctx, step.user, step.on); err != nil {
			t.Fatalf("SetVoiceReplies(%q, %v) error = %v", step.user, step.on, err)
		}
	}
	for user, want := range map[string]bool{"pair:anna": true, "pair:bob": false, "pair:carol": false} {
		got, err := s.VoiceReplies(ctx, user)
		if err != nil || got != want {
			t.Fatalf("VoiceReplies(%q) = %v, %v, want %v, nil", user, got, err, want)
		}
	}
}

func TestDeletePairingRemovesVoiceReplies(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	if _, err := s.EnsurePairing(ctx, Pairing{Name: "ben", Role: "member"}); err != nil {
		t.Fatalf("EnsurePairing() error = %v", err)
	}
	if err := s.SetVoiceReplies(ctx, "pair:ben", true); err != nil {
		t.Fatalf("SetVoiceReplies() error = %v", err)
	}
	if err := s.DeletePairing(ctx, "ben", PairingUser{Principal: "pair:ben", AgentKey: "user:pair:ben"}); err != nil {
		t.Fatalf("DeletePairing() error = %v", err)
	}
	if on, err := s.VoiceReplies(ctx, "pair:ben"); err != nil || on {
		t.Fatalf("VoiceReplies() after DeletePairing() = %v, %v, want false, nil", on, err)
	}
}
