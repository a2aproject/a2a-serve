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
	cmd := &cobra.Command{
		Use:   "rm <name>",
		Short: "Revoke the tokens and messenger links issued to a name, or remove a webhook node",
		Args:  cobra.ExactArgs(1),
		ValidArgsFunction: controlCompletions(sf, func(ctx context.Context, c *controlapi.Client) ([]string, error) {
			toks, err := c.Tokens(ctx)
			if err != nil {
				return nil, err
			}
			nodes, err := c.Nodes(ctx)
			if err != nil {
				return nil, err
			}
			names := make([]string, 0, len(toks)+len(nodes))
			for _, t := range toks {
				names = append(names, t.Name)
			}
			for _, n := range nodes {
				names = append(names, n.ID)
			}
			return names, nil
		}),
		RunE: func(cmd *cobra.Command, args []string) error {
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
	sf.addControlFlags(cmd.Flags())
	return cmd
}
