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

package schedules

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agenterr"
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

	got, err := Service{Store: s}.Add(ctx, schedule.Request{Name: "brief", Cron: "@daily", Prompt: "brief me", As: auth.LocalPairing, Chat: "telegram:local"}, auth.Owner(), "")
	if err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	want := struct{ User, DeliverUser, Chat string }{User: auth.OwnerID, DeliverUser: auth.OwnerID, Chat: "telegram:owner"}
	if diff := cmp.Diff(want, struct{ User, DeliverUser, Chat string }{got.UserID, got.DeliverUser, Info(got).Chat}); diff != "" {
		t.Fatalf("Add() wrong result (-want +got) diff = %s", diff)
	}
}

func TestAddScheduleWithinLimits(t *testing.T) {
	t.Parallel()
	limits := Limits{PerUser: 2, MinInterval: 5 * time.Minute}
	tests := []struct {
		name     string
		limits   Limits
		existing int
		req      schedule.Request
		wantErr  string
	}{
		{name: "within the limits", limits: limits, existing: 1, req: schedule.Request{Cron: "*/5 * * * *"}},
		{name: "one-time", limits: limits, existing: 1, req: schedule.Request{At: "2099-01-01 10:00"}},
		{name: "too many", limits: limits, existing: 2, req: schedule.Request{Cron: "@daily"}, wantErr: "at most 2 schedules"},
		{name: "every second", limits: limits, req: schedule.Request{Cron: "@every 1s"}, wantErr: "at least 5 minutes apart"},
		{name: "every minute", limits: limits, req: schedule.Request{Cron: "* * * * *"}, wantErr: "at least 5 minutes apart"},
		{name: "no limits", existing: 2, req: schedule.Request{Cron: "@every 1s"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := openStore(t)
			ctx := context.Background()
			m := Service{Store: s, Limits: tc.limits}
			for i := range tc.existing {
				if _, err := m.Add(ctx, schedule.Request{Name: fmt.Sprintf("old%d", i), Cron: "@daily", Prompt: "x"}, auth.Owner(), "ctx1"); err != nil {
					t.Fatalf("Add() error = %v", err)
				}
			}
			tc.req.Name, tc.req.Prompt = "new", "x"

			_, err := m.Add(ctx, tc.req, auth.Owner(), "ctx1")

			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Add() error = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !agenterr.IsRequest(err) {
				t.Fatalf("Add() error = %v, want a request error with %q", err, tc.wantErr)
			}
		})
	}
}
