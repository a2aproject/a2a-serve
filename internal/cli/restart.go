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
)

func newRestartCmd(cfg *globalConfig, sf *serverFlags) *cobra.Command {
	var quiet bool
	cmd := &cobra.Command{
		Use:   "restart",
		Short: "Stop the agent and start it again in the same mode, detached or as a service",
		Long: `Stop the agent and start it again in the same mode, detached or as a
user service, to apply settings which wait for a restart. The settings are
checked first, so an agent which couldn't start again isn't stopped. An
agent running in the foreground of a terminal is restarted there: press
Ctrl-C and run ` + "`a2a-serve start`" + ` again.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := sf.agentDir()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			client := sf.controlClient(d)
			if err := agentctl.Restart(ctx, d, client, quiet, cfg.progress()); err != nil {
				return err
			}
			if !cfg.IsJSON() {
				return nil
			}
			st, err := client.Status(ctx)
			if err != nil {
				return err
			}
			return cfg.PrintJSON(statusJSON{Name: st.Name, Harness: st.Harness, Dir: st.Dir, Running: true, Status: st})
		},
	}
	cmd.Flags().BoolVar(&quiet, "quiet", false, quietUsage)
	sf.addControlFlags(cmd.Flags())
	return cmd
}
