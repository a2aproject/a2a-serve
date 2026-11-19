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

package a2aserve_test

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/core/a2aserve"
	"github.com/a2aproject/a2a-serve/internal/toolrules"
)

func TestToolRulesDecide(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		rules  a2aserve.ToolRules
		call   a2aserve.ToolCall
		want   a2aserve.ToolDecision
		wantOK bool
	}{
		{name: "no entries", call: a2aserve.ToolCall{Kind: "read"}},
		{
			name:   "kind allow",
			rules:  a2aserve.ToolRules{Allow: []string{"read"}},
			call:   a2aserve.ToolCall{Kind: "read"},
			want:   a2aserve.ToolDecision{Allow: true, Rule: "read"},
			wantOK: true,
		},
		{
			name:   "kind glob deny",
			rules:  a2aserve.ToolRules{Deny: []string{"e*"}},
			call:   a2aserve.ToolCall{Kind: "edit"},
			want:   a2aserve.ToolDecision{Rule: "e*"},
			wantOK: true,
		},
		{
			name:  "kind not matched",
			rules: a2aserve.ToolRules{Allow: []string{"read"}},
			call:  a2aserve.ToolCall{Kind: "edit"},
		},
		{
			name:   "tool allow",
			rules:  a2aserve.ToolRules{Allow: []string{"tool:github.*"}},
			call:   a2aserve.ToolCall{Kind: "other", Tool: "github.create_issue", MCP: true},
			want:   a2aserve.ToolDecision{Allow: true, Rule: "tool:github.*"},
			wantOK: true,
		},
		{
			name:  "tool entry ignores a call without a tool",
			rules: a2aserve.ToolRules{Allow: []string{"tool:github.*"}},
			call:  a2aserve.ToolCall{Kind: "other"},
		},
		{
			name:   "command allow with trailing wildcard matches bare command",
			rules:  a2aserve.ToolRules{Allow: []string{"command:git status *"}},
			call:   a2aserve.ToolCall{Kind: "execute", Command: "git status"},
			want:   a2aserve.ToolDecision{Allow: true, Rule: "command:git status *"},
			wantOK: true,
		},
		{
			name:  "command entry never matches chained commands",
			rules: a2aserve.ToolRules{Allow: []string{"command:git *"}},
			call:  a2aserve.ToolCall{Kind: "execute", Command: "git status; rm -rf /"},
		},
		{
			name:   "command entry beats kind entry",
			rules:  a2aserve.ToolRules{Allow: []string{"command:git push *"}, Deny: []string{"execute"}},
			call:   a2aserve.ToolCall{Kind: "execute", Command: "git push origin"},
			want:   a2aserve.ToolDecision{Allow: true, Rule: "command:git push *"},
			wantOK: true,
		},
		{
			name:   "longer entry wins",
			rules:  a2aserve.ToolRules{Allow: []string{"command:git *"}, Deny: []string{"command:git push *"}},
			call:   a2aserve.ToolCall{Kind: "execute", Command: "git push origin"},
			want:   a2aserve.ToolDecision{Rule: "command:git push *"},
			wantOK: true,
		},
		{
			name:   "deny wins a tie",
			rules:  a2aserve.ToolRules{Allow: []string{"read"}, Deny: []string{"read"}},
			call:   a2aserve.ToolCall{Kind: "read"},
			want:   a2aserve.ToolDecision{Rule: "read"},
			wantOK: true,
		},
		{
			name:   "invalid entry matches nothing",
			rules:  a2aserve.ToolRules{Deny: []string{"reed", "tool:"}, Allow: []string{"read"}},
			call:   a2aserve.ToolCall{Kind: "read"},
			want:   a2aserve.ToolDecision{Allow: true, Rule: "read"},
			wantOK: true,
		},
		{
			name:  "only invalid entries",
			rules: a2aserve.ToolRules{Allow: []string{"*x"}},
			call:  a2aserve.ToolCall{Kind: "read"},
		},
		{
			name:   "context and task ids don't matter",
			rules:  a2aserve.ToolRules{Allow: []string{"search"}},
			call:   a2aserve.ToolCall{Kind: "search", ContextID: "c", TaskID: "t"},
			want:   a2aserve.ToolDecision{Allow: true, Rule: "search"},
			wantOK: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok, err := tc.rules.Decide(t.Context(), tc.call)
			if err != nil {
				t.Fatalf("ToolRules.Decide() error = %v, want nil", err)
			}
			if ok != tc.wantOK {
				t.Fatalf("ToolRules.Decide() ok = %v, want %v", ok, tc.wantOK)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("ToolRules.Decide() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestToolRulesValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		rules   a2aserve.ToolRules
		wantErr []string
	}{
		{name: "empty"},
		{name: "valid", rules: a2aserve.ToolRules{Allow: []string{"read", "tool:grafana.*"}, Deny: []string{"command:git push *"}}},
		{name: "bad allow entry", rules: a2aserve.ToolRules{Allow: []string{"read", "reed"}}, wantErr: []string{"allow", `"reed"`}},
		{name: "bad deny entry", rules: a2aserve.ToolRules{Deny: []string{"command:a | b"}}, wantErr: []string{"deny", `"command:a | b"`}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.rules.Validate()
			if len(tc.wantErr) == 0 {
				if err != nil {
					t.Fatalf("ToolRules.Validate() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ToolRules.Validate() error = nil, want one with %q", tc.wantErr)
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("ToolRules.Validate() error = %v, want it to contain %q", err, want)
				}
			}
		})
	}
}

func TestToolKinds(t *testing.T) {
	t.Parallel()
	if diff := cmp.Diff(toolrules.Kinds, a2aserve.ToolKinds); diff != "" {
		t.Fatalf("a2aserve.ToolKinds wrong result (-want +got) diff = %s", diff)
	}
	if &a2aserve.ToolKinds[0] == &toolrules.Kinds[0] {
		t.Fatal("a2aserve.ToolKinds shares its backing array with the engine's kinds, want a copy")
	}
}
