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
	"strings"

	"github.com/a2aproject/a2a-serve/internal/flagparse"
	"github.com/a2aproject/a2a-serve/internal/mailboxapi"
	"github.com/a2aproject/a2a-serve/internal/output"
	"github.com/spf13/cobra"
)

func newMailboxPeersCmd(cfg *globalConfig) *cobra.Command {
	var rf nodeClientFlags
	cmd := &cobra.Command{
		Use:   "peers",
		Short: "List agents this webhook node can call with its node token",
		Long: "List the hosting agent and any advertised sibling webhook nodes that this node " +
			"can call with `a2a send` using its node token.\n\n" + flagparse.SecretHelp,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := nodeContext(cmd, cfg)
			defer cancel()
			c, err := rf.nodeClient(cfg)
			if err != nil {
				return err
			}
			res, err := c.Peers(ctx)
			if err != nil {
				return err
			}
			if cfg.IsJSON() {
				return output.PrintList(cfg.Printer, res.Peers)
			}
			rows := make([][]string, 0, len(res.Peers))
			for _, p := range res.Peers {
				rows = append(rows, []string{p.Name, p.URL, oneLine(p.Description, 120), formatPeerSkills(p.Skills)})
			}
			return cfg.PrintTableOrEmpty([]string{"NAME", "URL", "DESCRIPTION", "SKILLS"}, rows, "no peers available")
		},
	}
	rf.register(cmd.Flags())
	return cmd
}

func formatPeerSkills(skills []mailboxapi.PeerSkill) string {
	if len(skills) == 0 {
		return ""
	}
	parts := make([]string, len(skills))
	for i, sk := range skills {
		if sk.Description != "" {
			parts[i] = sk.ID + ": " + oneLine(sk.Description, 80)
		} else {
			parts[i] = sk.ID
		}
	}
	return strings.Join(parts, "; ")
}
