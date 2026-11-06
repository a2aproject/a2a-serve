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

func TestMessengers(t *testing.T) {
	t.Parallel()
	email := &agentdir.EmailConfig{Address: "coder@example.com", IMAP: "imap:993", SMTP: "smtp:465", Password: "secret:EMAIL_PASSWORD", TrustedAuthServ: "mx.example.com"}
	tests := []struct {
		name    string
		running []controlapi.MessengerStatus
		want    []Messenger
	}{
		{
			name: "stopped agent",
			want: []Messenger{
				{Name: "telegram", State: MessengerStopped},
				{Name: "email", Account: "coder@example.com", State: MessengerStopped},
			},
		},
		{
			name:    "running agent",
			running: []controlapi.MessengerStatus{{Name: "telegram", Account: "@coder_bot"}, {Name: "email", Account: "coder@example.com", Error: "dial imap: refused"}},
			want: []Messenger{
				{Name: "telegram", Account: "@coder_bot", State: MessengerRunning},
				{Name: "email", Account: "coder@example.com", State: MessengerFailing, Error: "dial imap: refused"},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := &agentdir.Config{Name: "coder", Harness: "opencode"}
			if err := cfg.SetMessenger(agentdir.MessengerTelegram, "secret:TELEGRAM_TOKEN", nil); err != nil {
				t.Fatalf("SetMessenger() error = %v", err)
			}
			cfg.Messengers.Email = email
			d := newAgent(t, cfg, agentdir.Secrets{})
			if tc.running != nil {
				serveFake(t, d, &fakeServer{messengers: tc.running})
			}

			got, err := Messengers(t.Context(), d, controlapi.NewClient(d))
			if err != nil {
				t.Fatalf("Messengers() error = %v", err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("Messengers() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}
