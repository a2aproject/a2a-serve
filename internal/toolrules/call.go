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

package toolrules

import (
	"cmp"
	"slices"
	"strings"
)

// Call is a tool call that an agent asks permission for.
type Call struct {
	Kind string
	// Tool is the MCP tool of the call as <server>.<tool>. A call of kind
	// other whose title doesn't name a tool of a known server has its title
	// here, as the agent sent it.
	Tool string
	// MCP reports whether Tool names a tool of an MCP server.
	MCP bool
	// Command is the shell command of a call of kind execute.
	Command string
}

// NewCall describes a tool call from what the agent reports: its kind,
// title and raw input. servers are the names of the MCP servers that the
// agent has, with which the agent's tool names map to <server>.<tool>.
func NewCall(kind, title string, input any, servers []string) Call {
	c := Call{Kind: kind}
	if tool, ok := mcpTool(title, servers); ok {
		c.Tool, c.MCP = tool, true
		return c
	}
	if (kind == KindOther || kind == "") && title != "" && !strings.ContainsFunc(title, isSpace) {
		c.Tool = title
	}
	if kind == KindExecute {
		if m, ok := input.(map[string]any); ok {
			cmd, _ := m["command"].(string)
			c.Command = strings.TrimSpace(cmd)
		}
	}
	return c
}

// mcpTool returns the <server>.<tool> name of an agent's tool name: Claude
// Code names MCP tools mcp__<server>__<tool>, and opencode
// <server>_<tool>, with the characters besides letters, digits, _ and -
// replaced by _ in both. The title comes from the agent, so a name with
// other characters isn't taken for an MCP tool.
func mcpTool(title string, servers []string) (string, bool) {
	if title == "" || sanitize(title) != title {
		return "", false
	}
	// Longer names first, so that a server named a_b wins over a.
	sorted := slices.SortedFunc(slices.Values(servers), func(a, b string) int { return cmp.Compare(len(b), len(a)) })
	if rest, ok := strings.CutPrefix(title, "mcp__"); ok {
		for _, s := range sorted {
			if tool, ok := strings.CutPrefix(rest, sanitize(s)+"__"); ok && tool != "" {
				return s + "." + tool, true
			}
		}
		if server, tool, ok := strings.Cut(rest, "__"); ok && server != "" && tool != "" {
			return server + "." + tool, true
		}
		return "", false
	}
	for _, s := range sorted {
		if tool, ok := strings.CutPrefix(title, sanitize(s)+"_"); ok && tool != "" {
			return s + "." + tool, true
		}
	}
	return "", false
}

// sanitize replaces the characters of an MCP server name that agents don't
// keep in tool names.
func sanitize(name string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			return r
		}
		return '_'
	}, name)
}
