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
	"errors"
	"strings"

	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/credstore"
)

func newPairCmd(cfg *globalConfig, sf *serverFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pair",
		Short: "Pair callers and webhook nodes with the agent, or join another agent",
		Long: `On the agent's side, "invite" creates a pairing and prints its one-time code;
"list", "get", "set" and "rm" manage the pairings. The caller redeems the code
with ` + "`a2a-serve pair join <agent-url> <code>`" + `, or by sending "join <code>"
to the agent's bot.

On the caller's side, "join" saves the agent's token to
~/.config/a2a-serve/credentials.json, which the a2a CLI sends with requests to
that agent. "list" also shows the agents you joined, and "get" and "rm" take
an agent URL.`,
		Example: `  a2a-serve pair invite anna
  a2a-serve pair join https://coder.tail1234.ts.net 3MZ8-LQ0D
  a2a-serve pair list
  a2a-serve pair set anna --owner
  a2a-serve pair rm anna`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	sf.addControlFlags(cmd.PersistentFlags())
	cmd.AddCommand(
		newPairInviteCmd(cfg, sf),
		newPairJoinCmd(cfg, sf),
		newPairListCmd(cfg, sf),
		newPairGetCmd(cfg, sf),
		newPairSetCmd(cfg, sf),
		newPairRmCmd(cfg, sf),
	)
	return cmd
}

// isAgentURL reports whether arg names a joined agent rather than a pairing.
func isAgentURL(arg string) bool { return strings.Contains(arg, "://") }

// joinedCredential returns the saved token of the agent at url.
func joinedCredential(cfg *globalConfig, url string) (credstore.Credential, error) {
	store, err := cfg.credentialStore()
	if err != nil {
		return credstore.Credential{}, err
	}
	creds, err := store.List()
	if err != nil {
		return credstore.Credential{}, err
	}
	for _, c := range creds {
		if credstore.Normalize(c.URL) == credstore.Normalize(url) {
			return c, nil
		}
	}
	return credstore.Credential{}, errors.New("you haven't joined " + url + "; join with `a2a-serve pair join <agent-url> <code>`")
}

// joinedCompletions completes the agent URLs of the saved tokens.
func joinedCompletions(cfg *globalConfig) ([]cobra.Completion, error) {
	store, err := cfg.credentialStore()
	if err != nil {
		return nil, err
	}
	creds, err := store.List()
	if err != nil {
		return nil, err
	}
	urls := make([]cobra.Completion, 0, len(creds))
	for _, c := range creds {
		urls = append(urls, c.URL)
	}
	return urls, nil
}
