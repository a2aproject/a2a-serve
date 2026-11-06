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
	"github.com/spf13/cobra"
)

const (
	groupRun       = "run"
	groupConfigure = "configure"
	groupPairing   = "pairing"
	groupNode      = "node"
	groupRelay     = "relay"
	groupSetup     = "setup"
)

// commandGroups maps the subcommands of `a2a-serve` to their help group.
var commandGroups = map[string]string{
	"start":      groupRun,
	"status":     groupRun,
	"card":       groupRun,
	"logs":       groupRun,
	"stop":       groupRun,
	"restart":    groupRun,
	"inspect":    groupRun,
	"config":     groupConfigure,
	"recipe":     groupConfigure,
	"channel":    groupConfigure,
	"schedule":   groupConfigure,
	"backup":     groupConfigure,
	"pair":       groupPairing,
	"contact":    groupPairing,
	"peers":      groupPairing,
	"mailbox":    groupNode,
	"relay":      groupRelay,
	"doctor":     groupSetup,
	"completion": groupSetup,
	"version":    groupSetup,
	"help":       groupSetup,
}

// addGroups lists the subcommands of serve in groups in its help.
func addGroups(serve *cobra.Command) {
	serve.AddGroup(
		&cobra.Group{ID: groupRun, Title: "Run the agent:"},
		&cobra.Group{ID: groupConfigure, Title: "Configure the agent:"},
		&cobra.Group{ID: groupPairing, Title: "Pairing and contacts:"},
		&cobra.Group{ID: groupNode, Title: "Webhook node:"},
		&cobra.Group{ID: groupRelay, Title: "Relay:"},
		&cobra.Group{ID: groupSetup, Title: "Setup:"},
	)
	for _, c := range serve.Commands() {
		if c.GroupID == "" {
			c.GroupID = commandGroups[c.Name()]
		}
	}
}
