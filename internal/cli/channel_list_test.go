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
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
)

func TestChannelListOffline(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		channels []string
		want     string
	}{
		{name: "configured", channels: []string{agentdir.ChannelTelegram, agentdir.ChannelDiscord}, want: "CHANNEL   ACCOUNT  STATE\ntelegram           stopped\ndiscord            stopped\n"},
		{name: "none", want: "no channels; add one with `a2a serve channel add <telegram|discord|email>`\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := &agentdir.Config{Name: "coder", Harness: "opencode"}
			for _, kind := range tc.channels {
				if err := cfg.SetChannel(kind, "secret:TOKEN", nil); err != nil {
					t.Fatalf("SetChannel() error = %v", err)
				}
			}
			d := newTestAgent(t, cfg)

			got, err := runCMD(t, "channel", "list", "--dir", d.Root)
			if err != nil {
				t.Fatalf("a2a serve channel list error = %v", err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("a2a serve channel list wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}
