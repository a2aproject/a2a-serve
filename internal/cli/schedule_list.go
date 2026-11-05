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
	"cmp"

	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/output"
)

func newScheduleListCmd(cfg *globalConfig, sf *serverFlags) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List schedules, with their last and next runs",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			c, closer, err := sf.manager(cmd.Context())
			if err != nil {
				return err
			}
			defer joinClose(&err, closer)
			all, err := c.Schedules(cmd.Context())
			if err != nil {
				return err
			}
			if cfg.IsJSON() {
				return output.PrintList(cfg.Printer, all)
			}
			rows := make([][]string, 0, len(all))
			for _, s := range all {
				when := s.Cron
				if when == "" {
					when = "once"
				}
				if s.TZ != "" {
					when += " (" + s.TZ + ")"
				}
				last := formatScheduleTime(s.LastRun)
				if s.LastError != "" {
					last += " ! " + s.LastError
				}
				rows = append(rows, []string{s.Name, when, auth.DisplayPrincipal(s.User), cmp.Or(s.Chat, "-"), formatScheduleTime(s.NextRun), last, oneLine(s.Prompt, 40)})
			}
			return cfg.PrintTableOrEmpty([]string{"NAME", "WHEN", "AS", "CHAT", "NEXT", "LAST", "PROMPT"}, rows,
				"no schedules; add one with `a2a-serve schedule add <name> --cron <expr> --prompt <prompt>`")
		},
	}
}
