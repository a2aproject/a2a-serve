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

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/a2aproject/a2a-serve/internal/cardinfo"
)

func TestContactCard(t *testing.T) {
	t.Parallel()
	card := cardinfo.Summary{Description: "Writes Go.", Skills: []cardinfo.Skill{{ID: "review", Name: "Code review"}}}
	tests := []struct {
		name     string
		url      string
		card     cardinfo.Summary
		wantErr  error
		wantCard cardinfo.Summary
	}{
		{name: "same URL", url: "http://coder", card: card, wantCard: card},
		{name: "cleared", url: "http://coder", wantCard: cardinfo.Summary{}},
		{name: "moved contact", url: "http://old", card: card, wantErr: ErrNotFound, wantCard: cardinfo.Summary{Description: "Old."}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, ctx := openStore(t), context.Background()
			if err := s.PutContact(ctx, Contact{Name: "coder", URL: "http://coder", Via: "direct", Card: cardinfo.Summary{Description: "Old."}}); err != nil {
				t.Fatalf("PutContact() error = %v", err)
			}
			if err := s.SetContactCard(ctx, "coder", tc.url, tc.card); !errors.Is(err, tc.wantErr) {
				t.Fatalf("SetContactCard() error = %v, want %v", err, tc.wantErr)
			}
			got, err := s.Contact(ctx, "coder")
			if err != nil {
				t.Fatalf("Contact() error = %v", err)
			}
			if diff := cmp.Diff(tc.wantCard, got.Card, cmpopts.EquateEmpty()); diff != "" {
				t.Fatalf("Contact() wrong card (-want +got) diff = %s", diff)
			}
			all, err := s.Contacts(ctx)
			if err != nil {
				t.Fatalf("Contacts() error = %v", err)
			}
			if diff := cmp.Diff(tc.wantCard, all[0].Card, cmpopts.EquateEmpty()); diff != "" {
				t.Fatalf("Contacts() wrong card (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestSetContactCardOfRemovedContact(t *testing.T) {
	t.Parallel()
	s, ctx := openStore(t), context.Background()
	if err := s.SetContactCard(ctx, "coder", "http://coder", cardinfo.Summary{Description: "Writes Go."}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetContactCard() error = %v, want %v", err, ErrNotFound)
	}
}
