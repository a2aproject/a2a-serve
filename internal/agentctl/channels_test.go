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

package agentctl

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
)

func TestChannels(t *testing.T) {
	t.Parallel()
	email := &agentdir.EmailConfig{Address: "coder@example.com", IMAP: "imap:993", SMTP: "smtp:465", Password: "secret:EMAIL_PASSWORD", TrustedAuthServ: "mx.example.com"}
	tests := []struct {
		name    string
		running []controlapi.ChannelStatus
		want    []Channel
	}{
		{
			name: "stopped agent",
			want: []Channel{
				{Name: "telegram", State: ChannelStopped},
				{Name: "email", Account: "coder@example.com", State: ChannelStopped},
			},
		},
		{
			name:    "running agent",
			running: []controlapi.ChannelStatus{{Name: "telegram", Account: "@coder_bot"}, {Name: "email", Account: "coder@example.com", Error: "dial imap: refused"}},
			want: []Channel{
				{Name: "telegram", Account: "@coder_bot", State: ChannelRunning},
				{Name: "email", Account: "coder@example.com", State: ChannelFailing, Error: "dial imap: refused"},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := &agentdir.Config{Name: "coder", Harness: "opencode"}
			if err := cfg.SetChannel(agentdir.ChannelTelegram, "secret:TELEGRAM_TOKEN", nil); err != nil {
				t.Fatalf("SetChannel() error = %v", err)
			}
			cfg.Channels.Email = email
			d := newAgent(t, cfg, agentdir.Secrets{})
			if tc.running != nil {
				serveFake(t, d, &fakeServer{channels: tc.running})
			}

			got, err := Channels(t.Context(), d, controlapi.NewClient(d))
			if err != nil {
				t.Fatalf("Channels() error = %v", err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("Channels() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}
