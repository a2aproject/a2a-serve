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
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/clierr"
	"github.com/a2aproject/a2a-serve/internal/doctor"
	"github.com/a2aproject/a2a-serve/internal/output"
	"github.com/a2aproject/a2a-serve/internal/relaydir"
)

func newDoctorCmd(cfg *globalConfig, sf *serverFlags) *cobra.Command {
	var fix, online bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check the setup of this machine and of the agent, and fix what can be fixed locally",
		Long: `Check the a2a CLI and its config, the sandbox, and, inside an agent
directory or with --dir, the agent's config, secrets, server and service.
In a relay directory, check only the relay's settings, state, agentmesh-one
and data. Every failure names the command that fixes it. Exits 1 if a
check failed.

The egress check tells whether the Docker sandbox's connections go through
the server. While the agent runs, it reports the server's own check, which
runs a container at each start and turns egress control off when the
container can't set up its network.

--fix enables command plugins and the credential helper in
~/.config/a2a-cli/config.yaml and makes credentials.json and .a2a/state
private. It never installs software and never touches a running agent.
--online also checks bot tokens, whether the agent image can be pulled,
that the speech service of --voice transcribes and speaks, and that the
relay answers at its URL with the Agent Mesh version this a2a-serve pins.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := doctorEnv(cfg)
			if err != nil {
				return err
			}
			run, err := doctorRun(env, sf, online)
			if err != nil {
				return err
			}
			results := run(cmd.Context())
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
					results = run(cmd.Context())
				}
			}
			if cfg.IsJSON() {
				err = output.PrintList(cfg.Printer, results)
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
	cmd.Flags().BoolVar(&online, "online", false, "Also run checks that call external services")
	return cmd
}

// doctorRun returns the checks of the agent of --dir or the cwd, of the
// relay there if there is no agent, or of the machine only.
func doctorRun(env doctor.Env, sf *serverFlags, online bool) (func(context.Context) []doctor.Result, error) {
	d, err := sf.agentDir()
	if err == nil {
		return func(ctx context.Context) []doctor.Result {
			return doctor.Run(ctx, env, doctor.Options{Dir: &d, Online: online})
		}, nil
	}
	if !errors.Is(err, agentdir.ErrNotFound) {
		return nil, err
	}
	rf := relayFlags{dir: sf.dir}
	rd, rerr := rf.relayDir()
	switch {
	case rerr == nil:
		// A relay.yaml that doesn't parse fails its own check.
		b := relayBackends[0]
		if conf, err := rd.ReadConfig(); err == nil {
			b = relayBackendOf(conf)
		}
		bin, err := b.binaryPath()
		if err != nil {
			return nil, err
		}
		opts := doctor.RelayOptions{Dir: rd, Binary: bin, Program: b.release.Program, Version: b.version, CheckBinary: b.check, Online: online}
		return func(ctx context.Context) []doctor.Result {
			return doctor.RunRelay(ctx, env, opts)
		}, nil
	case sf.dir != "":
		return nil, err
	case !errors.Is(rerr, relaydir.ErrNotFound):
		return nil, rerr
	}
	return func(ctx context.Context) []doctor.Result {
		return doctor.Run(ctx, env, doctor.Options{Online: online})
	}, nil
}

func doctorEnv(cfg *globalConfig) (doctor.Env, error) {
	env, err := doctor.System()
	if err != nil {
		return doctor.Env{}, err
	}
	if cfg.credentials != nil {
		env.Credentials = cfg.credentials
	}
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
