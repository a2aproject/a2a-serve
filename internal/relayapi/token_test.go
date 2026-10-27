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

package relayapi

import (
	"testing"
)

func TestParseNodeToken(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		token      string
		wantBase   string
		wantNodeID string
		wantOK     bool
	}{
		{name: "plain name", token: NewNodeToken("http://x/relay", "grok", "s"), wantBase: "http://x/relay", wantNodeID: "grok", wantOK: true},
		{name: "name with dots", token: NewNodeToken("http://x/relay", "grok.bot.v2", "s"), wantBase: "http://x/relay", wantNodeID: "grok.bot.v2", wantOK: true},
		{name: "not a node token", token: "a2p_secret"},
		{name: "no secret", token: "a2rn.aHR0cDovL3g.grok"},
		{name: "empty secret", token: "a2rn.aHR0cDovL3g.grok."},
		{name: "empty node ID", token: "a2rn.aHR0cDovL3g..s"},
		{name: "bad base64", token: "a2rn.!!.grok.s"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			base, id, ok := ParseNodeToken(tc.token)
			if base != tc.wantBase || id != tc.wantNodeID || ok != tc.wantOK {
				t.Fatalf("ParseNodeToken(%q) = %q, %q, %v, want %q, %q, %v", tc.token, base, id, ok, tc.wantBase, tc.wantNodeID, tc.wantOK)
			}
		})
	}
}
