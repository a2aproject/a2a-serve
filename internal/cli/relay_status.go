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
	"time"

	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/flagparse"
	"github.com/a2aproject/a2a-serve/internal/relay/control"
	"github.com/a2aproject/a2a-serve/internal/relay/meshbackend"
	"github.com/a2aproject/a2a-serve/internal/relaydir"
)

// relayStatus is the output of `relay status -o json`.
type relayStatus struct {
	URL     string    `json:"url"`
	Dir     string    `json:"dir,omitempty"`
	Running bool      `json:"running"`
	PID     int       `json:"pid,omitempty"`
	Started time.Time `json:"started,omitzero"`
	// Versions are those the running relay runs, and Pinned those of this
	// a2a-serve.
	Versions *control.Versions `json:"versions,omitempty"`
	Pinned   control.Versions  `json:"pinned"`
	// DataVersion is the Agent Mesh version that last ran on a local
	// relay's data.
	DataVersion string    `json:"dataVersion,omitempty"`
	Join        string    `json:"join,omitempty"`
	Members     *int      `json:"members,omitempty"`
	LastBackup  time.Time `json:"lastBackup,omitzero"`
}

func newRelayStatusCmd(cfg *globalConfig, f *relayFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the relay's address, versions and members",
		Long: `Show the relay's address, whether it runs, the versions of a2a-serve and
backend it runs and this a2a-serve pins, who can join, how many agents are
on it, and its last backup before an upgrade. A stopped local relay shows
what its relay directory says.

` + flagparse.SecretHelp,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := f.client(cfg)
			if err != nil {
				return err
			}
			st := relayStatus{Pinned: relayBackends[0].pinned()}
			live, err := c.Status(cmd.Context())
			switch {
			case err == nil:
				st.URL, st.Running, st.PID, st.Started = live.URL, true, live.PID, live.Started
				st.Versions, st.Join, st.Members = &live.Versions, live.Join, &live.Members
				st.Pinned = relayBackendNamed(live.Versions).pinned()
			case !isRelayNotRunning(err):
				return err
			}
			if f.url == "" {
				d, err := f.relayDir()
				if err != nil {
					return err
				}
				if err := localRelayStatus(d, &st); err != nil {
					return err
				}
			}
			if cfg.IsJSON() {
				return cfg.PrintJSON(st)
			}
			return printRelayStatus(cfg, st)
		},
	}
	f.addTargetFlags(cmd.Flags())
	return cmd
}

func localRelayStatus(d relaydir.Dir, st *relayStatus) error {
	st.Dir = d.Root
	conf, err := d.ReadConfig()
	if err != nil {
		return err
	}
	b := relayBackendOf(conf)
	st.Pinned = b.pinned()
	if b.id != relaydir.BackendAgentMesh {
		return nil
	}
	if st.URL == "" {
		st.URL = conf.URL
		if conf.Tunnel != "" {
			st.URL = "(a new " + conf.Tunnel + " quick tunnel URL on every start)"
		}
	}
	if st.DataVersion, err = d.RecordedVersion(); err != nil {
		return err
	}
	_, st.LastBackup, err = meshbackend.LastBackup(d)
	return err
}

func printRelayStatus(cfg *globalConfig, st relayStatus) error {
	var b strings.Builder
	if st.Running {
		fmt.Fprintf(&b, "relay %s running for %s, pid %d\n", st.URL, time.Since(st.Started).Round(time.Second), st.PID)
	} else {
		fmt.Fprintf(&b, "relay %s is not running; start it with `a2a-serve relay run`\n", st.URL)
	}
	if st.Dir != "" {
		fmt.Fprintf(&b, "  dir       %s\n", st.Dir)
	}
	pinnedName, pinnedVersion := st.Pinned.BackendOrDefault()
	if st.Versions != nil {
		name, version := st.Versions.BackendOrDefault()
		fmt.Fprintf(&b, "  runs      a2a-serve %s, %s %s\n", st.Versions.A2AServe, name, version)
	}
	if st.Versions == nil || *st.Versions != st.Pinned {
		fmt.Fprintf(&b, "  this      a2a-serve %s, %s %s\n", st.Pinned.A2AServe, pinnedName, pinnedVersion)
	}
	if st.Versions != nil && st.Dir != "" {
		if _, version := st.Versions.BackendOrDefault(); version != pinnedVersion {
			fmt.Fprintf(&b, "  → upgrade the relay to this a2a-serve's %s: a2a-serve relay upgrade\n", pinnedName)
		}
	}
	if !st.Running && st.DataVersion != "" {
		fmt.Fprintf(&b, "  data      last used by Agent Mesh %s\n", st.DataVersion)
	}
	if st.Join != "" {
		fmt.Fprintf(&b, "  join      %s\n", st.Join)
	}
	if st.Members != nil {
		fmt.Fprintf(&b, "  members   %d\n", *st.Members)
	}
	if !st.LastBackup.IsZero() {
		fmt.Fprintf(&b, "  backup    %s, before the last upgrade\n", formatTime(st.LastBackup))
	}
	_, err := fmt.Fprint(cfg.Out, b.String())
	return err
}
