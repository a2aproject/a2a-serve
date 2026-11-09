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
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/agentctl"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/backup"
	"github.com/a2aproject/a2a-serve/internal/clierr"
	"github.com/a2aproject/a2a-serve/internal/flagparse"
)

func newBackupRestoreCmd(cfg *globalConfig, sf *serverFlags) *cobra.Command {
	var (
		snapshot, key string
		takeOver      bool
	)
	cmd := &cobra.Command{
		Use:   "restore <backup-destination>",
		Short: "Recreate an agent from a backup snapshot",
		Long: `Recreate an agent's .a2a directory from a snapshot taken by
"a2a-serve backup now" or the running agent: its database, config, secrets,
recipe.lock and agent home directory. The latest snapshot is used unless
--snapshot names one.

The directory must have no .a2a, or a .a2a that holds only recipe.lock or
.gitignore, such as those a repository commits. Restore keeps those files.

The network identity (the tailnet machine or the Agent Mesh node) is restored
only with --take-over, so the old and the new host don't run as the same
machine. Without it, the agent joins the network as a new machine on its
first start. Stop the agent
on the old host before taking over.

` + flagparse.SecretHelp,
		Example: "  a2a-serve backup restore gs://my-bucket/agents/coder --dir ~/src/app --backup-key env:BACKUP_KEY",
		Args:    namedArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), sf.deadline)
			defer cancel()
			dir := sf.dir
			if dir == "" {
				wd, err := os.Getwd()
				if err != nil {
					return err
				}
				dir = wd
			}
			if key == "" {
				return clierr.Usage("--backup-key is required: the snapshots are encrypted with it")
			}
			d, err := agentdir.Open(dir)
			if err != nil {
				return err
			}
			if err := agentctl.CheckEnclosing(ctx, d); err != nil {
				return err
			}
			passphrase, err := flagparse.ResolveSecret("backup-key", key, cfg.secretReader(cfg.stderr()))
			if err != nil {
				return err
			}
			// Installed after the key prompt, which Ctrl-C still interrupts.
			ctx, stop := signalContext(ctx)
			defer stop()
			dest, err := backup.Open(args[0], nil)
			if err != nil {
				return clierr.Usage(err.Error())
			}
			if snapshot == "" {
				latest, err := backup.Latest(ctx, dest)
				if err != nil {
					return deadlineError(fmt.Errorf("%s: %w", args[0], err))
				}
				snapshot = latest.Name
			}
			notes, err := backup.Restore(ctx, dest, snapshot, passphrase, dir, takeOver)
			if err != nil {
				return deadlineError(err)
			}
			if cfg.IsJSON() {
				return cfg.PrintJSON(map[string]any{"snapshot": snapshot, "dir": dir, "takeOver": takeOver, "notes": notes})
			}
			var sb strings.Builder
			fmt.Fprintf(&sb, "✓ restored %s into %s\n  check it with `a2a-serve doctor`, then start it with `a2a-serve start`\n", snapshot, dir)
			for _, note := range notes {
				fmt.Fprintf(&sb, "! %s\n", note)
			}
			_, err = fmt.Fprint(cfg.Out, sb.String())
			return err
		},
	}
	f := cmd.Flags()
	f.StringVar(&sf.dir, "dir", "", "Directory of the restored agent (default: the current directory)")
	f.StringVar(&snapshot, "snapshot", "", "Snapshot to restore (default: the latest)")
	f.StringVar(&key, "backup-key", "", secretUsage("The backup key"))
	f.BoolVar(&takeOver, "take-over", false, "Also restore the network identity; stop the agent on the old host first")
	return cmd
}
