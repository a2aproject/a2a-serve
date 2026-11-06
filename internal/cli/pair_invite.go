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
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/clierr"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/flagparse"
)

func newPairInviteCmd(cfg *globalConfig, sf *serverFlags) *cobra.Command {
	var (
		req        controlapi.PairCodeRequest
		headers    []string
		advertised bool
	)
	cmd := &cobra.Command{
		Use:   "invite <name>",
		Short: "Create a one-time code that lets someone call or message the agent, or host a webhook node",
		Long: `Create a pairing called <name> and print its code: 8 characters, valid once
for 15 minutes. The caller redeems it for a token with
` + "`a2a-serve pair join <agent-url> <code>`" + `, or links a messenger account by
sending "join <code>" to the agent's bot. Another agent redeems it with
` + "`a2a-serve contact add --code`" + `. The first redemption sets the pairing's kind:
person, or agent for a contact. Inviting an existing name issues another
code, for another device or account; its tokens keep working. A new name has
up to 64 letters, digits, dots, hyphens or underscores, starts with a letter
or digit, and differs from the existing names by more than case.
` + "`a2a-serve pair set`" + ` changes a pairing, and rotates its credentials.

With --webhook, <name> is a webhook node: an agent which only runs when woken,
such as a Grok Bot routine. The agent hosts it at <agent-url>/n/<name>, calls
it as <name> with the a2a CLI, and wakes it with a signed POST to the
webhook. The node pairs with the code the same way, answers with
` + "`a2a-serve mailbox reply`" + `, and can call the agent with its token. A header
value may end in a secret value, env:VAR or -, as in
"Authorization: Bearer env:TOKEN".

` + flagparse.SecretHelp + `

--advertised tells every agent on the private network about the node: they
list it in ` + "`a2a-serve peers`" + ` as <name>@<this agent> and can add it with
` + "`a2a-serve contact add <name>@<this agent>`" + `, with no code. Their calls go
through this agent, which keeps the webhook's credentials, and it treats
them like any other caller from the private network.`,
		Example: `  a2a-serve pair invite grok
  a2a-serve pair invite anna --owner
  a2a-serve pair invite grok --webhook https://example.com/hooks/123 \
    --webhook-header "Authorization: Bearer env:GROK_WEBHOOK_TOKEN"
  a2a-serve pair invite grok --webhook https://example.com/hooks/123 --advertised`,
		Args: namedArgs,
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			req.Name = args[0]
			if req.Webhook == "" && (len(headers) > 0 || req.Description != "" || cmd.Flags().Changed("advertised")) {
				return clierr.Usage("--webhook-header, --description and --advertised go with --webhook")
			}
			hdrs, err := parseWebhookHeaders(headers, cfg.secretReader(cfg.stderr()))
			if err != nil {
				return err
			}
			req.Headers = hdrs
			if cmd.Flags().Changed("advertised") {
				req.Advertised = &advertised
			}
			c, closer, err := sf.manager(cmd.Context())
			if err != nil {
				return err
			}
			defer joinClose(&err, closer)
			resp, err := c.Pair(cmd.Context(), req)
			if err != nil {
				return err
			}
			if cfg.IsJSON() {
				if err := warnPairCode(cfg.stderr(), resp, !c.Running()); err != nil {
					return err
				}
				return cfg.PrintJSON(resp)
			}
			if err := printPairCode(cfg, resp); err != nil {
				return err
			}
			if err := warnPairCode(cfg.Out, resp, !c.Running()); err != nil {
				return err
			}
			return printAccess(cfg, resp.Access)
		},
	}
	f := cmd.Flags()
	f.BoolVar(&req.Owner, "owner", false, "Make the pairing an owner, who sees and approves every task; others see and approve their own")
	f.StringVar(&req.Webhook, "webhook", "", "Host a webhook node: the URL the agent POSTs wakes to")
	f.StringArrayVar(&headers, "webhook-header", nil, "Header sent with every wake: `Name: value`, such as \"Authorization: Bearer env:TOKEN\"; the value may end in env:VAR or - (repeatable)")
	f.StringVar(&req.Description, "description", "", "Description for the node's card")
	f.BoolVar(&advertised, "advertised", false, "Advertise the webhook node to the agents of the private network")
	return cmd
}

// parseWebhookHeaders parses "Name: value" headers, whose values may end in
// a secret value.
func parseWebhookHeaders(headers []string, r *flagparse.SecretReader) (map[string]string, error) {
	out := map[string]string{}
	for _, h := range headers {
		k, v, err := flagparse.ResolveSecretHeader("webhook-header", h, r)
		if err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, nil
}

func printPairCode(cfg *globalConfig, resp *controlapi.PairCodeResponse) error {
	mins := int(time.Until(resp.Expires).Round(time.Minute).Minutes())
	var sb strings.Builder
	if resp.Node != nil {
		fmt.Fprintf(&sb, "→ webhook node %s; the agent calls it as %q\n  send this to the agent behind the webhook (the code works once, for %d min):\n\n",
			resp.Node.Name, resp.Node.ID, mins)
		for l := range strings.SplitSeq(resp.Instructions, "\n") {
			sb.WriteString("    " + l + "\n")
		}
		fmt.Fprintf(&sb, "\n  you can call it yourself at %s\n", resp.Node.URL)
		if resp.Advertised {
			fmt.Fprintf(&sb, "  advertised to the private network; other agents add it with `a2a-serve contact add %s@<this agent>`\n", resp.Node.ID)
		}
	} else {
		fmt.Fprintf(&sb, "→ code %s (%d min, single use)\n  caller:    %s\n  messenger: send \"join %s\" to the agent's bot\n", resp.Code, mins, resp.PairCommand, resp.Code)
	}
	for _, h := range resp.Hints {
		sb.WriteString("  " + h + "\n")
	}
	_, err := fmt.Fprint(cfg.Out, sb.String())
	return err
}

// warnPairCode warns when the code can't be redeemed now, because the agent
// is stopped, or only on this machine.
func warnPairCode(w io.Writer, resp *controlapi.PairCodeResponse, offline bool) error {
	if offline {
		if _, err := fmt.Fprint(w, "! the agent is stopped: the code works once it runs, within its expiry; start it with `a2a-serve start --detach`,\n"+
			"  `a2a-serve status --url=local` prints the local URL to redeem it at; the private and public URLs appear after the agent starts\n"); err != nil {
			return err
		}
	}
	if !localOnly(resp.URL) {
		return nil
	}
	_, err := fmt.Fprint(w, "! this URL only works on this machine; to invite someone on another one, add a private or public URL with\n"+
		"  `a2a-serve config set --private tailscale` (or a relay URL) or `a2a-serve config set --public tailscale`, then `a2a-serve restart`\n")
	return err
}
