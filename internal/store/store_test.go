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
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/a2aproject/a2a-go/v2/integrations/executor/a2aacp"

	"github.com/a2aproject/a2a-serve/internal/cryptoutil"
	"github.com/a2aproject/a2a-serve/internal/pairapi"
)

func openStore(t *testing.T) *Store {
	t.Helper()
	return openStoreAt(t, filepath.Join(t.TempDir(), "server.db"))
}

func openStoreAt(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	return s
}

type userKey struct{}

func asUser(user string) context.Context {
	return context.WithValue(context.Background(), userKey{}, user)
}

func testViewer(ctx context.Context) (string, bool, error) {
	u, _ := ctx.Value(userKey{}).(string)
	return u, u == "owner", nil
}

func newTask(id, contextID string) *a2a.Task {
	return &a2a.Task{ID: a2a.TaskID(id), ContextID: contextID, Status: a2a.TaskStatus{State: a2a.TaskStateWorking}}
}

func TestTaskStoreVisibility(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ts := s.Tasks("", testViewer)
	alice, bob, owner := asUser("alice"), asUser("bob"), asUser("owner")

	if _, err := ts.Create(alice, newTask("t1", "c1")); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if _, err := ts.Create(alice, newTask("t1", "c1")); !errors.Is(err, taskstore.ErrTaskAlreadyExists) {
		t.Fatalf("Create() error = %v, want %v", err, taskstore.ErrTaskAlreadyExists)
	}
	if _, err := ts.Get(bob, "t1"); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Fatalf("Get() as other user error = %v, want %v", err, a2a.ErrTaskNotFound)
	}
	got, err := ts.Get(owner, "t1")
	if err != nil {
		t.Fatalf("Get() as owner error = %v", err)
	}
	if got.User != "alice" || got.Version != 1 {
		t.Fatalf("Get() = {User: %q, Version: %d}, want {alice, 1}", got.User, got.Version)
	}

	updated := newTask("t1", "c1")
	updated.Status.State = a2a.TaskStateCompleted
	v, err := ts.Update(owner, &taskstore.UpdateRequest{Task: updated, PrevVersion: 1})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if v != 2 {
		t.Fatalf("Update() = %d, want 2", v)
	}
	if _, err := ts.Update(alice, &taskstore.UpdateRequest{Task: updated, PrevVersion: 1}); !errors.Is(err, taskstore.ErrConcurrentModification) {
		t.Fatalf("Update() stale error = %v, want %v", err, taskstore.ErrConcurrentModification)
	}
	got, err = ts.Get(alice, "t1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.User != "alice" || got.Task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("Get() = {User: %q, State: %v}, want {alice, completed}", got.User, got.Task.Status.State)
	}

	if _, err := s.Tasks("n/grok", testViewer).Get(owner, "t1"); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Fatalf("Get() in other scope error = %v, want %v", err, a2a.ErrTaskNotFound)
	}
}

func TestTaskStoreList(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ts := s.Tasks("", testViewer)
	for _, id := range []string{"a", "b", "c"} {
		if _, err := ts.Create(asUser("alice"), newTask(id, "c1")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ts.Create(asUser("bob"), newTask("d", "c2")); err != nil {
		t.Fatal(err)
	}

	var ids []string
	req := &a2a.ListTasksRequest{PageSize: 2}
	for {
		resp, err := ts.List(asUser("owner"), req)
		if err != nil {
			t.Fatalf("List() error = %v", err)
		}
		if resp.TotalSize != 4 {
			t.Fatalf("List() TotalSize = %d, want 4", resp.TotalSize)
		}
		for _, task := range resp.Tasks {
			ids = append(ids, string(task.ID))
		}
		if resp.NextPageToken == "" {
			break
		}
		req.PageToken = resp.NextPageToken
	}
	if diff := cmp.Diff([]string{"d", "c", "b", "a"}, ids); diff != "" {
		t.Fatalf("List() wrong result (-want +got) diff = %s", diff)
	}

	resp, err := ts.List(asUser("bob"), &a2a.ListTasksRequest{})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(resp.Tasks) != 1 || resp.Tasks[0].ID != "d" {
		t.Fatalf("List() as bob = %v, want [d]", resp.Tasks)
	}
}

func TestFailUnfinished(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ts := s.Tasks("", testViewer)
	owner := asUser("owner")
	done := newTask("done", "c")
	done.Status.State = a2a.TaskStateCompleted
	for _, task := range []*a2a.Task{newTask("running", "c"), done} {
		if _, err := ts.Create(owner, task); err != nil {
			t.Fatal(err)
		}
	}
	n, err := ts.FailUnfinished(context.Background(), "restarted")
	if err != nil {
		t.Fatalf("FailUnfinished() error = %v", err)
	}
	if n != 1 {
		t.Fatalf("FailUnfinished() = %d, want 1", n)
	}
	got, err := ts.Get(owner, "running")
	if err != nil {
		t.Fatal(err)
	}
	if got.Task.Status.State != a2a.TaskStateFailed {
		t.Fatalf("Get() state = %v, want %v", got.Task.Status.State, a2a.TaskStateFailed)
	}
}

func ensurePairing(t *testing.T, s *Store, p Pairing) {
	t.Helper()
	if _, err := s.EnsurePairing(context.Background(), p); err != nil {
		t.Fatalf("EnsurePairing(%s) error = %v", p.Name, err)
	}
}

func TestPairings(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	ensurePairing(t, s, Pairing{Name: "anna", Role: "authenticated"})
	if _, err := s.EnsurePairing(ctx, Pairing{Name: "anna", Kind: "agent", Role: "owner"}); err != nil {
		t.Fatalf("EnsurePairing() of an existing pairing error = %v", err)
	}
	if err := s.UpdatePairing(ctx, Pairing{Name: "anna", Kind: "device", Role: "owner"}); err != nil {
		t.Fatalf("UpdatePairing() error = %v", err)
	}
	if err := s.UpdatePairing(ctx, Pairing{Name: "bob", Role: "owner"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("UpdatePairing() of a missing pairing error = %v, want %v", err, ErrNotFound)
	}

	all, err := s.Pairings(ctx)
	if err != nil {
		t.Fatalf("Pairings() error = %v", err)
	}
	want := []Pairing{{Name: "anna", Kind: "device", Role: "owner"}, {Name: LocalPairing, Kind: "person", Role: "owner"}}
	if diff := cmp.Diff(want, all, cmpopts.IgnoreFields(Pairing{}, "Created")); diff != "" {
		t.Fatalf("Pairings() wrong result (-want +got) diff = %s", diff)
	}
}

func TestDeletePairing(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	ensurePairing(t, s, Pairing{Name: "grok", Role: "authenticated"})
	if err := s.PutLink(ctx, Link{Channel: "telegram", Identity: "telegram:1", Pairing: "grok"}, "person"); err != nil {
		t.Fatalf("PutLink() error = %v", err)
	}
	if err := s.PutToken(ctx, "a2p_secret", Token{Pairing: "grok", URL: "http://x"}, "agent"); err != nil {
		t.Fatalf("PutToken() error = %v", err)
	}
	if err := s.PutCode(ctx, "CODE", Code{Pairing: "grok", Expires: time.Now().Add(time.Minute)}); err != nil {
		t.Fatalf("PutCode() error = %v", err)
	}
	for _, sc := range []Schedule{
		{Name: "as-pairing", Cron: "@daily", Prompt: "p", UserID: "pair:grok"},
		{Name: "as-node", Cron: "@daily", Prompt: "p", UserID: "node:grok"},
		{Name: "as-owner", Cron: "@daily", Prompt: "p", UserID: "owner"},
	} {
		if err := s.AddSchedule(ctx, sc); err != nil {
			t.Fatalf("AddSchedule(%s) error = %v", sc.Name, err)
		}
	}
	for _, w := range []Watch{
		{Contact: "ben", TaskID: "t1", OnBehalfOf: "pair:grok"},
		{Contact: "ben", TaskID: "t2", OnBehalfOf: "owner"},
	} {
		if err := s.PutWatch(ctx, w); err != nil {
			t.Fatalf("PutWatch(%s) error = %v", w.TaskID, err)
		}
	}

	if err := s.DeletePairing(ctx, "grok", "pair:grok", "node:grok"); err != nil {
		t.Fatalf("DeletePairing() error = %v", err)
	}

	if _, err := s.LookupToken(ctx, "a2p_secret"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("LookupToken() after DeletePairing() error = %v, want %v", err, ErrNotFound)
	}
	if _, err := s.LinkOf(ctx, "telegram", "telegram:1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("LinkOf() after DeletePairing() error = %v, want %v", err, ErrNotFound)
	}
	if _, err := s.TakeCode(ctx, "CODE", AnyCode); !errors.Is(err, ErrNotFound) {
		t.Fatalf("TakeCode() after DeletePairing() error = %v, want %v", err, ErrNotFound)
	}
	schedules, err := s.Schedules(ctx)
	if err != nil {
		t.Fatalf("Schedules() error = %v", err)
	}
	if len(schedules) != 1 || schedules[0].Name != "as-owner" {
		t.Fatalf("Schedules() after DeletePairing() = %+v, want only as-owner", schedules)
	}
	watches, err := s.Watches(ctx)
	if err != nil {
		t.Fatalf("Watches() error = %v", err)
	}
	if len(watches) != 1 || watches[0].TaskID != "t2" {
		t.Fatalf("Watches() after DeletePairing() = %+v, want only t2", watches)
	}
	if err := s.DeletePairing(ctx, "grok"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeletePairing() of a missing pairing error = %v, want %v", err, ErrNotFound)
	}
}

func TestPairingRemoval(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	ensurePairing(t, s, Pairing{Name: "anna", Kind: "person", Role: "authenticated"})
	ensurePairing(t, s, Pairing{Name: "bob", Kind: "person", Role: "authenticated"})
	ts := s.Tasks("", testViewer)
	finished := newTask("anna-done", "c-anna")
	finished.Status.State = a2a.TaskStateCompleted
	for user, tasks := range map[string][]*a2a.Task{
		"pair:anna": {newTask("anna-1", "c-anna"), newTask("anna-shared", "c-shared"), finished},
		"pair:bob":  {newTask("bob-1", "c-bob"), newTask("bob-shared", "c-shared")},
	} {
		for _, task := range tasks {
			if _, err := ts.Create(asUser(user), task); err != nil {
				t.Fatalf("Create(%s) error = %v", task.ID, err)
			}
		}
	}
	if err := s.PutToken(ctx, "a2p_anna", Token{Pairing: "anna", URL: "http://x"}, "person"); err != nil {
		t.Fatalf("PutToken() error = %v", err)
	}
	if err := s.PutLink(ctx, Link{Channel: "telegram", Identity: "telegram:1", Pairing: "anna"}, "person"); err != nil {
		t.Fatalf("PutLink() error = %v", err)
	}
	if err := s.PutCode(ctx, "CODE", Code{Pairing: "anna", Expires: time.Now().Add(time.Minute)}); err != nil {
		t.Fatalf("PutCode() error = %v", err)
	}
	for _, sc := range []Schedule{
		{Name: "anna-daily", Cron: "@daily", Prompt: "p", UserID: "pair:anna"},
		{Name: "bob-daily", Cron: "@daily", Prompt: "p", UserID: "pair:bob"},
	} {
		if err := s.AddSchedule(ctx, sc); err != nil {
			t.Fatalf("AddSchedule(%s) error = %v", sc.Name, err)
		}
	}
	if err := s.PutWatch(ctx, Watch{Contact: "ben", TaskID: "t1", OnBehalfOf: "pair:anna"}); err != nil {
		t.Fatalf("PutWatch() error = %v", err)
	}
	for _, q := range []Question{
		{ID: "q-anna", Token: "t-anna", TaskID: "anna-1", ContextID: "c-anna", User: "pair:anna"},
		{ID: "q-bob", Token: "t-bob", TaskID: "bob-1", ContextID: "c-bob", User: "pair:bob"},
	} {
		if err := s.PutQuestion(ctx, q); err != nil {
			t.Fatalf("PutQuestion(%s) error = %v", q.ID, err)
		}
	}
	for conv, contextID := range map[string]string{"anna-dm": "c-anna", "bob-dm": "c-bob", "group": "c-shared"} {
		if err := s.SetContext(ctx, "telegram", conv, contextID); err != nil {
			t.Fatalf("SetContext(%s) error = %v", conv, err)
		}
	}

	got, err := s.PairingRemoval(ctx, "anna", "pair:anna", "node:anna")
	if err != nil {
		t.Fatalf("Store.PairingRemoval() error = %v", err)
	}

	want := PairingRemoval{Tokens: 1, Links: 1, Codes: 1, Schedules: 1, Watches: 1, Questions: 1, Tasks: 3, Unfinished: 2, Conversations: 1}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("Store.PairingRemoval() wrong result (-want +got) diff = %s", diff)
	}
	if _, err := ts.Get(asUser("owner"), "anna-1"); err != nil {
		t.Fatalf("Get() after PairingRemoval() error = %v, want the task kept", err)
	}
	if _, err := s.Pairing(ctx, "anna"); err != nil {
		t.Fatalf("Pairing() after PairingRemoval() error = %v, want it kept", err)
	}
}

func TestDeletePairingForgetsItsConversations(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	ensurePairing(t, s, Pairing{Name: "anna", Kind: "person", Role: "authenticated"})
	ts := s.Tasks("", testViewer)
	owner := asUser("owner")
	photo := newTask("anna-1", "c-anna")
	photo.History = []*a2a.Message{a2a.NewMessage(a2a.MessageRoleUser, a2a.NewRawPart([]byte("photo")))}
	for user, tasks := range map[string][]*a2a.Task{
		"pair:anna": {photo, newTask("anna-shared", "c-shared")},
		"pair:bob":  {newTask("bob-1", "c-bob"), newTask("bob-shared", "c-shared")},
	} {
		for _, task := range tasks {
			if _, err := ts.Create(asUser(user), task); err != nil {
				t.Fatalf("Create(%s) error = %v", task.ID, err)
			}
		}
	}
	for _, q := range []Question{
		{ID: "q-anna", Token: "t-anna", TaskID: "anna-1", ContextID: "c-anna", User: "pair:anna"},
		{ID: "q-anna-remote", Token: "t-anna-remote", TaskID: "r1", ContextID: "c-anna", Contact: "ben", HitlID: "h", User: "pair:anna"},
		{ID: "q-bob", Token: "t-bob", TaskID: "bob-1", ContextID: "c-bob", User: "pair:bob"},
	} {
		if err := s.PutQuestion(ctx, q); err != nil {
			t.Fatalf("PutQuestion(%s) error = %v", q.ID, err)
		}
	}
	if err := s.SetPendingQuestion(ctx, "telegram", "anna-dm", "q-anna"); err != nil {
		t.Fatalf("SetPendingQuestion() error = %v", err)
	}
	for conv, contextID := range map[string]string{"anna-dm": "c-anna", "bob-dm": "c-bob", "group": "c-shared"} {
		if err := s.SetContext(ctx, "telegram", conv, contextID); err != nil {
			t.Fatalf("SetContext(%s) error = %v", conv, err)
		}
		if err := s.Sessions().Put(ctx, contextID, a2aacp.Session{ID: "s-" + contextID}); err != nil {
			t.Fatalf("Sessions().Put(%s) error = %v", contextID, err)
		}
	}
	if err := s.PutHandle(ctx, Handle{Handle: "ab", ContextID: "c-anna"}); err != nil {
		t.Fatalf("PutHandle() error = %v", err)
	}
	if err := s.PutCallTask(ctx, CallTask{ContextID: "c-anna", Agent: "ben", TaskID: "r1", RemoteContextID: "rc"}); err != nil {
		t.Fatalf("PutCallTask() error = %v", err)
	}

	if err := s.DeletePairing(ctx, "anna", "pair:anna", "node:anna"); err != nil {
		t.Fatalf("DeletePairing() error = %v", err)
	}

	for _, id := range []a2a.TaskID{"anna-1", "anna-shared"} {
		if _, err := ts.Get(owner, id); !errors.Is(err, a2a.ErrTaskNotFound) {
			t.Fatalf("Get(%s) after DeletePairing() error = %v, want %v", id, err, a2a.ErrTaskNotFound)
		}
	}
	for _, id := range []a2a.TaskID{"bob-1", "bob-shared"} {
		if _, err := ts.Get(owner, id); err != nil {
			t.Fatalf("Get(%s) of another user's task error = %v", id, err)
		}
	}
	for _, id := range []string{"q-anna", "q-anna-remote"} {
		if _, err := s.QuestionByID(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("QuestionByID(%s) after DeletePairing() error = %v, want %v", id, err, ErrNotFound)
		}
	}
	if _, err := s.QuestionByID(ctx, "q-bob"); err != nil {
		t.Fatalf("QuestionByID() of another user's question error = %v", err)
	}
	if _, err := s.PendingQuestion(ctx, "telegram", "anna-dm"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("PendingQuestion() after DeletePairing() error = %v, want %v", err, ErrNotFound)
	}
	if _, err := s.ContextOf(ctx, "telegram", "anna-dm"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ContextOf() of the removed user's chat error = %v, want %v", err, ErrNotFound)
	}
	for _, conv := range []string{"bob-dm", "group"} {
		if _, err := s.ContextOf(ctx, "telegram", conv); err != nil {
			t.Fatalf("ContextOf(%s) of a conversation with other tasks error = %v", conv, err)
		}
	}
	if _, ok, err := s.Sessions().Get(ctx, "c-anna"); err != nil || ok {
		t.Fatalf("Sessions().Get() of the removed user's conversation = %v, %v, want false, nil", ok, err)
	}
	if _, ok, err := s.Sessions().Get(ctx, "c-shared"); err != nil || !ok {
		t.Fatalf("Sessions().Get() of a conversation with other tasks = %v, %v, want true, nil", ok, err)
	}
	if _, err := s.HandleOf(ctx, "c-anna"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("HandleOf() after DeletePairing() error = %v, want %v", err, ErrNotFound)
	}
	if ok, err := s.HasCallTask(ctx, "c-anna", "ben", "r1"); err != nil || ok {
		t.Fatalf("HasCallTask() after DeletePairing() = %v, %v, want false, nil", ok, err)
	}
	var blobs int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM blobs`).Scan(&blobs); err != nil {
		t.Fatalf("counting blobs: %v", err)
	}
	if blobs != 0 {
		t.Fatalf("blobs after DeletePairing() = %d, want 0", blobs)
	}
}

func TestCodes(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	now := time.Now()
	s.now = func() time.Time { return now }
	ensurePairing(t, s, Pairing{Name: "grok", Role: "authenticated"})

	if err := s.PutCode(ctx, "K7Q29XF4", Code{Pairing: "grok", Expires: now.Add(time.Minute)}); err != nil {
		t.Fatalf("PutCode() error = %v", err)
	}
	if _, err := s.TakeCode(ctx, "K7Q29XF4", func(Code) bool { return false }); !errors.Is(err, ErrNotFound) {
		t.Fatalf("TakeCode() of a refused code error = %v, want %v", err, ErrNotFound)
	}
	got, err := s.TakeCode(ctx, "K7Q29XF4", AnyCode)
	if err != nil {
		t.Fatalf("TakeCode() error = %v", err)
	}
	if diff := cmp.Diff(Code{Pairing: "grok", Expires: got.Expires}, got); diff != "" {
		t.Fatalf("TakeCode() wrong result (-want +got) diff = %s", diff)
	}
	if _, err := s.TakeCode(ctx, "K7Q29XF4", AnyCode); !errors.Is(err, ErrNotFound) {
		t.Fatalf("TakeCode() second use error = %v, want %v", err, ErrNotFound)
	}

	if err := s.PutCode(ctx, "OLD", Code{Pairing: "grok", Expires: now.Add(-time.Second)}); err != nil {
		t.Fatalf("PutCode() error = %v", err)
	}
	if _, err := s.TakeCode(ctx, "OLD", AnyCode); !errors.Is(err, ErrCodeExpired) {
		t.Fatalf("TakeCode() expired error = %v, want %v", err, ErrCodeExpired)
	}
}

func TestRedeemCode(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	refused := errors.New("refused")
	tests := []struct {
		name  string
		grant func(Code) (Grant, error)
		// existingToken is stored before the code is redeemed.
		existingToken string
		expired       bool
		wantErr       bool
		wantPending   bool
		wantKind      string
		wantTokens    int
		wantLinks     int
	}{
		{
			name: "token",
			grant: func(Code) (Grant, error) {
				return Grant{Token: "a2p_new", TokenURL: "https://agent", Kind: "device"}, nil
			},
			wantKind:   "device",
			wantTokens: 1,
		},
		{
			name: "chat link",
			grant: func(c Code) (Grant, error) {
				return Grant{Link: &Link{Channel: "telegram", Identity: "telegram:1", Pairing: c.Pairing}, Kind: "person"}, nil
			},
			wantKind:  "person",
			wantLinks: 1,
		},
		{
			name:        "refused",
			grant:       func(Code) (Grant, error) { return Grant{}, refused },
			wantErr:     true,
			wantPending: true,
		},
		{
			name:          "failed write",
			existingToken: "a2p_taken",
			grant: func(Code) (Grant, error) {
				return Grant{Token: "a2p_taken", TokenURL: "https://agent", Kind: "device"}, nil
			},
			wantErr:     true,
			wantPending: true,
			wantTokens:  1,
		},
		{
			name:    "expired",
			expired: true,
			grant: func(Code) (Grant, error) {
				return Grant{Token: "a2p_new", TokenURL: "https://agent", Kind: "device"}, nil
			},
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := openStore(t)
			ensurePairing(t, s, Pairing{Name: "grok", Role: "authenticated"})
			if tc.existingToken != "" {
				if err := s.PutToken(ctx, tc.existingToken, Token{URL: "https://agent"}, ""); err != nil {
					t.Fatalf("PutToken() error = %v", err)
				}
			}
			expires := time.Now().Add(time.Minute)
			if tc.expired {
				expires = time.Now().Add(-time.Second)
			}
			if err := s.PutCode(ctx, "K7Q29XF4", Code{Pairing: "grok", Expires: expires}); err != nil {
				t.Fatalf("PutCode() error = %v", err)
			}

			if _, err := s.RedeemCode(ctx, "K7Q29XF4", tc.grant); (err != nil) != tc.wantErr {
				t.Fatalf("RedeemCode() error = %v, want error %v", err, tc.wantErr)
			}
			if _, err := s.TakeCode(ctx, "K7Q29XF4", AnyCode); (err == nil) != tc.wantPending {
				t.Fatalf("TakeCode() after RedeemCode() error = %v, want the code pending %v", err, tc.wantPending)
			}
			p, err := s.Pairing(ctx, "grok")
			if err != nil {
				t.Fatalf("Pairing() error = %v", err)
			}
			links, err := s.LinksOf(ctx, "grok")
			if err != nil {
				t.Fatalf("LinksOf() error = %v", err)
			}
			tokens, err := s.Tokens(ctx)
			if err != nil {
				t.Fatalf("Tokens() error = %v", err)
			}
			if p.Kind != tc.wantKind || len(tokens) != tc.wantTokens || len(links) != tc.wantLinks {
				t.Fatalf("after RedeemCode() kind = %q, tokens = %d, links = %d, want %q, %d, %d", p.Kind, len(tokens), len(links), tc.wantKind, tc.wantTokens, tc.wantLinks)
			}
		})
	}
}

func TestRedeemCodeOfNode(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	if err := s.PutNode(ctx, Node{ID: "grok", Name: "grok", Webhook: "https://x", Secret: "random"}); err != nil {
		t.Fatalf("PutNode() error = %v", err)
	}
	if err := s.PutCode(ctx, "K7Q29XF4", Code{Pairing: "grok", Expires: time.Now().Add(time.Minute)}); err != nil {
		t.Fatalf("PutCode() error = %v", err)
	}
	c, err := s.RedeemCode(ctx, "K7Q29XF4", func(Code) (Grant, error) { return Grant{NodeToken: "a2rn.token", NodeSecret: "wake"}, nil })
	if err != nil {
		t.Fatalf("RedeemCode() error = %v", err)
	}
	if c.Kind != pairapi.PairingWebhook {
		t.Fatalf("RedeemCode().Kind = %q, want %q", c.Kind, pairapi.PairingWebhook)
	}
	got, err := s.Node(ctx, "grok")
	if err != nil {
		t.Fatalf("Node() error = %v", err)
	}
	if got.TokenHash != cryptoutil.Hash("a2rn.token") || got.Secret != "wake" {
		t.Fatalf("Node() = %+v, want the token hash and the wake key of the new token", got)
	}
}

func TestTokens(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	ensurePairing(t, s, Pairing{Name: "grok", Role: "authenticated"})
	if err := s.PutLink(ctx, Link{Channel: "telegram", Identity: "telegram:1", Pairing: "grok"}, "person"); err != nil {
		t.Fatalf("PutLink() error = %v", err)
	}
	if err := s.PutToken(ctx, "a2p_secret", Token{Pairing: "grok", URL: "http://x"}, "agent"); err != nil {
		t.Fatalf("PutToken() error = %v", err)
	}
	if err := s.PutToken(ctx, "a2p_google", Token{Email: "anna@example.com", Domain: "example.com", URL: "http://x"}, ""); err != nil {
		t.Fatalf("PutToken() of a sign-in token error = %v", err)
	}

	got, err := s.LookupToken(ctx, "a2p_secret")
	if err != nil {
		t.Fatalf("LookupToken() error = %v", err)
	}
	if diff := cmp.Diff(Token{Pairing: "grok", URL: "http://x"}, got, cmpopts.IgnoreFields(Token{}, "Created")); diff != "" {
		t.Fatalf("LookupToken() wrong result (-want +got) diff = %s", diff)
	}
	got, err = s.LookupToken(ctx, "a2p_google")
	if err != nil {
		t.Fatalf("LookupToken() of a sign-in token error = %v", err)
	}
	if diff := cmp.Diff(Token{Email: "anna@example.com", Domain: "example.com", URL: "http://x"}, got, cmpopts.IgnoreFields(Token{}, "Created")); diff != "" {
		t.Fatalf("LookupToken() of a sign-in token wrong result (-want +got) diff = %s", diff)
	}
	p, err := s.Pairing(ctx, "grok")
	if err != nil || p.Kind != "person" {
		t.Fatalf("Pairing() = %+v, %v, want the kind of its first redemption, person", p, err)
	}

	if err := s.RevokeCredentials(ctx, "grok"); err != nil {
		t.Fatalf("RevokeCredentials() error = %v", err)
	}
	if _, err := s.LookupToken(ctx, "a2p_secret"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("LookupToken() after RevokeCredentials() error = %v, want %v", err, ErrNotFound)
	}
	if _, err := s.LinkOf(ctx, "telegram", "telegram:1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("LinkOf() after RevokeCredentials() error = %v, want %v", err, ErrNotFound)
	}
	if _, err := s.Pairing(ctx, "grok"); err != nil {
		t.Fatalf("Pairing() after RevokeCredentials() error = %v, want nil", err)
	}
}

func putSignIns(t *testing.T, s *Store, tokens map[string]Token) {
	t.Helper()
	for tok, tk := range tokens {
		if err := s.PutToken(context.Background(), tok, tk, ""); err != nil {
			t.Fatalf("PutToken(%s) error = %v", tok, err)
		}
	}
}

func tokenEmails(t *testing.T, s *Store) []string {
	t.Helper()
	all, err := s.Tokens(context.Background())
	if err != nil {
		t.Fatalf("Tokens() error = %v", err)
	}
	var out []string
	for _, tk := range all {
		out = append(out, tk.Pairing+tk.Email)
	}
	slices.Sort(out)
	return out
}

func TestDeleteSignIns(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	ensurePairing(t, s, Pairing{Name: "grok", Role: "authenticated"})
	if err := s.PutToken(ctx, "a2p_pairing", Token{Pairing: "grok", URL: "http://x"}, "agent"); err != nil {
		t.Fatalf("PutToken() error = %v", err)
	}
	putSignIns(t, s, map[string]Token{
		"a2p_laptop": {Email: "anna@example.com", URL: "http://x"},
		"a2p_phone":  {Email: "anna@example.com", URL: "https://y"},
		"a2p_bob":    {Email: "bob@example.com", URL: "http://x"},
	})

	n, err := s.DeleteSignIns(ctx, "Anna@Example.com")
	if err != nil {
		t.Fatalf("DeleteSignIns() error = %v", err)
	}

	if n != 2 {
		t.Fatalf("DeleteSignIns() = %d, want 2", n)
	}
	if diff := cmp.Diff([]string{"bob@example.com", "grok"}, tokenEmails(t, s)); diff != "" {
		t.Fatalf("Tokens() after DeleteSignIns() wrong result (-want +got) diff = %s", diff)
	}
	if n, err := s.DeleteSignIns(ctx, "anna@example.com"); err != nil || n != 0 {
		t.Fatalf("DeleteSignIns() again = %d, %v, want 0, nil", n, err)
	}
}

func TestPruneSignIns(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	ensurePairing(t, s, Pairing{Name: "grok", Role: "authenticated"})
	if err := s.PutToken(ctx, "a2p_pairing", Token{Pairing: "grok", URL: "http://x"}, "agent"); err != nil {
		t.Fatalf("PutToken() error = %v", err)
	}
	putSignIns(t, s, map[string]Token{
		"a2p_anna":  {Email: "anna@example.com", Domain: "example.com", URL: "http://x"},
		"a2p_bob":   {Email: "bob@example.com", URL: "http://x"},
		"a2p_carol": {Email: "carol@other.com", URL: "http://x"},
	})

	err := s.PruneSignIns(ctx, func(email, domain string) bool { return domain == "example.com" || email == "carol@other.com" })
	if err != nil {
		t.Fatalf("PruneSignIns() error = %v", err)
	}

	if diff := cmp.Diff([]string{"anna@example.com", "carol@other.com", "grok"}, tokenEmails(t, s)); diff != "" {
		t.Fatalf("Tokens() after PruneSignIns() wrong result (-want +got) diff = %s", diff)
	}
}

func TestPutNodeNeedsWebhookPairing(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	ensurePairing(t, s, Pairing{Name: "anna", Role: "authenticated"})

	if err := s.PutNode(ctx, Node{ID: "anna", Name: "anna", Webhook: "https://x"}); !errors.Is(err, ErrExists) {
		t.Fatalf("PutNode() over a person pairing error = %v, want %v", err, ErrExists)
	}
	if err := s.PutNode(ctx, Node{ID: "grok", Name: "grok", Webhook: "https://x"}); err != nil {
		t.Fatalf("PutNode() error = %v", err)
	}
	p, err := s.Pairing(ctx, "grok")
	if err != nil || p.Kind != "webhook" || p.Role != "authenticated" {
		t.Fatalf("Pairing() of a node = %+v, %v, want an authenticated webhook pairing", p, err)
	}
	if err := s.DeletePairing(ctx, "grok"); err != nil {
		t.Fatalf("DeletePairing() error = %v", err)
	}
	if _, err := s.Node(ctx, "grok"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Node() after DeletePairing() error = %v, want %v", err, ErrNotFound)
	}
}

func TestUpdateNode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		prepare func(t *testing.T, s *Store, read Node)
		wantErr error
	}{
		{name: "unchanged node"},
		{
			name: "node updated meanwhile",
			prepare: func(t *testing.T, s *Store, read Node) {
				other := read
				other.Description = "changed"
				if err := s.UpdateNode(context.Background(), read, other); err != nil {
					t.Fatalf("UpdateNode() error = %v", err)
				}
			},
			wantErr: ErrChanged,
		},
		{
			name: "node removed meanwhile",
			prepare: func(t *testing.T, s *Store, read Node) {
				if err := s.DeletePairing(context.Background(), read.ID); err != nil {
					t.Fatalf("DeletePairing() error = %v", err)
				}
			},
			wantErr: ErrNotFound,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := openStore(t)
			ctx := context.Background()
			if err := s.PutNode(ctx, Node{ID: "grok", Name: "grok", Webhook: "https://x", Secret: "s1", Headers: map[string]string{"A": "b"}}); err != nil {
				t.Fatalf("PutNode() error = %v", err)
			}
			read, err := s.Node(ctx, "grok")
			if err != nil {
				t.Fatalf("Node() error = %v", err)
			}
			if tc.prepare != nil {
				tc.prepare(t, s, read)
			}
			next := read
			next.Secret = "s2"

			err = s.UpdateNode(ctx, read, next)

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("s.UpdateNode() error = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr != nil {
				return
			}
			got, err := s.Node(ctx, "grok")
			if err != nil || got.Secret != "s2" {
				t.Fatalf("s.Node() = %+v, %v, want secret s2", got, err)
			}
		})
	}
}

func TestFirstSeen(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	for i, want := range []bool{true, false} {
		got, err := s.FirstSeen(ctx, "telegram:1:2")
		if err != nil {
			t.Fatalf("FirstSeen() error = %v", err)
		}
		if got != want {
			t.Fatalf("FirstSeen() call %d = %v, want %v", i, got, want)
		}
	}
}

func TestQuestionAnsweredOnce(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	q := Question{ID: "abcd1234", Token: "tok", TaskID: "t", ContextID: "c", Data: map[string]any{"id": "x"}}
	if err := s.PutQuestion(ctx, q); err != nil {
		t.Fatalf("PutQuestion() error = %v", err)
	}
	if err := s.AnswerQuestion(ctx, q.ID, "user:owner", "once"); err != nil {
		t.Fatalf("AnswerQuestion() error = %v", err)
	}
	if err := s.AnswerQuestion(ctx, q.ID, "user:owner", "reject"); !errors.Is(err, ErrAnswered) {
		t.Fatalf("AnswerQuestion() second error = %v, want %v", err, ErrAnswered)
	}
	got, err := s.QuestionByToken(ctx, "tok")
	if err != nil {
		t.Fatalf("QuestionByToken() error = %v", err)
	}
	if got.Answer != "once" || got.Data["id"] != "x" {
		t.Fatalf("QuestionByToken() = %+v, want answer once", got)
	}
	if err := s.AnswerQuestion(ctx, "missing1", "user:owner", "once"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("AnswerQuestion() unknown question error = %v, want %v", err, ErrNotFound)
	}
}

func TestOpenPathWithURLCharacters(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "a?b#c%d", "server.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
}

func TestCloseWaitsForWrites(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "server.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	started, release, written := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		written <- s.inTx(ctx, func(tx *sql.Tx) error {
			close(started)
			<-release
			_, err := tx.ExecContext(ctx, `INSERT INTO channel_state (key, value) VALUES ('k', 'v')`)
			return err
		})
	}()
	<-started
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("Close() = %v during a write, want it to wait", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-written; err != nil {
		t.Fatalf("inTx() error = %v", err)
	}
	if err := <-closed; err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := s.ChannelState().Set(ctx, "k2", "v2"); !errors.Is(err, ErrClosed) {
		t.Fatalf("ChannelState().Set() after Close() error = %v, want %v", err, ErrClosed)
	}

	reopened := openStoreAt(t, path)
	if v, ok, err := reopened.ChannelState().Get(ctx, "k"); err != nil || !ok || v != "v" {
		t.Fatalf("ChannelState().Get() = %q, %v, %v, want the write made before Close()", v, ok, err)
	}
}

func TestSessions(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	ss := s.Sessions()
	if _, ok, err := ss.Get(ctx, "c"); err != nil || ok {
		t.Fatalf("Get() = %v, %v, want false, nil", ok, err)
	}
	if err := ss.Put(ctx, "c", a2aacp.Session{ID: "s1"}); err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	want := a2aacp.Session{ID: "s2", AgentKey: "k"}
	if err := ss.Put(ctx, "c", want); err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	got, ok, err := ss.Get(ctx, "c")
	if err != nil || !ok {
		t.Fatalf("Get() = %v, %v, want true, nil", ok, err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("Get() wrong result (-want +got) diff = %s", diff)
	}
}

func TestRemoteQuestionStoredOnce(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	tests := []struct {
		name    string
		q       Question
		wantErr error
	}{
		{name: "remote", q: Question{ID: "q1", Token: "t1", TaskID: "task", Contact: "peer", HitlID: "tool1"}},
		{name: "same remote question", q: Question{ID: "q2", Token: "t2", TaskID: "task", Contact: "peer", HitlID: "tool1"}, wantErr: ErrExists},
		{name: "next remote question", q: Question{ID: "q3", Token: "t3", TaskID: "task", Contact: "peer", HitlID: "tool2"}},
		{name: "local", q: Question{ID: "q4", Token: "t4", TaskID: "task", HitlID: "tool1"}},
		{name: "same local question", q: Question{ID: "q5", Token: "t5", TaskID: "task", HitlID: "tool1"}},
	}
	for _, tc := range tests {
		if err := s.PutQuestion(ctx, tc.q); !errors.Is(err, tc.wantErr) {
			t.Fatalf("PutQuestion(%s) error = %v, want %v", tc.name, err, tc.wantErr)
		}
	}
}

func TestUnansweredQuestions(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	for _, id := range []string{"q1", "q2", "q3"} {
		if err := s.PutQuestion(ctx, Question{ID: id, Token: "tok-" + id, TaskID: "t", ContextID: "c", Data: map[string]any{"id": id}}); err != nil {
			t.Fatalf("PutQuestion(%s) error = %v", id, err)
		}
	}
	if err := s.AnswerQuestion(ctx, "q2", "user:owner", "once"); err != nil {
		t.Fatalf("AnswerQuestion() error = %v", err)
	}
	got, err := s.UnansweredQuestions(ctx)
	if err != nil {
		t.Fatalf("UnansweredQuestions() error = %v", err)
	}
	ids := make([]string, len(got))
	for i, q := range got {
		ids[i] = q.ID
	}
	if diff := cmp.Diff([]string{"q3", "q1"}, ids); diff != "" {
		t.Fatalf("UnansweredQuestions() wrong result (-want +got) diff = %s", diff)
	}
}

func TestQuestionMessages(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	if err := s.PutQuestion(ctx, Question{ID: "q1", Token: "t1", TaskID: "task"}); err != nil {
		t.Fatalf("PutQuestion() error = %v", err)
	}
	want := []QuestionMessage{
		{QuestionID: "q1", Channel: "telegram", Conv: "1", MessageID: "10", Text: "approve?"},
		{QuestionID: "q1", Channel: "discord", Conv: "2", MessageID: "20", Text: "approve?"},
	}
	for _, m := range want {
		if err := s.AddQuestionMessage(ctx, m); err != nil {
			t.Fatalf("AddQuestionMessage() error = %v", err)
		}
	}
	got, err := s.TakeQuestionMessages(ctx, "q1")
	if err != nil {
		t.Fatalf("TakeQuestionMessages() error = %v", err)
	}
	if diff := cmp.Diff(want, got, cmpopts.SortSlices(func(a, b QuestionMessage) bool { return a.Channel < b.Channel })); diff != "" {
		t.Fatalf("TakeQuestionMessages() wrong result (-want +got) diff = %s", diff)
	}
	if got, err := s.TakeQuestionMessages(ctx, "q1"); err != nil || len(got) != 0 {
		t.Fatalf("TakeQuestionMessages() again = %v, %v, want none", got, err)
	}
}

func TestPendingQuestion(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	if _, err := s.PendingQuestion(ctx, "telegram", "1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("PendingQuestion() error = %v, want %v", err, ErrNotFound)
	}
	for _, id := range []string{"q1", "q2"} {
		if err := s.SetPendingQuestion(ctx, "telegram", "1", id); err != nil {
			t.Fatalf("SetPendingQuestion() error = %v", err)
		}
	}
	if id, err := s.PendingQuestion(ctx, "telegram", "1"); err != nil || id != "q2" {
		t.Fatalf("PendingQuestion() = %q, %v, want q2", id, err)
	}
	if err := s.ClearPendingQuestion(ctx, "telegram", "1"); err != nil {
		t.Fatalf("ClearPendingQuestion() error = %v", err)
	}
	if _, err := s.PendingQuestion(ctx, "telegram", "1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("PendingQuestion() after clearing error = %v, want %v", err, ErrNotFound)
	}
}

func TestHandles(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	h := Handle{Handle: "k3", ContextID: "c1"}
	if err := s.PutHandle(ctx, h); err != nil {
		t.Fatalf("PutHandle() error = %v", err)
	}
	for name, dup := range map[string]Handle{
		"taken handle": {Handle: "k3", ContextID: "c2"},
		"same context": {Handle: "zz", ContextID: "c1"},
	} {
		if err := s.PutHandle(ctx, dup); !errors.Is(err, ErrExists) {
			t.Fatalf("PutHandle(%s) error = %v, want %v", name, err, ErrExists)
		}
	}
	got, err := s.HandleOf(ctx, "c1")
	if err != nil {
		t.Fatalf("HandleOf() error = %v", err)
	}
	if diff := cmp.Diff(h, got); diff != "" {
		t.Fatalf("HandleOf() wrong result (-want +got) diff = %s", diff)
	}
	if _, err := s.HandleOf(ctx, "c2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("HandleOf() of another context error = %v, want %v", err, ErrNotFound)
	}
}

func TestCallTasks(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	for range 2 {
		if err := s.PutCallTask(ctx, CallTask{ContextID: "c1", Agent: "coder", TaskID: "t1", RemoteContextID: "r1"}); err != nil {
			t.Fatalf("PutCallTask() error = %v", err)
		}
	}
	tests := []struct {
		name  string
		check func() (bool, error)
		want  bool
	}{
		{name: "its task", check: func() (bool, error) { return s.HasCallTask(ctx, "c1", "coder", "t1") }, want: true},
		{name: "task of another conversation", check: func() (bool, error) { return s.HasCallTask(ctx, "c2", "coder", "t1") }},
		{name: "task at another agent", check: func() (bool, error) { return s.HasCallTask(ctx, "c1", "reviewer", "t1") }},
		{name: "its context", check: func() (bool, error) { return s.HasCallContext(ctx, "c1", "coder", "r1") }, want: true},
		{name: "context of another conversation", check: func() (bool, error) { return s.HasCallContext(ctx, "c2", "coder", "r1") }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := tc.check()
			if err != nil {
				t.Fatalf("check error = %v", err)
			}
			if got != tc.want {
				t.Fatalf("check = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHasOthersTasks(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	anna := s.Tasks("", func(context.Context) (string, bool, error) { return "pair:anna", false, nil })
	if _, err := anna.Create(ctx, &a2a.Task{ID: "t1", ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	tests := []struct {
		user string
		want bool
	}{
		{user: "pair:anna"},
		{user: "pair:ben", want: true},
	}
	for _, tc := range tests {
		t.Run(tc.user, func(t *testing.T) {
			t.Parallel()
			got, err := anna.HasOthersTasks(ctx, "c1", tc.user)
			if err != nil {
				t.Fatalf("HasOthersTasks() error = %v", err)
			}
			if got != tc.want {
				t.Fatalf("HasOthersTasks(%q) = %v, want %v", tc.user, got, tc.want)
			}
		})
	}
}

func TestTaskAttachmentsStoredOnce(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ts := s.Tasks("", testViewer)
	owner := asUser("owner")
	photo := bytes.Repeat([]byte{0xff, 0xd8, 0x01}, 4096)
	attachment := a2a.NewRawPart(photo)
	attachment.Filename, attachment.MediaType = "photo.jpg", "image/jpeg"
	task := newTask("t1", "c1")
	task.History = []*a2a.Message{a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("what is this?"), attachment)}
	if _, err := ts.Create(owner, task); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	for v := 1; v < 4; v++ {
		task.Artifacts = append(task.Artifacts, &a2a.Artifact{ID: a2a.NewArtifactID(), Parts: []*a2a.Part{a2a.NewTextPart("chunk")}})
		if _, err := ts.Update(owner, &taskstore.UpdateRequest{Task: task, PrevVersion: taskstore.TaskVersion(v)}); err != nil {
			t.Fatalf("Update() error = %v", err)
		}
	}

	var rowSize, blobs int
	if err := s.db.QueryRow(`SELECT length(data) FROM tasks WHERE id = 't1'`).Scan(&rowSize); err != nil {
		t.Fatalf("reading the task row: %v", err)
	}
	if rowSize >= len(photo) {
		t.Fatalf("task row size = %d, want less than the attachment (%d)", rowSize, len(photo))
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM blobs`).Scan(&blobs); err != nil {
		t.Fatalf("counting blobs: %v", err)
	}
	if blobs != 1 {
		t.Fatalf("blobs = %d, want 1", blobs)
	}

	got, err := ts.Get(owner, "t1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if diff := cmp.Diff(task, got.Task); diff != "" {
		t.Fatalf("Get() wrong result (-want +got) diff = %s", diff)
	}
	latest, err := ts.LatestInContext(context.Background(), "c1")
	if err != nil {
		t.Fatalf("LatestInContext() error = %v", err)
	}
	if diff := cmp.Diff(task, latest); diff != "" {
		t.Fatalf("LatestInContext() wrong result (-want +got) diff = %s", diff)
	}
	list, err := ts.List(owner, &a2a.ListTasksRequest{IncludeArtifacts: true})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if diff := cmp.Diff([]*a2a.Task{task}, list.Tasks); diff != "" {
		t.Fatalf("List() wrong result (-want +got) diff = %s", diff)
	}
}

func TestUpdateKeepsCallerTask(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ts := s.Tasks("", testViewer)
	owner := asUser("owner")
	task := newTask("t1", "c1")
	task.Status.Message = a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewRawPart([]byte("file")))
	if _, err := ts.Create(owner, task); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	want := a2a.NewRawPart([]byte("file"))
	if diff := cmp.Diff(want, task.Status.Message.Parts[0]); diff != "" {
		t.Fatalf("Create() changed the task (-want +got) diff = %s", diff)
	}
}

func TestPrune(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	start := time.Now()
	now := start
	s.now = func() time.Time { return now }
	s.SetTaskTTL(24 * time.Hour)
	ts := s.Tasks("", testViewer)
	owner := asUser("owner")

	old := newTask("old", "c-old")
	old.History = []*a2a.Message{a2a.NewMessage(a2a.MessageRoleUser, a2a.NewRawPart([]byte("photo")))}
	if _, err := ts.Create(owner, old); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if _, err := ts.Create(owner, newTask("running", "c-running")); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	now = start.Add(2 * time.Hour)
	if _, err := ts.Update(owner, &taskstore.UpdateRequest{
		Task:  newTask("running", "c-running"),
		Event: a2a.NewStatusUpdateEvent(newTask("running", "c-running"), a2a.TaskStateInputRequired, nil),
	}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	for _, q := range []Question{
		{ID: "q-old", Token: "t-old", TaskID: "old", ContextID: "c-old"},
		{ID: "q-running", Token: "t-running", TaskID: "running", ContextID: "c-running"},
	} {
		if err := s.PutQuestion(ctx, q); err != nil {
			t.Fatalf("PutQuestion() error = %v", err)
		}
	}
	if err := s.SetPendingQuestion(ctx, "telegram", "1", "q-old"); err != nil {
		t.Fatalf("SetPendingQuestion() error = %v", err)
	}
	for _, h := range []Handle{{Handle: "ab", ContextID: "c-old"}, {Handle: "cd", ContextID: "c-running"}} {
		if err := s.PutHandle(ctx, h); err != nil {
			t.Fatalf("PutHandle() error = %v", err)
		}
	}
	ensurePairing(t, s, Pairing{Name: "invited", Role: "authenticated"})
	ensurePairing(t, s, Pairing{Name: "joined", Kind: "person", Role: "authenticated"})
	for code, pairing := range map[string]string{"EXPIRED": "invited", "JOINED": "joined"} {
		if err := s.PutCode(ctx, code, Code{Pairing: pairing, Expires: start.Add(time.Minute)}); err != nil {
			t.Fatalf("PutCode() error = %v", err)
		}
	}

	now = start.Add(25 * time.Hour)
	if err := s.PutCode(ctx, "FRESH", Code{Pairing: "joined", Expires: now.Add(time.Minute)}); err != nil {
		t.Fatalf("PutCode() error = %v", err)
	}
	if err := s.Prune(ctx); err != nil {
		t.Fatalf("Prune() error = %v", err)
	}

	if _, err := ts.Get(owner, "old"); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Fatalf("Get() of a pruned task error = %v, want %v", err, a2a.ErrTaskNotFound)
	}
	if _, err := ts.Get(owner, "running"); err != nil {
		t.Fatalf("Get() of a recently updated task error = %v", err)
	}
	if _, err := s.QuestionByID(ctx, "q-old"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("QuestionByID() of a pruned task error = %v, want %v", err, ErrNotFound)
	}
	if _, err := s.QuestionByID(ctx, "q-running"); err != nil {
		t.Fatalf("QuestionByID() of a recently updated task error = %v", err)
	}
	if _, err := s.PendingQuestion(ctx, "telegram", "1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("PendingQuestion() of a pruned question error = %v, want %v", err, ErrNotFound)
	}
	if _, err := s.HandleByName(ctx, "ab"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("HandleByName() of a pruned context error = %v, want %v", err, ErrNotFound)
	}
	if _, err := s.HandleByName(ctx, "cd"); err != nil {
		t.Fatalf("HandleByName() of a live context error = %v", err)
	}
	if _, err := s.TakeCode(ctx, "EXPIRED", AnyCode); !errors.Is(err, ErrNotFound) {
		t.Fatalf("TakeCode() of an expired code error = %v, want %v", err, ErrNotFound)
	}
	if _, err := s.TakeCode(ctx, "FRESH", AnyCode); err != nil {
		t.Fatalf("TakeCode() of a fresh code error = %v", err)
	}
	if _, err := s.Pairing(ctx, "invited"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Pairing() nobody redeemed a code of error = %v, want %v", err, ErrNotFound)
	}
	if _, err := s.Pairing(ctx, "joined"); err != nil {
		t.Fatalf("Pairing() of a redeemed pairing error = %v, want nil", err)
	}
	var blobs int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM blobs`).Scan(&blobs); err != nil {
		t.Fatalf("counting blobs: %v", err)
	}
	if blobs != 0 {
		t.Fatalf("blobs after pruning = %d, want 0", blobs)
	}
}

func TestPruneKeepsPairingOfTakenCode(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	if err := s.InviteCode(ctx, Pairing{Name: "invited", Role: "authenticated"}, "CODE", time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("InviteCode() error = %v", err)
	}
	if _, err := s.TakeCode(ctx, "CODE", AnyCode); err != nil {
		t.Fatalf("TakeCode() error = %v", err)
	}

	if err := s.Prune(ctx); err != nil {
		t.Fatalf("Prune() error = %v", err)
	}

	if err := s.PutToken(ctx, "tok", Token{Pairing: "invited"}, "person"); err != nil {
		t.Fatalf("PutToken() after Prune() error = %v", err)
	}
}

func TestPruneForgetsOldRemoteQuestions(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	start := time.Now()
	now := start
	s.now = func() time.Time { return now }
	s.SetTaskTTL(24 * time.Hour)
	for _, q := range []Question{
		{ID: "q-unanswered", Token: "t1", TaskID: "r1", Contact: "helper", HitlID: "h1"},
		{ID: "q-answered", Token: "t2", TaskID: "r2", Contact: "helper", HitlID: "h2"},
	} {
		if err := s.PutQuestion(ctx, q); err != nil {
			t.Fatalf("PutQuestion() error = %v", err)
		}
	}
	if err := s.AnswerQuestion(ctx, "q-answered", "owner", "once"); err != nil {
		t.Fatalf("AnswerQuestion() error = %v", err)
	}
	now = start.Add(time.Hour)
	if err := s.PutQuestion(ctx, Question{ID: "q-fresh", Token: "t3", TaskID: "r3", Contact: "helper", HitlID: "h3"}); err != nil {
		t.Fatalf("PutQuestion() error = %v", err)
	}

	now = start.Add(24*time.Hour + time.Minute)
	if err := s.Prune(ctx); err != nil {
		t.Fatalf("Prune() error = %v", err)
	}

	for id, wantErr := range map[string]error{"q-unanswered": ErrNotFound, "q-answered": ErrNotFound, "q-fresh": nil} {
		if _, err := s.QuestionByID(ctx, id); !errors.Is(err, wantErr) {
			t.Fatalf("QuestionByID(%s) after Prune() error = %v, want %v", id, err, wantErr)
		}
	}
}

func TestStaleQuestions(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	ts := s.Tasks("", testViewer)
	owner := asUser("owner")
	for id, state := range map[string]a2a.TaskState{"waiting": a2a.TaskStateInputRequired, "failed": a2a.TaskStateFailed, "shown": a2a.TaskStateCanceled, "answered": a2a.TaskStateFailed, "quiet": a2a.TaskStateFailed} {
		task := newTask(id, "c-"+id)
		task.Status.State = state
		if _, err := ts.Create(owner, task); err != nil {
			t.Fatalf("Create() error = %v", err)
		}
	}
	for _, q := range []Question{
		{ID: "q-waiting", Token: "t1", TaskID: "waiting"},
		{ID: "q-failed", Token: "t2", TaskID: "failed"},
		{ID: "q-shown", Token: "t3", TaskID: "shown"},
		{ID: "q-answered", Token: "t4", TaskID: "answered"},
		{ID: "q-quiet", Token: "t5", TaskID: "quiet"},
		{ID: "q-pruned", Token: "t6", TaskID: "pruned"},
		{ID: "q-remote", Token: "t7", TaskID: "remote", Contact: "helper", HitlID: "h1"},
	} {
		if err := s.PutQuestion(ctx, q); err != nil {
			t.Fatalf("PutQuestion() error = %v", err)
		}
	}
	for conv, id := range map[string]string{"1": "q-waiting", "2": "q-failed", "3": "q-answered", "4": "q-pruned", "5": "q-remote"} {
		if err := s.SetPendingQuestion(ctx, "telegram", conv, id); err != nil {
			t.Fatalf("SetPendingQuestion() error = %v", err)
		}
	}
	if err := s.AddQuestionMessage(ctx, QuestionMessage{QuestionID: "q-shown", Channel: "telegram", Conv: "6", MessageID: "m1", Text: "git push?"}); err != nil {
		t.Fatalf("AddQuestionMessage() error = %v", err)
	}
	if err := s.AnswerQuestion(ctx, "q-answered", "owner", "once"); err != nil {
		t.Fatalf("AnswerQuestion() error = %v", err)
	}

	got, err := s.StaleQuestions(ctx)

	if err != nil {
		t.Fatalf("StaleQuestions() error = %v", err)
	}
	slices.Sort(got)
	if diff := cmp.Diff([]string{"q-failed", "q-pruned", "q-shown"}, got); diff != "" {
		t.Fatalf("StaleQuestions() wrong result (-want +got) diff = %s", diff)
	}
}

func TestReplaceCode(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	expires := time.Now().Add(time.Minute)
	ensurePairing(t, s, Pairing{Name: "grok", Role: "authenticated"})
	if err := s.PutCode(ctx, "OTHER", Code{Pairing: "grok", Expires: expires}); err != nil {
		t.Fatalf("PutCode() error = %v", err)
	}
	for _, code := range []string{"FIRST", "SECOND"} {
		if err := s.ReplaceCode(ctx, code, Code{Pairing: LocalPairing, Expires: expires}); err != nil {
			t.Fatalf("ReplaceCode(%s) error = %v", code, err)
		}
	}
	tests := []struct {
		code    string
		wantErr error
	}{
		{code: "FIRST", wantErr: ErrNotFound},
		{code: "SECOND"},
		{code: "OTHER"},
	}
	for _, tc := range tests {
		if _, err := s.TakeCode(ctx, tc.code, AnyCode); !errors.Is(err, tc.wantErr) {
			t.Fatalf("TakeCode(%s) error = %v, want %v", tc.code, err, tc.wantErr)
		}
	}
}

func TestCallerBlobReferencesStripped(t *testing.T) {
	t.Parallel()
	secret := []byte("another task's attachment")
	sum := sha256.Sum256(secret)
	real := hex.EncodeToString(sum[:])
	tests := []struct {
		name string
		ref  string
	}{
		{name: "unknown hash", ref: "deadbeef"},
		{name: "stored hash", ref: real},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := openStore(t)
			ts := s.Tasks("", testViewer)
			owner := asUser("owner")
			victim := newTask("victim", "c0")
			victim.History = []*a2a.Message{a2a.NewMessage(a2a.MessageRoleUser, a2a.NewRawPart(secret))}
			if _, err := ts.Create(owner, victim); err != nil {
				t.Fatalf("Create() error = %v", err)
			}
			part := a2a.NewTextPart("hello")
			part.Metadata = map[string]any{blobKey: tc.ref, "keep": "me"}
			task := newTask("t1", "c1")
			task.History = []*a2a.Message{a2a.NewMessage(a2a.MessageRoleUser, part)}
			if _, err := ts.Create(owner, task); err != nil {
				t.Fatalf("Create() error = %v", err)
			}

			got, err := ts.Get(owner, "t1")
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			wantPart := a2a.NewTextPart("hello")
			wantPart.Metadata = map[string]any{"keep": "me"}
			if diff := cmp.Diff(a2a.ContentParts{wantPart}, got.Task.History[0].Parts); diff != "" {
				t.Fatalf("Get() wrong result (-want +got) diff = %s", diff)
			}
			list, err := ts.List(owner, &a2a.ListTasksRequest{})
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}
			if len(list.Tasks) != 2 {
				t.Fatalf("List() returned %d tasks, want 2", len(list.Tasks))
			}
		})
	}
}

func TestMissingBlobDoesNotFailReads(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ts := s.Tasks("", testViewer)
	owner := asUser("owner")
	task := newTask("t1", "c1")
	task.History = []*a2a.Message{a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("see"), a2a.NewRawPart([]byte("lost")))}
	if _, err := ts.Create(owner, task); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if _, err := s.db.Exec(`DELETE FROM blobs`); err != nil {
		t.Fatalf("deleting blobs: %v", err)
	}

	got, err := ts.Get(owner, "t1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	want := a2a.ContentParts{a2a.NewTextPart("see"), a2a.NewTextPart(missingBlobText)}
	if diff := cmp.Diff(want, got.Task.History[0].Parts); diff != "" {
		t.Fatalf("Get() wrong result (-want +got) diff = %s", diff)
	}
}

func TestReadsDoNotWaitForWrites(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	started, release, written := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		written <- s.inTx(ctx, func(tx *sql.Tx) error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started
	readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	_, err := s.Schedules(readCtx)

	close(release)
	if werr := <-written; werr != nil {
		t.Fatalf("inTx() error = %v", werr)
	}
	if err != nil {
		t.Fatalf("Schedules() during a write error = %v", err)
	}
}

func TestWritesAreReadBack(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		path func(t *testing.T) string
	}{
		{name: "file", path: func(t *testing.T) string { return filepath.Join(t.TempDir(), "server.db") }},
		{name: "memory", path: func(*testing.T) string { return ":memory:" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := openStoreAt(t, tc.path(t))
			ctx := context.Background()
			for i := range 20 {
				name := fmt.Sprintf("s%d", i)
				if err := s.AddSchedule(ctx, Schedule{Name: name, Cron: "@daily", Prompt: "x", UserID: "user:owner", Conversation: "new", CreatedBy: "user:owner"}); err != nil {
					t.Fatalf("AddSchedule(%s) error = %v", name, err)
				}
				if _, err := s.Schedule(ctx, name); err != nil {
					t.Fatalf("Schedule(%s) right after AddSchedule() error = %v", name, err)
				}
			}
		})
	}
}

func TestOpenSetsSynchronousNormal(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	var got int
	if err := s.db.QueryRow(`PRAGMA synchronous`).Scan(&got); err != nil {
		t.Fatalf("reading synchronous: %v", err)
	}
	const normal = 1
	if got != normal {
		t.Fatalf("PRAGMA synchronous = %d, want %d (NORMAL)", got, normal)
	}
}

func TestLookupTokenRecordsUseOncePerMinute(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	start := time.Now()
	now := start
	s.now = func() time.Time { return now }
	ensurePairing(t, s, Pairing{Name: "grok", Role: "authenticated"})
	if err := s.PutToken(ctx, "a2p_secret", Token{Pairing: "grok", URL: "http://x"}, "agent"); err != nil {
		t.Fatalf("PutToken() error = %v", err)
	}
	tests := []struct {
		name string
		at   time.Duration
		want time.Duration
	}{
		{name: "first use", at: 0, want: 0},
		{name: "within a minute", at: 30 * time.Second, want: 0},
		{name: "after a minute", at: 61 * time.Second, want: 61 * time.Second},
		{name: "again within a minute", at: 90 * time.Second, want: 61 * time.Second},
	}
	for _, tc := range tests {
		now = start.Add(tc.at)
		if _, err := s.LookupToken(ctx, "a2p_secret"); err != nil {
			t.Fatalf("LookupToken() at %s error = %v", tc.name, err)
		}
		tokens, err := s.Tokens(ctx)
		if err != nil {
			t.Fatalf("Tokens() error = %v", err)
		}
		want := start.Add(tc.want)
		if got := tokens[0].LastUsed; !got.Equal(want) {
			t.Fatalf("Tokens()[0].LastUsed at %s = %v, want %v", tc.name, got, want)
		}
	}
}

func TestLookupTokenSucceedsWhenRecordingUseFails(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	ensurePairing(t, s, Pairing{Name: "grok", Role: "authenticated"})
	if err := s.PutToken(ctx, "a2p_secret", Token{Pairing: "grok", URL: "http://x"}, "agent"); err != nil {
		t.Fatalf("PutToken() error = %v", err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER no_updates BEFORE UPDATE ON tokens BEGIN SELECT RAISE(ABORT, 'database is locked'); END`); err != nil {
		t.Fatalf("creating the trigger: %v", err)
	}

	got, err := s.LookupToken(ctx, "a2p_secret")
	if err != nil {
		t.Fatalf("LookupToken() error = %v", err)
	}
	if got.Pairing != "grok" {
		t.Fatalf("LookupToken().Pairing = %q, want grok", got.Pairing)
	}
}

func TestPruneForgetsOldSeenKeys(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	start := time.Now()
	now := start
	s.now = func() time.Time { return now }
	if _, err := s.FirstSeen(ctx, "old"); err != nil {
		t.Fatalf("FirstSeen() error = %v", err)
	}
	now = start.Add(seenRetention + time.Hour)
	if _, err := s.FirstSeen(ctx, "fresh"); err != nil {
		t.Fatalf("FirstSeen() error = %v", err)
	}
	var rows int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM seen`).Scan(&rows); err != nil {
		t.Fatalf("counting seen keys: %v", err)
	}
	if rows != 2 {
		t.Fatalf("seen keys after FirstSeen() = %d, want 2: only Prune forgets old keys", rows)
	}

	if err := s.Prune(ctx); err != nil {
		t.Fatalf("Prune() error = %v", err)
	}

	for _, tc := range []struct {
		key  string
		want bool
	}{{"old", true}, {"fresh", false}} {
		got, err := s.FirstSeen(ctx, tc.key)
		if err != nil {
			t.Fatalf("FirstSeen(%q) error = %v", tc.key, err)
		}
		if got != tc.want {
			t.Fatalf("FirstSeen(%q) after Prune() = %v, want %v", tc.key, got, tc.want)
		}
	}
}

func TestSeenKeysAreIndexedByTime(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	rows, err := s.db.Query(`EXPLAIN QUERY PLAN DELETE FROM seen WHERE at < 1`)
	if err != nil {
		t.Fatalf("explaining the delete: %v", err)
	}
	defer closeRows(rows)
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatalf("reading the plan: %v", err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the plan: %v", err)
	}
	if got := strings.Join(plan, "; "); !strings.Contains(got, "seen_at") {
		t.Fatalf("query plan of deleting old seen keys = %q, want it to use the seen_at index", got)
	}
}
