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

package approval_test

import (
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aext/structuredinput"
	"github.com/google/go-cmp/cmp"
	"github.com/google/jsonschema-go/jsonschema"

	"github.com/a2aproject/a2a-serve/core/approval"
	"github.com/a2aproject/a2a-serve/internal/question"
)

func agentQuestion(t *testing.T, id, title string, content *jsonschema.Schema, toolCall map[string]any, options ...[2]string) *a2a.Message {
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
	parts := []*a2a.Part{a2a.NewTextPart("Run git push?"), part}
	if toolCall != nil {
		parts = append(parts, a2a.NewDataPart(map[string]any{"toolCall": toolCall}))
	}
	return a2a.NewMessage(a2a.MessageRoleAgent, parts...)
}

func askedLikeTheServer(t *testing.T, msg *a2a.Message, rule string, number bool) a2a.TaskStatus {
	t.Helper()
	msg, err := question.WithServerOptions(msg, rule)
	if err != nil {
		t.Fatalf("question.WithServerOptions() error = %v", err)
	}
	status := a2a.TaskStatus{State: a2a.TaskStateInputRequired, Message: msg}
	if !number {
		return status
	}
	ev, err := question.NewNumbering(nil).Number(a2a.NewStatusUpdateEvent(&a2a.Task{ID: "task", ContextID: "ctx"}, a2a.TaskStateInputRequired, msg))
	if err != nil {
		t.Fatalf("Numbering.Number() error = %v", err)
	}
	status.Message = ev.(*a2a.TaskStatusUpdateEvent).Status.Message
	return status
}

func TestFromStatus(t *testing.T) {
	t.Parallel()
	toolCall := map[string]any{"id": "tool1", "title": "git push", "kind": "execute", "input": map[string]any{"command": "git push"}}
	permission := func(t *testing.T) *a2a.Message {
		return agentQuestion(t, "tool1", "git push", nil, toolCall,
			[2]string{"once", "Allow"}, [2]string{"always", "Always allow"}, [2]string{"reject", "Reject"})
	}
	tests := []struct {
		name   string
		status func(t *testing.T) a2a.TaskStatus
		want   *approval.Request
	}{
		{
			name: "numbered permission with the server's options",
			status: func(t *testing.T) a2a.TaskStatus {
				return askedLikeTheServer(t, permission(t), "command:git push *", true)
			},
			want: &approval.Request{
				ID: "tool1", Type: approval.TypePermission, Title: "git push", Seq: 1,
				Text: "Run git push?",
				Options: []approval.Option{
					{ID: approval.OptionOnce, Label: "Allow once"},
					{ID: approval.OptionAlways, Label: "Always allow git push *"},
					{ID: approval.OptionReject, Label: "Deny"},
				},
				ToolCall: &approval.ToolCall{ID: "tool1", Title: "git push", Kind: "execute", Input: map[string]any{"command": "git push"}},
			},
		},
		{
			name: "permission without a number or a tool call",
			status: func(t *testing.T) a2a.TaskStatus {
				return a2a.TaskStatus{State: a2a.TaskStateInputRequired, Message: agentQuestion(t, "tool1", "git push", nil, nil,
					[2]string{"once", "Allow"}, [2]string{"reject", "Reject"})}
			},
			want: &approval.Request{
				ID: "tool1", Type: approval.TypePermission, Title: "git push", Text: "Run git push?",
				Options: []approval.Option{{ID: approval.OptionOnce, Label: "Allow"}, {ID: approval.OptionReject, Label: "Reject"}},
			},
		},
		{
			name: "form",
			status: func(t *testing.T) a2a.TaskStatus {
				return a2a.TaskStatus{State: a2a.TaskStateInputRequired, Message: agentQuestion(t, "elicitation-1", "Pick a branch", &jsonschema.Schema{Type: "object"}, nil,
					[2]string{"accept", "Accept"}, [2]string{"decline", "Decline"})}
			},
			want: &approval.Request{
				ID: "elicitation-1", Type: approval.TypeForm, Title: "Pick a branch", Text: "Run git push?",
				Options: []approval.Option{{ID: "accept", Label: "Accept"}, {ID: "decline", Label: "Decline"}},
			},
		},
		{
			name: "working task",
			status: func(t *testing.T) a2a.TaskStatus {
				return a2a.TaskStatus{State: a2a.TaskStateWorking, Message: permission(t)}
			},
		},
		{
			name: "plain input request",
			status: func(t *testing.T) a2a.TaskStatus {
				return a2a.TaskStatus{State: a2a.TaskStateInputRequired, Message: a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("Which branch?"))}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := approval.FromStatus(tc.status(t))
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("approval.FromStatus() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestAnswerIsParsedByTheEngine(t *testing.T) {
	t.Parallel()
	type answer struct {
		Option string `json:"option"`
		Seq    int    `json:"seq"`
	}
	tests := []struct {
		name    string
		status  func(t *testing.T) a2a.TaskStatus
		option  string
		wantSeq int
	}{
		{
			name: "once for a numbered permission",
			status: func(t *testing.T) a2a.TaskStatus {
				return askedLikeTheServer(t, agentQuestion(t, "tool1", "git push", nil, nil,
					[2]string{"once", "Allow"}, [2]string{"always", "Always allow"}, [2]string{"reject", "Reject"}), "command:git push *", true)
			},
			option:  approval.OptionOnce,
			wantSeq: 1,
		},
		{
			name: "always for a numbered permission",
			status: func(t *testing.T) a2a.TaskStatus {
				return askedLikeTheServer(t, agentQuestion(t, "tool1", "git push", nil, nil,
					[2]string{"once", "Allow"}, [2]string{"always", "Always allow"}, [2]string{"reject", "Reject"}), "command:git push *", true)
			},
			option:  approval.OptionAlways,
			wantSeq: 1,
		},
		{
			name: "reject for a numbered permission",
			status: func(t *testing.T) a2a.TaskStatus {
				return askedLikeTheServer(t, agentQuestion(t, "tool1", "git push", nil, nil,
					[2]string{"once", "Allow"}, [2]string{"always", "Always allow"}, [2]string{"reject", "Reject"}), "command:git push *", true)
			},
			option:  approval.OptionReject,
			wantSeq: 1,
		},
		{
			name: "permission without a number",
			status: func(t *testing.T) a2a.TaskStatus {
				return askedLikeTheServer(t, agentQuestion(t, "tool1", "git push", nil, nil,
					[2]string{"once", "Allow"}, [2]string{"reject", "Reject"}), "", false)
			},
			option: approval.OptionOnce,
		},
		{
			name: "form option",
			status: func(t *testing.T) a2a.TaskStatus {
				return a2a.TaskStatus{State: a2a.TaskStateInputRequired, Message: agentQuestion(t, "elicitation-1", "Pick a branch", &jsonschema.Schema{Type: "object"}, nil,
					[2]string{"accept", "Accept"}, [2]string{"decline", "Decline"})}
			},
			option: "decline",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			task := &a2a.Task{ID: "task", ContextID: "ctx", Status: tc.status(t)}
			r := approval.FromStatus(task.Status)
			if r == nil {
				t.Fatalf("approval.FromStatus() = nil, want a request")
			}
			reply, err := approval.Answer(task, r, tc.option)
			if err != nil {
				t.Fatalf("approval.Answer() error = %v", err)
			}
			if reply.TaskID != "task" || reply.ContextID != "ctx" || reply.Role != a2a.MessageRoleUser {
				t.Fatalf("approval.Answer() = task %q, context %q, role %q, want task, ctx, user", reply.TaskID, reply.ContextID, reply.Role)
			}
			req, err := structuredinput.PendingRequest(task)
			if err != nil {
				t.Fatalf("structuredinput.PendingRequest() error = %v", err)
			}
			var p structuredinput.Processor
			res, err := p.Process(t.Context(), req, reply)
			if err != nil {
				t.Fatalf("Processor.Process() error = %v", err)
			}
			if !res.Accepted() {
				t.Fatalf("Processor.Process() outcome = %v, want accepted", res.Outcome)
			}
			var got answer
			if err := res.Decode(&got); err != nil {
				t.Fatalf("ProcessorResult.Decode() error = %v", err)
			}
			if diff := cmp.Diff(answer{Option: tc.option, Seq: tc.wantSeq}, got); diff != "" {
				t.Fatalf("approval.Answer() wrong answer (-want +got) diff = %s", diff)
			}
			if got := question.FromStatus(task.Status).Check(question.ReplyRef(reply)); got != question.Matches {
				t.Fatalf("question.Check(question.ReplyRef(approval.Answer())) = %v, want %v", got, question.Matches)
			}
		})
	}
}

func TestAnswerErrors(t *testing.T) {
	t.Parallel()
	task := &a2a.Task{ID: "task", ContextID: "ctx"}
	r := &approval.Request{ID: "tool1", Options: []approval.Option{{ID: approval.OptionOnce}}}
	tests := []struct {
		name   string
		task   *a2a.Task
		r      *approval.Request
		option string
	}{
		{name: "option the request doesn't offer", task: task, r: r, option: approval.OptionAlways},
		{name: "no task", r: r, option: approval.OptionOnce},
		{name: "no request", task: task, option: approval.OptionOnce},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got, err := approval.Answer(tc.task, tc.r, tc.option); err == nil {
				t.Fatalf("approval.Answer() = %v, want an error", got)
			}
		})
	}
}
