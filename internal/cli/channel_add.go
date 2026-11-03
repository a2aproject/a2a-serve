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
	"maps"
	"slices"

	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agentsrv"
	"github.com/a2aproject/a2a-serve/internal/clierr"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
)

func newChannelAddCmd(cfg *globalConfig, sf *serverFlags) *cobra.Command {
	var (
		token string
		email agentdir.EmailSettings
	)
	cmd := &cobra.Command{
		Use:   "add <telegram|discord|email>",
		Short: "Add a messenger, or replace its settings",
		Example: `  a2a serve channel add telegram --token env:CODER_BOT_TOKEN
  a2a serve channel add email --email-address coder@example.com --email-imap imap.example.com:993 \
    --email-smtp smtp.example.com:465 --email-password env:MAIL_PASSWORD --email-trusted-authserv mx.example.com`,
		Args:      cobra.ExactArgs(1),
		ValidArgs: agentdir.ChannelKinds,
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := sf.agentDir()
			if err != nil {
				return err
			}
			var s agentdir.Settings
			if !slices.Contains(agentdir.ChannelKinds, args[0]) {
				return clierr.Usage("unknown channel " + args[0] + ": want telegram, discord or email")
			}
			if args[0] != agentdir.ChannelEmail && token == "" {
				return clierr.Usage("--token is required")
			}
			if token == stdinSecret {
				v, err := cfg.secretReader(cfg.stderr()).read("--token")
				if err != nil {
					return err
				}
				if v == "" {
					return clierr.Usage("--token: empty secret")
				}
				token = v
			}
			switch args[0] {
			case agentdir.ChannelTelegram:
				s.Telegram = token
			case agentdir.ChannelDiscord:
				s.Discord = token
			case agentdir.ChannelEmail:
				s.Email = &email
			}
			conf, secrets, err := d.ApplySettings(s)
			var se *agentdir.SettingsError
			if errors.As(err, &se) {
				return clierr.Usage(se.Error())
			}
			if err != nil {
				return err
			}
			req := controlapi.ChannelRequestFromConfig(conf, args[0])
			req.Secrets = secrets
			st, err := sf.controlClient(d).AddChannel(cmd.Context(), req)
			if errors.Is(err, controlapi.ErrNotRunning) {
				return addChannelOffline(cmd.Context(), cfg, d, conf, secrets, args[0])
			}
			if err != nil {
				return err
			}
			if cfg.IsJSON() {
				return cfg.PrintJSON(st)
			}
			_, err = fmt.Fprintf(cfg.Out, "✓ %s %s\n", st.Name, st.Account)
			return err
		},
	}
	f := cmd.Flags()
	f.StringVar(&token, "token", "", "Bot token: env:VAR or the token")
	registerEmailFlags(f, &email, "password, env:VAR or the value")
	return cmd
}

// addChannelOffline checks the channel's credentials before saving it for
// the next start, since no running agent can check them.
func addChannelOffline(ctx context.Context, cfg *globalConfig, d agentdir.Dir, conf *agentdir.Config, secrets agentdir.Secrets, kind string) error {
	all, err := d.LoadSecrets()
	if err != nil {
		return err
	}
	maps.Copy(all, secrets)
	if err := agentsrv.ValidateChannel(ctx, conf, all, kind); err != nil {
		return err
	}
	if err := d.SaveSecrets(secrets); err != nil {
		return err
	}
	if err := d.SaveConfig(conf); err != nil {
		return err
	}
	_, err = fmt.Fprintf(cfg.Out, "✓ saved %s; it starts with the agent (a2a serve)\n", kind)
	return err
}
