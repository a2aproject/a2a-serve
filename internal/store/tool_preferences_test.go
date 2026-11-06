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
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func TestToolPreferences(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)
	s.now = func() time.Time { now = now.Add(time.Second); return now }
	for _, rule := range []string{"command:kubectl get *", "tool:grafana.query", "command:kubectl get *"} {
		if err := s.AddToolPreference(ctx, "pair:anna", rule); err != nil {
			t.Fatalf("AddToolPreference(%q) error = %v", rule, err)
		}
	}
	if err := s.AddToolPreference(ctx, "pair:bob", "edit"); err != nil {
		t.Fatalf("AddToolPreference() error = %v", err)
	}
	if err := s.DeleteToolPreference(ctx, "pair:anna", "tool:grafana.query"); err != nil {
		t.Fatalf("DeleteToolPreference() error = %v", err)
	}
	if err := s.DeleteToolPreference(ctx, "pair:anna", "edit"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteToolPreference() of another user's rule error = %v, want %v", err, ErrNotFound)
	}
	got, err := s.ToolPreferences(ctx, "pair:anna")
	if err != nil {
		t.Fatalf("ToolPreferences() error = %v", err)
	}
	if diff := cmp.Diff([]string{"command:kubectl get *"}, got); diff != "" {
		t.Fatalf("ToolPreferences() wrong result (-want +got) diff = %s", diff)
	}
}

func TestToolPreferencesLimit(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	for i := range MaxToolPreferences {
		if err := s.AddToolPreference(ctx, "pair:anna", fmt.Sprintf("command:cmd%d *", i)); err != nil {
			t.Fatalf("AddToolPreference() error = %v", err)
		}
	}
	if err := s.AddToolPreference(ctx, "pair:anna", "edit"); !errors.Is(err, ErrPreferenceLimit) {
		t.Fatalf("AddToolPreference() past the limit error = %v, want %v", err, ErrPreferenceLimit)
	}
	if err := s.AddToolPreference(ctx, "pair:anna", "command:cmd0 *"); err != nil {
		t.Fatalf("AddToolPreference() of an allowed rule at the limit error = %v", err)
	}
}

func TestCountToolTap(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	var got []int
	for _, contextID := range []string{"c1", "c1", "c2", "c1", "c3"} {
		n, err := s.CountToolTap(ctx, "pair:anna", "edit", contextID)
		if err != nil {
			t.Fatalf("CountToolTap() error = %v", err)
		}
		got = append(got, n)
	}
	if diff := cmp.Diff([]int{1, 1, 2, 2, 3}, got); diff != "" {
		t.Fatalf("CountToolTap() wrong counts (-want +got) diff = %s", diff)
	}
	if err := s.ResetToolTaps(ctx, "pair:anna", "edit"); err != nil {
		t.Fatalf("ResetToolTaps() error = %v", err)
	}
	if n, err := s.ToolTaps(ctx, "pair:anna", "edit"); err != nil || n != 0 {
		t.Fatalf("ToolTaps() after ResetToolTaps() = %d, %v, want 0", n, err)
	}
}

func TestToolTapsExpire(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)
	s.now = func() time.Time { return now }
	if _, err := s.CountToolTap(ctx, "pair:anna", "edit", "c1"); err != nil {
		t.Fatalf("CountToolTap() error = %v", err)
	}
	now = now.Add(ToolTapRetention + time.Second)
	if err := s.Prune(ctx); err != nil {
		t.Fatalf("Prune() error = %v", err)
	}
	if n, err := s.ToolTaps(ctx, "pair:anna", "edit"); err != nil || n != 0 {
		t.Fatalf("ToolTaps() after the retention = %d, %v, want 0", n, err)
	}
}

func TestToolSuggestions(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	for _, sg := range []ToolSuggestion{
		{ID: "s-1", User: "pair:anna", Rule: "edit", ContextID: "c1"},
		{ID: "s-2", User: "pair:anna", Rule: "edit", ContextID: "c2"},
	} {
		if err := s.PutToolSuggestion(ctx, sg); err != nil {
			t.Fatalf("PutToolSuggestion() error = %v", err)
		}
	}
	if _, err := s.TakeToolSuggestion(ctx, "s-1", "pair:anna"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("TakeToolSuggestion() of a replaced offer error = %v, want %v", err, ErrNotFound)
	}
	if _, err := s.TakeToolSuggestion(ctx, "s-2", "pair:bob"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("TakeToolSuggestion() of another user's offer error = %v, want %v", err, ErrNotFound)
	}
	got, err := s.TakeToolSuggestion(ctx, "s-2", "pair:anna")
	if err != nil {
		t.Fatalf("TakeToolSuggestion() error = %v", err)
	}
	if diff := cmp.Diff(ToolSuggestion{ID: "s-2", User: "pair:anna", Rule: "edit", ContextID: "c2"}, got); diff != "" {
		t.Fatalf("TakeToolSuggestion() wrong result (-want +got) diff = %s", diff)
	}
	if _, err := s.TakeToolSuggestion(ctx, "s-2", "pair:anna"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("TakeToolSuggestion() twice error = %v, want %v", err, ErrNotFound)
	}
}
