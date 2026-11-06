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
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/lifecycle"
)

func newStopCmd(cfg *globalConfig, sf *serverFlags) *cobra.Command {
	var uninstall bool
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop the agent",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := sf.serverDir()
			if err != nil {
				return err
			}
			res, err := lifecycle.Stop(cmd.Context(), d, sf.controlClient(d), lifecycle.StopOptions{Wait: lifecycle.WaitStopped, Uninstall: uninstall})
			if err != nil {
				return err
			}
			name := res.Name
			if name == "" {
				name = d.NameOrDefault()
			}
			problem := configProblem(d)
			if cfg.IsJSON() {
				return cfg.PrintJSON(stopResult{Name: name, Stopped: res.Running, ServiceInstalled: res.ServiceInstalled && !res.ServiceRemoved, ServiceRemoved: res.ServiceRemoved, ConfigError: problem})
			}
			var sb strings.Builder
			if res.Running {
				fmt.Fprintf(&sb, "✓ stopped %s\n", name)
			} else {
				fmt.Fprintf(&sb, "%s is not running\n", name)
			}
			switch {
			case res.ServiceRemoved:
				fmt.Fprintf(&sb, "✓ removed the %s service\n", name)
			case uninstall:
				sb.WriteString("  no service was installed\n")
			case res.ServiceInstalled:
				sb.WriteString("  it starts again at login; remove the service with `a2a-serve stop --uninstall`\n")
			}
			writeConfigProblem(&sb, problem)
			_, err = fmt.Fprint(cfg.Out, sb.String())
			return err
		},
	}
	cmd.Flags().BoolVar(&uninstall, "uninstall", false, "Also remove the launchd or systemd service")
	sf.addControlFlags(cmd.Flags())
	return cmd
}

// stopResult is the JSON output of stop.
type stopResult struct {
	Name string `json:"name"`
	// Stopped is false if the agent wasn't running.
	Stopped bool `json:"stopped"`
	// ServiceInstalled is true if the agent starts again at login.
	ServiceInstalled bool `json:"serviceInstalled"`
	ServiceRemoved   bool `json:"serviceRemoved"`
	// ConfigError is why config.yaml doesn't load, if it doesn't.
	ConfigError string `json:"configError,omitempty"`
}

// configProblem describes why config.yaml of d doesn't load, or returns ""
// if it does.
func configProblem(d agentdir.Dir) string {
	if !d.Exists() {
		return d.ConfigPath() + " does not exist"
	}
	if _, err := d.LoadConfig(); err != nil {
		return strings.ReplaceAll(err.Error(), "\n", "; ")
	}
	return ""
}

func writeConfigProblem(sb *strings.Builder, problem string) {
	if problem != "" {
		fmt.Fprintf(sb, "! the config is invalid: %s\n", problem)
	}
}
