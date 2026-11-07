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
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aext/structuredinput"
	"github.com/a2aproject/a2a-go/v2/integrations/executor/a2aacp"
	"github.com/google/go-cmp/cmp"
	"github.com/google/jsonschema-go/jsonschema"

	"github.com/a2aproject/a2a-serve/internal/store"
)

func questionMessage(t *testing.T, id, title string, content *jsonschema.Schema, options ...[2]string) *a2a.Message {
	t.Helper()
	optionSchema := &jsonschema.Schema{Type: "string"}
	for _, o := range options {
		optionSchema.OneOf = append(optionSchema.OneOf, &jsonschema.Schema{Const: jsonschema.Ptr[any](o[0]), Title: o[1]})
	}
	schema := &jsonschema.Schema{Type: "object", Title: title, Properties: map[string]*jsonschema.Schema{"option": optionSchema}}
	if content != nil {
		schema.Properties["content"] = content
	}
	part, err := structuredinput.NewSchemaPart(id, schema)
	if err != nil {
		t.Fatalf("structuredinput.NewSchemaPart() error = %v", err)
	}
	return a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("Run git push?"), part)
}

func permissionMessage(t *testing.T) *a2a.Message {
	t.Helper()
	return questionMessage(t, "tool1", "git push", nil, [2]string{"once", "Allow"}, [2]string{"always", "Always allow"}, [2]string{"reject", ""})
}

func withToolCall(msg *a2a.Message, kind string) *a2a.Message {
	msg.Parts = append(msg.Parts, a2a.NewDataPart(map[string]any{"toolCall": map[string]any{"id": "tool1", "kind": kind}}))
	return msg
}

func TestFromStatus(t *testing.T) {
	t.Parallel()
	inputRequired := func(msg *a2a.Message) a2a.TaskStatus {
		return a2a.TaskStatus{State: a2a.TaskStateInputRequired, Message: msg}
	}
	otherSchema, err := structuredinput.NewSchemaPart("q", &jsonschema.Schema{Type: "object", Properties: map[string]*jsonschema.Schema{"branch": {Type: "string"}}})
	if err != nil {
		t.Fatalf("structuredinput.NewSchemaPart() error = %v", err)
	}
	tests := []struct {
		name   string
		status a2a.TaskStatus
		want   *Question
	}{
		{
			name:   "permission",
			status: inputRequired(permissionMessage(t)),
			want: &Question{ID: "tool1", Type: TypePermission, Title: "git push", Text: "Run git push?",
				Options: []Option{{ID: "once", Label: "Allow"}, {ID: "always", Label: "Always allow"}, {ID: "reject", Label: "reject"}}},
		},
		{
			name:   "permission with its tool call",
			status: inputRequired(withToolCall(permissionMessage(t), "execute")),
			want: &Question{ID: "tool1", Type: TypePermission, Title: "git push", Text: "Run git push?", Kind: "execute", ToolCallID: "tool1",
				Options: []Option{{ID: "once", Label: "Allow"}, {ID: "always", Label: "Always allow"}, {ID: "reject", Label: "reject"}}},
		},
		{
			name: "form",
			status: inputRequired(questionMessage(t, "elicitation-1", "Pick a branch", &jsonschema.Schema{Type: "object"},
				[2]string{"accept", "Accept"}, [2]string{"decline", "Decline"})),
			want: &Question{ID: "elicitation-1", Type: TypeForm, Title: "Pick a branch", Text: "Run git push?",
				Options: []Option{{ID: "accept", Label: "Accept"}, {ID: "decline", Label: "Decline"}}},
		},
		{name: "working", status: a2a.TaskStatus{State: a2a.TaskStateWorking, Message: permissionMessage(t)}},
		{name: "plain input request", status: inputRequired(a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("Which branch?")))},
		{name: "input request without options", status: inputRequired(a2a.NewMessage(a2a.MessageRoleAgent, otherSchema))},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := FromStatus(tc.status)
			if diff := cmp.Diff(tc.want, got, cmp.FilterPath(func(p cmp.Path) bool { return p.Last().String() == ".Data" }, cmp.Ignore())); diff != "" {
				t.Fatalf("FromStatus() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestFromRecordKeepsText(t *testing.T) {
	t.Parallel()
	q := FromMessage(permissionMessage(t))
	rec := store.Question{Data: map[string]any{"id": q.ID, "type": q.Type, "title": q.Title, "options": q.Data["options"], "text": q.Text}}
	got := FromRecord(rec)
	if got.Display() != "Run git push?" || len(got.Options) != 3 {
		t.Fatalf("FromRecord() = %+v, want the text and three options", got)
	}
}

func TestDisplay(t *testing.T) {
	t.Parallel()
	const warning = "⚠ This request has hidden formatting characters, shown as [U+XXXX].\n"
	tests := []struct {
		name string
		q    Question
		want string
	}{
		{name: "text", q: Question{Title: "git push", Text: "Run git push?"}, want: "Run git push?"},
		{name: "title without text", q: Question{Title: "git push"}, want: "git push"},
		{name: "override", q: Question{Text: "rm -rf ~ #\u202Ehs.tset"}, want: warning + "rm -rf ~ #[U+202E]hs.tset"},
		{name: "title with override", q: Question{Title: "cat \u202Dtxt"}, want: warning + "cat [U+202D]txt"},
		{
			name: "embeddings and isolates",
			q:    Question{Text: "\u202Aa\u202Bb\u202Cc\u2066d\u2067e\u2068f\u2069"},
			want: warning + "[U+202A]a[U+202B]b[U+202C]c[U+2066]d[U+2067]e[U+2068]f[U+2069]",
		},
		{name: "zero-width spaces", q: Question{Text: "r\u200Bm \uFEFF-rf\u2060"}, want: warning + "r[U+200B]m [U+FEFF]-rf[U+2060]"},
		{
			name: "emoji sequences, joiners and marks",
			q:    Question{Text: "👩\u200D💻 🇺🇦 می\u200Cخواهم \u200Fשלום\u200E ok"},
			want: "👩\u200D💻 🇺🇦 می\u200Cخواهم \u200Fשלום\u200E ok",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.q.Display(); got != tc.want {
				t.Fatalf("Display() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMatch(t *testing.T) {
	t.Parallel()
	q := FromMessage(permissionMessage(t))
	tests := []struct {
		reply  string
		want   string
		wantOK bool
	}{
		{reply: "Allow", want: "once", wantOK: true},
		{reply: "always allow", want: "always", wantOK: true},
		{reply: "approve.", want: "once", wantOK: true},
		{reply: " Deny! ", want: "reject", wantOK: true},
		{reply: "reject", want: "reject", wantOK: true},
		{reply: "lgtm", want: "once", wantOK: true},
		{reply: "allow always", want: "always", wantOK: true},
		{reply: "decline", want: "reject", wantOK: true},
		{reply: "maybe"},
	}
	for _, tc := range tests {
		t.Run(tc.reply, func(t *testing.T) {
			t.Parallel()
			got, ok := q.Match(tc.reply)
			if ok != tc.wantOK || got.ID != tc.want {
				t.Fatalf("Match(%q) = %q, %v, want %q, %v", tc.reply, got.ID, ok, tc.want, tc.wantOK)
			}
		})
	}
	if got, want := q.Words(), "approve, always, deny"; got != want {
		t.Fatalf("Words() = %q, want %q", got, want)
	}
}

func TestAnswerMessage(t *testing.T) {
	t.Parallel()
	msg, err := AnswerMessage("task", "ctx", "tool1", "once")
	if err != nil {
		t.Fatalf("AnswerMessage() error = %v", err)
	}
	task := &a2a.Task{ID: "task", ContextID: "ctx", Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired, Message: permissionMessage(t)}}
	req, err := structuredinput.PendingRequest(task)
	if err != nil {
		t.Fatalf("structuredinput.PendingRequest() error = %v", err)
	}
	var p structuredinput.Processor
	res, err := p.Process(t.Context(), req, msg)
	if err != nil {
		t.Fatalf("Processor.Process() error = %v", err)
	}
	if !res.Accepted() {
		t.Fatalf("Processor.Process() outcome = %v, want accepted", res.Outcome)
	}
	var got a2aacp.Answer
	if err := res.Decode(&got); err != nil {
		t.Fatalf("ProcessorResult.Decode() error = %v", err)
	}
	if diff := cmp.Diff(a2aacp.Answer{Option: "once"}, got); diff != "" {
		t.Fatalf("AnswerMessage() wrong result (-want +got) diff = %s", diff)
	}
	if msg.TaskID != "task" || msg.ContextID != "ctx" {
		t.Fatalf("AnswerMessage() = task %q, context %q, want task, ctx", msg.TaskID, msg.ContextID)
	}
}
