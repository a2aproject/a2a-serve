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
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
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

func newService(t *testing.T, h a2asrv.RequestHandler, opts ...func(*Deps)) *Service {
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
	policy := auth.Policy{Anonymous: []string{auth.MemberAnyone}}
	deps := Deps{
		Records: NewRecords(RecordsDeps{
			Store:   s,
			Tasks:   s.Tasks("", all),
			Chain:   &auth.Chain{Store: s, Policy: func() auth.Policy { return policy }},
			BaseURL: func() string { return "https://agent.example" },
		}),
		Sender:   h,
		Notifier: &fakeNotifier{},
		Config:   func() *agentdir.Config { return &agentdir.Config{Name: "assistant"} },
	}
	for _, opt := range opts {
		opt(&deps)
	}
	return New(deps)
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

func (n *fakeNotifier) MarkClosed(context.Context, string) {}

func (n *fakeNotifier) Follow(_ context.Context, _ store.Question, _ auth.Principal, deliver func() error) error {
	return deliver()
}

// storeTask stores a task in state whose status asks the question of
// permissionMessage.
func storeTask(t *testing.T, svc *Service, id a2a.TaskID, state a2a.TaskState) {
	t.Helper()
	task := &a2a.Task{ID: id, ContextID: "c1", Status: a2a.TaskStatus{State: state, Message: permissionMessage(t)}}
	if _, err := svc.tasks.Create(context.Background(), task); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
}

func TestAnswerUndeliveredStaysOpen(t *testing.T) {
	t.Parallel()
	h := &sendingHandler{err: errors.New("agent unreachable")}
	svc := newService(t, h)
	ctx := context.Background()
	storeTask(t, svc, "t1", a2a.TaskStateInputRequired)
	q := FromMessage(permissionMessage(t))
	rec, err := svc.NewRecord(ctx, "t1", "c1", "", "pair:anna", q)
	if err != nil {
		t.Fatalf("NewRecord() error = %v", err)
	}

	if err := svc.Answer(ctx, rec, auth.Owner(), q.Options[0]); err == nil {
		t.Fatalf("Answer() with a failing delivery error = nil, want error")
	}

	got, err := svc.store.QuestionByID(ctx, rec.ID)
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
	member := func(id string) auth.Principal { return auth.Principal{ID: id, Role: auth.RoleAuthenticated} }
	tests := []struct {
		name       string
		by         auth.Principal
		wantErr    error
		wantCaller auth.Principal
	}{
		{name: "starter", by: member("pair:anna"), wantCaller: member("pair:anna")},
		{name: "owner", by: auth.Owner(), wantCaller: auth.Owner()},
		{name: "agent naming the starter", by: auth.Principal{ID: "agent:assistant", Role: auth.RoleAuthenticated, OnBehalfOf: "pair:anna"}, wantErr: ErrNotApprover},
		{name: "starter as anonymous", by: auth.Principal{ID: "pair:anna", Role: auth.RoleAnonymous}, wantErr: ErrNotApprover},
		{name: "another authenticated caller", by: member("pair:bob"), wantErr: ErrNotApprover},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := &sendingHandler{}
			svc := newService(t, h)
			ctx := context.Background()
			storeTask(t, svc, "t1", a2a.TaskStateInputRequired)
			q := FromMessage(permissionMessage(t))
			rec, err := svc.NewRecord(ctx, "t1", "c1", "", "pair:anna", q)
			if err != nil {
				t.Fatalf("NewRecord() error = %v", err)
			}

			err = svc.Answer(ctx, rec, tc.by, q.Options[0])

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Answer() by %s error = %v, want %v", tc.by.ID, err, tc.wantErr)
			}
			if tc.wantErr == nil && h.caller.ID != tc.wantCaller.ID {
				t.Fatalf("answer sent as %+v, want %+v", h.caller, tc.wantCaller)
			}
		})
	}
}

func TestAnswerClosedQuestion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		state a2a.TaskState
		store bool
	}{
		{name: "failed task", state: a2a.TaskStateFailed, store: true},
		{name: "canceled task", state: a2a.TaskStateCanceled, store: true},
		{name: "pruned task"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := &sendingHandler{}
			svc := newService(t, h)
			ctx := context.Background()
			if tc.store {
				storeTask(t, svc, "t1", tc.state)
			}
			q := FromMessage(permissionMessage(t))
			rec, err := svc.NewRecord(ctx, "t1", "c1", "", "pair:anna", q)
			if err != nil {
				t.Fatalf("NewRecord() error = %v", err)
			}

			err = svc.Answer(ctx, rec, auth.Owner(), q.Options[0])

			if !errors.Is(err, ErrClosed) {
				t.Fatalf("Answer() error = %v, want %v", err, ErrClosed)
			}
			if h.caller.ID != "" {
				t.Fatalf("Answer() sent the answer as %q, want it not sent", h.caller.ID)
			}
			got, err := svc.store.QuestionByID(ctx, rec.ID)
			if err != nil {
				t.Fatalf("QuestionByID() error = %v", err)
			}
			if got.AnsweredBy != "" {
				t.Fatalf("Answer() recorded an answer by %q, want none", got.AnsweredBy)
			}
		})
	}
}

func TestCloseKeepsQuestionClosed(t *testing.T) {
	t.Parallel()
	h := &sendingHandler{}
	svc := newService(t, h)
	ctx := context.Background()
	storeTask(t, svc, "t1", a2a.TaskStateInputRequired)
	q := FromMessage(permissionMessage(t))
	rec, err := svc.NewRecord(ctx, "t1", "c1", "", "pair:anna", q)
	if err != nil {
		t.Fatalf("NewRecord() error = %v", err)
	}

	svc.Close(ctx, rec.ID)

	closed, err := svc.store.QuestionByID(ctx, rec.ID)
	if err != nil {
		t.Fatalf("QuestionByID() error = %v", err)
	}
	if err := svc.Answer(ctx, closed, auth.Owner(), q.Options[0]); !errors.Is(err, ErrClosed) {
		t.Fatalf("Answer() error = %v, want %v", err, ErrClosed)
	}
	open, err := svc.store.UnansweredQuestions(ctx)
	if err != nil {
		t.Fatalf("UnansweredQuestions() error = %v", err)
	}
	if len(open) != 0 {
		t.Fatalf("UnansweredQuestions() = %+v, want none", open)
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
			n := &fakeNotifier{ownerChat: tc.ownerChat, done: make(chan struct{})}
			svc := newService(t, &sendingHandler{}, func(d *Deps) {
				d.Notifier = n
				d.Unattended = func(string) bool { return tc.unattended }
			})
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
				if recs, err = svc.store.UnansweredQuestions(ctx); err != nil {
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

func TestHandlePage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		user         string
		wantGet      int
		wantPost     int
		wantAnswerBy string
	}{
		{name: "owner's task", user: auth.OwnerID, wantGet: http.StatusOK, wantPost: http.StatusSeeOther, wantAnswerBy: auth.OwnerID},
		{name: "authenticated pairing's task", user: "pair:anna", wantGet: http.StatusOK, wantPost: http.StatusSeeOther, wantAnswerBy: "pair:anna"},
		{name: "anonymous sender's task", user: "email:stranger@example.org", wantGet: http.StatusForbidden, wantPost: http.StatusForbidden},
		{name: "unlinked chat account's task", user: "telegram:9", wantGet: http.StatusForbidden, wantPost: http.StatusForbidden},
		{name: "removed pairing's task", user: "pair:gone", wantGet: http.StatusForbidden, wantPost: http.StatusForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := &sendingHandler{}
			svc := newService(t, h)
			ctx := context.Background()
			if _, err := svc.store.EnsurePairing(ctx, store.Pairing{Name: "anna", Kind: "person", Role: auth.RoleAuthenticated}); err != nil {
				t.Fatalf("EnsurePairing() error = %v", err)
			}
			storeTask(t, svc, "t1", a2a.TaskStateInputRequired)
			q := FromMessage(permissionMessage(t))
			rec, err := svc.NewRecord(ctx, "t1", "c1", "", tc.user, q)
			if err != nil {
				t.Fatalf("NewRecord() error = %v", err)
			}
			page := func(method string, body string) int {
				req := httptest.NewRequest(method, "/hitl/"+rec.Token, strings.NewReader(body))
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				req.SetPathValue("token", rec.Token)
				w := httptest.NewRecorder()
				svc.HandlePage(w, req)
				return w.Code
			}

			if got := page(http.MethodGet, ""); got != tc.wantGet {
				t.Fatalf("HandlePage() GET status = %d, want %d", got, tc.wantGet)
			}
			if got := page(http.MethodPost, url.Values{"option": {"once"}}.Encode()); got != tc.wantPost {
				t.Fatalf("HandlePage() POST status = %d, want %d", got, tc.wantPost)
			}

			got, err := svc.store.QuestionByID(ctx, rec.ID)
			if err != nil {
				t.Fatalf("QuestionByID() error = %v", err)
			}
			if got.AnsweredBy != tc.wantAnswerBy {
				t.Fatalf("HandlePage() answered by %q, want %q", got.AnsweredBy, tc.wantAnswerBy)
			}
			if h.caller.ID != tc.wantAnswerBy {
				t.Fatalf("HandlePage() sent the answer as %q, want %q", h.caller.ID, tc.wantAnswerBy)
			}
		})
	}
}

func TestPageURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		user string
		want bool
	}{
		{name: "owner's task", user: auth.OwnerID, want: true},
		{name: "authenticated pairing's task", user: "pair:anna", want: true},
		{name: "anonymous sender's task", user: "email:stranger@example.org"},
		{name: "unlinked chat account's task", user: "telegram:9"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc := newService(t, &sendingHandler{})
			ctx := context.Background()
			if _, err := svc.store.EnsurePairing(ctx, store.Pairing{Name: "anna", Kind: "person", Role: auth.RoleAuthenticated}); err != nil {
				t.Fatalf("EnsurePairing() error = %v", err)
			}
			rec, err := svc.NewRecord(ctx, "t1", "c1", "", tc.user, FromMessage(permissionMessage(t)))
			if err != nil {
				t.Fatalf("NewRecord() error = %v", err)
			}

			got, err := svc.PageURL(ctx, rec)

			if err != nil {
				t.Fatalf("PageURL() error = %v", err)
			}
			want := ""
			if tc.want {
				want = "https://agent.example/hitl/" + rec.Token
			}
			if got != want {
				t.Fatalf("PageURL() = %q, want %q", got, want)
			}
		})
	}
}

func TestOutcome(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		answered  string
		by        string
		err       error
		wantLabel string
		wantBy    string
	}{
		{name: "answer was recorded", err: nil, wantLabel: "Allow", wantBy: "web"},
		{name: "another answer won", answered: "reject", by: "pair:anna", err: store.ErrAnswered, wantLabel: "reject", wantBy: "anna"},
		{name: "same answer won", answered: "once", by: auth.OwnerID, err: store.ErrAnswered, wantLabel: "Allow", wantBy: "owner"},
		{name: "delivery failed", err: errors.New("agent unreachable"), wantLabel: "Allow", wantBy: "web"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc := newService(t, &sendingHandler{})
			ctx := context.Background()
			q := FromMessage(permissionMessage(t))
			rec, err := svc.NewRecord(ctx, "t1", "c1", "", "pair:anna", q)
			if err != nil {
				t.Fatalf("NewRecord() error = %v", err)
			}
			if tc.answered != "" {
				if err := svc.store.AnswerQuestion(ctx, rec.ID, tc.by, tc.answered); err != nil {
					t.Fatalf("AnswerQuestion() error = %v", err)
				}
			}
			once, ok := q.Match("once")
			if !ok {
				t.Fatal("question has no option once")
			}

			label, by := svc.Outcome(ctx, rec, once, "web", tc.err)

			if label != tc.wantLabel || by != tc.wantBy {
				t.Fatalf("svc.Outcome() = %q, %q, want %q, %q", label, by, tc.wantLabel, tc.wantBy)
			}
		})
	}
}

func TestHandlePageSetsSecurityHeaders(t *testing.T) {
	t.Parallel()
	svc := newService(t, &sendingHandler{})
	ctx := context.Background()
	storeTask(t, svc, "t1", a2a.TaskStateInputRequired)
	rec, err := svc.NewRecord(ctx, "t1", "c1", "", auth.OwnerID, FromMessage(permissionMessage(t)))
	if err != nil {
		t.Fatalf("NewRecord() error = %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/hitl/"+rec.Token, nil)
	req.SetPathValue("token", rec.Token)
	w := httptest.NewRecorder()

	svc.HandlePage(w, req)

	for _, h := range []string{"Content-Security-Policy", "X-Content-Type-Options", "Referrer-Policy", "Cache-Control"} {
		if w.Header().Get(h) == "" {
			t.Errorf("HandlePage() header %s is empty, want it set", h)
		}
	}
}

func TestCheckAnswerHidesOthersTasks(t *testing.T) {
	t.Parallel()
	svc := newService(t, &sendingHandler{})
	storeTask(t, svc, "t1", a2a.TaskStateInputRequired)
	anna := auth.Principal{ID: "pair:anna", Role: auth.RoleAuthenticated}
	ctx := context.Background()

	if err := svc.CheckAnswer(ctx, auth.Owner(), "t1"); err != nil {
		t.Fatalf("CheckAnswer(owner) error = %v, want nil", err)
	}
	random := svc.CheckAnswer(ctx, anna, "random")
	others := svc.CheckAnswer(ctx, anna, "t1")
	if random != nil || !errors.Is(others, a2a.ErrTaskNotFound) {
		t.Fatalf("CheckAnswer(anna, owner's task) error = %v, want %v as for a random ID", others, a2a.ErrTaskNotFound)
	}
}
