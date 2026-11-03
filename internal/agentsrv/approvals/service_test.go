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

package approvals

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// sendingHandler answers SendMessage with err, recording the caller.
type sendingHandler struct {
	a2asrv.RequestHandler
	err    error
	caller auth.Principal
}

func (h *sendingHandler) SendMessage(ctx context.Context, _ *a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
	h.caller, _ = auth.Caller(ctx)
	return nil, h.err
}

func newService(t *testing.T, h a2asrv.RequestHandler) *Service {
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
	all := func(context.Context) (string, bool, error) { return "", true, nil }
	return &Service{Store: s, Tasks: s.Tasks("", all), Handler: h}
}

// fakeNotifier records the questions sent to chats.
type fakeNotifier struct {
	ownerChat *Chat
	mu        sync.Mutex
	asked     []Chat
	headers   []string
	done      chan struct{}
}

func (n *fakeNotifier) Ask(_ context.Context, to Chat, header string, _ store.Question) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.asked = append(n.asked, to)
	n.headers = append(n.headers, header)
	close(n.done)
}

func (n *fakeNotifier) OwnerChat(context.Context) (Chat, bool) {
	if n.ownerChat == nil {
		return Chat{}, false
	}
	return *n.ownerChat, true
}

func (n *fakeNotifier) MarkAnswered(context.Context, string, string, string) {}

func TestAnswerUndeliveredStaysOpen(t *testing.T) {
	t.Parallel()
	h := &sendingHandler{err: errors.New("agent unreachable")}
	svc := newService(t, h)
	ctx := context.Background()
	q := FromMessage(permissionMessage(t))
	rec, err := svc.NewRecord(ctx, "t1", "c1", "", "pair:anna", q)
	if err != nil {
		t.Fatalf("NewRecord() error = %v", err)
	}

	if err := svc.Answer(ctx, rec, auth.Owner(), q.Options[0]); err == nil {
		t.Fatalf("Answer() with a failing delivery error = nil, want error")
	}

	got, err := svc.Store.QuestionByID(ctx, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AnsweredBy != "" {
		t.Fatalf("question answered by %q after a failed delivery, want it open", got.AnsweredBy)
	}
	h.err = nil
	if err := svc.Answer(ctx, rec, auth.Owner(), q.Options[0]); err != nil {
		t.Fatalf("Answer() again error = %v, want nil", err)
	}
}

func TestAnswer(t *testing.T) {
	t.Parallel()
	member := func(id string) auth.Principal { return auth.Principal{ID: id, Roles: []string{auth.RoleMember}} }
	tests := []struct {
		name       string
		by         auth.Principal
		wantErr    error
		wantCaller auth.Principal
	}{
		{name: "starter", by: member("pair:anna"), wantCaller: member("pair:anna")},
		{name: "owner", by: auth.Owner(), wantCaller: auth.Owner()},
		{name: "agent on behalf of the starter", by: auth.Principal{ID: "agent:assistant", OnBehalfOf: "pair:anna"}, wantCaller: auth.Principal{ID: "agent:assistant", OnBehalfOf: "pair:anna"}},
		{name: "another member", by: member("pair:bob"), wantErr: ErrNotApprover},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := &sendingHandler{}
			svc := newService(t, h)
			ctx := context.Background()
			q := FromMessage(permissionMessage(t))
			rec, err := svc.NewRecord(ctx, "t1", "c1", "", "pair:anna", q)
			if err != nil {
				t.Fatalf("NewRecord() error = %v", err)
			}

			err = svc.Answer(ctx, rec, tc.by, q.Options[0])

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Answer() by %s error = %v, want %v", tc.by.ID, err, tc.wantErr)
			}
			if tc.wantErr == nil && (h.caller.ID != tc.wantCaller.ID || h.caller.OnBehalfOf != tc.wantCaller.OnBehalfOf) {
				t.Fatalf("answer sent as %+v, want %+v", h.caller, tc.wantCaller)
			}
		})
	}
}

func TestOnQuestion(t *testing.T) {
	t.Parallel()
	owner := Chat{Channel: "telegram", Conv: "dm1"}
	tests := []struct {
		name       string
		unattended bool
		ownerChat  *Chat
		question   *Question
		wantAsked  []Chat
		wantRecord bool
	}{
		{name: "attended task", ownerChat: &owner},
		{name: "unattended task", unattended: true, ownerChat: &owner, wantAsked: []Chat{owner}, wantRecord: true},
		{name: "unattended task without an owner chat", unattended: true, wantRecord: true},
		{name: "not a permission question", unattended: true, ownerChat: &owner, question: &Question{Type: "input"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc := newService(t, &sendingHandler{})
			n := &fakeNotifier{ownerChat: tc.ownerChat, done: make(chan struct{})}
			svc.Notifier = n
			svc.Unattended = func(string) bool { return tc.unattended }
			q := tc.question
			if q == nil {
				q = FromMessage(permissionMessage(t))
			}
			ctx := auth.WithPrincipal(context.Background(), auth.Principal{ID: "pair:anna"})

			svc.OnQuestion(ctx, &a2asrv.ExecutorContext{TaskID: "t1", ContextID: "c1"}, q)

			var recs []store.Question
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				var err error
				if recs, err = svc.Store.UnansweredQuestions(ctx); err != nil {
					t.Fatalf("UnansweredQuestions() error = %v", err)
				}
				if !tc.wantRecord || len(recs) > 0 {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if got := len(recs) > 0; got != tc.wantRecord {
				t.Fatalf("OnQuestion() stored a question = %v, want %v", got, tc.wantRecord)
			}
			if tc.wantRecord && recs[0].User != "pair:anna" {
				t.Fatalf("OnQuestion() question user = %q, want the starter", recs[0].User)
			}
			if tc.wantAsked != nil {
				<-n.done
			}
			n.mu.Lock()
			defer n.mu.Unlock()
			if diff := cmp.Diff(tc.wantAsked, n.asked); diff != "" {
				t.Fatalf("OnQuestion() asked chats wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}
