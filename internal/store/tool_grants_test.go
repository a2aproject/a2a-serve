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
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func TestToolGrants(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)
	s.now = func() time.Time { now = now.Add(time.Second); return now }
	for _, g := range []struct{ contextID, user, rule string }{
		{"c1", "pair:anna", "edit"},
		{"c1", "pair:anna", "command:kubectl get *"},
		{"c1", "pair:bob", "tool:grafana.query"},
		{"c2", "pair:anna", "read"},
		{"c1", "pair:anna", "edit"},
	} {
		if err := s.AddToolGrant(ctx, g.contextID, g.user, g.rule); err != nil {
			t.Fatalf("AddToolGrant() error = %v", err)
		}
	}
	got, err := s.ToolGrants(ctx, "c1", "pair:anna")
	if err != nil {
		t.Fatalf("ToolGrants() error = %v", err)
	}
	if diff := cmp.Diff([]string{"edit", "command:kubectl get *"}, got); diff != "" {
		t.Fatalf("ToolGrants() wrong result (-want +got) diff = %s", diff)
	}
}

func TestToolGrantsKeepTheLatest(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)
	s.now = func() time.Time { now = now.Add(time.Second); return now }
	for i := range MaxToolGrants + 1 {
		if err := s.AddToolGrant(ctx, "c1", "pair:anna", fmt.Sprintf("command:cmd%d *", i)); err != nil {
			t.Fatalf("AddToolGrant() error = %v", err)
		}
	}
	got, err := s.ToolGrants(ctx, "c1", "pair:anna")
	if err != nil {
		t.Fatalf("ToolGrants() error = %v", err)
	}
	if len(got) != MaxToolGrants || got[len(got)-1] != "command:cmd1 *" {
		t.Fatalf("ToolGrants() = %d grants ending with %q, want %d ending with %q", len(got), got[len(got)-1], MaxToolGrants, "command:cmd1 *")
	}
}

func TestDeletePairingDeletesItsGrants(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	ensurePairing(t, s, Pairing{Name: "bob", Kind: "person", Role: "authenticated"})
	ts := s.Tasks("", testViewer)
	for user, id := range map[string]string{"pair:anna": "anna-1", "pair:bob": "bob-1"} {
		if _, err := ts.Create(asUser(user), newTask(id, "c-group")); err != nil {
			t.Fatalf("Create(%s) error = %v", id, err)
		}
	}
	if err := s.AddToolGrant(ctx, "c-group", "pair:bob", "edit"); err != nil {
		t.Fatalf("AddToolGrant() error = %v", err)
	}

	if err := s.DeletePairing(ctx, "bob", "pair:bob"); err != nil {
		t.Fatalf("DeletePairing() error = %v", err)
	}

	if rules, err := s.ToolGrants(ctx, "c-group", "pair:bob"); err != nil || len(rules) != 0 {
		t.Fatalf("ToolGrants() of the removed user in a conversation with other tasks = %q, %v, want none", rules, err)
	}
}
