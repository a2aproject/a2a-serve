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

// Package configured creates the channels an agent's config sets up.
package configured

import (
	"context"
	"fmt"
	"strings"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/channel"
	"github.com/a2aproject/a2a-serve/internal/channel/discord"
	"github.com/a2aproject/a2a-serve/internal/channel/email"
	"github.com/a2aproject/a2a-serve/internal/channel/telegram"
)

// Options configures the channels [New] creates.
type Options struct {
	// KV keeps the channels' state. Nil keeps none.
	KV channel.KV
	// TelegramURL overrides the Telegram Bot API URL.
	TelegramURL string
	// SaveSecret, if set, stores a secret that a channel replaced, such as
	// a rotated OAuth refresh token.
	SaveSecret func(name, value string) error
}

// All creates the channels of the config.
func All(cfg *agentdir.Config, secrets agentdir.Secrets, opts Options) ([]channel.Channel, error) {
	var out []channel.Channel
	for _, kind := range agentdir.ChannelKinds {
		if !cfg.HasChannel(kind) {
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

// Validate checks the credentials of the channel of kind configured in cfg,
// such as a bot token, without starting it.
func Validate(ctx context.Context, cfg *agentdir.Config, secrets agentdir.Secrets, kind string) error {
	ch, err := New(cfg, secrets, kind, Options{})
	if err != nil {
		return agenterr.Invalid(err)
	}
	return channel.Validate(ctx, ch)
}

// New creates the channel of kind configured in cfg.
func New(cfg *agentdir.Config, secrets agentdir.Secrets, kind string, opts Options) (channel.Channel, error) {
	switch kind {
	case agentdir.ChannelTelegram:
		if cfg.Channels.Telegram == nil {
			return nil, fmt.Errorf("telegram is not configured")
		}
		tok, err := secrets.Resolve(cfg.Channels.Telegram.Token)
		if err != nil {
			return nil, fmt.Errorf("telegram: %w", err)
		}
		ch := telegram.New(tok)
		if opts.TelegramURL != "" {
			ch.ServerURL = opts.TelegramURL
		}
		return ch, nil
	case agentdir.ChannelDiscord:
		if cfg.Channels.Discord == nil {
			return nil, fmt.Errorf("discord is not configured")
		}
		tok, err := secrets.Resolve(cfg.Channels.Discord.Token)
		if err != nil {
			return nil, fmt.Errorf("discord: %w", err)
		}
		return discord.New(tok, opts.KV), nil
	case agentdir.ChannelEmail:
		ec := cfg.Channels.Email
		if ec == nil {
			return nil, fmt.Errorf("email is not configured")
		}
		if ec.TrustedAuthServ == "" {
			return nil, fmt.Errorf("email: trusted_authserv must be set, or no mail could be trusted")
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
	return nil, fmt.Errorf("unknown channel %q: want telegram, discord or email", kind)
}
