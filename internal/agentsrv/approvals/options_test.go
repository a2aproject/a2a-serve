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
	"github.com/google/go-cmp/cmp"
	"github.com/google/jsonschema-go/jsonschema"

	"github.com/a2aproject/a2a-serve/internal/a2atext"
	"github.com/a2aproject/a2a-serve/internal/store"
)

func TestWithServerOptions(t *testing.T) {
	t.Parallel()
	asked := func(t *testing.T, options ...[2]string) *a2a.Message {
		msg := questionMessage(t, "tool1", "git push", nil, options...)
		msg.Parts[0] = a2a.NewTextPart("Permission required: git push.\nReply with one of: " + "once, always, reject.")
		return msg
	}
	agentOptions := [][2]string{{"once", "Allow"}, {"always", "Always"}, {"reject", "Deny"}, {"plan_auto", "Yes, and auto-accept edits"}}
	tests := []struct {
		name      string
		msg       *a2a.Message
		rule      string
		want      []Option
		wantAgent map[string]string
		wantText  string
	}{
		{
			name:      "with a rule",
			msg:       asked(t, agentOptions...),
			rule:      "command:git push *",
			want:      []Option{{ID: "once", Label: "Allow once"}, {ID: "always", Label: "Always allow git push *"}, {ID: "reject", Label: "Deny"}},
			wantAgent: map[string]string{"once": "once", "always": "once", "reject": "reject"},
			wantText:  "Permission required: git push.\nReply with one of: once, always (allows git push * in this conversation), reject.",
		},
		{
			name:      "without a rule",
			msg:       asked(t, agentOptions...),
			want:      []Option{{ID: "once", Label: "Allow once"}, {ID: "reject", Label: "Deny"}},
			wantAgent: map[string]string{"once": "once", "reject": "reject"},
			wantText:  "Permission required: git push.\nReply with one of: once, reject.",
		},
		{
			name:      "agent that only allows always",
			msg:       asked(t, [2]string{"always", "Always"}, [2]string{"reject_always", "Never"}),
			rule:      "tool:grafana.query",
			want:      []Option{{ID: "once", Label: "Allow once"}, {ID: "always", Label: "Always allow grafana.query"}, {ID: "reject", Label: "Deny"}},
			wantAgent: map[string]string{"once": "always", "always": "always", "reject": "reject_always"},
			wantText:  "Permission required: git push.\nReply with one of: once, always (allows grafana.query in this conversation), reject.",
		},
		{
			name:     "agent that offers no way to allow",
			msg:      asked(t, [2]string{"reject", "Deny"}),
			rule:     "edit",
			want:     []Option{{ID: "reject", Label: "Deny"}},
			wantText: "Permission required: git push.\nReply with one of: once, always, reject.",
		},
		{
			name:     "form",
			msg:      questionMessage(t, "f1", "Pick a branch", &jsonschema.Schema{Type: "object"}, [2]string{"accept", "Accept"}, [2]string{"decline", "Decline"}),
			rule:     "edit",
			want:     []Option{{ID: "accept", Label: "Accept"}, {ID: "decline", Label: "Decline"}},
			wantText: "Run git push?",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			msg, err := WithServerOptions(tc.msg, tc.rule)
			if err != nil {
				t.Fatalf("WithServerOptions() error = %v", err)
			}
			q := FromMessage(msg)
			if diff := cmp.Diff(tc.want, q.Options); diff != "" {
				t.Fatalf("WithServerOptions() wrong options (-want +got) diff = %s", diff)
			}
			if diff := cmp.Diff(tc.wantAgent, q.AgentOptions); diff != "" {
				t.Fatalf("WithServerOptions() wrong agent options (-want +got) diff = %s", diff)
			}
			if got := a2atext.Message(msg); got != tc.wantText {
				t.Fatalf("WithServerOptions() text = %q, want %q", got, tc.wantText)
			}
			again, err := WithServerOptions(msg, "read")
			if err != nil || again != msg {
				t.Fatalf("WithServerOptions() of a rewritten question = %v, %v, want it unchanged", again, err)
			}
		})
	}
}

func TestServerOptionsRule(t *testing.T) {
	t.Parallel()
	msg, err := WithServerOptions(questionMessage(t, "tool1", "git push", nil, [2]string{"once", "Allow"}), "command:git push *")
	if err != nil {
		t.Fatalf("WithServerOptions() error = %v", err)
	}
	if got := FromMessage(msg).Rule; got != "command:git push *" {
		t.Fatalf("FromMessage().Rule = %q, want %q", got, "command:git push *")
	}
	rec := FromRecord(store.Question{Data: FromMessage(msg).Data})
	if rec.Rule != "command:git push *" {
		t.Fatalf("FromRecord().Rule = %q, want %q", rec.Rule, "command:git push *")
	}
	if got, want := rec.Words(), "approve, always (allows git push * in this conversation)"; got != want {
		t.Fatalf("Words() = %q, want %q", got, want)
	}
}
