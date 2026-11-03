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

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/clierr"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/secretref"
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
` + "`a2a serve pair join <agent-url> <code>`" + `, or links a messenger account by
sending "join <code>" to the agent's bot. Inviting an existing name issues
another code, for another device or account; its tokens keep working.
` + "`a2a serve pair set`" + ` changes a pairing, and rotates its credentials.

With --webhook, <name> is a webhook node: an agent which only runs when woken,
such as a Grok Bot routine. The agent hosts it at <agent-url>/n/<name>, calls
it as <name> with the a2a CLI, and wakes it with a signed POST to the
webhook. The node pairs with the code the same way, answers with
` + "`a2a serve mailbox reply`" + `, and can call the agent with its token. A header
value may end in a secret value, env:VAR or -, as in
"Authorization: Bearer env:TOKEN".

--advertised tells every agent on the private network about the node: they
list it in ` + "`a2a serve peers`" + ` as <name>@<this agent> and can add it with
` + "`a2a serve contact add <name>@<this agent>`" + `, with no code. Their calls go
through this agent, which keeps the webhook's credentials, and it treats
them like any other caller from the private network.`,
		Example: `  a2a serve pair invite grok
  a2a serve pair invite anna --role owner
  a2a serve pair invite grok --webhook https://example.com/hooks/123 \
    --webhook-header "Authorization: Bearer env:GROK_WEBHOOK_TOKEN"
  a2a serve pair invite grok --webhook https://example.com/hooks/123 --advertised`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			req.Name = args[0]
			if !cmd.Flags().Changed("role") {
				req.Role = ""
			}
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
			c, err := sf.control()
			if err != nil {
				return err
			}
			resp, err := c.Pair(cmd.Context(), req)
			if err != nil {
				return err
			}
			if cfg.IsJSON() {
				if err := warnLocalOnly(cfg.stderr(), resp); err != nil {
					return err
				}
				return cfg.PrintJSON(resp)
			}
			if err := printPairCode(cfg, resp); err != nil {
				return err
			}
			return warnLocalOnly(cfg.Out, resp)
		},
	}
	f := cmd.Flags()
	f.StringVar(&req.Role, "role", "member", "`Role`: member (can talk) or owner (can also approve)")
	f.StringVar(&req.Webhook, "webhook", "", "Host a webhook node: the URL the agent POSTs wakes to")
	f.StringArrayVar(&headers, "webhook-header", nil, "Header sent with every wake: `Name: value`, such as \"Authorization: Bearer env:TOKEN\"; the value may end in env:VAR or - (repeatable)")
	f.StringVar(&req.Description, "description", "", "Description for the node's card")
	f.BoolVar(&advertised, "advertised", false, "Advertise the webhook node to the agents of the private network")
	must(cmd.RegisterFlagCompletionFunc("role", fixedCompletions(agentdir.RoleMember, agentdir.RoleOwner)))
	return cmd
}

// parseWebhookHeaders parses "Name: value" headers, whose values may end in
// a secret value.
func parseWebhookHeaders(headers []string, r *secretref.Reader) (map[string]string, error) {
	out := map[string]string{}
	for _, h := range headers {
		k, v, err := secretref.ResolveHeader("webhook-header", h, r)
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
			fmt.Fprintf(&sb, "  advertised to the private network; other agents add it with `a2a serve contact add %s@<this agent>`\n", resp.Node.ID)
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

// warnLocalOnly warns when the code is redeemed at a URL which only works
// on this machine, which is useless to invite someone else.
func warnLocalOnly(w io.Writer, resp *controlapi.PairCodeResponse) error {
	if !controlapi.LocalOnly(resp.URL) {
		return nil
	}
	_, err := fmt.Fprint(w, "! this URL only works on this machine; to invite someone on another one, add a private or public URL with\n"+
		"  `a2a serve config set --private tailscale` (or lan) or `a2a serve config set --public tailscale` (or ngrok), then `a2a serve restart`\n")
	return err
}
