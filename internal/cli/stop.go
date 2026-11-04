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
	"strings"

	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/agentctl"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/service"
)

func newStopCmd(cfg *globalConfig, sf *serverFlags) *cobra.Command {
	var uninstall bool
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop the agent",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := sf.agentDir()
			if err != nil {
				return err
			}
			conf, err := d.LoadConfig()
			if err != nil {
				return err
			}
			client := sf.controlClient(d)
			_, err = client.Status(cmd.Context())
			running := err == nil
			if err != nil && !errors.Is(err, controlapi.ErrNotRunning) {
				return err
			}
			name := d.ServiceName()
			installed := service.Installed(name)
			if installed && uninstall {
				if err := service.Uninstall(name); err != nil {
					return err
				}
			}
			// A detached agent, or one the service manager didn't stop.
			err = client.Stop(cmd.Context())
			switch {
			case errors.Is(err, controlapi.ErrNotRunning):
			case err != nil:
				return err
			default:
				if err := agentctl.WaitStopped(cmd.Context(), d); err != nil {
					return err
				}
			}
			if cfg.IsJSON() {
				return cfg.PrintJSON(stopResult{Name: conf.Name, Stopped: running, ServiceInstalled: installed && !uninstall, ServiceRemoved: installed && uninstall})
			}
			var sb strings.Builder
			if running {
				fmt.Fprintf(&sb, "✓ stopped %s\n", conf.Name)
			} else {
				fmt.Fprintf(&sb, "%s is not running\n", conf.Name)
			}
			switch {
			case uninstall && installed:
				fmt.Fprintf(&sb, "✓ removed the %s service\n", conf.Name)
			case uninstall:
				sb.WriteString("  no service was installed\n")
			case installed:
				sb.WriteString("  it starts again at login; remove the service with `a2a-serve stop --uninstall`\n")
			}
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
}
