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

// Package tools decides which tool calls an agent may make. A [Decider]
// answers a [Call], or reports that it has no answer so that the caller can
// ask someone else. [Rules] decide by entries, and [Chain] asks several
// deciders in turn.
//
// An entry is one of:
//
//   - A glob of ACP tool kinds ([Kinds]), such as "read" or "*".
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
package tools

import (
	"context"
	"fmt"
	"slices"

	"github.com/a2aproject/a2a-serve/internal/toolrules"
)

// Kinds are the ACP tool kinds that kind entries match.
var Kinds = slices.Clone(toolrules.Kinds)

// Call is a tool call that an agent asks permission for.
type Call struct {
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

// Decision is the answer of a [Decider].
type Decision struct {
	Allow bool
	// Rule is the entry that decided, for logs and records.
	Rule string
}

// Decider answers tool permissions.
type Decider interface {
	// Decide answers c, or returns false to leave the call to the caller.
	Decide(ctx context.Context, c Call) (Decision, bool, error)
}

// DeciderFunc is a [Decider] that is a function.
type DeciderFunc func(ctx context.Context, c Call) (Decision, bool, error)

// Decide calls f.
func (f DeciderFunc) Decide(ctx context.Context, c Call) (Decision, bool, error) {
	return f(ctx, c)
}

// Rules decide by entries, such as "read", "tool:github.*" and
// "command:git push *". See the package documentation for their syntax.
type Rules struct {
	Allow []string
	Deny  []string
}

// Decide answers with the most specific entry that matches c, or returns
// false if none does. Deny wins between equally specific entries.
func (r Rules) Decide(_ context.Context, c Call) (Decision, bool, error) {
	d, ok := toolrules.Rules{Allow: r.Allow, Deny: r.Deny}.Decide(engineCall(c))
	if !ok {
		return Decision{}, false, nil
	}
	return Decision{Allow: d.Allow, Rule: d.Entry}, true, nil
}

// Validate reports the first entry that is invalid.
func (r Rules) Validate() error {
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

// Chain returns a [Decider] that asks each of ds in turn. The first that
// decides answers. An error from a decider stops the chain and is returned.
func Chain(ds ...Decider) Decider {
	ds = slices.Clone(ds)
	return DeciderFunc(func(ctx context.Context, c Call) (Decision, bool, error) {
		for _, d := range ds {
			if d == nil {
				continue
			}
			dec, ok, err := d.Decide(ctx, c)
			if err != nil {
				return Decision{}, false, err
			}
			if ok {
				return dec, true, nil
			}
		}
		return Decision{}, false, nil
	})
}

// Match returns the first of entries that matches c.
func Match(entries []string, c Call) (rule string, ok bool) {
	return toolrules.Match(entries, engineCall(c))
}

func engineCall(c Call) toolrules.Call {
	return toolrules.Call{Kind: c.Kind, Tool: c.Tool, MCP: c.MCP, Command: c.Command}
}
