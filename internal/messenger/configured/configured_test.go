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

package configured

import (
	"context"
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agenterr"
)

func TestNewErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cfg  agentdir.Config
		kind string
	}{
		{name: "unknown kind", kind: "fax"},
		{name: "telegram not configured", kind: agentdir.MessengerTelegram},
		{name: "discord not configured", kind: agentdir.MessengerDiscord},
		{name: "email not configured", kind: agentdir.MessengerEmail},
		{
			name: "email without trusted authserv",
			cfg:  agentdir.Config{Messengers: agentdir.MessengersConfig{Email: &agentdir.EmailConfig{Address: "a@example.com"}}},
			kind: agentdir.MessengerEmail,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := New(&tc.cfg, agentdir.Secrets{}, tc.kind, Options{}); err == nil {
				t.Fatalf("New(%q) error = nil, want error", tc.kind)
			}
		})
	}
}

func TestAll(t *testing.T) {
	t.Parallel()
	cfg := &agentdir.Config{Messengers: agentdir.MessengersConfig{
		Telegram: &agentdir.TelegramConfig{Token: "secret:TELEGRAM_TOKEN"},
		Discord:  &agentdir.DiscordConfig{Token: "secret:DISCORD_TOKEN"},
	}}
	secrets := agentdir.Secrets{"TELEGRAM_TOKEN": "t", "DISCORD_TOKEN": "d"}
	chans, err := All(cfg, secrets, Options{})
	if err != nil {
		t.Fatalf("All() error = %v", err)
	}
	var got []string
	for _, ch := range chans {
		got = append(got, ch.Name())
	}
	if diff := cmp.Diff([]string{agentdir.MessengerTelegram, agentdir.MessengerDiscord}, got); diff != "" {
		t.Fatalf("All() wrong result (-want +got) diff = %s", diff)
	}
}

func TestValidateInvalidConfig(t *testing.T) {
	t.Parallel()
	err := Validate(context.Background(), &agentdir.Config{}, agentdir.Secrets{}, agentdir.MessengerTelegram)
	var invalid *agenterr.InvalidError
	if !errors.As(err, &invalid) {
		t.Fatalf("Validate() error = %v, want an InvalidError", err)
	}
}
