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

package mailboxapi

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
		{name: "plain name", token: NewNodeToken("http://x/mailbox", "grok", "s"), wantBase: "http://x/mailbox", wantNodeID: "grok", wantOK: true},
		{name: "name with dots", token: NewNodeToken("http://x/mailbox", "grok.bot.v2", "s"), wantBase: "http://x/mailbox", wantNodeID: "grok.bot.v2", wantOK: true},
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

func TestAgentURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		token  string
		want   string
		wantOK bool
	}{
		{name: "mailbox token", token: NewNodeToken(APIBase("https://agent.example.com/"), "grok", "s"), want: "https://agent.example.com", wantOK: true},
		{name: "legacy relay token", token: NewNodeToken("https://agent.example.com/relay", "grok", "s"), want: "https://agent.example.com", wantOK: true},
		{name: "not a node token", token: "a2p_secret"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := AgentURL(tc.token)
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("AgentURL(%q) = %q, %v, want %q, %v", tc.token, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestAPIBase(t *testing.T) {
	t.Parallel()
	tests := []struct {
		url  string
		want string
	}{
		{url: "https://agent.example.com", want: "https://agent.example.com/mailbox"},
		{url: "https://agent.example.com/", want: "https://agent.example.com/mailbox"},
		{url: "https://agent.example.com/mailbox", want: "https://agent.example.com/mailbox"},
		{url: "https://agent.example.com/relay", want: "https://agent.example.com/relay"},
	}
	for _, tc := range tests {
		if got := APIBase(tc.url); got != tc.want {
			t.Fatalf("APIBase(%q) = %q, want %q", tc.url, got, tc.want)
		}
	}
}

func TestEnvToken(t *testing.T) {
	tests := []struct {
		name   string
		node   string
		legacy string
		want   string
	}{
		{name: "node token", node: "new", want: "new"},
		{name: "legacy relay token", legacy: "old", want: "old"},
		{name: "node token wins", node: "new", legacy: "old", want: "new"},
		{name: "none"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvNodeToken, tc.node)
			t.Setenv(LegacyEnvNodeToken, tc.legacy)
			if got := EnvToken(); got != tc.want {
				t.Fatalf("EnvToken() = %q, want %q", got, tc.want)
			}
		})
	}
}
