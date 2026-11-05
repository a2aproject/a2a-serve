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
	"strings"

	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/flagparse"
	"github.com/a2aproject/a2a-serve/internal/relay/control"
)

func newRelayRemoveCmd(cfg *globalConfig, f *relayFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "remove <agent|email|peer-id>",
		Aliases: []string{"rm"},
		Short:   "Remove agents from the relay",
		Long: `Remove agents from the relay at once: the agent with that name or peer ID,
or every agent of a person, given their email. The relay refuses their next
request, and the other agents drop them. A removed agent can't join again
with its identity.

` + flagparse.SecretHelp,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := f.client(cfg)
			if err != nil {
				return err
			}
			res, err := c.Remove(cmd.Context(), control.RemoveRequest{Target: args[0]})
			if err != nil {
				return err
			}
			if cfg.IsJSON() {
				return cfg.PrintJSON(res)
			}
			for _, m := range res.Removed {
				if _, err := fmt.Fprintf(cfg.Out, "✓ removed %s (%s, %s)\n", orDash(m.Agent), m.Who(), m.Peer); err != nil {
					return err
				}
			}
			if strings.Contains(args[0], "@") {
				_, err = fmt.Fprintf(cfg.Out, "  %s can join again with a new agent while the relay admits them\n", args[0])
			}
			return err
		},
	}
	f.addTargetFlags(cmd.Flags())
	return cmd
}
