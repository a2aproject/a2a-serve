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
	"slices"

	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/clierr"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/flagparse"
	"github.com/a2aproject/a2a-serve/internal/pairapi"
)

func newPairSetCmd(cfg *globalConfig, sf *serverFlags) *cobra.Command {
	var (
		req         controlapi.PairingUpdate
		headers     []string
		description string
		advertised  bool
		owner       bool
		sandbox     sandboxFlags
	)
	cmd := &cobra.Command{
		Use:   "set <name>",
		Short: "Change a pairing, or rotate its credentials",
		Long: `Change a pairing in place: whether it is an owner and its kind (person,
device or agent), what its agent container gets, or the webhook, headers,
description and advertising of a webhook node. Its tokens keep working, and
a change applies to their next request. A change of --storage or --secrets
applies to its agent container once its current turn ends.

--rotate revokes every token of the pairing and its pending codes, and
prints a new code. The caller joins again with the code and --replace, and
keeps its conversations and tasks. For a webhook node, --rotate also
changes the secret its wakes are signed with. Linked messenger accounts
stay linked.

` + flagparse.SecretHelp,
		Example: `  a2a-serve pair set anna --owner
  a2a-serve pair set anna --owner=false
  a2a-serve pair set laptop --kind device
  a2a-serve pair set ben --storage shared --secrets all
  a2a-serve pair set ben --secrets none
  a2a-serve pair set anna --rotate
  a2a-serve pair set grok --webhook https://example.com/hooks/456 --advertised`,
		Args:              namedArgs,
		ValidArgsFunction: pairingCompletions(cfg, sf),
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			changes := []string{"owner", "kind", "webhook", "webhook-header", "description", "advertised", "rotate", "storage", "secrets"}
			if !slices.ContainsFunc(changes, cmd.Flags().Changed) {
				return clierr.Usage("nothing to change: pass --owner, --kind, --webhook, --webhook-header, --description, --advertised, --rotate, --storage or --secrets")
			}
			if req.Sandbox, err = sandbox.change(cmd); err != nil {
				return err
			}
			if cmd.Flags().Changed("owner") {
				req.Owner = &owner
			}
			hdrs, err := parseWebhookHeaders(headers, cfg.secretReader(cfg.stderr()))
			if err != nil {
				return err
			}
			req.Headers = hdrs
			if cmd.Flags().Changed("description") {
				req.Description = &description
			}
			if cmd.Flags().Changed("advertised") {
				req.Advertised = &advertised
			}
			c, closer, err := sf.manager(cmd.Context())
			if err != nil {
				return err
			}
			defer joinClose(&err, closer)
			resp, err := c.UpdatePairing(cmd.Context(), args[0], req)
			if err != nil {
				return err
			}
			if cfg.IsJSON() {
				return cfg.PrintJSON(resp)
			}
			if _, err := fmt.Fprintf(cfg.Out, "✓ updated %s\n", args[0]); err != nil {
				return err
			}
			if resp.Code != nil {
				if _, err := fmt.Fprintln(cfg.Out, "  its tokens were revoked; pair again with the new code:"); err != nil {
					return err
				}
				if err := printPairCode(cfg, resp.Code); err != nil {
					return err
				}
				if err := warnPairCode(cfg.Out, resp.Code, !c.Running()); err != nil {
					return err
				}
			}
			return printAccess(cfg, resp.Access)
		},
	}
	f := cmd.Flags()
	f.BoolVar(&owner, "owner", false, "Make the pairing an owner, who sees and approves every task, or not with --owner=false")
	f.StringVar(&req.Kind, "kind", "", "Kind of the pairing: `person|device|agent`")
	f.StringVar(&req.Webhook, "webhook", "", "The URL the agent POSTs wakes to, for a webhook node")
	f.StringArrayVar(&headers, "webhook-header", nil, "Header sent with every wake, replacing the current ones: `Name: value`; the value may end in env:VAR or - (repeatable)")
	f.StringVar(&description, "description", "", "Description for the node's card")
	f.BoolVar(&advertised, "advertised", false, "Advertise the webhook node to the agents of the private network")
	f.BoolVar(&req.Rotate, "rotate", false, "Revoke every token of the pairing and print a new code")
	sandbox.register(cmd)
	must(cmd.RegisterFlagCompletionFunc("kind", fixedCompletions(pairapi.PairingPerson, pairapi.PairingDevice, pairapi.PairingAgent)))
	return cmd
}
