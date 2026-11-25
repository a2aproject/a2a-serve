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
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/agentctl"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agentprofile"
	"github.com/a2aproject/a2a-serve/internal/clierr"
	"github.com/a2aproject/a2a-serve/internal/flagparse"
	"github.com/a2aproject/a2a-serve/internal/lifecycle"
	"github.com/a2aproject/a2a-serve/internal/output"
	"github.com/a2aproject/a2a-serve/internal/service"
)

// newHarnessCmd builds `a2a-serve start <agent>`, which creates the agent on
// the first run and starts it. In an existing agent the flags go through
// the `config set` path first. "acp" starts any ACP agent given with
// --command.
func newHarnessCmd(cfg *globalConfig, sf *serverFlags, name string) *cobra.Command {
	var (
		flags settingsFlags
		mode  runMode
		yes   bool
	)
	short := fmt.Sprintf("Start %s agent in this directory", output.WithArticle(name))
	if name == agentdir.HarnessACP {
		short = "Start any ACP agent (--command) in this directory"
	}
	cmd := &cobra.Command{
		Use:   name,
		Short: short,
		Long: short + `. The first run saves the settings to .a2a/config.yaml; in an
existing agent, the flags change them like ` + "`a2a-serve config set`" + `. In a
subdirectory of an agent, pass --dir to create a separate agent while that
agent is stopped. The Docker sandbox refuses your home directory or / as the
workspace unless --allow-broad-mount names it.

` + flagparse.SecretHelp,
		Example: fmt.Sprintf(`  a2a-serve start %[1]s --name coder --secret %[2]s
  a2a-serve start %[1]s --private tailscale --allow-tools read,search --detach
  a2a-serve start %[1]s --workspace ~/src/app --mount ~/docs:/docs:ro`, name, exampleKey(name)),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := harnessDir(sf.dir)
			if err != nil {
				return err
			}
			s, err := flags.settings(cfg.secretReader(cfg.stderr()))
			if err != nil {
				return err
			}
			if s.NetworkProvider, err = installNetworkProvider(cmd.Context(), cfg, s.NetworkProvider, yes); err != nil {
				return err
			}
			if d.Exists() {
				conf, err := d.ReadConfig()
				if err != nil {
					return err
				}
				if err := conf.CheckHarness(name); err != nil {
					return err
				}
			}
			s.Harness = name
			res, err := agentctl.Set(cmd.Context(), d, sf.controlClient(d), s, false)
			if err != nil {
				return err
			}
			if res.Running {
				if mode.service {
					if err := lifecycle.CheckServiceStart(d, service.System{}, res.Name); err != nil {
						if perr := printConfigResult(cfg, res, restartHint(cmd.Context(), sf, d)); perr != nil {
							return perr
						}
						return err
					}
				}
				return printStartRunning(cfg, res, restartHint(cmd.Context(), sf, d))
			}
			nameNote := agentdir.NameNote(s.Name)
			if !res.Created {
				// runAgent prints the warnings of the config with the agent
				// line; the note about the name given is this command's.
				shown := *res
				if !cfg.IsJSON() {
					shown.Warnings = nil
					if nameNote != "" {
						shown.Warnings = []string{nameNote}
					}
				}
				if err := printConfigResult(cfg, &shown, ""); err != nil {
					return err
				}
			} else if nameNote != "" {
				if err := printWarnings(cfg, []string{nameNote}); err != nil {
					return err
				}
			}
			mode.created = res.Created
			return runAgent(cmd.Context(), cfg, d, mode)
		},
	}
	flags.register(cmd, agentdir.CoreFlags)
	f := cmd.Flags()
	f.StringVar(&sf.dir, "dir", "", "Agent directory (default: the current directory)")
	mode.addRunFlags(f)
	addYesFlag(f, &yes)
	setSection(f, sectionRun, "dir", "yes")
	cmd.SetUsageFunc(sectionedUsage)
	hideImageWithoutDocker(cmd, func() (agentdir.Dir, error) { return harnessDir(sf.dir) })
	return cmd
}

// harnessDir is --dir, or the current directory: a first run creates the
// agent where it is started.
func harnessDir(dir string) (agentdir.Dir, error) {
	wd, err := os.Getwd()
	if err != nil {
		return agentdir.Dir{}, err
	}
	return harnessDirFrom(wd, dir)
}

// harnessDirFrom is harnessDir run in wd. Without --dir it refuses a
// directory inside another agent, where every other command finds that
// agent.
func harnessDirFrom(wd, dir string) (agentdir.Dir, error) {
	if dir != "" {
		return agentdir.Open(dir)
	}
	d, err := agentdir.Open(wd)
	if err != nil || d.Exists() {
		return d, err
	}
	parent, err := agentdir.FindEnclosing(filepath.Dir(d.Root))
	if errors.Is(err, agentdir.ErrNotFound) {
		return d, nil
	}
	if err != nil {
		return agentdir.Dir{}, err
	}
	return agentdir.Dir{}, clierr.Usage(fmt.Sprintf("%s is inside the agent in %s; start that agent with `a2a-serve start`, or pass --dir . to create a separate agent here", d.Root, parent.Root))
}

// exampleKey is the secret that the examples of the start command of
// harness pass: the key of its model provider.
func exampleKey(harness string) string {
	switch harness {
	case "claude":
		return agentprofile.ClaudeKeys()[0]
	case "copilot":
		return agentprofile.CopilotKeys()[0]
	case "agy":
		return agentprofile.AgyKeys()[0]
	}
	return "OPENROUTER_API_KEY"
}
