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
	"context"
	"time"

	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/controlapi"
)

func newScheduleCmd(cfg *globalConfig, sf *serverFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "schedule",
		Short: "Run prompts on a schedule",
		Long: `Run a prompt on a schedule with no person on the other side, such as a weekday
briefing. Each run is an ordinary task of the agent, run as a chosen user,
and its result and approval requests can go to a chat, where replying
continues the conversation. A run missed while the agent was stopped runs
once when it starts.

The harness can also create schedules during a conversation, such as for
"remind me on Friday at 5"; they run as the user who asked and report back
in the same conversation.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	sf.addControlFlags(cmd.PersistentFlags())
	cmd.AddCommand(
		newScheduleAddCmd(cfg, sf),
		newScheduleListCmd(cfg, sf),
		newScheduleRunCmd(cfg, sf),
		newScheduleRmCmd(cfg, sf),
	)
	return cmd
}

// scheduleNames completes the names of the running agent's schedules.
func scheduleNames(sf *serverFlags) cobra.CompletionFunc {
	return controlCompletions(sf, func(ctx context.Context, c *controlapi.Client) ([]string, error) {
		all, err := c.Schedules(ctx)
		if err != nil {
			return nil, err
		}
		names := make([]string, 0, len(all))
		for _, s := range all {
			names = append(names, s.Name)
		}
		return names, nil
	})
}

func formatScheduleTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04 MST")
}
