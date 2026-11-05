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
	"slices"

	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/agentctl"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/clierr"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/secretref"
)

func newChannelAddCmd(cfg *globalConfig, sf *serverFlags) *cobra.Command {
	var (
		token string
		email agentdir.EmailSettings
	)
	cmd := &cobra.Command{
		Use:   "add <telegram|discord|email>",
		Short: "Add a messenger, or replace its settings",
		Long:  "Add a messenger, or replace its settings.\n\n" + secretref.Help,
		Example: `  a2a-serve channel add telegram --token env:CODER_BOT_TOKEN
  a2a-serve channel add email --email-address coder@example.com --email-imap imap.example.com:993 \
    --email-smtp smtp.example.com:465 --email-password env:MAIL_PASSWORD --email-trusted-authserv mx.example.com`,
		Args:      namedArgs,
		ValidArgs: agentdir.ChannelKinds,
		RunE: func(cmd *cobra.Command, args []string) (err error) {
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
			r := cfg.secretReader(cfg.stderr())
			for flag, dst := range map[string]*string{
				"token": &token, "email-password": &email.Password,
				"email-oauth-client-secret": &email.OAuthClientSecret, "email-oauth-refresh-token": &email.OAuthRefreshToken,
			} {
				if *dst == "" {
					continue
				}
				ref, err := secretref.Parse(flag, *dst, r)
				if err != nil {
					return err
				}
				// Env and stored references stay references, which the
				// settings store under their names.
				if ref.Kind == secretref.Literal || ref.Kind == secretref.Prompted {
					*dst = ref.Value
				}
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
			m, closer, err := agentctl.Open(cmd.Context(), d, sf.controlClient(d))
			if err != nil {
				return err
			}
			defer joinClose(&err, closer)
			st, err := m.AddChannel(cmd.Context(), req)
			if err != nil {
				return err
			}
			return printChannelAdded(cfg, st, m.Running())
		},
	}
	f := cmd.Flags()
	f.StringVar(&token, "token", "", secretUsage("Bot token"))
	registerEmailFlags(f, &email)
	return cmd
}

// printChannelAdded prints the channel added to the agent, which starts it
// at once if running.
func printChannelAdded(cfg *globalConfig, st *controlapi.ChannelStatus, running bool) error {
	if cfg.IsJSON() {
		return cfg.PrintJSON(st)
	}
	if !running {
		_, err := fmt.Fprintf(cfg.Out, "✓ saved %s; it starts with the agent (a2a-serve start)\n", st.Name)
		return err
	}
	if _, err := fmt.Fprintf(cfg.Out, "✓ %s %s\n", st.Name, st.Account); err != nil {
		return err
	}
	if st.OwnerLink == nil {
		return nil
	}
	_, err := fmt.Fprintln(cfg.Out, st.OwnerLink.Line())
	return err
}
