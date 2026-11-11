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
	"time"

	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/clierr"
	"github.com/a2aproject/a2a-serve/internal/flagparse"
	"github.com/a2aproject/a2a-serve/internal/relay/control"
)

func newRelayInviteCmd(cfg *globalConfig, f *relayFlags) *cobra.Command {
	var expires time.Duration
	cmd := &cobra.Command{
		Use:   "invite <name>",
		Short: "Create a token that joins one agent to the relay",
		Long: `Create an invite: a single-use token that joins one agent to the relay
without a sign-in, such as an agent on a server. The token is printed once.
The name has up to 64 letters, digits, dots, hyphens or underscores, and
starts with a letter or digit.

An unused invite expires. ` + "`a2a-serve relay members`" + ` lists the unused invites
and the invite each agent joined with. ` + "`a2a-serve relay remove <name>`" + `
revokes the unused invites of that name.

An invited agent keeps renewing its membership, even after it slept for days,
until it is removed with ` + "`a2a-serve relay remove`" + `. One that stays away
longer than 30 days needs a new invite.

Inviting the name of an invited agent the relay removed lets that agent
back. It joins again with its identity, so it keeps its URLs.

` + flagparse.SecretHelp,
		Example: `  a2a-serve relay invite ci-bot
  a2a-serve relay invite ci-bot --expires 72h
  a2a-serve relay invite ci-bot --url https://relay.example.com --admin-token env:RELAY_ADMIN_TOKEN`,
		Args: namedArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			hours := int(expires / time.Hour)
			if expires%time.Hour != 0 || hours < 1 || hours > control.MaxInviteHours {
				return clierr.Usage(fmt.Sprintf("--expires takes whole hours from 1h to %dh, got %v", control.MaxInviteHours, expires))
			}
			c, err := f.client(cfg)
			if err != nil {
				return err
			}
			inv, err := c.Invite(cmd.Context(), control.InviteRequest{Name: args[0], Hours: hours})
			if err != nil {
				return err
			}
			if cfg.IsJSON() {
				return cfg.PrintJSON(inv)
			}
			if _, err := fmt.Fprintf(cfg.Out, "invite %s for %s, one agent, until %s:\n  %s\non that agent: a2a-serve config set --private %s --public %s --network-token -\non a new agent: a2a-serve config set --agent <agent> --private %s --public %s --network-token -\n",
				inv.Name, inv.URL, inv.Expires.UTC().Format("2006-01-02 15:04 MST"), inv.Token, inv.URL, inv.URL, inv.URL, inv.URL); err != nil {
				return err
			}
			if inv.LetBack != nil {
				_, err = fmt.Fprintf(cfg.Out, "✓ let back %s (%s): it joins again with its identity when it restarts\n", inv.LetBack.Agent, inv.LetBack.Peer)
			}
			return err
		},
	}
	cmd.Flags().DurationVar(&expires, "expires", control.DefaultInviteHours*time.Hour, "How long the invite stays valid, in whole hours up to 168h")
	f.addTargetFlags(cmd.Flags())
	return cmd
}
