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

	"github.com/google/go-cmp/cmp"
)

func TestDeletingAgentDeletesItsWatches(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		put    func(*Store) error
		delete func(*Store) error
	}{
		{
			name: "contact",
			put: func(s *Store) error {
				return s.PutContact(context.Background(), Contact{Name: "coder", URL: "http://x"})
			},
			delete: func(s *Store) error { return s.DeleteContact(context.Background(), "coder") },
		},
		{
			name:   "webhook pairing",
			put:    func(s *Store) error { return s.PutNode(context.Background(), Node{ID: "coder", Name: "coder"}) },
			delete: func(s *Store) error { return s.DeletePairing(context.Background(), "coder") },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, ctx := openStore(t), context.Background()
			if err := tc.put(s); err != nil {
				t.Fatal(err)
			}
			for _, w := range []Watch{{Contact: "coder", TaskID: "t1"}, {Contact: "other", TaskID: "t2"}} {
				if err := s.PutWatch(ctx, w); err != nil {
					t.Fatal(err)
				}
			}

			if err := tc.delete(s); err != nil {
				t.Fatalf("delete error = %v", err)
			}

			ws, err := s.Watches(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, w := range ws {
				got = append(got, w.Contact)
			}
			if diff := cmp.Diff([]string{"other"}, got); diff != "" {
				t.Fatalf("Watches() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestSetWatchFailing(t *testing.T) {
	t.Parallel()
	s, ctx := openStore(t), context.Background()
	if err := s.PutWatch(ctx, Watch{Contact: "coder", TaskID: "t1"}); err != nil {
		t.Fatal(err)
	}
	since := time.Unix(0, 1_700_000_000_000_000_000)
	for _, want := range []time.Time{since, {}} {
		if err := s.SetWatchFailing(ctx, "coder", "t1", want); err != nil {
			t.Fatalf("SetWatchFailing() error = %v", err)
		}
		ws, err := s.Watches(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(ws) != 1 || !ws[0].FailingSince.Equal(want) {
			t.Fatalf("Watches() = %+v, want one failing since %v", ws, want)
		}
	}
}

func TestPutWatchAgainStartsAGeneration(t *testing.T) {
	t.Parallel()
	s, ctx := openStore(t), context.Background()
	w := Watch{Contact: "coder", TaskID: "t1", OriginContextID: "c1", OnBehalfOf: "user:owner"}
	if err := s.PutWatch(ctx, w); err != nil {
		t.Fatal(err)
	}
	first, err := s.Watch(ctx, "coder", "t1")
	if err != nil {
		t.Fatalf("Watch() error = %v", err)
	}
	if err := s.PutWatch(ctx, Watch{Contact: "coder", TaskID: "t1", OriginContextID: "c2"}); err != nil {
		t.Fatal(err)
	}

	got, err := s.Watch(ctx, "coder", "t1")
	if err != nil {
		t.Fatalf("Watch() error = %v", err)
	}
	if got.Generation != first.Generation+1 || got.OriginContextID != "c1" {
		t.Fatalf("Watch() after PutWatch() again = %+v, want the first watch in generation %d", got, first.Generation+1)
	}
	if err := s.DeleteWatch(ctx, "coder", "t1", first.Generation); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteWatch() of an older generation error = %v, want %v", err, ErrNotFound)
	}
	if err := s.DeleteWatch(ctx, "coder", "t1", got.Generation); err != nil {
		t.Fatalf("DeleteWatch() error = %v", err)
	}
	if _, err := s.Watch(ctx, "coder", "t1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Watch() after DeleteWatch() error = %v, want %v", err, ErrNotFound)
	}
}

func TestSetWatchState(t *testing.T) {
	t.Parallel()
	s, ctx := openStore(t), context.Background()
	if err := s.PutWatch(ctx, Watch{Contact: "coder", TaskID: "t1"}); err != nil {
		t.Fatal(err)
	}
	at := time.Unix(0, 1_700_000_000_000_000_000)

	if err := s.SetWatchState(ctx, "coder", "t1", "TASK_STATE_WORKING", at); err != nil {
		t.Fatalf("SetWatchState() error = %v", err)
	}

	got, err := s.Watch(ctx, "coder", "t1")
	if err != nil {
		t.Fatalf("Watch() error = %v", err)
	}
	if got.State != "TASK_STATE_WORKING" || !got.StateChanged.Equal(at) {
		t.Fatalf("Watch() = %+v, want state TASK_STATE_WORKING since %v", got, at)
	}
}
