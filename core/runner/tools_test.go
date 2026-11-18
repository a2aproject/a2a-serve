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

package runner

import (
	"context"
	"testing"

	"github.com/a2aproject/a2a-go/v2/integrations/executor/a2aacp"

	"github.com/a2aproject/a2a-serve/internal/toolrules"
)

func TestToolDeciderReadsCurrentRules(t *testing.T) {
	t.Parallel()
	rules := toolrules.Rules{}
	decider := &toolDecider{rules: func() toolrules.Rules { return rules }}
	q := a2aacp.Question{Options: []string{a2aacp.OptionOnce, a2aacp.OptionReject}, Tool: &a2aacp.ToolCall{Kind: "read"}}

	if _, ok := decider.AutoAnswer(context.Background(), nil, q); ok {
		t.Fatalf("AutoAnswer() without rules ok = true, want false")
	}
	rules = toolrules.Rules{Allow: []string{"read"}}
	got, ok := decider.AutoAnswer(context.Background(), nil, q)
	if !ok || got.Option != a2aacp.OptionOnce {
		t.Fatalf("AutoAnswer() after allowing read = %+v, %v, want %q", got, ok, a2aacp.OptionOnce)
	}
}

func TestToolDeciderAnswers(t *testing.T) {
	t.Parallel()
	rules := toolrules.Rules{
		Allow: []string{"tool:grafana.*", "command:kubectl get *"},
		Deny:  []string{"execute", "other"},
	}
	decider := &toolDecider{
		rules:   func() toolrules.Rules { return rules },
		servers: func() []string { return []string{"grafana"} },
	}
	options := []string{a2aacp.OptionAlways, a2aacp.OptionOnce, a2aacp.OptionReject}
	tests := []struct {
		name    string
		options []string
		tool    a2aacp.ToolCall
		want    string
		wantOK  bool
	}{
		{name: "allowed MCP tool of claude", tool: a2aacp.ToolCall{Kind: "other", Title: "mcp__grafana__query"}, want: a2aacp.OptionOnce, wantOK: true},
		{name: "allowed MCP tool of opencode", tool: a2aacp.ToolCall{Kind: "other", Title: "grafana_query"}, want: a2aacp.OptionOnce, wantOK: true},
		{name: "denied MCP tool", tool: a2aacp.ToolCall{Kind: "other", Title: "mcp__gmail__send"}, want: a2aacp.OptionReject, wantOK: true},
		{name: "allowed command", tool: a2aacp.ToolCall{Kind: "execute", Title: "kubectl get pods", Input: map[string]any{"command": "kubectl get pods"}}, want: a2aacp.OptionOnce, wantOK: true},
		{name: "denied command", tool: a2aacp.ToolCall{Kind: "execute", Title: "kubectl get pods; rm -rf /", Input: map[string]any{"command": "kubectl get pods; rm -rf /"}}, want: a2aacp.OptionReject, wantOK: true},
		{name: "allowed without once", options: []string{a2aacp.OptionAlways, a2aacp.OptionReject}, tool: a2aacp.ToolCall{Kind: "other", Title: "grafana_query"}, want: a2aacp.OptionAlways, wantOK: true},
		{name: "denied without reject", options: []string{a2aacp.OptionOnce}, tool: a2aacp.ToolCall{Kind: "execute"}, want: a2aacp.OptionCancel, wantOK: true},
		{name: "no rule", tool: a2aacp.ToolCall{Kind: "edit"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts := tc.options
			if opts == nil {
				opts = options
			}
			got, ok := decider.AutoAnswer(context.Background(), nil, a2aacp.Question{Options: opts, Tool: &tc.tool})
			if ok != tc.wantOK || got.Option != tc.want {
				t.Fatalf("AutoAnswer() = %q, %v, want %q, %v", got.Option, ok, tc.want, tc.wantOK)
			}
		})
	}
}
