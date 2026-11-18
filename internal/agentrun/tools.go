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
	"log/slog"
	"slices"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/integrations/executor/a2aacp"

	"github.com/a2aproject/a2a-serve/internal/toolrules"
)

// ByRule is the By of a decision of the tool rules.
const ByRule = "rule"

// toolDecider answers tool permissions with the tool rules of the current
// spec, so that changing them doesn't need a restart, and then with
// Hooks.Decide. It logs each answer by what decided it.
type toolDecider struct {
	rules func() toolrules.Rules
	// servers returns the names of the MCP servers of the project, which
	// name the MCP tools of calls.
	servers func() []string
	decide  func(ctx context.Context, execCtx *a2asrv.ExecutorContext, call toolrules.Call) (Decision, bool, error)
	denied  func(execCtx *a2asrv.ExecutorContext, title string)
}

// call describes a tool call of a permission request.
func (d *toolDecider) call(kind, title string, input any) toolrules.Call {
	var servers []string
	if d.servers != nil {
		servers = d.servers()
	}
	return toolrules.NewCall(kind, title, input, servers)
}

func (d *toolDecider) AutoAnswer(ctx context.Context, execCtx *a2asrv.ExecutorContext, q a2aacp.Question) (a2aacp.Answer, bool) {
	if q.Tool == nil {
		return a2aacp.Answer{}, false
	}
	var taskID a2a.TaskID
	if execCtx != nil {
		taskID = execCtx.TaskID
	}
	call := d.call(q.Tool.Kind, q.Tool.Title, q.Tool.Input)
	dec, ok := Decision{}, false
	if rule, decided := d.rules().Decide(call); decided {
		dec, ok = Decision{Decision: rule, By: ByRule}, true
	} else if d.decide != nil && execCtx != nil {
		var err error
		if dec, ok, err = d.decide(ctx, execCtx, call); err != nil {
			slog.Warn("reading what a person allowed, asking", "task", taskID, "error", err)
			return a2aacp.Answer{}, false
		}
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
	slog.Info("approval answered by a "+dec.By, dec.LogAttrs(execCtx.TaskID, q.Tool.Kind, ans.Option)...)
	return ans, true
}

// LogAttrs are the attributes of the log line of the approval of task that
// dec answered with option. A decision of another kind than a rule comes
// from an answer to a command, which can hold secrets, so it names only the
// program.
func (dec Decision) LogAttrs(task a2a.TaskID, kind, option string) []any {
	entry := dec.Entry
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
