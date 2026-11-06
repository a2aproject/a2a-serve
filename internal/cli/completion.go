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
	"time"

	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/clierr"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
)

// scriptProgram is the program the completion scripts complete. They ask
// it for completions with `a2a-serve __complete <args>`.
const scriptProgram = "a2a-serve"

// completionTimeout bounds the control calls of a completion, which runs
// while the user waits at the prompt.
const completionTimeout = 2 * time.Second

func newCompletionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "completion <bash|zsh|fish|powershell>",
		Short: "Print a shell completion script",
		Long: `Print a completion script for ` + scriptProgram + `. It completes commands, agent
names, messenger kinds, enum flags, and the contacts and pairings of a running agent.

  source <(a2a-serve completion bash)
  a2a-serve completion zsh > "${fpath[1]}/_` + scriptProgram + `"
  a2a-serve completion fish > ~/.config/fish/completions/` + scriptProgram + `.fish`,
		Args:      namedArgs,
		ValidArgs: []string{"bash", "zsh", "fish", "powershell"},
		RunE: func(cmd *cobra.Command, args []string) error {
			root := cmd.Root()
			out := cmd.OutOrStdout()
			switch args[0] {
			case "bash":
				return root.GenBashCompletionV2(out, true)
			case "zsh":
				return root.GenZshCompletion(out)
			case "fish":
				return root.GenFishCompletion(out, true)
			case "powershell":
				return root.GenPowerShellCompletionWithDesc(out)
			}
			return clierr.Usage(fmt.Sprintf("unknown shell %q: want bash, zsh, fish or powershell", args[0]))
		},
	}
	markTextOnly(cmd)
	return cmd
}

// must panics on errors which only a programming mistake causes, such as
// registering a completion for a flag that doesn't exist.
func must(err error) {
	if err != nil {
		panic(err)
	}
}

func fixedCompletions(values ...string) cobra.CompletionFunc {
	return cobra.FixedCompletions(values, cobra.ShellCompDirectiveNoFileComp)
}

// controlCompletions completes the first argument with the names list
// returns from the running agent, and nothing when it doesn't run.
func controlCompletions(sf *serverFlags, list func(ctx context.Context, c *controlapi.Client) ([]string, error)) cobra.CompletionFunc {
	return func(cmd *cobra.Command, args []string, _ string) ([]cobra.Completion, cobra.ShellCompDirective) {
		if len(args) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		c, err := sf.control()
		if err != nil {
			cobra.CompDebugln(err.Error(), false)
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), completionTimeout)
		defer cancel()
		names, err := list(ctx, c)
		if err != nil {
			cobra.CompDebugln(err.Error(), false)
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return names, cobra.ShellCompDirectiveNoFileComp
	}
}
