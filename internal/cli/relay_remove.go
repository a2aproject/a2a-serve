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

	"github.com/a2aproject/a2a-serve/internal/relay/control"
	"github.com/a2aproject/a2a-serve/internal/secretref"
)

func newRelayRemoveCmd(cfg *globalConfig, f *relayFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "remove <agent|email|peer-id>",
		Aliases: []string{"rm"},
		Short:   "Remove agents from the relay",
		Long: `Remove agents from the relay at once: the agent with that name or peer ID,
or every agent of a person, given their email. The relay refuses their next
request, and the other agents drop them. A removed agent can't join again
with its identity, and removing an agent that signed in bans the Google
account behind it, so the person can't sign in again either.

For people who sign in, --allow-email decides: when the relay starts, it
removes the agents of people the list no longer admits, and lets back the
removed people it admits. To keep someone out, also drop them from
--allow-email. Invite a removed invited agent again to let it back.

` + secretref.Help,
		Args: namedArgs,
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
			for _, email := range res.Admitted {
				if _, err := fmt.Fprintf(cfg.Out, "! %s is on --allow-email, so the relay lets them back when it starts; drop them from it to keep them out\n", email); err != nil {
					return err
				}
			}
			return nil
		},
	}
	f.addTargetFlags(cmd.Flags())
	return cmd
}
