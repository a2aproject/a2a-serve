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
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/backup"
	"github.com/a2aproject/a2a-serve/internal/output"
)

func newBackupCmd(cfg *globalConfig, sf *serverFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Take and list encrypted backup snapshots of the agent",
		Long: `Snapshots hold a consistent copy of the database (conversations, pairings,
contacts, schedules), config.yaml, the secrets, the harness home and the
network identity, encrypted with --backup-key. A running agent takes one every
--backup-interval and keeps them for --backup-retention, always keeping the
latest. The workspace isn't included: it is usually in git.

Set them up with:

  a2a serve config set --backup gs://my-bucket/agents/coder --backup-key env:BACKUP_KEY --backup-interval 1h

Restore an agent on another host with "a2a serve restore".`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	sf.addDirFlag(cmd.PersistentFlags())
	cmd.PersistentFlags().DurationVar(&sf.timeout, "timeout", 10*time.Minute, "Timeout of the command, which writes to or lists the backup destination")
	cmd.AddCommand(
		newBackupNowCmd(cfg, sf),
		newBackupListCmd(cfg, sf),
	)
	return cmd
}

// agentBackups returns the agent of the current directory and the
// destination and settings of its backups.
func agentBackups(sf *serverFlags) (agentdir.Dir, backup.Dest, backup.Config, error) {
	d, err := sf.agentDir()
	if err != nil {
		return agentdir.Dir{}, nil, backup.Config{}, err
	}
	conf, err := d.LoadConfig()
	if err != nil {
		return agentdir.Dir{}, nil, backup.Config{}, err
	}
	secrets, err := d.LoadSecrets()
	if err != nil {
		return agentdir.Dir{}, nil, backup.Config{}, err
	}
	dest, bcfg, err := backup.ForAgent(conf, secrets, nil)
	return d, dest, bcfg, err
}

func newBackupNowCmd(cfg *globalConfig, sf *serverFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "now",
		Short: "Take a snapshot now",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			d, dest, bcfg, err := agentBackups(sf)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), sf.timeout)
			defer cancel()
			snap, err := backup.Take(ctx, d, dest, bcfg, time.Now())
			if err != nil {
				return err
			}
			if cfg.IsJSON() {
				return cfg.PrintJSON(snap)
			}
			_, err = fmt.Fprintf(cfg.Out, "✓ took snapshot %s\n", snap.Name)
			return err
		},
	}
}

func newBackupListCmd(cfg *globalConfig, sf *serverFlags) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the agent's snapshots, oldest first",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, dest, bcfg, err := agentBackups(sf)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), sf.timeout)
			defer cancel()
			snaps, err := backup.List(ctx, dest, bcfg.Agent)
			if err != nil {
				return err
			}
			if cfg.IsJSON() {
				return output.PrintList(cfg.Printer, snaps)
			}
			rows := make([][]string, 0, len(snaps))
			for _, s := range snaps {
				rows = append(rows, []string{s.Name, s.Taken.Local().Format("2006-01-02 15:04 MST")})
			}
			return cfg.PrintTableOrEmpty([]string{"SNAPSHOT", "TAKEN"}, rows, "no snapshots yet; take one with `a2a serve backup now`")
		},
	}
}
