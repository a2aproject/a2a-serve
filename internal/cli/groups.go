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

package cli

import (
	"slices"

	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/harness"
)

const (
	groupAgents      = "agents"
	groupTestServers = "test-servers"
	groupManage      = "manage"
	groupPairing     = "pairing"
	groupNode        = "node"
	groupHelpers     = "helpers"
)

// commandGroups maps the subcommands of `a2a serve` to their help group.
// Harness commands are agents.
var commandGroups = map[string]string{
	"acp":        groupAgents,
	"echo":       groupTestServers,
	"proxy":      groupTestServers,
	"script":     groupTestServers,
	"status":     groupManage,
	"url":        groupManage,
	"open":       groupManage,
	"logs":       groupManage,
	"stop":       groupManage,
	"restart":    groupManage,
	"config":     groupManage,
	"channel":    groupManage,
	"contact":    groupManage,
	"peers":      groupManage,
	"schedule":   groupManage,
	"backup":     groupManage,
	"restore":    groupManage,
	"pair":       groupPairing,
	"tokens":     groupPairing,
	"mailbox":    groupNode,
	"doctor":     groupHelpers,
	"credential": groupHelpers,
	"version":    groupHelpers,
	"completion": groupHelpers,
	"help":       groupHelpers,
}

// addGroups lists the subcommands of serve in groups in its help.
func addGroups(serve *cobra.Command) {
	serve.AddGroup(
		&cobra.Group{ID: groupAgents, Title: "Agents:"},
		&cobra.Group{ID: groupTestServers, Title: "Test servers:"},
		&cobra.Group{ID: groupManage, Title: "Manage the agent:"},
		&cobra.Group{ID: groupPairing, Title: "Pairing:"},
		&cobra.Group{ID: groupNode, Title: "Webhook node:"},
		&cobra.Group{ID: groupHelpers, Title: "Helpers:"},
	)
	for _, c := range serve.Commands() {
		if c.GroupID != "" {
			continue
		}
		c.GroupID = commandGroups[c.Name()]
		if slices.Contains(harness.ProfileNames(), c.Name()) {
			c.GroupID = groupAgents
		}
	}
}
