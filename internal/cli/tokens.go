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
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/a2aproject/a2a-serve/internal/credstore"
)

func newTokensCmd(cfg *globalConfig) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tokens",
		Short: "Pair with an agent, and manage the saved agent tokens",
		Long: `On the caller's side: "add" exchanges a pairing code from an agent's owner
(` + "`a2a serve pair invite`" + `) for a token and saves it to
~/.config/a2a-serve/credentials.json; "list" and "rm" manage the saved tokens.
The a2a CLI attaches them through the credential helper
` + "`" + credstore.HelperCommand + "`" + `.`,
		Example: `  a2a serve tokens add https://assistant.tail1234.ts.net 3MZ8-LQ0D
  a2a serve tokens list
  a2a serve tokens rm https://assistant.tail1234.ts.net`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(
		newTokensAddCmd(cfg),
		newTokensListCmd(cfg),
		newTokensRmCmd(cfg),
	)
	return cmd
}

// addRequestTimeout adds --timeout, which bounds the HTTP requests of a
// command to an agent.
func addRequestTimeout(f *pflag.FlagSet, cfg *globalConfig) {
	f.DurationVar(&cfg.timeout, "timeout", 30*time.Second, "Request timeout")
}

// savedTokenCompletions completes the agent URLs of the saved tokens.
func savedTokenCompletions(cfg *globalConfig) cobra.CompletionFunc {
	return func(cmd *cobra.Command, args []string, _ string) ([]cobra.Completion, cobra.ShellCompDirective) {
		if len(args) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		store, err := cfg.credentialStore()
		if err != nil {
			cobra.CompDebugln(err.Error(), false)
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		creds, err := store.List()
		if err != nil {
			cobra.CompDebugln(err.Error(), false)
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		urls := make([]cobra.Completion, 0, len(creds))
		for _, c := range creds {
			urls = append(urls, c.URL)
		}
		return urls, cobra.ShellCompDirectiveNoFileComp
	}
}
