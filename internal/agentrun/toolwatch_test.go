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

package agentrun

import (
	"context"
	"iter"
	"strconv"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/integrations/executor/a2aacp"
	"github.com/google/go-cmp/cmp"
)

func TestToolWatchReportsEndedToolCalls(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("é", MaxToolOutput)
	tests := []struct {
		name    string
		updates []string
		want    []ToolResult
	}{
		{
			name: "content of the last update",
			updates: []string{
				`{"sessionUpdate":"tool_call","toolCallId":"t1","title":"go test","kind":"execute","status":"pending"}`,
				`{"sessionUpdate":"tool_call_update","toolCallId":"t1","status":"in_progress"}`,
				`{"sessionUpdate":"tool_call_update","toolCallId":"t1","status":"completed","content":[{"type":"content","content":{"type":"text","text":"ok"}},{"type":"diff","path":"a"}]}`,
			},
			want: []ToolResult{{ID: "t1", Kind: "execute", Title: "go test", Status: "completed", Output: "ok"}},
		},
		{
			name: "raw output",
			updates: []string{
				`{"sessionUpdate":"tool_call","toolCallId":"t1","title":"ls","kind":"execute","status":"pending"}`,
				`{"sessionUpdate":"tool_call_update","toolCallId":"t1","status":"failed","rawOutput":{"output":"no such file","metadata":{}}}`,
			},
			want: []ToolResult{{ID: "t1", Kind: "execute", Title: "ls", Status: "failed", Output: "no such file"}},
		},
		{
			name: "raw output of another shape",
			updates: []string{
				`{"sessionUpdate":"tool_call","toolCallId":"t1","title":"x","kind":"other","status":"completed","rawOutput":{"n":1}}`,
			},
			want: []ToolResult{{ID: "t1", Kind: "other", Title: "x", Status: "completed", Output: `{"n":1}`}},
		},
		{
			name: "output cut at a character",
			updates: []string{
				`{"sessionUpdate":"tool_call","toolCallId":"t1","title":"cat","kind":"read","status":"completed","rawOutput":"` + long + `"}`,
			},
			want: []ToolResult{{ID: "t1", Kind: "read", Title: "cat", Status: "completed", Output: long[:MaxToolOutput], Truncated: true}},
		},
		{
			name: "once per call",
			updates: []string{
				`{"sessionUpdate":"tool_call","toolCallId":"t1","title":"a","kind":"read","status":"completed"}`,
				`{"sessionUpdate":"tool_call_update","toolCallId":"t1","status":"completed"}`,
				`{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hi"}}`,
			},
			want: []ToolResult{{ID: "t1", Kind: "read", Title: "a", Status: "completed"}, {ID: "t1", Status: "completed"}},
		},
		{
			name:    "pending",
			updates: []string{`{"sessionUpdate":"tool_call","toolCallId":"t1","title":"a","kind":"read","status":"pending"}`},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got []ToolResult
			w := newToolWatch(func(_ context.Context, _ *a2asrv.ExecutorContext, r ToolResult) { got = append(got, r) })
			execCtx := &a2asrv.ExecutorContext{TaskID: "task", ContextID: "ctx"}
			conv := w.converter(a2aacp.ConverterFunc(a2aacp.ConvertUpdate))
			for _, raw := range tc.updates {
				typ, _, _ := strings.Cut(strings.TrimPrefix(raw, `{"sessionUpdate":"`), `"`)
				if _, err := conv.Convert(t.Context(), execCtx, a2aacp.Update{Type: typ, Raw: []byte(raw)}); err != nil {
					t.Fatalf("Converter.Convert() error = %v", err)
				}
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("ToolDone got wrong results (-want +got) diff = %s", diff)
			}
		})
	}
}

// fakeExecutor yields its events, after it converts update with conv.
type fakeExecutor struct {
	conv   a2aacp.Converter
	update string
	events []a2a.Event
}

func (e fakeExecutor) Execute(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if _, err := e.conv.Convert(ctx, execCtx, a2aacp.Update{Type: "tool_call", Raw: []byte(e.update)}); err != nil {
			yield(nil, err)
			return
		}
		for _, ev := range e.events {
			if !yield(ev, nil) {
				return
			}
		}
	}
}

func (fakeExecutor) Cancel(context.Context, *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(func(a2a.Event, error) bool) {}
}

func TestToolWatchForgetsATaskWhoseExecutionEnds(t *testing.T) {
	t.Parallel()
	execCtx := &a2asrv.ExecutorContext{TaskID: "task"}
	tests := []struct {
		name  string
		state a2a.TaskState
		want  int
	}{
		{name: "completed", state: a2a.TaskStateCompleted, want: 0},
		{name: "failed", state: a2a.TaskStateFailed, want: 0},
		{name: "waits for an answer", state: a2a.TaskStateInputRequired, want: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newToolWatch(func(context.Context, *a2asrv.ExecutorContext, ToolResult) {})
			exec := w.guard(fakeExecutor{
				conv:   w.converter(a2aacp.ConverterFunc(a2aacp.ConvertUpdate)),
				update: `{"sessionUpdate":"tool_call","toolCallId":"t1","title":"git push","kind":"execute","status":"pending"}`,
				events: []a2a.Event{a2a.NewStatusUpdateEvent(execCtx, tc.state, nil)},
			})
			for _, err := range exec.Execute(t.Context(), execCtx) {
				if err != nil {
					t.Fatalf("Execute() error = %v", err)
				}
			}
			w.mu.Lock()
			defer w.mu.Unlock()
			if got := len(w.tasks); got != tc.want {
				t.Fatalf("tasks followed after the execution = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestToolWatchFollowsBoundedTasks(t *testing.T) {
	t.Parallel()
	w := newToolWatch(func(context.Context, *a2asrv.ExecutorContext, ToolResult) {})
	conv := w.converter(a2aacp.ConverterFunc(a2aacp.ConvertUpdate))
	for i := range maxWatchedTasks + 10 {
		execCtx := &a2asrv.ExecutorContext{TaskID: a2a.TaskID(strconv.Itoa(i))}
		raw := `{"sessionUpdate":"tool_call","toolCallId":"t1","status":"pending"}`
		if _, err := conv.Convert(t.Context(), execCtx, a2aacp.Update{Type: "tool_call", Raw: []byte(raw)}); err != nil {
			t.Fatal(err)
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.tasks) != maxWatchedTasks || len(w.order) != maxWatchedTasks {
		t.Fatalf("tasks followed = %d in an order of %d, want %d", len(w.tasks), len(w.order), maxWatchedTasks)
	}
}
