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
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/a2aproject/a2a-serve/internal/clierr"
	"github.com/a2aproject/a2a-serve/internal/flagparse"
	"github.com/a2aproject/a2a-serve/internal/relay/control"
	"github.com/a2aproject/a2a-serve/internal/relaydir"
)

func newRelayCmd(cfg *globalConfig) *cobra.Command {
	var f relayFlags
	cmd := &cobra.Command{
		Use:   "relay",
		Short: "Run an Agent Mesh relay that agents join from anywhere",
		Long: `Run an Agent Mesh relay: agents on laptops and in hosted sandboxes connect
out to it, call each other by name, and get public URLs on it. Everyone the
relay admits can call everyone else it admits.

A relay lives in a relay directory, like an agent: settings in
.a2a/relay.yaml, state in .a2a/state/relay. The other relay commands manage
the relay of the nearest relay directory (or --dir) through its control
socket, or a relay elsewhere with --url and its admin token.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(
		newRelayRunCmd(cfg, &f),
		newRelayStatusCmd(cfg, &f),
		newRelayInviteCmd(cfg, &f),
		newRelayMembersCmd(cfg, &f),
		newRelayRemoveCmd(cfg, &f),
		newRelayUpgradeCmd(cfg, &f),
	)
	return cmd
}

// relayFlags select the relay a command manages.
type relayFlags struct {
	dir        string
	url        string
	adminToken string
	timeout    time.Duration
}

const relayDirUsage = "Relay directory (default: the nearest directory with .a2a/relay.yaml, walking up from the current one)"

// addTargetFlags adds the flags of a command that calls a running relay.
func (f *relayFlags) addTargetFlags(fs *pflag.FlagSet) {
	fs.StringVar(&f.dir, "dir", "", relayDirUsage)
	fs.StringVar(&f.url, "url", "", "Manage the relay at this URL, with --admin-token")
	fs.StringVar(&f.adminToken, "admin-token", "", "The admin token of the relay at --url, as a "+flagparse.SecretSyntax)
	fs.DurationVar(&f.timeout, "timeout", time.Minute, "Timeout of each call to the relay, a `duration` such as 30s or 2m")
}

// relayDir returns the relay of --dir, or the nearest one up from the cwd.
func (f *relayFlags) relayDir() (relaydir.Dir, error) {
	if f.dir != "" {
		d, err := relaydir.Open(f.dir)
		if err != nil {
			return relaydir.Dir{}, err
		}
		if !d.Exists() {
			return relaydir.Dir{}, relaydir.NotFoundIn(d.Root)
		}
		return d, nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return relaydir.Dir{}, err
	}
	return relaydir.Find(wd)
}

// client returns a client of the relay at --url, or of the local relay
// through its control socket.
func (f *relayFlags) client(cfg *globalConfig) (*control.Client, error) {
	if f.url == "" {
		if f.adminToken != "" {
			return nil, clierr.Usage("--admin-token goes with --url; a local relay is managed through its control socket")
		}
		d, err := f.relayDir()
		if err != nil {
			return nil, err
		}
		return control.NewSocketClient(d.SocketPath(), f.timeout), nil
	}
	if f.dir != "" {
		return nil, clierr.Usage("pass --url or --dir, not both")
	}
	if err := relaydir.CheckURL(f.url); err != nil {
		return nil, err
	}
	if f.adminToken == "" {
		return nil, clierr.Usage("--url needs --admin-token, the relay's admin token")
	}
	token, err := flagparse.ResolveSecret("admin-token", f.adminToken, cfg.secretReader(cfg.stderr()))
	if err != nil {
		return nil, err
	}
	return control.NewRemoteClient(f.url, token, f.timeout, cfg.httpTransport), nil
}

// isRelayNotRunning reports whether err means the local relay isn't running.
func isRelayNotRunning(err error) bool {
	return errors.Is(err, control.ErrNotRunning)
}
