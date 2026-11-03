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

func newScheduleRmCmd(cfg *globalConfig, sf *serverFlags) *cobra.Command {
	return &cobra.Command{
		Use:               "rm <name>",
		Aliases:           []string{"delete"},
		Short:             "Remove a schedule",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: scheduleNames(sf),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := sf.control()
			if err != nil {
				return err
			}
			if err := c.DeleteSchedule(cmd.Context(), args[0]); err != nil {
				return err
			}
			return cfg.PrintRemoved("schedule", args[0], "✓ removed schedule "+args[0])
		},
	}
}
