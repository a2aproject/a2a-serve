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

	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/agentctl"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
)

func newConfigUnsetCmd(cfg *globalConfig, sf *serverFlags) *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "unset <setting> [<entry>...]",
		Short: "Remove entries from a list setting, or reset a setting to its default",
		Long: `Remove entries from a list setting, or the whole list without entries, or
reset a setting to its default. The setting is the flag name without --.
Entries are named by their key: a secret or env by name, a mount by its
destination, others by value. Unsetting a secret also deletes its value
from .a2a/state/secrets. Messengers are removed with ` + "`a2a-serve channel rm`" + `.

Settings: ` + strings.Join(agentdir.SettingNames(), ", "),
		Example: `  a2a-serve config unset secret OPENROUTER_API_KEY
  a2a-serve config unset env DEBUG
  a2a-serve config unset mount /docs
  a2a-serve config unset allow-email '*@example.com'
  a2a-serve config unset private`,
		Args:      namedArgs,
		ValidArgs: agentdir.SettingNames(),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := agentdir.CheckUnset(args[0]); err != nil {
				return err
			}
			d, err := sf.agentDir()
			if err != nil {
				return err
			}
			res, err := agentctl.Unset(cmd.Context(), d, sf.controlClient(d), args[0], args[1:], dryRun)
			if err != nil {
				return err
			}
			return printConfigResult(cfg, res)
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print the diff without saving")
	return cmd
}
