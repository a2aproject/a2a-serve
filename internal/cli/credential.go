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
	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/mailboxapi"
)

func newCredentialCmd(cfg *globalConfig) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "credential",
		Short: "Credential helper that gives the a2a CLI the saved agent tokens",
		Long: `A credential helper for the a2a CLI, following the Bazel credential helper
protocol (https://github.com/EngFlow/credential-helper-spec). It returns the
token saved by "a2a serve tokens add" for an agent URL, or the node token in
$` + mailboxapi.EnvNodeToken + ` for requests to its agent.

Enable it for the a2a CLI with a2a-serve doctor --fix, or in
~/.config/a2a-cli/config.yaml:

  credential-helper:
    - command: [a2a-serve, credential]`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newCredentialGetCmd(cfg))
	return cmd
}
