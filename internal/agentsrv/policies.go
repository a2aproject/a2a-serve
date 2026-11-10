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

package agentsrv

import (
	"slices"
	"sync"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/harness"
	"github.com/a2aproject/a2a-serve/internal/toolrules"
)

// policies writes the settings that the agent processes in containers
// can't override: one policy for the owner's, and one for those of members
// and guests, whose homes are locked. The agent reads its policy when each
// session starts, so a policy names every MCP server that sessions got
// since the server started: a session of another container may use one
// that the current .mcp.json dropped.
type policies struct {
	dir agentdir.Dir

	mu  sync.Mutex
	mcp []string
}

func newPolicies(dir agentdir.Dir) *policies { return &policies{dir: dir} }

// write writes the policy of the agent processes of cfg, those of members
// and guests with lockHome, adding mcpServers, and returns its folder. The
// agent asks before every shell command while the rules deny some, since
// it would run read-only ones, and those that a skill allows, itself.
func (p *policies) write(cfg *agentdir.Config, lockHome bool, mcpServers []string) (string, error) {
	profile, err := harness.Lookup(cfg.Harness, cfg.Command)
	if err != nil {
		return "", err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, name := range mcpServers {
		if !slices.Contains(p.mcp, name) {
			p.mcp = append(p.mcp, name)
		}
	}
	dir := p.dir.PolicyDir(lockHome)
	return dir, harness.WritePolicy(profile, dir, harness.Policy{
		LockHome:    lockHome,
		AskCommands: toolrules.DeniesCommands(cfg.Approvals.Deny),
		MCPServers:  slices.Clone(p.mcp),
	})
}
