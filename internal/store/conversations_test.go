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

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/a2atext"
)

// contextIDs returns the contexts in the history of user in a conversation
// of telegram.
func contextIDs(t *testing.T, s *Store, conv, user string) []string {
	t.Helper()
	convs, err := s.Conversations(context.Background(), "telegram", conv, user)
	if err != nil {
		t.Fatalf("Conversations() error = %v", err)
	}
	var out []string
	for _, c := range convs {
		out = append(out, c.ContextID)
	}
	return out
}

func TestConversationsKeepRecentContextsPerChatAndUser(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	start := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	use := func(i int, conv, user, contextID string) {
		s.now = func() time.Time { return start.Add(time.Duration(i) * time.Minute) }
		if err := s.UseConversation(ctx, "telegram", conv, user, contextID); err != nil {
			t.Fatalf("UseConversation() error = %v", err)
		}
	}
	for i := range MaxConversations + 2 {
		use(i, "dm1", "pair:anna", fmt.Sprintf("c%02d", i))
	}
	use(20, "dm1", "pair:anna", "c03")
	use(21, "dm1", "pair:bob", "b1")
	use(22, "dm2", "pair:anna", "x1")

	tests := []struct {
		conv, user string
		want       []string
	}{
		{conv: "dm1", user: "pair:anna", want: []string{"c03", "c11", "c10", "c09", "c08", "c07", "c06", "c05", "c04", "c02"}},
		{conv: "dm1", user: "pair:bob", want: []string{"b1"}},
		{conv: "dm2", user: "pair:anna", want: []string{"x1"}},
		{conv: "dm2", user: "pair:bob"},
	}
	for _, tc := range tests {
		t.Run(tc.conv+" "+tc.user, func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(tc.want, contextIDs(t, s, tc.conv, tc.user)); diff != "" {
				t.Fatalf("Conversations() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestNewConversationBindsChatAndRecordsIt(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	if err := s.UseConversation(ctx, "telegram", "dm1", "pair:anna", "c1"); err != nil {
		t.Fatalf("UseConversation() error = %v", err)
	}
	s.now = func() time.Time { return time.Now().Add(time.Minute) }

	if err := s.NewConversation(ctx, "telegram", "dm1", "pair:anna", "c2"); err != nil {
		t.Fatalf("NewConversation() error = %v", err)
	}

	if got, err := s.ContextOf(ctx, "telegram", "dm1"); err != nil || got != "c2" {
		t.Fatalf("ContextOf() = %q, %v, want c2, nil", got, err)
	}
	if diff := cmp.Diff([]string{"c2", "c1"}, contextIDs(t, s, "dm1", "pair:anna")); diff != "" {
		t.Fatalf("Conversations() wrong result (-want +got) diff = %s", diff)
	}
}

func TestNewConversationKeepsContextFromBeforeHistory(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		history map[string]string
		want    map[string][]string
	}{
		{
			name: "context without history",
			want: map[string][]string{"pair:anna": {"c2", "c1"}},
		},
		{
			name:    "context in the history of someone else",
			history: map[string]string{"pair:bob": "c1"},
			want:    map[string][]string{"pair:anna": {"c2"}, "pair:bob": {"c1"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := openStore(t)
			ctx := context.Background()
			if err := s.SetContext(ctx, "telegram", "dm1", "c1"); err != nil {
				t.Fatalf("SetContext() error = %v", err)
			}
			for user, contextID := range tc.history {
				if err := s.UseConversation(ctx, "telegram", "dm1", user, contextID); err != nil {
					t.Fatalf("UseConversation() error = %v", err)
				}
			}
			later := time.Now().Add(time.Minute)
			s.now = func() time.Time { return later }

			if err := s.NewConversation(ctx, "telegram", "dm1", "pair:anna", "c2"); err != nil {
				t.Fatalf("NewConversation() error = %v", err)
			}

			for user, want := range tc.want {
				if diff := cmp.Diff(want, contextIDs(t, s, "dm1", user)); diff != "" {
					t.Fatalf("Conversations(%s) wrong result (-want +got) diff = %s", user, diff)
				}
			}
		})
	}
}

func TestSwitchConversation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		conv      string
		user      string
		contextID string
		wantErr   error
	}{
		{name: "own earlier context", conv: "dm1", user: "pair:anna", contextID: "c1"},
		{name: "context of another user", conv: "dm1", user: "pair:anna", contextID: "b1", wantErr: ErrNotFound},
		{name: "own context of another chat", conv: "dm1", user: "pair:anna", contextID: "x1", wantErr: ErrNotFound},
		{name: "unknown context", conv: "dm1", user: "pair:anna", contextID: "nope", wantErr: ErrNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := openStore(t)
			ctx := context.Background()
			for _, u := range []struct{ conv, user, contextID string }{
				{"dm1", "pair:anna", "c1"},
				{"dm1", "pair:bob", "b1"},
				{"dm2", "pair:anna", "x1"},
			} {
				if err := s.UseConversation(ctx, "telegram", u.conv, u.user, u.contextID); err != nil {
					t.Fatalf("UseConversation() error = %v", err)
				}
			}
			if err := s.NewConversation(ctx, "telegram", "dm1", "pair:anna", "c2"); err != nil {
				t.Fatalf("NewConversation() error = %v", err)
			}
			s.now = func() time.Time { return time.Now().Add(time.Minute) }

			err := s.SwitchConversation(ctx, "telegram", tc.conv, tc.user, tc.contextID)

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("SwitchConversation() error = %v, want %v", err, tc.wantErr)
			}
			want := tc.contextID
			if tc.wantErr != nil {
				want = "c2"
			}
			if got, err := s.ContextOf(ctx, "telegram", tc.conv); err != nil || got != want {
				t.Fatalf("ContextOf() = %q, %v, want %q, nil", got, err, want)
			}
			if got := contextIDs(t, s, tc.conv, tc.user)[0]; got != want {
				t.Fatalf("Conversations()[0] = %q, want %q", got, want)
			}
		})
	}
}

func TestDeletePairingRemovesConversations(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	if _, err := s.EnsurePairing(ctx, Pairing{Name: "ben", Role: "member"}); err != nil {
		t.Fatalf("EnsurePairing() error = %v", err)
	}
	for _, user := range []string{"pair:ben", "pair:anna"} {
		if err := s.UseConversation(ctx, "telegram", "group", user, "c1"); err != nil {
			t.Fatalf("UseConversation() error = %v", err)
		}
	}

	if err := s.DeletePairing(ctx, "ben", PairingUser{Principal: "pair:ben", AgentKey: "user:pair:ben"}); err != nil {
		t.Fatalf("DeletePairing() error = %v", err)
	}

	if got := contextIDs(t, s, "group", "pair:ben"); len(got) != 0 {
		t.Fatalf("Conversations() of a deleted pairing = %q, want none", got)
	}
	if diff := cmp.Diff([]string{"c1"}, contextIDs(t, s, "group", "pair:anna")); diff != "" {
		t.Fatalf("Conversations() of another user wrong result (-want +got) diff = %s", diff)
	}
}

func TestForgetMessengerRemovesItsConversations(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	for _, m := range []string{"telegram", "discord"} {
		if err := s.UseConversation(ctx, m, "dm1", "pair:anna", "c-"+m); err != nil {
			t.Fatalf("UseConversation() error = %v", err)
		}
	}

	if err := s.ForgetMessenger(ctx, "telegram"); err != nil {
		t.Fatalf("ForgetMessenger() error = %v", err)
	}

	if got := contextIDs(t, s, "dm1", "pair:anna"); len(got) != 0 {
		t.Fatalf("Conversations() of a forgotten messenger = %q, want none", got)
	}
	if got, err := s.Conversations(ctx, "discord", "dm1", "pair:anna"); err != nil || len(got) != 1 {
		t.Fatalf("Conversations() of another messenger = %v, %v, want one", got, err)
	}
}

func TestPruneRemovesConversationsWithoutTasks(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	start := time.Now()
	now := start
	s.now = func() time.Time { return now }
	s.SetTaskTTL(24 * time.Hour)
	ts := s.Tasks("", testViewer)
	for _, id := range []string{"pruned", "kept"} {
		task := newTask(id, "c-"+id)
		task.Status.State = a2a.TaskStateCompleted
		if _, err := ts.Create(asUser("owner"), task); err != nil {
			t.Fatalf("Create() error = %v", err)
		}
	}
	for _, contextID := range []string{"c-pruned", "c-kept", "c-unused"} {
		if err := s.UseConversation(ctx, "telegram", "dm1", "pair:anna", contextID); err != nil {
			t.Fatalf("UseConversation() error = %v", err)
		}
	}
	now = start.Add(48 * time.Hour)
	if _, err := ts.Create(asUser("owner"), newTask("running", "c-kept")); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := s.NewConversation(ctx, "telegram", "dm1", "pair:anna", "c-new"); err != nil {
		t.Fatalf("NewConversation() error = %v", err)
	}

	if err := s.Prune(ctx); err != nil {
		t.Fatalf("Prune() error = %v", err)
	}

	if diff := cmp.Diff([]string{"c-new", "c-kept"}, contextIDs(t, s, "dm1", "pair:anna")); diff != "" {
		t.Fatalf("Conversations() after Prune() wrong result (-want +got) diff = %s", diff)
	}
}

func TestContextSummary(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ts := s.Tasks("", testViewer)
	owner := asUser("owner")
	for i, prompt := range []string{"write a changelog entry", "and the date", "thanks"} {
		task := newTask(fmt.Sprintf("t%d", i), "c1")
		task.History = []*a2a.Message{a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(prompt))}
		if _, err := ts.Create(owner, task); err != nil {
			t.Fatalf("Create() error = %v", err)
		}
	}
	tests := []struct {
		contextID string
		turns     int
		prompt    string
	}{
		{contextID: "c1", turns: 3, prompt: "write a changelog entry"},
		{contextID: "c2"},
	}
	for _, tc := range tests {
		t.Run(tc.contextID, func(t *testing.T) {
			t.Parallel()
			got, err := ts.ContextSummary(context.Background(), tc.contextID)
			if err != nil {
				t.Fatalf("ContextSummary() error = %v", err)
			}
			if got.Turns != tc.turns || a2atext.Message(got.Prompt) != tc.prompt {
				t.Fatalf("ContextSummary(%q) = %d turns, %q, want %d, %q", tc.contextID, got.Turns, a2atext.Message(got.Prompt), tc.turns, tc.prompt)
			}
		})
	}
}
