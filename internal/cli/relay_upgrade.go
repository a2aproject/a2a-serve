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
	"time"

	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/relay/control"
	"github.com/a2aproject/a2a-serve/internal/relaydir"
)

func newRelayUpgradeCmd(cfg *globalConfig, f *relayFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Upgrade the local relay to the backend version this a2a-serve pins",
		Long: `Upgrade the local relay to the version of its backend this a2a-serve pins.
Run it after you install a newer a2a-serve. It downloads the new
agentmesh-one or slimctl, then restarts a running relay, or upgrades the data
of a stopped one.

A newer Agent Mesh migrates the relay's database, and an older one can't use
it again, so the upgrade backs up a SQLite database first and keeps the last
three backups. Back up a PostgreSQL database yourself before you upgrade. A
relay elsewhere upgrades when it runs the new a2a-serve.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := f.relayDir()
			if err != nil {
				return err
			}
			conf, err := d.ReadConfig()
			if err != nil {
				return err
			}
			b := relayBackendOf(conf)
			if _, err := b.binary(cmd.Context(), cfg); err != nil {
				return err
			}
			c := control.NewSocketClient(d.SocketPath(), f.timeout)
			st, err := c.Status(cmd.Context())
			if err == nil {
				return restartRelay(cmd, cfg, c, st, b, f.timeout)
			}
			if !isRelayNotRunning(err) {
				return err
			}
			return upgradeStoppedRelay(cmd, cfg, d, b)
		},
	}
	cmd.Flags().StringVar(&f.dir, "dir", "", relayDirUsage)
	cmd.Flags().DurationVar(&f.timeout, "timeout", 3*time.Minute, "How long to wait for the relay to restart, a `duration` such as 30s or 2m")
	return cmd
}

func restartRelay(cmd *cobra.Command, cfg *globalConfig, c *control.Client, before *control.Status, b relayBackend, timeout time.Duration) error {
	want := b.pinned()
	if before.Versions == want {
		_, err := fmt.Fprintf(cfg.Out, "✓ the relay already runs a2a-serve %s with %s %s\n", want.A2AServe, b.name, b.version)
		return err
	}
	if err := c.Restart(cmd.Context()); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(cfg.progress(), "… restarting the relay"); err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	for {
		select {
		case <-cmd.Context().Done():
			return cmd.Context().Err()
		case <-time.After(500 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the relay didn't come back within %v; see its log", timeout)
		}
		st, err := c.Status(cmd.Context())
		if err != nil {
			if isRelayNotRunning(err) {
				continue
			}
			return err
		}
		if st.PID == before.PID && st.Started.Equal(before.Started) {
			continue
		}
		if st.Versions != want {
			name, version := st.Versions.BackendOrDefault()
			return fmt.Errorf("the relay restarted with a2a-serve %s and %s %s, and this a2a-serve is %s with %s; the relay runs the a2a-serve it was started with, which wasn't replaced", st.Versions.A2AServe, name, version, want.A2AServe, b.version)
		}
		if cfg.IsJSON() {
			return cfg.PrintJSON(st.Versions)
		}
		_, err = fmt.Fprintf(cfg.Out, "✓ the relay runs a2a-serve %s with %s %s\n", st.Versions.A2AServe, b.name, b.version)
		return err
	}
}

func upgradeStoppedRelay(cmd *cobra.Command, cfg *globalConfig, d relaydir.Dir, b relayBackend) (err error) {
	release, err := d.Lock()
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, release()) }()
	conf, err := d.Resolve()
	if err != nil {
		return err
	}
	res, err := b.newFunc("", io.Discard).Prepare(cmd.Context(), d, conf, time.Now())
	if err != nil {
		return err
	}
	if cfg.IsJSON() {
		return cfg.PrintJSON(res)
	}
	msg := fmt.Sprintf("✓ the relay's data is ready for %s %s; start it with `a2a-serve relay run`\n", b.name, res.To)
	if res.Backup != "" {
		msg += fmt.Sprintf("  backup of the relay's database: %s\n", res.Backup)
	}
	_, err = fmt.Fprint(cfg.Out, msg)
	return err
}
