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
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/agentctl"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
)

func newStatusCmd(cfg *globalConfig, sf *serverFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show whether the agent runs, its URLs and channels",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := sf.agentDir()
			if err != nil {
				return err
			}
			st, err := sf.controlClient(d).Status(cmd.Context())
			if errors.Is(err, controlapi.ErrNotRunning) {
				conf, cerr := d.LoadConfig()
				if cerr != nil {
					return cerr
				}
				if cfg.IsJSON() {
					return cfg.PrintJSON(statusJSON{Name: conf.Name, Harness: conf.Harness, Dir: d.Root})
				}
				_, err := fmt.Fprintf(cfg.Out, "%s (%s) in %s is not running; start it with `a2a serve --detach`\n", conf.Name, conf.Harness, d.Root)
				return err
			}
			if err != nil {
				return err
			}
			pending, _, err := agentctl.Pending(cmd.Context(), d)
			if err != nil {
				return err
			}
			if cfg.IsJSON() {
				return cfg.PrintJSON(statusJSON{Name: st.Name, Harness: st.Harness, Dir: st.Dir, Running: true, Status: st, Pending: pending})
			}
			return printStatus(cfg, st, pending)
		},
	}
	sf.addControlFlags(cmd.Flags())
	return cmd
}

// statusJSON is the status of a running or stopped agent; the fields of
// [controlapi.Status] are only set while it runs.
type statusJSON struct {
	Name    string `json:"name"`
	Harness string `json:"harness"`
	Dir     string `json:"dir"`
	Running bool   `json:"running"`
	*controlapi.Status
	// Pending are the settings saved since the agent started.
	Pending []string `json:"pending,omitempty"`
}

func printStatus(cfg *globalConfig, st *controlapi.Status, pending []string) error {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s (%s) running for %s, pid %d\n", st.Name, st.Harness, time.Since(st.Started).Round(time.Second), st.PID)
	fmt.Fprintf(&sb, "  dir      %s\n  local    %s\n", st.Dir, st.Local)
	if st.Private != "" {
		fmt.Fprintf(&sb, "  private  %s\n", st.Private)
	}
	if st.Public != "" {
		fmt.Fprintf(&sb, "  public   %s\n", st.Public)
	}
	for _, c := range st.Channels {
		fmt.Fprintf(&sb, "  %-8s %s\n", c.Name, c.Account)
		if c.Error != "" {
			fmt.Fprintf(&sb, "  %-8s ! failing, retrying: %s\n", "", c.Error)
		}
	}
	if len(st.Owners) > 0 {
		fmt.Fprintf(&sb, "  owner    %s\n", strings.Join(st.Owners, ", "))
	}
	states := make([]string, 0, len(st.Tasks))
	for _, s := range slices.Sorted(maps.Keys(st.Tasks)) {
		states = append(states, fmt.Sprintf("%d %s", st.Tasks[s], s))
	}
	if len(states) > 0 {
		fmt.Fprintf(&sb, "  tasks    %s\n", strings.Join(states, ", "))
	}
	fmt.Fprintf(&sb, "  contacts %d, webhook nodes %d\n", st.Contacts, st.Nodes)
	if len(pending) > 0 {
		fmt.Fprintf(&sb, "  pending  %s (a2a serve restart)\n", strings.Join(pending, ", "))
	}
	_, err := fmt.Fprint(cfg.Out, sb.String())
	return err
}
