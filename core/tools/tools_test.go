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

package tools_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/core/tools"
	"github.com/a2aproject/a2a-serve/internal/toolrules"
)

func TestRulesDecide(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		rules  tools.Rules
		call   tools.Call
		want   tools.Decision
		wantOK bool
	}{
		{name: "no entries", call: tools.Call{Kind: "read"}},
		{
			name:   "kind allow",
			rules:  tools.Rules{Allow: []string{"read"}},
			call:   tools.Call{Kind: "read"},
			want:   tools.Decision{Allow: true, Rule: "read"},
			wantOK: true,
		},
		{
			name:   "kind glob deny",
			rules:  tools.Rules{Deny: []string{"e*"}},
			call:   tools.Call{Kind: "edit"},
			want:   tools.Decision{Rule: "e*"},
			wantOK: true,
		},
		{
			name:  "kind not matched",
			rules: tools.Rules{Allow: []string{"read"}},
			call:  tools.Call{Kind: "edit"},
		},
		{
			name:   "tool allow",
			rules:  tools.Rules{Allow: []string{"tool:github.*"}},
			call:   tools.Call{Kind: "other", Tool: "github.create_issue", MCP: true},
			want:   tools.Decision{Allow: true, Rule: "tool:github.*"},
			wantOK: true,
		},
		{
			name:  "tool entry ignores a call without a tool",
			rules: tools.Rules{Allow: []string{"tool:github.*"}},
			call:  tools.Call{Kind: "other"},
		},
		{
			name:   "command allow with trailing wildcard matches bare command",
			rules:  tools.Rules{Allow: []string{"command:git status *"}},
			call:   tools.Call{Kind: "execute", Command: "git status"},
			want:   tools.Decision{Allow: true, Rule: "command:git status *"},
			wantOK: true,
		},
		{
			name:  "command entry never matches chained commands",
			rules: tools.Rules{Allow: []string{"command:git *"}},
			call:  tools.Call{Kind: "execute", Command: "git status; rm -rf /"},
		},
		{
			name:   "command entry beats kind entry",
			rules:  tools.Rules{Allow: []string{"command:git push *"}, Deny: []string{"execute"}},
			call:   tools.Call{Kind: "execute", Command: "git push origin"},
			want:   tools.Decision{Allow: true, Rule: "command:git push *"},
			wantOK: true,
		},
		{
			name:   "longer entry wins",
			rules:  tools.Rules{Allow: []string{"command:git *"}, Deny: []string{"command:git push *"}},
			call:   tools.Call{Kind: "execute", Command: "git push origin"},
			want:   tools.Decision{Rule: "command:git push *"},
			wantOK: true,
		},
		{
			name:   "deny wins a tie",
			rules:  tools.Rules{Allow: []string{"read"}, Deny: []string{"read"}},
			call:   tools.Call{Kind: "read"},
			want:   tools.Decision{Rule: "read"},
			wantOK: true,
		},
		{
			name:   "invalid entry matches nothing",
			rules:  tools.Rules{Deny: []string{"reed", "tool:"}, Allow: []string{"read"}},
			call:   tools.Call{Kind: "read"},
			want:   tools.Decision{Allow: true, Rule: "read"},
			wantOK: true,
		},
		{
			name:  "only invalid entries",
			rules: tools.Rules{Allow: []string{"*x"}},
			call:  tools.Call{Kind: "read"},
		},
		{
			name:   "context and task ids don't matter",
			rules:  tools.Rules{Allow: []string{"search"}},
			call:   tools.Call{Kind: "search", ContextID: "c", TaskID: "t"},
			want:   tools.Decision{Allow: true, Rule: "search"},
			wantOK: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok, err := tc.rules.Decide(t.Context(), tc.call)
			if err != nil {
				t.Fatalf("Rules.Decide() error = %v, want nil", err)
			}
			if ok != tc.wantOK {
				t.Fatalf("Rules.Decide() ok = %v, want %v", ok, tc.wantOK)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("Rules.Decide() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestRulesValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		rules   tools.Rules
		wantErr []string
	}{
		{name: "empty"},
		{name: "valid", rules: tools.Rules{Allow: []string{"read", "tool:grafana.*"}, Deny: []string{"command:git push *"}}},
		{name: "bad allow entry", rules: tools.Rules{Allow: []string{"read", "reed"}}, wantErr: []string{"allow", `"reed"`}},
		{name: "bad deny entry", rules: tools.Rules{Deny: []string{"command:a | b"}}, wantErr: []string{"deny", `"command:a | b"`}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.rules.Validate()
			if len(tc.wantErr) == 0 {
				if err != nil {
					t.Fatalf("Rules.Validate() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Rules.Validate() error = nil, want one with %q", tc.wantErr)
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("Rules.Validate() error = %v, want it to contain %q", err, want)
				}
			}
		})
	}
}

func TestChain(t *testing.T) {
	t.Parallel()
	errBoom := errors.New("boom")
	decide := func(d tools.Decision, ok bool, err error, calls *[]string, name string) tools.Decider {
		return tools.DeciderFunc(func(context.Context, tools.Call) (tools.Decision, bool, error) {
			*calls = append(*calls, name)
			return d, ok, err
		})
	}
	tests := []struct {
		name      string
		build     func(calls *[]string) []tools.Decider
		want      tools.Decision
		wantOK    bool
		wantErr   error
		wantCalls []string
	}{
		{name: "no deciders", build: func(*[]string) []tools.Decider { return nil }},
		{
			name: "first that decides answers",
			build: func(c *[]string) []tools.Decider {
				return []tools.Decider{
					decide(tools.Decision{}, false, nil, c, "a"),
					decide(tools.Decision{Allow: true, Rule: "b"}, true, nil, c, "b"),
					decide(tools.Decision{Rule: "c"}, true, nil, c, "c"),
				}
			},
			want:      tools.Decision{Allow: true, Rule: "b"},
			wantOK:    true,
			wantCalls: []string{"a", "b"},
		},
		{
			name: "none decides",
			build: func(c *[]string) []tools.Decider {
				return []tools.Decider{
					decide(tools.Decision{}, false, nil, c, "a"),
					decide(tools.Decision{}, false, nil, c, "b"),
				}
			},
			wantCalls: []string{"a", "b"},
		},
		{
			name: "error stops the chain",
			build: func(c *[]string) []tools.Decider {
				return []tools.Decider{
					decide(tools.Decision{}, false, errBoom, c, "a"),
					decide(tools.Decision{Allow: true}, true, nil, c, "b"),
				}
			},
			wantErr:   errBoom,
			wantCalls: []string{"a"},
		},
		{
			name: "error wins over a decision of the same decider",
			build: func(c *[]string) []tools.Decider {
				return []tools.Decider{decide(tools.Decision{Allow: true}, true, errBoom, c, "a")}
			},
			wantErr:   errBoom,
			wantCalls: []string{"a"},
		},
		{
			name: "rules in a chain",
			build: func(c *[]string) []tools.Decider {
				return []tools.Decider{
					tools.Rules{Deny: []string{"edit"}},
					tools.Rules{Allow: []string{"read"}},
					decide(tools.Decision{Rule: "fallback"}, true, nil, c, "fallback"),
				}
			},
			want:   tools.Decision{Allow: true, Rule: "read"},
			wantOK: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var calls []string
			chain := tools.Chain(tc.build(&calls)...)
			got, ok, err := chain.Decide(t.Context(), tools.Call{Kind: "read"})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Chain().Decide() error = %v, want %v", err, tc.wantErr)
			}
			if ok != tc.wantOK {
				t.Fatalf("Chain().Decide() ok = %v, want %v", ok, tc.wantOK)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("Chain().Decide() wrong result (-want +got) diff = %s", diff)
			}
			if diff := cmp.Diff(tc.wantCalls, calls); diff != "" {
				t.Fatalf("Chain().Decide() asked deciders wrong (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestMatch(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		entries  []string
		call     tools.Call
		wantRule string
		wantOK   bool
	}{
		{name: "no entries", call: tools.Call{Kind: "read"}},
		{name: "first match wins", entries: []string{"edit", "r*", "read"}, call: tools.Call{Kind: "read"}, wantRule: "r*", wantOK: true},
		{name: "invalid entry skipped", entries: []string{"reed", "tool:x.*"}, call: tools.Call{Tool: "x.y", MCP: true}, wantRule: "tool:x.*", wantOK: true},
		{name: "no match", entries: []string{"edit"}, call: tools.Call{Kind: "read"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rule, ok := tools.Match(tc.entries, tc.call)
			if rule != tc.wantRule || ok != tc.wantOK {
				t.Fatalf("tools.Match() = %q, %v, want %q, %v", rule, ok, tc.wantRule, tc.wantOK)
			}
		})
	}
}

func TestKinds(t *testing.T) {
	t.Parallel()
	if diff := cmp.Diff(toolrules.Kinds, tools.Kinds); diff != "" {
		t.Fatalf("tools.Kinds wrong result (-want +got) diff = %s", diff)
	}
	if &tools.Kinds[0] == &toolrules.Kinds[0] {
		t.Fatal("tools.Kinds shares its backing array with the engine's kinds, want a copy")
	}
}
