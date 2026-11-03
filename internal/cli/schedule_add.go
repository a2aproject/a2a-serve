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
	"fmt"

	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/clierr"
	"github.com/a2aproject/a2a-serve/internal/schedule"
)

func newScheduleAddCmd(cfg *globalConfig, sf *serverFlags) *cobra.Command {
	var req schedule.Request
	cmd := &cobra.Command{
		Use:   "add <name>",
		Short: "Add a schedule",
		Example: `  a2a serve schedule add morning-brief --cron "0 7 * * 1-5" --tz Europe/Zurich \
    --prompt "Summarize today's meetings." --chat telegram:owner
  a2a serve schedule add triage --cron "@every 2h" --prompt "Triage new issues." --thread same
  a2a serve schedule add report --at "2026-11-06 17:00" --prompt "Remind me to send the report." --chat telegram`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			req.Name = args[0]
			if err := req.Validate(); err != nil {
				return clierr.Usage(err.Error())
			}
			c, _, done, err := sf.manager(cmd.Context())
			if err != nil {
				return err
			}
			defer joinClose(&err, done)
			info, err := c.AddSchedule(cmd.Context(), req)
			if err != nil {
				return err
			}
			if cfg.IsJSON() {
				return cfg.PrintJSON(info)
			}
			_, err = fmt.Fprintf(cfg.Out, "✓ added schedule %s; next run %s\n", info.Name, formatScheduleTime(info.NextRun))
			return err
		},
	}
	req.RegisterFlags(cmd.Flags(), true)
	return cmd
}
