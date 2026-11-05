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

	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/agentctl"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/lifecycle"
	"github.com/a2aproject/a2a-serve/internal/secretref"
)

func newConfigSetCmd(cfg *globalConfig, sf *serverFlags) *cobra.Command {
	var (
		flags   settingsFlags
		dryRun  bool
		restart bool
	)
	cmd := &cobra.Command{
		Use:   "set <flags>",
		Short: "Change settings with the flags of `a2a-serve start <agent>`, and the settings only it takes",
		Long: `Change settings with the flags of ` + "`a2a-serve start <agent>`" + `, and the
operational settings only config set takes: limits, network-provider
options, backups and Google sign-in. Messengers are set with
` + "`a2a-serve channel add`" + `. With --agent, config set also creates an
agent, to be started with ` + "`a2a-serve start`" + `. Scalars are
overwritten. List entries are added, or replace the entry with the same key:
--secret by name, --env by NAME, --mount by destination, and the other
lists by value. Remove entries with ` + "`a2a-serve config unset`" + `. A change to who
may call the agent, or to its networks, prints who may call afterwards.

` + secretref.Help,
		Example: `  a2a-serve config set --sandbox local --allow-tools read,search --secret ANTHROPIC_API_KEY
  a2a-serve config set --tailscale-authkey - --restart
  a2a-serve config set --mount ~/docs:/docs:ro --dry-run`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := sf.agentDir()
			// With --agent, a new agent is configured before its first
			// start, such as with the credentials a headless start needs.
			if errors.Is(err, agentdir.ErrNotFound) && flags.s.Harness != "" {
				d, err = harnessDir(sf.dir)
			}
			if err != nil {
				return err
			}
			s, err := flags.settings(cfg.secretReader(cfg.stderr()))
			if err != nil {
				return err
			}
			res, err := agentctl.Set(cmd.Context(), d, sf.controlClient(d), s, dryRun)
			if err != nil {
				return err
			}
			if err := printConfigResult(cfg, res); err != nil {
				return err
			}
			if !restart || len(res.Restart) == 0 {
				return nil
			}
			return lifecycle.Restart(cmd.Context(), d, sf.controlClient(d), false, cfg.progress())
		},
	}
	flags.register(cmd, coreSettings|operationalSettings|harnessSetting)
	cmd.SetUsageFunc(sectionedUsage)
	hideImageForLocalSandbox(cmd, sf.agentDir)
	f := cmd.Flags()
	f.BoolVar(&dryRun, "dry-run", false, "Print the diff without saving")
	f.BoolVar(&restart, "restart", false, "Restart the running agent if a change needs it")
	return cmd
}
