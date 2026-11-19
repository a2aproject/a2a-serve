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
	"cmp"
	"context"
	"log/slog"
	"slices"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/integrations/executor/a2aacp"

	"github.com/a2aproject/a2a-serve/internal/toolrules"
)

// ByRule is the By of a decision of the tool rules.
const ByRule = "rule"

// ToolCall is what a decider of the frontend sees of the decision of a tool
// call in the ctx of Decide, and records about it.
type ToolCall struct {
	// ExecCtx is the turn of the call, or nil.
	ExecCtx *a2asrv.ExecutorContext
	// By is what decided, such as a preference, ByRule if unset, and User
	// whose.
	By, User string
}

type toolCallKey struct{}

// ToolCallOf returns the decision of a tool call that ctx is for, or nil.
func ToolCallOf(ctx context.Context) *ToolCall {
	tc, _ := ctx.Value(toolCallKey{}).(*ToolCall)
	return tc
}

// Call is a tool call that the agent asks permission for, in the
// conversation ContextID and the task TaskID.
type Call struct {
	toolrules.Call
	ContextID, TaskID string
}

// DecideTool answers a tool call with Allow and the Rule that decided, or
// returns false to ask about it.
type DecideTool func(ctx context.Context, c Call) (Decision, bool, error)

// toolDecider answers tool permissions with the decider of the runner,
// whose rules can change without a restart. It logs each answer by what
// decided it.
type toolDecider struct {
	tools DecideTool
	// servers returns the names of the MCP servers of the project, which
	// name the MCP tools of calls.
	servers func() []string
	denied  func(execCtx *a2asrv.ExecutorContext, title string)
	log     func() *slog.Logger
}

func (d *toolDecider) AutoAnswer(ctx context.Context, execCtx *a2asrv.ExecutorContext, q a2aacp.Question) (a2aacp.Answer, bool) {
	if q.Tool == nil || d.tools == nil {
		return a2aacp.Answer{}, false
	}
	var servers []string
	if d.servers != nil {
		servers = d.servers()
	}
	call := Call{Call: toolrules.NewCall(q.Tool.Kind, q.Tool.Title, q.Tool.Input, servers)}
	var taskID a2a.TaskID
	if execCtx != nil {
		taskID = execCtx.TaskID
		call.ContextID, call.TaskID = execCtx.ContextID, string(execCtx.TaskID)
	}
	tc := &ToolCall{ExecCtx: execCtx}
	dec, ok, err := d.tools(context.WithValue(ctx, toolCallKey{}, tc), call)
	if err != nil {
		d.logger().Warn("reading what a person allowed, asking", "task", taskID, "error", err)
		return a2aacp.Answer{}, false
	}
	if !ok {
		return a2aacp.Answer{}, false
	}
	ans, ok := RuleAnswer(q.Options, dec.Allow)
	if !ok || execCtx == nil {
		return ans, ok
	}
	if !dec.Allow && d.denied != nil {
		d.denied(execCtx, q.Title)
	}
	by := cmp.Or(tc.By, ByRule)
	full := Decision{Allow: dec.Allow, Rule: dec.Rule, By: by, User: tc.User}
	d.logger().Info("approval answered by a "+by, full.LogAttrs(execCtx.TaskID, q.Tool.Kind, ans.Option)...)
	return ans, true
}

func (d *toolDecider) logger() *slog.Logger {
	if d.log == nil {
		return slog.Default()
	}
	return d.log()
}

// Decision is how a tool call is answered without asking, and who decided.
type Decision struct {
	Allow bool
	// Rule is the entry that decided.
	Rule string
	// By is what decided, such as a preference, and User whose.
	By, User string
}

// LogAttrs are the attributes of the log line of the approval of task that
// dec answered with option. A decision of another kind than a rule comes
// from an answer to a command, which can hold secrets, so it names only the
// program.
func (dec Decision) LogAttrs(task a2a.TaskID, kind, option string) []any {
	entry := dec.Rule
	if dec.By != ByRule {
		entry = toolrules.Redact(entry)
	}
	attrs := []any{"task", task, "rule", entry, "kind", kind}
	if dec.User != "" {
		attrs = append(attrs, "user", dec.User)
	}
	return append(attrs, "option", option)
}

// RuleAnswer returns the option that allows a tool call once, or always if
// the agent offers no other way to allow it, or the option that rejects
// it, canceling it if the agent offers no way to reject it.
func RuleAnswer(options []string, allow bool) (a2aacp.Answer, bool) {
	if !allow {
		if id := firstOffered(options, a2aacp.OptionReject, a2aacp.OptionRejectAlways); id != "" {
			return a2aacp.Answer{Option: id}, true
		}
		return a2aacp.Answer{Option: a2aacp.OptionCancel}, true
	}
	if id := firstOffered(options, a2aacp.OptionOnce, a2aacp.OptionAlways); id != "" {
		return a2aacp.Answer{Option: id}, true
	}
	return a2aacp.Answer{}, false
}

// firstOffered returns the first of ids that options has, or "".
func firstOffered(options []string, ids ...string) string {
	for _, id := range ids {
		if slices.Contains(options, id) {
			return id
		}
	}
	return ""
}
