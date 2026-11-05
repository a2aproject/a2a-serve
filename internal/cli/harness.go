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
	"github.com/a2aproject/a2a-serve/internal/clierr"
	"github.com/a2aproject/a2a-serve/internal/flagparse"
	"github.com/a2aproject/a2a-serve/internal/output"
)

// newHarnessCmd builds `a2a-serve start <agent>`, which creates the agent on
// the first run and starts it. In an existing agent the flags go through
// the `config set` path first. "acp" starts any ACP agent given with
// --command.
func newHarnessCmd(cfg *globalConfig, sf *serverFlags, name string) *cobra.Command {
	var (
		flags settingsFlags
		mode  runMode
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
subdirectory of an agent, pass --dir to create a separate agent.

` + flagparse.SecretHelp,
		Example: fmt.Sprintf(`  a2a-serve start %[1]s --name coder --secret OPENROUTER_API_KEY
  a2a-serve start %[1]s --private tailscale --allow-tools read,search --detach
  a2a-serve start %[1]s --workspace ~/src/app --mount ~/docs:/docs:ro`, name),
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
			if !res.Created {
				if err := printConfigResult(cfg, res); err != nil {
					return err
				}
			}
			if res.Running {
				_, err := fmt.Fprintf(cfg.Out, "✓ %s is already running (a2a-serve status)\n", res.Name)
				return err
			}
			mode.created = res.Created
			return runAgent(cmd.Context(), cfg, d, mode)
		},
	}
	flags.register(cmd, agentdir.CoreFlags)
	f := cmd.Flags()
	f.StringVar(&sf.dir, "dir", "", "Agent directory (default: the current directory)")
	mode.addRunFlags(f)
	setSection(f, sectionRun, "dir")
	cmd.SetUsageFunc(sectionedUsage)
	hideImageForLocalSandbox(cmd, func() (agentdir.Dir, error) { return harnessDir(sf.dir) })
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
	parent, err := agentdir.Find(filepath.Dir(d.Root))
	if errors.Is(err, agentdir.ErrNotFound) {
		return d, nil
	}
	if err != nil {
		return agentdir.Dir{}, err
	}
	return agentdir.Dir{}, clierr.Usage(fmt.Sprintf("%s is inside the agent in %s; start that agent with `a2a-serve start`, or pass --dir . to create a separate agent here", d.Root, parent.Root))
}
