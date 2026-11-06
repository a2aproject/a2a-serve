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
	"cmp"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
)

func TestMessengerListOffline(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		output     string
		messengers []string
		want       string
	}{
		{name: "configured", messengers: []string{agentdir.MessengerTelegram, agentdir.MessengerDiscord}, want: "MESSENGER  ACCOUNT  STATE\ntelegram            stopped\ndiscord             stopped\n"},
		{name: "none", want: "no messengers; add one with `a2a-serve messenger add <telegram|discord|email>`\n"},
		{name: "jsonl", output: "jsonl", messengers: []string{agentdir.MessengerTelegram, agentdir.MessengerDiscord}, want: `{"name":"telegram","state":"stopped"}` + "\n" + `{"name":"discord","state":"stopped"}` + "\n"},
		{name: "json none", output: "json", want: "[]\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := &agentdir.Config{Name: "coder", Harness: "opencode"}
			for _, kind := range tc.messengers {
				if err := cfg.SetMessenger(kind, "secret:TOKEN", nil); err != nil {
					t.Fatalf("SetMessenger() error = %v", err)
				}
			}
			d := newTestAgent(t, cfg)

			got, err := runCMD(t, "-o", cmp.Or(tc.output, "text"), "messenger", "list", "--dir", d.Root)
			if err != nil {
				t.Fatalf("a2a-serve messenger list error = %v", err)
			}
			if diff := gocmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("a2a-serve messenger list wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}
