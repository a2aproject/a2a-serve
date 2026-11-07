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
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/credstore"
	"github.com/a2aproject/a2a-serve/internal/output"
)

// Sides of a pair list entry.
const (
	sideInvited = "invited"
	sideJoined  = "joined"
)

// invitedPairing and joinedPairing are the entries of `pair list -o json`.
type invitedPairing struct {
	Side string `json:"side"`
	controlapi.Pairing
}

type joinedPairing struct {
	Side string `json:"side"`
	joinedAgent
}

func newPairListCmd(cfg *globalConfig, sf *serverFlags) *cobra.Command {
	var joined, invited bool
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the pairings of the agent and the agents you joined",
		Long: `List the pairings of the agent with their kind and role: people, devices,
other agents and webhook nodes, and the built-in local pairing, which is you.
Then list the agents you joined with ` + "`a2a-serve pair join`" + `. Run it in an agent
directory; --joined lists only the agents you joined, from any directory.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			var pairings []controlapi.Pairing
			if !joined {
				c, closer, err := sf.manager(cmd.Context())
				if err != nil {
					return err
				}
				defer joinClose(&err, closer)
				if pairings, err = c.Pairings(cmd.Context()); err != nil {
					return err
				}
			}
			var agents []joinedAgent
			if !invited {
				if agents, err = listJoined(cfg); err != nil {
					return err
				}
			}
			if cfg.IsJSON() {
				out := make([]any, 0, len(pairings)+len(agents))
				for _, p := range pairings {
					out = append(out, invitedPairing{Side: sideInvited, Pairing: p})
				}
				for _, a := range agents {
					out = append(out, joinedPairing{Side: sideJoined, joinedAgent: a})
				}
				return output.PrintList(cfg.Printer, out)
			}
			sections := !joined && !invited
			if !joined {
				if err := printInvited(cfg, pairings, sections); err != nil {
					return err
				}
			}
			if sections {
				if _, err := fmt.Fprintln(cfg.Out); err != nil {
					return err
				}
			}
			if invited {
				return nil
			}
			return printJoined(cfg, agents, sections)
		},
	}
	cmd.Flags().BoolVar(&joined, "joined", false, "List only the agents you joined")
	cmd.Flags().BoolVar(&invited, "invited", false, "List only the pairings of the agent")
	cmd.MarkFlagsMutuallyExclusive("joined", "invited")
	return cmd
}

func printInvited(cfg *globalConfig, all []controlapi.Pairing, heading bool) error {
	if heading {
		if _, err := fmt.Fprintln(cfg.Out, "Invited to this agent:"); err != nil {
			return err
		}
	}
	rows := make([][]string, 0, len(all))
	for _, p := range all {
		rows = append(rows, []string{p.Name, orDash(p.Kind), p.Role, linkedMessengers(p), lastUsed(p)})
	}
	return cfg.PrintTableOrEmpty([]string{"NAME", "KIND", "ROLE", "LINKED", "LAST USED"}, rows, "no pairings; invite someone with `a2a-serve pair invite <name>`")
}

func printJoined(cfg *globalConfig, all []joinedAgent, heading bool) error {
	if heading {
		if _, err := fmt.Fprintln(cfg.Out, "Joined by you:"); err != nil {
			return err
		}
	}
	rows := make([][]string, 0, len(all))
	for _, j := range all {
		rows = append(rows, []string{j.URL, j.Agent, j.Principal, j.Joined.Format(time.DateTime)})
	}
	return cfg.PrintTableOrEmpty([]string{"URL", "AGENT", "AS", "JOINED"}, rows, "no joined agents; join one with `a2a-serve pair join <agent-url> <code>`")
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

func listJoined(cfg *globalConfig) ([]joinedAgent, error) {
	store, err := cfg.credentialStore()
	if err != nil {
		return nil, err
	}
	creds, err := store.ListJoined()
	if err != nil {
		return nil, err
	}
	out := make([]joinedAgent, 0, len(creds))
	for _, c := range creds {
		out = append(out, joinedOf(c))
	}
	return out, nil
}

// linkedMessengers names the messengers of the accounts linked to p.
func linkedMessengers(p controlapi.Pairing) string {
	var out []string
	for _, a := range p.Accounts {
		if !slices.Contains(out, a.Messenger) {
			out = append(out, a.Messenger)
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
