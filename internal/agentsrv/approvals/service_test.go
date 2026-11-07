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

	if _, err := svc.Answer(ctx, rec, auth.Owner(), q.Options[0], "owner"); err == nil {
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
	if out, err := svc.Answer(ctx, rec, auth.Owner(), q.Options[0], "owner"); err != nil || out != Answered {
		t.Fatalf("Answer() again = %v, %v, want %v", out, err, Answered)
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

			_, err = svc.Answer(ctx, rec, tc.by, q.Options[0], "web")

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

			out, err := svc.Answer(ctx, rec, auth.Owner(), q.Options[0], "owner")

			if err != nil || out != Closed {
				t.Fatalf("Answer() = %v, %v, want %v", out, err, Closed)
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
	if out, err := svc.Answer(ctx, closed, auth.Owner(), q.Options[0], "owner"); err != nil || out != Closed {
		t.Fatalf("Answer() = %v, %v, want %v", out, err, Closed)
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
	owner := Chat{Messenger: "telegram", Conv: "dm1"}
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

func TestAnsweredWith(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		answered  string
		by        string
		wantLabel string
		wantBy    string
	}{
		{name: "another answer won", answered: "reject", by: "pair:anna", wantLabel: "reject", wantBy: "anna"},
		{name: "same answer won", answered: "once", by: auth.OwnerID, wantLabel: "Allow", wantBy: "owner"},
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
			if err := svc.store.AnswerQuestion(ctx, rec.ID, tc.by, tc.answered); err != nil {
				t.Fatalf("AnswerQuestion() error = %v", err)
			}
			once, ok := q.Match("once")
			if !ok {
				t.Fatal("question has no option once")
			}

			label, by := svc.answeredWith(ctx, rec, once, "web")

			if label != tc.wantLabel || by != tc.wantBy {
				t.Fatalf("svc.answeredWith() = %q, %q, want %q, %q", label, by, tc.wantLabel, tc.wantBy)
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

func TestCheckAnswer(t *testing.T) {
	t.Parallel()
	anonymous := auth.Principal{ID: auth.AnonymousID, Role: auth.RoleAnonymous}
	anna := auth.Principal{ID: "pair:anna", Role: auth.RoleAuthenticated}
	starterScope := func(ctx context.Context) (string, bool, error) {
		p, _ := auth.Caller(ctx)
		return p.ID, false, nil
	}
	tests := []struct {
		name    string
		starter auth.Principal
		caller  auth.Principal
		taskID  a2a.TaskID
		want    error
	}{
		{name: "owner", starter: anonymous, caller: auth.Owner(), taskID: "t1"},
		{name: "starter", starter: anna, caller: anna, taskID: "t1"},
		{name: "another user's task", starter: anonymous, caller: anna, taskID: "t1", want: a2a.ErrTaskNotFound},
		{name: "anonymous starter", starter: anonymous, caller: anonymous, taskID: "t1", want: a2a.ErrUnauthorized},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc := newService(t, &sendingHandler{})
			ctx := auth.WithPrincipal(context.Background(), tc.starter)
			task := &a2a.Task{ID: "t1", ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired, Message: permissionMessage(t)}}
			if _, err := svc.store.Tasks("", starterScope).Create(ctx, task); err != nil {
				t.Fatalf("Create() error = %v", err)
			}

			err := svc.CheckAnswer(context.Background(), tc.caller, tc.taskID)

			if !errors.Is(err, tc.want) || (tc.want == nil && err != nil) {
				t.Fatalf("CheckAnswer(%s, %s) error = %v, want %v", tc.caller.ID, tc.taskID, err, tc.want)
			}
		})
	}
}

// scriptedHandler answers each SendMessage with the next of errs, and nil
// once they run out.
type scriptedHandler struct {
	a2asrv.RequestHandler
	mu    sync.Mutex
	errs  []error
	calls int
}

func (h *scriptedHandler) SendMessage(context.Context, *a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls++
	if len(h.errs) == 0 {
		return nil, nil
	}
	err := h.errs[0]
	h.errs = h.errs[1:]
	return nil, err
}

func (h *scriptedHandler) sent() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

var errInProgress = a2a.NewError(a2a.ErrUnsupportedOperation, "task execution is already in progress")

func TestAnswerOutcome(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		state          a2a.TaskState
		answeredBy     string
		sendErr        error
		want           Outcome
		wantErr        bool
		wantSent       int
		wantAnsweredBy string
	}{
		{name: "delivered", state: a2a.TaskStateInputRequired, want: Answered, wantSent: 1, wantAnsweredBy: auth.OwnerID},
		{name: "task ended", state: a2a.TaskStateCompleted, want: Closed},
		{name: "answered before", state: a2a.TaskStateInputRequired, answeredBy: "pair:anna", want: AlreadyAnswered, wantAnsweredBy: "pair:anna"},
		{name: "task busy with another answer", state: a2a.TaskStateInputRequired, sendErr: errInProgress, want: InProgress, wantSent: 1},
		{name: "delivery failed", state: a2a.TaskStateInputRequired, sendErr: errors.New("agent unreachable"), wantErr: true, wantSent: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := &scriptedHandler{errs: []error{tc.sendErr}}
			svc := newService(t, h)
			ctx := context.Background()
			storeTask(t, svc, "t1", tc.state)
			q := FromMessage(permissionMessage(t))
			rec, err := svc.NewRecord(ctx, "t1", "c1", "", "pair:anna", q)
			if err != nil {
				t.Fatalf("NewRecord() error = %v", err)
			}
			if tc.answeredBy != "" {
				if err := svc.store.AnswerQuestion(ctx, rec.ID, tc.answeredBy, "reject"); err != nil {
					t.Fatalf("AnswerQuestion() error = %v", err)
				}
			}

			got, err := svc.Answer(ctx, rec, auth.Owner(), q.Options[0], "owner")

			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("Answer() = %v, %v, want %v, error %v", got, err, tc.want, tc.wantErr)
			}
			if n := h.sent(); n != tc.wantSent {
				t.Fatalf("Answer() sent %d answers, want %d", n, tc.wantSent)
			}
			stored, err := svc.store.QuestionByID(ctx, rec.ID)
			if err != nil {
				t.Fatalf("QuestionByID() error = %v", err)
			}
			if stored.AnsweredBy != tc.wantAnsweredBy {
				t.Fatalf("Answer() left the question answered by %q, want %q", stored.AnsweredBy, tc.wantAnsweredBy)
			}
		})
	}
}

func TestAnswerTask(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		record     bool
		answeredBy string
		question   string
		sendErr    error
		want       Outcome
		wantSent   int
	}{
		{name: "no record", want: Answered, wantSent: 1},
		{name: "open record", record: true, want: Answered, wantSent: 1},
		{name: "record a person answered", record: true, answeredBy: "pair:anna", want: AlreadyAnswered},
		{name: "task asks another question", question: "other", want: Closed},
		{name: "task busy with another answer", sendErr: errInProgress, want: InProgress, wantSent: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := &scriptedHandler{errs: []error{tc.sendErr}}
			svc := newService(t, h)
			ctx := context.Background()
			storeTask(t, svc, "t1", a2a.TaskStateInputRequired)
			q := FromMessage(permissionMessage(t))
			if tc.record {
				rec, err := svc.NewRecord(ctx, "t1", "c1", "", "pair:anna", q)
				if err != nil {
					t.Fatalf("NewRecord() error = %v", err)
				}
				if tc.answeredBy != "" {
					if err := svc.store.AnswerQuestion(ctx, rec.ID, tc.answeredBy, "once"); err != nil {
						t.Fatalf("AnswerQuestion() error = %v", err)
					}
				}
			}
			asked := *q
			if tc.question != "" {
				asked.ID = tc.question
			}
			reject, ok := q.Match("reject")
			if !ok {
				t.Fatal("question has no option to reject it")
			}

			got, err := svc.AnswerTask(ctx, "t1", "c1", &asked, timeoutPrincipal, reject, "timeout")

			if err != nil || got != tc.want {
				t.Fatalf("AnswerTask() = %v, %v, want %v", got, err, tc.want)
			}
			if n := h.sent(); n != tc.wantSent {
				t.Fatalf("AnswerTask() sent %d answers, want %d", n, tc.wantSent)
			}
		})
	}
}

func TestUserApprovalTimeoutRetriesWhileTheTaskIsBusy(t *testing.T) {
	t.Parallel()
	h := &scriptedHandler{errs: []error{errInProgress}}
	svc := newService(t, h, func(d *Deps) {
		d.User = func(context.Context, string) bool { return true }
		d.UserTimeout = time.Millisecond
	})
	storeTask(t, svc, "t1", a2a.TaskStateInputRequired)

	svc.OnQuestion(context.Background(), &a2asrv.ExecutorContext{TaskID: "t1", ContextID: "c1"}, FromMessage(permissionMessage(t)))

	deadline := time.Now().Add(5 * time.Second)
	for h.sent() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("timeout sent %d answers, want a second one after the task was busy", h.sent())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestUserApprovalTimeoutRetriesWhileAnotherAnswerHoldsTheQuestion(t *testing.T) {
	t.Parallel()
	h := &scriptedHandler{}
	svc := newService(t, h, func(d *Deps) {
		d.User = func(context.Context, string) bool { return true }
		d.UserTimeout = time.Millisecond
	})
	ctx := context.Background()
	storeTask(t, svc, "t1", a2a.TaskStateInputRequired)
	q := FromMessage(permissionMessage(t))
	rec, err := svc.NewRecord(ctx, "t1", "c1", "", "pair:anna", q)
	if err != nil {
		t.Fatalf("NewRecord() error = %v", err)
	}
	if err := svc.store.AnswerQuestion(ctx, rec.ID, "pair:anna", "once"); err != nil {
		t.Fatalf("AnswerQuestion() error = %v", err)
	}

	svc.OnQuestion(ctx, &a2asrv.ExecutorContext{TaskID: "t1", ContextID: "c1"}, q)
	time.Sleep(100 * time.Millisecond)
	if n := h.sent(); n != 0 {
		t.Fatalf("timeout sent %d answers while another answer held the question, want 0", n)
	}
	svc.Release(ctx, "t1", q.ID, "pair:anna")

	deadline := time.Now().Add(5 * time.Second)
	for h.sent() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("timeout sent no answer after the other answer was released")
		}
		time.Sleep(10 * time.Millisecond)
	}
	got, err := svc.store.QuestionByID(ctx, rec.ID)
	if err != nil {
		t.Fatalf("QuestionByID() error = %v", err)
	}
	if got.AnsweredBy != timeoutPrincipal.ID {
		t.Fatalf("question answered by %q, want %q", got.AnsweredBy, timeoutPrincipal.ID)
	}
}
