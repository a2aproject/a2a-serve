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

package manage

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/schedule"
	"github.com/a2aproject/a2a-serve/internal/store"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	return s
}

func TestResolveUser(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	if _, err := s.EnsurePairing(context.Background(), store.Pairing{Name: "anna", Kind: "person", Role: auth.RoleAuthenticated}); err != nil {
		t.Fatalf("EnsurePairing() error = %v", err)
	}
	tests := []struct {
		who     string
		want    string
		wantErr bool
	}{
		{who: "", want: auth.OwnerID},
		{who: auth.OwnerName, want: auth.OwnerID},
		{who: auth.OwnerID, want: auth.OwnerID},
		{who: auth.LocalPairing, want: auth.OwnerID},
		{who: "pair:local", want: auth.OwnerID},
		{who: "anna", want: "pair:anna"},
		{who: "pair:anna", want: "pair:anna"},
		{who: "nobody", wantErr: true},
		{who: "email:anna@example.com", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.who, func(t *testing.T) {
			t.Parallel()
			got, err := ResolveUser(context.Background(), s, tc.who)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ResolveUser(%q) error = %v, want error %v", tc.who, err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("ResolveUser(%q) = %q, want %q", tc.who, got, tc.want)
			}
		})
	}
}

func TestAddScheduleAsLocalRunsAsOwner(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	if err := s.PutLink(ctx, store.Link{Channel: "telegram", Identity: "telegram:1", Pairing: auth.LocalPairing, Conv: "dm1"}, ""); err != nil {
		t.Fatalf("PutLink() error = %v", err)
	}

	got, err := Schedules{Store: s}.Add(ctx, schedule.Request{Name: "brief", Cron: "@daily", Prompt: "brief me", As: auth.LocalPairing, Chat: "telegram:local"}, auth.Owner(), "")
	if err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	want := struct{ User, DeliverUser, Chat string }{User: auth.OwnerID, DeliverUser: auth.OwnerID, Chat: "telegram:owner"}
	if diff := cmp.Diff(want, struct{ User, DeliverUser, Chat string }{got.UserID, got.DeliverUser, ScheduleInfo(got).Chat}); diff != "" {
		t.Fatalf("Add() wrong result (-want +got) diff = %s", diff)
	}
}
