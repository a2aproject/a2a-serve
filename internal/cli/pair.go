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

func newPairCmd(cfg *globalConfig, sf *serverFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pair",
		Short: "Pair callers and webhook nodes with the agent",
		Long: `"invite", "list" and "rm" manage who may call or message the agent.
"invite" creates a one-time code, which the caller redeems on their side
with ` + "`a2a serve tokens add <agent-url> <code>`" + `, or by sending
"link <code>" to the agent's bot.`,
		Example: `  a2a serve pair invite anna
  a2a serve pair list
  a2a serve pair rm anna`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(
		newPairInviteCmd(cfg, sf),
		newPairListCmd(cfg, sf),
		newPairRmCmd(cfg, sf),
	)
	return cmd
}
