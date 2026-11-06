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
	"time"

	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/flagparse"
	"github.com/a2aproject/a2a-serve/internal/output"
	"github.com/a2aproject/a2a-serve/internal/relay/control"
)

func newRelayMembersCmd(cfg *globalConfig, f *relayFlags) *cobra.Command {
	var banned bool
	cmd := &cobra.Command{
		Use:     "members",
		Aliases: []string{"ls"},
		Short:   "List the agents on the relay and the unused invites",
		Long: `List the agents on the relay: the agents they serve, who they joined as (an
email for a signed-in agent, or the name of its invite), when they joined,
when they last reported to the relay, and their peer ID. Then list the
unused invites and when they expire. ` + "`a2a-serve relay remove <name>`" + ` revokes
an unused invite.

` + flagparse.SecretHelp,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := f.client(cfg)
			if err != nil {
				return err
			}
			ms, err := c.Members(cmd.Context(), banned)
			if err != nil {
				return err
			}
			if cfg.IsJSON() {
				return output.PrintList(cfg.Printer, ms)
			}
			return printMembers(cfg, ms, banned)
		},
	}
	cmd.Flags().BoolVar(&banned, "banned", false, "Also list the removed members")
	f.addTargetFlags(cmd.Flags())
	return cmd
}

func printMembers(cfg *globalConfig, ms []control.Member, banned bool) error {
	headers := []string{"AGENT", "WHO", "JOINED", "SEEN", "PEER"}
	if banned {
		headers = append(headers, "REMOVED")
	}
	rows := make([][]string, 0, len(ms))
	for _, m := range ms {
		joined := formatTime(m.Joined)
		if m.Unused {
			joined = "unused until " + formatTime(m.Expires)
		}
		row := []string{orDash(m.Agent), m.Who(), joined, formatTime(m.Seen), orDash(m.Peer)}
		if banned {
			row = append(row, map[bool]string{true: "yes", false: "-"}[m.Banned])
		}
		rows = append(rows, row)
	}
	return cfg.PrintTableOrEmpty(headers, rows, "no agents on the relay; invite one with `a2a-serve relay invite <name>`")
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04")
}
