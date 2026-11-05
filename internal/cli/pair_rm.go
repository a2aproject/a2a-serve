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
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/pairapi"
)

func newPairRmCmd(cfg *globalConfig, sf *serverFlags) *cobra.Command {
	return &cobra.Command{
		Use:     "rm <name|agent-url>",
		Aliases: []string{"delete"},
		Short:   "Revoke a pairing, remove a webhook node, or forget an agent you joined",
		Long: `Delete a pairing with its tokens, linked messenger accounts, pending
codes, the schedules that run as it, and its tasks, approvals and
conversations, or remove a webhook node with its schedules. A new pairing
with the same name starts with none of them. The person may still call the
agent through the private network or an --allow-email rule; the command
says so, and prints who may call afterwards.

` + "`a2a-serve pair rm local`" + ` revokes the token of this machine and your linked
chat accounts, and keeps the pairing; the next start issues a new token.

With an agent URL, forget the token you saved with ` + "`a2a-serve pair join`" + `.`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: pairingCompletions(cfg, sf),
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			if isAgentURL(args[0]) {
				store, err := cfg.credentialStore()
				if err != nil {
					return err
				}
				ok, err := store.Delete(args[0])
				if err != nil {
					return err
				}
				if !ok {
					return fmt.Errorf("you haven't joined %s", args[0])
				}
				return cfg.PrintRemoved("joined", args[0], "✓ deleted the token for "+args[0])
			}
			c, closer, err := sf.manager(cmd.Context())
			if err != nil {
				return err
			}
			defer joinClose(&err, closer)
			resp, err := c.Revoke(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if cfg.IsJSON() {
				out := map[string]any{"removed": true, "kind": resp.Kind, "name": args[0]}
				if resp.Kept {
					out["kept"] = true
				}
				if len(resp.StillAllowed) > 0 {
					out["stillAllowed"] = resp.StillAllowed
				}
				if resp.Access != nil {
					out["access"] = resp.Access
				}
				return cfg.PrintJSON(out)
			}
			if _, err := fmt.Fprint(cfg.Out, removedText(args[0], resp)); err != nil {
				return err
			}
			return printAccess(cfg, resp.Access)
		},
	}
}

func removedText(name string, resp *controlapi.RevokeResponse) string {
	switch {
	case resp.Kind == pairapi.PairingWebhook:
		return fmt.Sprintf("✓ removed node %s; the agent can no longer wake it\n", name)
	case resp.Kept:
		return fmt.Sprintf("✓ revoked the token and chat links of %s; the next start issues a new token for this machine\n", name)
	case len(resp.StillAllowed) > 0:
		return fmt.Sprintf("✓ removed the pairing %s\n! %s may still call the agent as one of: %s\n", name, name, strings.Join(resp.StillAllowed, "; "))
	}
	return fmt.Sprintf("✓ %s can no longer call the agent\n", name)
}

// pairingCompletions completes the pairings of the running agent, and the
// agents this user joined.
func pairingCompletions(cfg *globalConfig, sf *serverFlags) cobra.CompletionFunc {
	pairings := controlCompletions(sf, func(ctx context.Context, c *controlapi.Client) ([]string, error) {
		all, err := c.Pairings(ctx)
		if err != nil {
			return nil, err
		}
		names := make([]string, 0, len(all))
		for _, p := range all {
			names = append(names, p.Name)
		}
		return names, nil
	})
	return func(cmd *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
		names, directive := pairings(cmd, args, toComplete)
		if len(args) > 0 {
			return names, directive
		}
		urls, err := joinedCompletions(cfg)
		if err != nil {
			cobra.CompDebugln(err.Error(), false)
		}
		return append(names, urls...), cobra.ShellCompDirectiveNoFileComp
	}
}
