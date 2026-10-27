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
	"io"

	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agentsrv"
	"github.com/a2aproject/a2a-serve/internal/clierr"
	"github.com/a2aproject/a2a-serve/internal/doctor"
	"github.com/a2aproject/a2a-serve/internal/launch"
	"github.com/a2aproject/a2a-serve/internal/service"
)

func newDoctorCmd(cfg *globalConfig, sf *serverFlags) *cobra.Command {
	var fix, online bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check the setup of this machine and of the agent, and fix what can be fixed locally",
		Long: `Check the a2a CLI and its config, the sandbox, and, inside an agent
directory or with --dir, the agent's config, secrets, server and service.
Every failure names the command which fixes it. Exits 1 if a check failed.

--fix enables command plugins and the credential helper in
~/.config/a2a-cli/config.yaml and makes credentials.json and .a2a/state
private. It never installs software and never touches a running agent.
--online also checks bot tokens and whether the harness image can be pulled.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := doctorEnv(cfg)
			if err != nil {
				return err
			}
			opts := doctor.Options{Online: online}
			d, err := sf.agentDir()
			switch {
			case err == nil:
				opts.Dir = &d
			case !errors.Is(err, agentdir.ErrNotFound) || sf.dir != "":
				return err
			}
			results := doctor.Run(cmd.Context(), env, opts)
			if fix {
				out := cfg.Out
				if cfg.IsJSON() {
					out = cfg.stderr()
				}
				applied, err := applyFixes(out, results)
				if err != nil {
					return err
				}
				if applied {
					results = doctor.Run(cmd.Context(), env, opts)
				}
			}
			if cfg.IsJSON() {
				err = cfg.PrintJSON(results)
			} else {
				err = doctor.Render(cfg.Out, results)
			}
			if err != nil {
				return err
			}
			if doctor.Failed(results) {
				return clierr.Exit(1, "a check failed")
			}
			return nil
		},
	}
	sf.addDirFlag(cmd.Flags())
	cmd.Flags().BoolVar(&fix, "fix", false, "Apply the local fixes and check again")
	cmd.Flags().BoolVar(&online, "online", false, "Also run checks which call external services")
	return cmd
}

func doctorEnv(cfg *globalConfig) (doctor.Env, error) {
	env, err := doctor.System()
	if err != nil {
		return doctor.Env{}, err
	}
	if cfg.credentials != nil {
		env.Credentials = cfg.credentials
	}
	env.ServiceUnit = func(d agentdir.Dir) (string, error) {
		name := launch.ServiceName(d, nil)
		if !service.Installed(name) {
			return "", nil
		}
		return service.UnitFile(name)
	}
	env.ValidateChannel = agentsrv.ValidateChannel
	return env, nil
}

// applyFixes applies the fixable results, printing each fix to out, and
// reports whether any was applied.
func applyFixes(out io.Writer, results []doctor.Result) (bool, error) {
	applied := false
	for _, r := range results {
		if !r.Fixable() {
			continue
		}
		msg, err := r.Apply()
		line := "✓ fixed: " + msg
		if err != nil {
			line = fmt.Sprintf("✗ fixing %s: %v", r.ID, err)
		} else {
			applied = true
		}
		if _, err := fmt.Fprintln(out, line); err != nil {
			return applied, err
		}
	}
	return applied, nil
}
