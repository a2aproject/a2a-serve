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

// Package configured creates the messengers an agent's config sets up.
package configured

import (
	"context"
	"fmt"
	"strings"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/messenger"
	"github.com/a2aproject/a2a-serve/internal/messenger/discord"
	"github.com/a2aproject/a2a-serve/internal/messenger/email"
	"github.com/a2aproject/a2a-serve/internal/messenger/telegram"
)

// Options configures the messengers [New] creates.
type Options struct {
	// KV keeps the messengers' state. Nil keeps none.
	KV messenger.KV
	// TelegramURL overrides the Telegram Bot API URL.
	TelegramURL string
	// SaveSecret, if set, stores a secret that a messenger replaced, such as
	// a rotated OAuth refresh token.
	SaveSecret func(name, value string) error
	// Commands are the chat commands the messengers list in their menus.
	Commands []messenger.Command
}

// All creates the messengers of the config.
func All(cfg *agentdir.Config, secrets agentdir.Secrets, opts Options) ([]messenger.Messenger, error) {
	var out []messenger.Messenger
	for _, kind := range agentdir.MessengerKinds {
		if !cfg.HasMessenger(kind) {
			continue
		}
		ch, err := New(cfg, secrets, kind, opts)
		if err != nil {
			return nil, err
		}
		out = append(out, ch)
	}
	return out, nil
}

// Validate checks the credentials of the messenger of kind configured in cfg,
// such as a bot token, without starting it.
func Validate(ctx context.Context, cfg *agentdir.Config, secrets agentdir.Secrets, kind string) error {
	ch, err := New(cfg, secrets, kind, Options{})
	if err != nil {
		return agenterr.Invalid(err)
	}
	return messenger.Validate(ctx, ch)
}

// New creates the messenger of kind configured in cfg.
func New(cfg *agentdir.Config, secrets agentdir.Secrets, kind string, opts Options) (messenger.Messenger, error) {
	switch kind {
	case agentdir.MessengerTelegram:
		if cfg.Messengers.Telegram == nil {
			return nil, fmt.Errorf("telegram is not configured")
		}
		tok, err := secrets.Resolve(cfg.Messengers.Telegram.Token)
		if err != nil {
			return nil, fmt.Errorf("telegram: %w", err)
		}
		ch := telegram.New(tok, opts.Commands)
		if opts.TelegramURL != "" {
			ch.ServerURL = opts.TelegramURL
		}
		return ch, nil
	case agentdir.MessengerDiscord:
		if cfg.Messengers.Discord == nil {
			return nil, fmt.Errorf("discord is not configured")
		}
		tok, err := secrets.Resolve(cfg.Messengers.Discord.Token)
		if err != nil {
			return nil, fmt.Errorf("discord: %w", err)
		}
		return discord.New(tok, opts.KV, opts.Commands), nil
	case agentdir.MessengerEmail:
		ec := cfg.Messengers.Email
		if ec == nil {
			return nil, fmt.Errorf("email is not configured")
		}
		if ec.TrustedAuthServ == "" {
			return nil, fmt.Errorf("email: trustedAuthServ must be set, or no mail could be trusted")
		}
		pw, err := secrets.Resolve(ec.Password)
		if err != nil {
			return nil, fmt.Errorf("email: %w", err)
		}
		cfg := email.Config{
			Address: ec.Address, IMAP: ec.IMAP, SMTP: ec.SMTP, Username: ec.Username, Password: pw,
			Mailbox: ec.Mailbox, TrustedAuthServ: ec.TrustedAuthServ,
		}
		if ec.OAuthRefreshToken != "" {
			refresh, err := secrets.Resolve(ec.OAuthRefreshToken)
			if err != nil {
				return nil, fmt.Errorf("email: OAuth refresh token: %w", err)
			}
			clientSecret, err := secrets.Resolve(ec.OAuthClientSecret)
			if err != nil {
				return nil, fmt.Errorf("email: OAuth client secret: %w", err)
			}
			cfg.OAuth = &email.OAuthConfig{TokenURL: ec.OAuthTokenURL, ClientID: ec.OAuthClientID, ClientSecret: clientSecret, RefreshToken: refresh}
			if name, ok := strings.CutPrefix(ec.OAuthRefreshToken, agentdir.SecretPrefix); ok && opts.SaveSecret != nil {
				cfg.OAuth.SaveRefreshToken = func(tok string) error { return opts.SaveSecret(name, tok) }
			}
		}
		return email.New(cfg, opts.KV), nil
	}
	return nil, fmt.Errorf("unknown messenger %q: want telegram, discord or email", kind)
}
