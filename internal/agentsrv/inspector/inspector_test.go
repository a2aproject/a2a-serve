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

package inspector_test

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aext/structuredinput"
	"github.com/google/go-cmp/cmp"
	"github.com/google/jsonschema-go/jsonschema"

	"github.com/a2aproject/a2a-serve/internal/agentsrv/approvals"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/inspector"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/store"
)

var csrfField = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

type fakeSender struct{}

func (fakeSender) SendMessage(context.Context, *a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
	return nil, nil
}

type testInspector struct {
	*inspector.Inspector
	store  *store.Store
	tasks  *store.TaskStore
	client *http.Client
	url    string
}

func startInspector(t *testing.T) *testInspector {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	all := func(context.Context) (string, bool, error) { return "", true, nil }
	tasks := st.Tasks("", all)
	in := inspector.New(inspector.Deps{
		Store:     st,
		Tasks:     tasks,
		Approvals: approvals.New(approvals.Deps{Records: approvals.NewRecords(approvals.RecordsDeps{Store: st, Tasks: tasks})}),
		ListPairings: func(context.Context) ([]controlapi.Pairing, error) {
			return []controlapi.Pairing{{Name: "anna", Kind: "person", Role: "owner"}}, nil
		},
		Sender:    fakeSender{},
		AgentName: func() string { return "coder" },
		BaseURL:   func() string { return "https://coder.example.com" },
		Status: func(context.Context) (*controlapi.Status, error) {
			return &controlapi.Status{Name: "coder", Harness: "opencode"}, nil
		},
		MarkAnswered: func(context.Context, string, string, string) {},
	})
	mux := http.NewServeMux()
	in.Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &testInspector{Inspector: in, store: st, tasks: tasks, client: client, url: ts.URL}
}

func (ti *testInspector) do(t *testing.T, req *http.Request) (int, string) {
	t.Helper()
	resp, err := ti.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s error = %v", req.Method, req.URL, err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("closing: %v", err)
		}
	}()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

func (ti *testInspector) get(t *testing.T, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, ti.url+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	return ti.do(t, req)
}

func (ti *testInspector) post(t *testing.T, path string, form url.Values) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ti.url+path, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return ti.do(t, req)
}

func (ti *testInspector) login(t *testing.T) string {
	t.Helper()
	if code, _ := ti.get(t, inspector.Path+"/login?code="+ti.LoginCode()); code != http.StatusSeeOther {
		t.Fatalf("GET login with a fresh code = %d, want %d", code, http.StatusSeeOther)
	}
	code, body := ti.get(t, inspector.Path+"/")
	if code != http.StatusOK {
		t.Fatalf("GET %s/ with the session = %d %q, want the overview", inspector.Path, code, body)
	}
	return body
}

func TestInspectorLogin(t *testing.T) {
	t.Parallel()
	ti := startInspector(t)
	if code, _ := ti.get(t, inspector.Path+"/"); code != http.StatusUnauthorized {
		t.Fatalf("GET %s/ without a session = %d, want %d", inspector.Path, code, http.StatusUnauthorized)
	}
	if code, _ := ti.get(t, inspector.Path+"/login?code=guess"); code != http.StatusUnauthorized {
		t.Fatalf("GET login with an unknown code = %d, want %d", code, http.StatusUnauthorized)
	}
	login := inspector.Path + "/login?code=" + ti.LoginCode()
	if code, _ := ti.get(t, login); code != http.StatusSeeOther {
		t.Fatalf("GET login with a fresh code = %d, want %d", code, http.StatusSeeOther)
	}
	if code, body := ti.get(t, inspector.Path+"/"); code != http.StatusOK || !strings.Contains(body, "<h1>coder") {
		t.Fatalf("GET %s/ with the session = %d %q, want the overview", inspector.Path, code, body)
	}
	other := startInspector(t)
	if code, _ := other.get(t, login); code != http.StatusUnauthorized {
		t.Fatalf("GET login of another inspector = %d, want %d", code, http.StatusUnauthorized)
	}
	reused := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := reused.Get(ti.url + login)
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET login a second time = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestInspectorSetsSecurityHeaders(t *testing.T) {
	t.Parallel()
	ti := startInspector(t)
	resp, err := ti.client.Get(ti.url + inspector.Path + "/")
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"Content-Security-Policy", "X-Content-Type-Options", "Referrer-Policy", "Cache-Control"} {
		if resp.Header.Get(h) == "" {
			t.Fatalf("GET %s/ header %s is empty, want it set", inspector.Path, h)
		}
	}
}

func TestInspectorPendingApprovals(t *testing.T) {
	t.Parallel()
	ti := startInspector(t)
	ctx := context.Background()
	rec := store.Question{
		ID: "q1", Token: "tok1", TaskID: "t1", ContextID: "c1", Contact: "reviewer", HitlID: "h1",
		Data: map[string]any{"id": "h1", "type": "permission", "title": "Push?", "text": "git push origin main", "options": []any{map[string]any{"id": "allow", "label": "Allow"}}},
	}
	if err := ti.store.PutQuestion(ctx, rec); err != nil {
		t.Fatalf("PutQuestion() error = %v", err)
	}
	got, err := ti.PendingApprovals(ctx)
	if err != nil {
		t.Fatalf("Inspector.PendingApprovals() error = %v", err)
	}
	want := []inspector.Approval{{
		Action: inspector.Path + "/questions/q1", Text: "git push origin main", ContextID: "c1", Contact: "reviewer",
		Options: []approvals.Option{{ID: "allow", Label: "Allow"}},
	}}
	if diff := cmp.Diff(want, got, cmp.FilterPath(func(p cmp.Path) bool { return p.Last().String() == ".Created" }, cmp.Ignore())); diff != "" {
		t.Fatalf("Inspector.PendingApprovals() wrong result (-want +got) diff = %s", diff)
	}
	overview := ti.login(t)
	for _, text := range []string{"git push origin main", "reviewer", inspector.Path + "/questions/q1", "anna"} {
		if !strings.Contains(overview, text) {
			t.Fatalf("overview = %q, want it to contain %q", overview, text)
		}
	}
}

func TestInspectorApprovalCount(t *testing.T) {
	t.Parallel()
	ti := startInspector(t)
	ctx := context.Background()
	for _, q := range []store.Question{
		{ID: "q1", Token: "tok1", TaskID: "remote", ContextID: "c1", Contact: "reviewer", HitlID: "h1"},
		{ID: "q2", Token: "tok2", TaskID: "remote", ContextID: "c1", Contact: "reviewer", HitlID: "h2"},
		{ID: "q3", Token: "tok3", TaskID: "t1", ContextID: "c1"},
	} {
		if err := ti.store.PutQuestion(ctx, q); err != nil {
			t.Fatalf("PutQuestion(%s) error = %v", q.ID, err)
		}
	}
	if err := ti.store.AnswerQuestion(ctx, "q2", "user:owner", "allow"); err != nil {
		t.Fatalf("AnswerQuestion() error = %v", err)
	}
	options := &jsonschema.Schema{Type: "string", OneOf: []*jsonschema.Schema{{Const: jsonschema.Ptr[any]("allow"), Title: "Allow"}}}
	schema, err := structuredinput.NewSchemaPart("tool1", &jsonschema.Schema{Type: "object", Properties: map[string]*jsonschema.Schema{"option": options}})
	if err != nil {
		t.Fatalf("structuredinput.NewSchemaPart() error = %v", err)
	}
	for id, status := range map[a2a.TaskID]a2a.TaskStatus{
		"t1": {State: a2a.TaskStateInputRequired, Message: a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("Push?"), schema)},
		"t2": {State: a2a.TaskStateInputRequired, Message: a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("Which branch?"))},
		"t3": {State: a2a.TaskStateWorking},
	} {
		if _, err := ti.tasks.Create(ctx, &a2a.Task{ID: id, ContextID: "c1", Status: status}); err != nil {
			t.Fatalf("TaskStore.Create(%s) error = %v", id, err)
		}
	}

	pending, err := ti.PendingApprovals(ctx)
	if err != nil {
		t.Fatalf("Inspector.PendingApprovals() error = %v", err)
	}
	got, err := ti.ApprovalCount(ctx)
	if err != nil {
		t.Fatalf("Inspector.ApprovalCount() error = %v", err)
	}
	if got != 2 || len(pending) != 2 {
		t.Fatalf("Inspector.ApprovalCount() = %d with %d pending approvals, want 2", got, len(pending))
	}
}

func TestInspectorAnswerRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		path string
		form url.Values
		want int
	}{
		{name: "unknown approval", path: "/questions/nope", form: url.Values{"option": {"allow"}}, want: http.StatusNotFound},
		{name: "unknown option", path: "/questions/q1", form: url.Values{"option": {"maybe"}}, want: http.StatusBadRequest},
		{name: "unknown task", path: "/tasks/nope/answer", form: url.Values{"option": {"allow"}}, want: http.StatusNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ti := startInspector(t)
			rec := store.Question{
				ID: "q1", Token: "tok1", TaskID: "t1", ContextID: "c1", Contact: "reviewer", HitlID: "h1",
				Data: map[string]any{"id": "h1", "type": "permission", "options": []any{map[string]any{"id": "allow", "label": "Allow"}}},
			}
			if err := ti.store.PutQuestion(context.Background(), rec); err != nil {
				t.Fatalf("PutQuestion() error = %v", err)
			}
			m := csrfField.FindStringSubmatch(ti.login(t))
			if m == nil {
				t.Fatalf("overview has no CSRF token")
			}
			tc.form.Set("csrf", m[1])
			if code, body := ti.post(t, inspector.Path+tc.path, tc.form); code != tc.want {
				t.Fatalf("POST %s = %d %q, want %d", tc.path, code, body, tc.want)
			}
		})
	}
}

func TestInspectorRejectsPostWithoutCSRFToken(t *testing.T) {
	t.Parallel()
	ti := startInspector(t)
	ti.login(t)
	if code, _ := ti.post(t, inspector.Path+"/questions/q1", url.Values{"option": {"allow"}}); code != http.StatusForbidden {
		t.Fatalf("POST answer without the CSRF token = %d, want %d", code, http.StatusForbidden)
	}
}

func TestInspectorConversation(t *testing.T) {
	t.Parallel()
	ti := startInspector(t)
	ctx := context.Background()
	user := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hello there"))
	task := &a2a.Task{
		ID: "t1", ContextID: "c1",
		Status:  a2a.TaskStatus{State: a2a.TaskStateCompleted, Message: a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("hi back"))},
		History: []*a2a.Message{user},
	}
	asBob := ti.store.Tasks("", func(context.Context) (string, bool, error) { return "pair:bob", false, nil })
	if _, err := asBob.Create(ctx, task); err != nil {
		t.Fatalf("TaskStore.Create() error = %v", err)
	}
	overview := ti.login(t)
	for _, text := range []string{inspector.Path + "/conversations/c1", "hello there", "completed", "<td>bob</td>"} {
		if !strings.Contains(overview, text) {
			t.Fatalf("overview = %q, want it to contain %q", overview, text)
		}
	}
	code, page := ti.get(t, inspector.Path+"/conversations/c1")
	if code != http.StatusOK {
		t.Fatalf("GET conversation = %d %q, want %d", code, page, http.StatusOK)
	}
	for _, text := range []string{"hello there", "hi back", "Task t1", "· bob ·"} {
		if !strings.Contains(page, text) {
			t.Fatalf("conversation = %q, want it to contain %q", page, text)
		}
	}
	if code, _ := ti.get(t, inspector.Path+"/conversations/unknown"); code != http.StatusNotFound {
		t.Fatalf("GET unknown conversation = %d, want %d", code, http.StatusNotFound)
	}
}
