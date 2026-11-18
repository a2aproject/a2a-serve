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

// Package project words what an agent's configuration means for the MCP
// servers and skills of its workspace.
package project

import (
	"fmt"
	"slices"
	"strings"

	"github.com/a2aproject/a2a-serve/core/workspace"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
)

// Vars returns the values ${VAR} in a project's .mcp.json can take: the
// --env and --secret values of the agent, which the harness sees anyway.
// Other secrets, such as bot tokens, stay out, because the harness can edit
// .mcp.json.
func Vars(cfg *agentdir.Config, secrets agentdir.Secrets) func(string) (string, bool) {
	return func(name string) (string, bool) {
		for _, kv := range slices.Backward(cfg.Env) {
			if k, v, ok := strings.Cut(kv, "="); ok && k == name {
				return v, true
			}
		}
		if !slices.Contains(cfg.Secrets, name) {
			return "", false
		}
		v, ok := secrets[name]
		return v, ok && v != ""
	}
}

// UnknownServer is a name of a list of disabled servers that .mcp.json has
// no server for.
type UnknownServer struct {
	Name string
	// Suggestion is the server whose name is closest, or "".
	Suggestion string
	// Known are the names of the servers .mcp.json has.
	Known []string
}

func (u UnknownServer) String() string {
	msg := fmt.Sprintf("--disable-mcp %s names no server of %s", u.Name, workspace.MCPFile)
	switch {
	case u.Suggestion != "":
		return msg + ": did you mean " + u.Suggestion + "?"
	case len(u.Known) == 0:
		return msg + "; it has none"
	}
	return msg + "; it has " + strings.Join(u.Known, ", ")
}

// UnknownDisabled returns the names of disabled that servers don't have.
func UnknownDisabled(servers []workspace.MCPServer, disabled []string) []UnknownServer {
	known := make([]string, len(servers))
	for i, s := range servers {
		known[i] = s.Name
	}
	var out []UnknownServer
	for _, name := range disabled {
		if slices.Contains(known, name) {
			continue
		}
		out = append(out, UnknownServer{Name: name, Suggestion: agentdir.Closest(name, known), Known: known})
	}
	return out
}
