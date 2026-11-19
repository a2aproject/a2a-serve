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
	"fmt"
	"slices"

	"github.com/a2aproject/a2a-serve/internal/agentrun"
	"github.com/a2aproject/a2a-serve/internal/toolrules"
)

// ToolKinds are the ACP tool kinds that kind entries of [ToolRules] match.
var ToolKinds = slices.Clone(toolrules.Kinds)

// ToolCall is a tool call that an agent asks permission for.
type ToolCall struct {
	// Kind is the ACP tool kind: read, edit, execute, and so on.
	Kind string
	// Tool is <server>.<tool> for an MCP tool.
	Tool string
	// MCP reports whether Tool names a tool of an MCP server.
	MCP bool
	// Command is the shell command of a call of kind execute.
	Command string
	// ContextID and TaskID identify the conversation and the task that make
	// the call.
	ContextID string
	TaskID    string
}

// ToolDecision is the answer of a [ToolDecider].
type ToolDecision struct {
	Allow bool
	// Rule is the entry that decided, for logs and records.
	Rule string
}

// ToolDecider answers tool permissions. The deciders of Config.Tools are
// asked in order, and the first that decides answers.
type ToolDecider interface {
	// Decide answers c, or returns false to leave the call to the next
	// decider, and after the last to the caller of the task.
	Decide(ctx context.Context, c ToolCall) (ToolDecision, bool, error)
}

// ToolDeciderFunc is a [ToolDecider] that is a function.
type ToolDeciderFunc func(ctx context.Context, c ToolCall) (ToolDecision, bool, error)

// Decide calls f.
func (f ToolDeciderFunc) Decide(ctx context.Context, c ToolCall) (ToolDecision, bool, error) {
	return f(ctx, c)
}

// ToolRules decide tool calls by entries. An entry is one of:
//
//   - A glob of ACP tool kinds ([ToolKinds]), such as "read" or "*".
//   - "tool:<server>.<tool>", a glob of the MCP tools of a call, such as
//     "tool:github.*".
//   - "command:<command>", a glob of the shell command of an execute call,
//     such as "command:git push *". A command that chains or substitutes
//     other commands (with ; & | ` $( < > or a line break) matches no
//     command entry, so it always asks.
//
// In a glob, * matches any run of characters. The most specific entry that
// matches a call decides: tool and command entries before kind entries, then
// the entry with more characters besides *. Between equally specific
// entries, deny decides. An invalid entry matches nothing.
type ToolRules struct {
	Allow []string
	Deny  []string
}

// Decide answers with the most specific entry that matches c, or returns
// false if none does.
func (r ToolRules) Decide(_ context.Context, c ToolCall) (ToolDecision, bool, error) {
	d, ok := toolrules.Rules{Allow: r.Allow, Deny: r.Deny}.Decide(toolrules.Call{Kind: c.Kind, Tool: c.Tool, MCP: c.MCP, Command: c.Command})
	if !ok {
		return ToolDecision{}, false, nil
	}
	return ToolDecision{Allow: d.Allow, Rule: d.Entry}, true, nil
}

// Validate reports the first entry that is invalid.
func (r ToolRules) Validate() error {
	for _, list := range []struct {
		name    string
		entries []string
	}{{"allow", r.Allow}, {"deny", r.Deny}} {
		for _, s := range list.entries {
			if err := toolrules.Validate(s); err != nil {
				return fmt.Errorf("%s entry %q: %w", list.name, s, err)
			}
		}
	}
	return nil
}

// decideTools returns the engine's decider that asks each of ds in turn,
// or nil without any. An error from a decider stops it and is returned.
func decideTools(ds []ToolDecider) agentrun.DecideTool {
	ds = slices.DeleteFunc(slices.Clone(ds), func(d ToolDecider) bool { return d == nil })
	if len(ds) == 0 {
		return nil
	}
	return func(ctx context.Context, c agentrun.Call) (agentrun.Decision, bool, error) {
		call := ToolCall{Kind: c.Kind, Tool: c.Tool, MCP: c.MCP, Command: c.Command, ContextID: c.ContextID, TaskID: c.TaskID}
		for _, d := range ds {
			dec, ok, err := d.Decide(ctx, call)
			if err != nil {
				return agentrun.Decision{}, false, err
			}
			if ok {
				return agentrun.Decision{Allow: dec.Allow, Rule: dec.Rule}, true, nil
			}
		}
		return agentrun.Decision{}, false, nil
	}
}
