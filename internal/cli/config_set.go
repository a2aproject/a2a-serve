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

	"github.com/a2aproject/a2a-serve/internal/agentctl"
)

func newConfigSetCmd(cfg *globalConfig, sf *serverFlags) *cobra.Command {
	var (
		flags   settingsFlags
		dryRun  bool
		restart bool
		quiet   bool
	)
	cmd := &cobra.Command{
		Use:   "set <flags>",
		Short: "Change settings with the flags of `a2a serve <harness>`",
		Long: `Change settings with the flags of ` + "`a2a serve <harness>`" + `. Scalars are
overwritten. List entries are added, or replace the entry with the same key:
--secret by name, --env by NAME, --mount by destination, --identity by login,
and --allow-tools, --deny-tools and --approver by value. Remove entries with
` + "`a2a serve config unset`" + `.

Secret-valued flags (--telegram, --discord, --email-password,
--tailscale-authkey, --ngrok-authtoken) take - to read the value from a
prompt without echo, or from stdin, keeping it out of the shell history.`,
		Example: `  a2a serve config set --sandbox local --allow-tools read,search --secret ANTHROPIC_API_KEY
  a2a serve config set --telegram - --restart
  a2a serve config set --mount ~/docs:/docs:ro --dry-run`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := sf.agentDir()
			if err != nil {
				return err
			}
			s, err := flags.settings(newSecretReader(cfg.stderr()))
			if err != nil {
				return err
			}
			res, err := agentctl.Set(cmd.Context(), d, s, dryRun)
			if err != nil {
				return err
			}
			if err := printConfigResult(cfg, res); err != nil {
				return err
			}
			if !restart || len(res.Restart) == 0 {
				return nil
			}
			return agentctl.Restart(cmd.Context(), d, quiet, cfg.Out)
		},
	}
	flags.register(cmd, true)
	f := cmd.Flags()
	f.BoolVar(&dryRun, "dry-run", false, "Print the diff without saving")
	f.BoolVar(&restart, "restart", false, "Restart the running agent if a change needs it")
	f.BoolVar(&quiet, "quiet", false, "With --restart, keep the harness image output in the log")
	return cmd
}
