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

	"github.com/a2aproject/a2a-serve/internal/agentctl"
	"github.com/a2aproject/a2a-serve/internal/output"
)

func newChannelListCmd(cfg *globalConfig, sf *serverFlags) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the messengers, and whether they run",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := sf.agentDir()
			if err != nil {
				return err
			}
			channels, err := agentctl.Channels(cmd.Context(), d, sf.controlClient(d))
			if err != nil {
				return err
			}
			if cfg.IsJSON() {
				return output.PrintList(cfg.Printer, channels)
			}
			rows := make([][]string, 0, len(channels))
			for _, c := range channels {
				state := c.State
				if c.Error != "" {
					state += ": " + c.Error
				}
				rows = append(rows, []string{c.Name, c.Account, state})
			}
			return cfg.PrintTableOrEmpty([]string{"CHANNEL", "ACCOUNT", "STATE"}, rows, "no channels; add one with `a2a-serve channel add <telegram|discord|email>`")
		},
	}
}
