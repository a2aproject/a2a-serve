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
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/credstore"
	"github.com/a2aproject/a2a-serve/internal/output"
)

func newPairListCmd(cfg *globalConfig, sf *serverFlags) *cobra.Command {
	var joined bool
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the pairings of the agent, or the agents you joined",
		Long: `In an agent directory, list the pairings of the agent with their kind and
role: people, devices, other agents and webhook nodes, and the built-in local
pairing, which is you. Outside one, or with --joined, list the agents you joined with
` + "`a2a-serve pair join`" + `.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			caller, err := sf.callerView(joined)
			if err != nil {
				return err
			}
			if caller {
				return listJoined(cfg)
			}
			c, closer, err := sf.manager(cmd.Context())
			if err != nil {
				return err
			}
			defer joinClose(&err, closer)
			all, err := c.Pairings(cmd.Context())
			if err != nil {
				return err
			}
			if cfg.IsJSON() {
				return output.PrintList(cfg.Printer, all)
			}
			rows := make([][]string, 0, len(all))
			for _, p := range all {
				rows = append(rows, []string{p.Name, orDash(p.Kind), p.Role, linkedChannels(p), lastUsed(p)})
			}
			return cfg.PrintTableOrEmpty([]string{"NAME", "KIND", "ROLE", "LINKED", "LAST USED"}, rows, "no pairings; invite someone with `a2a-serve pair invite <name>`")
		},
	}
	cmd.Flags().BoolVar(&joined, "joined", false, "List the agents you joined, also in an agent directory")
	return cmd
}

// joinedAgent is a saved token, as pair list and get show it.
type joinedAgent struct {
	URL       string    `json:"url"`
	Kind      string    `json:"kind"`
	Agent     string    `json:"agent"`
	Principal string    `json:"principal"`
	Joined    time.Time `json:"joined"`
}

func joinedOf(c credstore.Credential) joinedAgent {
	kind := c.Kind
	if kind == "" {
		kind = "caller"
	}
	return joinedAgent{URL: c.URL, Kind: kind, Agent: c.Agent, Principal: c.Principal, Joined: c.Created}
}

func listJoined(cfg *globalConfig) error {
	store, err := cfg.credentialStore()
	if err != nil {
		return err
	}
	creds, err := store.List()
	if err != nil {
		return err
	}
	out := make([]joinedAgent, 0, len(creds))
	rows := make([][]string, 0, len(creds))
	for _, c := range creds {
		j := joinedOf(c)
		out = append(out, j)
		rows = append(rows, []string{j.URL, j.Agent, j.Principal, j.Joined.Format(time.DateTime)})
	}
	if cfg.IsJSON() {
		return output.PrintList(cfg.Printer, out)
	}
	return cfg.PrintTableOrEmpty([]string{"URL", "AGENT", "AS", "JOINED"}, rows, "no joined agents; join one with `a2a-serve pair join <agent-url> <code>`")
}

// linkedChannels names the messengers of the accounts linked to p.
func linkedChannels(p controlapi.Pairing) string {
	var out []string
	for _, a := range p.Accounts {
		if !slices.Contains(out, a.Channel) {
			out = append(out, a.Channel)
		}
	}
	return orDash(strings.Join(out, ","))
}

func lastUsed(p controlapi.Pairing) string {
	switch {
	case p.Node != nil && !p.Node.Paired:
		return "not paired"
	case p.Node != nil:
		return "-"
	case !p.LastUsed.IsZero():
		return p.LastUsed.Format(time.DateTime)
	case len(p.Tokens) == 0 && len(p.Accounts) == 0 && len(p.PendingCodes) > 0:
		return "code pending"
	case len(p.Tokens) == 0 && len(p.Accounts) == 0:
		return "no credentials"
	}
	return "never"
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
