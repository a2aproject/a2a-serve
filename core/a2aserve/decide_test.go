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

package a2aserve

import (
	"context"
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentrun"
	"github.com/a2aproject/a2a-serve/internal/toolrules"
)

func TestDecideToolsAsksInOrder(t *testing.T) {
	t.Parallel()
	errBoom := errors.New("boom")
	decide := func(d ToolDecision, ok bool, err error, calls *[]string, name string) ToolDecider {
		return ToolDeciderFunc(func(context.Context, ToolCall) (ToolDecision, bool, error) {
			*calls = append(*calls, name)
			return d, ok, err
		})
	}
	tests := []struct {
		name      string
		build     func(calls *[]string) []ToolDecider
		want      agentrun.Decision
		wantOK    bool
		wantErr   error
		wantCalls []string
	}{
		{
			name: "first that decides answers",
			build: func(c *[]string) []ToolDecider {
				return []ToolDecider{
					decide(ToolDecision{}, false, nil, c, "a"),
					nil,
					decide(ToolDecision{Allow: true, Rule: "b"}, true, nil, c, "b"),
					decide(ToolDecision{Rule: "c"}, true, nil, c, "c"),
				}
			},
			want:      agentrun.Decision{Allow: true, Rule: "b"},
			wantOK:    true,
			wantCalls: []string{"a", "b"},
		},
		{
			name: "none decides",
			build: func(c *[]string) []ToolDecider {
				return []ToolDecider{decide(ToolDecision{}, false, nil, c, "a"), decide(ToolDecision{}, false, nil, c, "b")}
			},
			wantCalls: []string{"a", "b"},
		},
		{
			name: "error stops the deciders",
			build: func(c *[]string) []ToolDecider {
				return []ToolDecider{decide(ToolDecision{}, false, errBoom, c, "a"), decide(ToolDecision{Allow: true}, true, nil, c, "b")}
			},
			wantErr:   errBoom,
			wantCalls: []string{"a"},
		},
		{
			name: "error wins over a decision of the same decider",
			build: func(c *[]string) []ToolDecider {
				return []ToolDecider{decide(ToolDecision{Allow: true}, true, errBoom, c, "a")}
			},
			wantErr:   errBoom,
			wantCalls: []string{"a"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var calls []string
			got, ok, err := decideTools(tc.build(&calls))(t.Context(), agentrun.Call{})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("decideTools()() error = %v, want %v", err, tc.wantErr)
			}
			if ok != tc.wantOK {
				t.Fatalf("decideTools()() ok = %v, want %v", ok, tc.wantOK)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("decideTools()() wrong result (-want +got) diff = %s", diff)
			}
			if diff := cmp.Diff(tc.wantCalls, calls); diff != "" {
				t.Fatalf("decideTools()() asked deciders wrong (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestDecideToolsWithoutDecidersAsksAboutEveryCall(t *testing.T) {
	t.Parallel()
	if d := decideTools([]ToolDecider{nil}); d != nil {
		t.Fatal("decideTools([nil]) = a decider, want nil")
	}
}

func TestDecideToolsPassesTheCall(t *testing.T) {
	t.Parallel()
	var got ToolCall
	d := decideTools([]ToolDecider{ToolDeciderFunc(func(_ context.Context, c ToolCall) (ToolDecision, bool, error) {
		got = c
		return ToolDecision{}, false, nil
	})})
	call := agentrun.Call{Call: toolrules.Call{Kind: "execute", Command: "git push"}, ContextID: "ctx", TaskID: "task"}
	if _, _, err := d(t.Context(), call); err != nil {
		t.Fatalf("decideTools()() error = %v", err)
	}
	want := ToolCall{Kind: "execute", Command: "git push", ContextID: "ctx", TaskID: "task"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("decideTools()() passed a wrong call (-want +got) diff = %s", diff)
	}
}
