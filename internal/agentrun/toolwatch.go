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
	"encoding/json"
	"iter"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/integrations/executor/a2aacp"
)

// MaxToolOutput bounds the Output of a [ToolResult].
const MaxToolOutput = 64 << 10

// ToolResult is a tool call that the agent reported as ended.
type ToolResult struct {
	ID, Kind, Title string
	// Status is completed or failed.
	Status string
	// Output is the text the agent reported for the call, cut to
	// MaxToolOutput bytes, and Truncated whether it was cut.
	Output    string
	Truncated bool
}

// maxWatchedTasks bounds the tasks whose tool calls a toolWatch follows;
// the oldest is forgotten first.
const maxWatchedTasks = 1024

// toolWatch follows the tool calls of each task in the agent's updates,
// and passes those that end to done. It forgets a task once an execution
// of it ends without waiting for an answer.
type toolWatch struct {
	done func(ctx context.Context, execCtx *a2asrv.ExecutorContext, t ToolResult)

	mu sync.Mutex
	// tasks are the tool calls that haven't ended, by task and ID, and
	// order the tasks, oldest first.
	tasks map[a2a.TaskID]map[string]*ToolResult
	order []a2a.TaskID
}

func newToolWatch(done func(context.Context, *a2asrv.ExecutorContext, ToolResult)) *toolWatch {
	return &toolWatch{done: done, tasks: map[a2a.TaskID]map[string]*ToolResult{}}
}

// guard returns inner, whose executions make the watch forget their task
// when they end, unless the task waits for an answer.
func (w *toolWatch) guard(inner a2asrv.AgentExecutor) a2asrv.AgentExecutor {
	if w.done == nil {
		return inner
	}
	return &toolWatchedExecutor{AgentExecutor: inner, w: w}
}

type toolWatchedExecutor struct {
	a2asrv.AgentExecutor
	w *toolWatch
}

func (e *toolWatchedExecutor) Execute(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		paused := false
		defer func() {
			if !paused {
				e.w.forget(execCtx.TaskID)
			}
		}()
		for ev, err := range e.AgentExecutor.Execute(ctx, execCtx) {
			paused = waits(ev)
			if !yield(ev, err) {
				return
			}
		}
	}
}

// waits reports whether ev leaves its task waiting for an answer.
func waits(ev a2a.Event) bool {
	var state a2a.TaskState
	switch ev := ev.(type) {
	case *a2a.TaskStatusUpdateEvent:
		state = ev.Status.State
	case *a2a.Task:
		state = ev.Status.State
	}
	return state == a2a.TaskStateInputRequired || state == a2a.TaskStateAuthRequired
}

func (w *toolWatch) forget(id a2a.TaskID) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.tasks[id]; !ok {
		return
	}
	delete(w.tasks, id)
	w.order = slices.DeleteFunc(w.order, func(t a2a.TaskID) bool { return t == id })
}

// calls returns the tool calls of the task id, which it starts following.
// It runs with w.mu held.
func (w *toolWatch) calls(id a2a.TaskID) map[string]*ToolResult {
	if calls, ok := w.tasks[id]; ok {
		return calls
	}
	if len(w.order) >= maxWatchedTasks {
		delete(w.tasks, w.order[0])
		w.order = slices.Delete(w.order, 0, 1)
	}
	calls := map[string]*ToolResult{}
	w.tasks[id] = calls
	w.order = append(w.order, id)
	return calls
}

// converter returns next, which also shows the watch every update.
func (w *toolWatch) converter(next a2aacp.Converter) a2aacp.Converter {
	if w.done == nil {
		return next
	}
	return a2aacp.ConverterFunc(func(ctx context.Context, execCtx *a2asrv.ExecutorContext, u a2aacp.Update) (*a2aacp.ConverterResult, error) {
		w.observe(ctx, execCtx, u)
		return next.Convert(ctx, execCtx, u)
	})
}

// toolUpdate is what the watch reads of a tool_call or tool_call_update.
type toolUpdate struct {
	ID      string `json:"toolCallId"`
	Title   string `json:"title"`
	Kind    string `json:"kind"`
	Status  string `json:"status"`
	Content []struct {
		Type    string `json:"type"`
		Content struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"content"`
	RawOutput json.RawMessage `json:"rawOutput"`
}

func (w *toolWatch) observe(ctx context.Context, execCtx *a2asrv.ExecutorContext, u a2aacp.Update) {
	if u.Type != "tool_call" && u.Type != "tool_call_update" {
		return
	}
	var up toolUpdate
	if err := json.Unmarshal(u.Raw, &up); err != nil || up.ID == "" {
		slog.Debug("decoding a tool call update", "task", execCtx.TaskID, "error", err)
		return
	}
	w.mu.Lock()
	calls := w.calls(execCtx.TaskID)
	tc := calls[up.ID]
	if tc == nil {
		tc = &ToolResult{ID: up.ID}
		calls[up.ID] = tc
	}
	if up.Title != "" {
		tc.Title = up.Title
	}
	if up.Kind != "" {
		tc.Kind = up.Kind
	}
	if out, ok := up.output(); ok {
		tc.Output = out
	}
	ended := up.Status == "completed" || up.Status == "failed"
	var res ToolResult
	if ended {
		tc.Status = up.Status
		res = *tc
		delete(calls, up.ID)
	}
	w.mu.Unlock()
	if !ended {
		return
	}
	res.Output, res.Truncated = truncateUTF8(res.Output, MaxToolOutput)
	w.done(ctx, execCtx, res)
}

// output returns the text of the update's content, or else its raw
// output, and whether it has either.
func (up toolUpdate) output() (string, bool) {
	var texts []string
	for _, c := range up.Content {
		if c.Type == "content" && c.Content.Type == "text" && c.Content.Text != "" {
			texts = append(texts, c.Content.Text)
		}
	}
	if len(texts) > 0 {
		return strings.Join(texts, "\n"), true
	}
	if len(up.RawOutput) == 0 || string(up.RawOutput) == "null" {
		return "", false
	}
	var s string
	if err := json.Unmarshal(up.RawOutput, &s); err == nil {
		return s, true
	}
	var obj struct {
		Output *string `json:"output"`
	}
	if err := json.Unmarshal(up.RawOutput, &obj); err == nil && obj.Output != nil {
		return *obj.Output, true
	}
	return string(up.RawOutput), true
}

// truncateUTF8 cuts s to at most n bytes without splitting a character,
// and reports whether it cut.
func truncateUTF8(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n], true
}
