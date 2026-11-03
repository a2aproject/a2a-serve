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

	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/pairapi"
)

func newPairRmCmd(cfg *globalConfig, sf *serverFlags) *cobra.Command {
	return &cobra.Command{
		Use:               "rm <name|agent-url>",
		Aliases:           []string{"delete"},
		Short:             "Revoke a pairing, remove a webhook node, or forget an agent you joined",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: pairingCompletions(cfg, sf),
		RunE: func(cmd *cobra.Command, args []string) error {
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
			c, err := sf.control()
			if err != nil {
				return err
			}
			resp, err := c.Revoke(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if resp.Kind == pairapi.KindNode {
				return cfg.PrintRemoved(resp.Kind, args[0], fmt.Sprintf("✓ removed node %s; the agent can no longer wake it", args[0]))
			}
			return cfg.PrintRemoved(pairapi.KindCaller, args[0], fmt.Sprintf("✓ %s can no longer call the agent", args[0]))
		},
	}
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
