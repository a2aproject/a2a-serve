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

	"github.com/a2aproject/a2a-serve/internal/output"
	"github.com/spf13/cobra"
)

func newPeersCmd(cfg *globalConfig, sf *serverFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "peers",
		Short: "List agents found on the private network, and the pairings they advertise",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := sf.control()
			if err != nil {
				return err
			}
			peers, err := c.Peers(cmd.Context())
			if err != nil {
				return err
			}
			ads, err := c.Advertisements(cmd.Context())
			if err != nil {
				return err
			}
			if cfg.IsJSON() {
				out := make([]peerRow, 0, len(peers)+len(ads))
				for _, p := range peers {
					out = append(out, peerRow{Kind: "agent", Name: p.Name, URL: p.URL, Description: p.Description, Online: p.Online})
				}
				for _, a := range ads {
					out = append(out, peerRow{Kind: "advertised", Name: a.Key, Description: a.Description})
				}
				return output.PrintList(cfg.Printer, out)
			}
			rows := make([][]string, 0, len(peers))
			for _, p := range peers {
				state := "offline"
				if p.Online {
					state = "online"
				}
				rows = append(rows, []string{p.Name, p.URL, p.Description, state})
			}
			if err := cfg.PrintTableOrEmpty([]string{"NAME", "URL", "DESCRIPTION", "STATE"}, rows, "no agents found on the private network"); err != nil {
				return err
			}
			if len(ads) == 0 {
				return nil
			}
			adRows := make([][]string, 0, len(ads))
			for _, a := range ads {
				adRows = append(adRows, []string{a.Key, a.Description})
			}
			if _, err := fmt.Fprintln(cfg.Out, "\nAdvertised pairings (add one with `a2a serve contact add <name>@<agent>`):"); err != nil {
				return err
			}
			return cfg.PrintTableOrEmpty([]string{"PAIRING", "DESCRIPTION"}, adRows, "")
		},
	}
	sf.addControlFlags(cmd.Flags())
	return cmd
}

// peerRow is an agent of the private network, or a pairing one advertises.
type peerRow struct {
	// Kind is agent or advertised.
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	URL         string `json:"url,omitempty"`
	Description string `json:"description,omitempty"`
	Online      bool   `json:"online"`
}
