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
	"log/slog"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/harness"
)

const quietUsage = "Print less on the terminal: keep the harness image pull and build output in the log"

// addRunFlags adds the flags of how an agent runs, which are not saved.
func (m *runMode) addRunFlags(f *pflag.FlagSet) {
	f.BoolVar(&m.detach, "detach", false, "Run the agent in the background")
	f.BoolVar(&m.service, "service", false, "Install and start the agent as a launchd or systemd user service")
	f.BoolVar(&m.quiet, "quiet", false, quietUsage)
	setSection(f, sectionRun, "detach", "service", "quiet")
}

// hideImageForLocalSandbox hides --image from the help of cmd when the
// sandbox is local, given with --sandbox or saved in the agent of dir.
func hideImageForLocalSandbox(cmd *cobra.Command, dir func() (agentdir.Dir, error)) {
	help := cmd.HelpFunc()
	cmd.SetHelpFunc(func(c *cobra.Command, args []string) {
		if sandboxIsLocal(c, dir) {
			c.Flags().Lookup("image").Hidden = true
		}
		help(c, args)
	})
}

func sandboxIsLocal(c *cobra.Command, dir func() (agentdir.Dir, error)) bool {
	if v := c.Flags().Lookup("sandbox").Value.String(); v != "" {
		return v == agentdir.SandboxLocal
	}
	d, err := dir()
	if err != nil || !d.Exists() {
		return false
	}
	conf, err := d.ReadConfig()
	if err != nil {
		slog.Debug("reading the config for help", "error", err)
		return false
	}
	return conf.Sandbox == agentdir.SandboxLocal
}

func newRunCmd(cfg *globalConfig, sf *serverFlags) *cobra.Command {
	var mode runMode
	cmd := &cobra.Command{
		Use:   "run [<harness>|echo|proxy|exec]",
		Short: "Start the agent, or a local test server",
		Long: `Without an argument, start the agent saved in the nearest .a2a directory
(or --dir). On a first run in a terminal, it asks a few questions instead.

With a harness, create the agent in this directory on the first run, or apply
the flags to the existing agent like ` + "`a2a serve config set`" + `, then start it.

echo, proxy and exec run a local test server instead, without an agent
directory.`,
		Example: `  a2a serve run opencode --name coder
  a2a serve run
  a2a serve run --detach
  a2a serve run echo --port 9000`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConfiguredAgent(cmd, cfg, sf, mode)
		},
	}
	f := cmd.Flags()
	sf.addDirFlag(f)
	mode.addRunFlags(f)
	for _, name := range harness.ProfileNames() {
		cmd.AddCommand(newHarnessCmd(cfg, sf, name))
	}
	cmd.AddCommand(
		newHarnessCmd(cfg, sf, agentdir.HarnessACP),
		newEchoCmd(cfg),
		newProxyCmd(cfg),
		newExecCmd(cfg),
	)
	markTextOnly(cmd)
	return cmd
}
