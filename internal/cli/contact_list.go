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

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/output"
	"github.com/spf13/cobra"
)

// peersNote says that the agent may call everyone on its private network
// besides its contacts.
const peersNote = "the agent may also call every agent on the private network (--private-allow-outgoing everyone); `a2a-serve peers` lists them"

func newContactListCmd(cfg *globalConfig, sf *serverFlags) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List contacts",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			c, closer, err := sf.manager(cmd.Context())
			if err != nil {
				return err
			}
			defer joinClose(&err, closer)
			cs, err := c.Contacts(cmd.Context())
			if err != nil {
				return err
			}
			if cfg.IsJSON() {
				return output.PrintList(cfg.Printer, cs)
			}
			peers, err := callsPeers(sf)
			if err != nil {
				return err
			}
			if len(cs) == 0 && peers {
				_, err := fmt.Fprintln(cfg.Out, peersNote)
				return err
			}
			rows := make([][]string, 0, len(cs))
			for _, x := range cs {
				auth := "network identity"
				if x.HasToken {
					auth = "token"
				}
				rows = append(rows, []string{x.Name, x.URL, x.Via, auth})
			}
			if err := cfg.PrintTableOrEmpty([]string{"NAME", "URL", "VIA", "AUTH"}, rows, "no contacts; add one with `a2a-serve contact add <name>`"); err != nil || !peers {
				return err
			}
			_, err = fmt.Fprintln(cfg.Out, "\n"+peersNote)
			return err
		},
	}
}

// callsPeers reports whether the saved settings let the agent call everyone
// on its private network.
func callsPeers(sf *serverFlags) (bool, error) {
	d, err := sf.agentDir()
	if err != nil {
		return false, err
	}
	saved, err := d.ReadConfig()
	if err != nil {
		return false, err
	}
	private := saved.Network.Private != "" && saved.Network.Private != agentdir.NetworkNone
	return private && saved.Policy.PrivateAllowOutgoing == agentdir.AccessEveryone, nil
}
