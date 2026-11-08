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

package project

import (
	"slices"
	"strings"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
)

// Vars returns the values ${VAR} in a project's .mcp.json can take: the
// --env and --secret values of the agent, which the harness sees anyway,
// except the model API keys that a sandbox keeps from it. Those reach the
// MCP servers that name them, and the server warns about it on start.
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
