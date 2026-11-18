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

package agentcfg

import (
	"slices"
	"testing"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agentprofile"
)

func TestWarnings(t *testing.T) {
	t.Parallel()
	openrouter := []string{"OPENROUTER_API_KEY"}
	tests := []struct {
		name string
		cfg  agentdir.Config
		want int
	}{
		{name: "opencode with OpenRouter and no model", cfg: agentdir.Config{Harness: "opencode", Secrets: openrouter}, want: 1},
		{name: "opencode with OpenRouter and a model", cfg: agentdir.Config{Harness: "opencode", Secrets: openrouter, Model: "openrouter/a/b"}},
		{name: "opencode with another key", cfg: agentdir.Config{Harness: "opencode", Secrets: []string{"ANTHROPIC_API_KEY"}}},
		{name: "claude with OpenRouter", cfg: agentdir.Config{Harness: "claude", Secrets: openrouter}},
		{name: "opencode model without its key", cfg: agentdir.Config{Harness: "opencode", Model: "google/gemini-3-flash-preview"}, want: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Warnings(&tc.cfg); len(got) != tc.want {
				t.Fatalf("Warnings() = %q, want %d warnings", got, tc.want)
			}
		})
	}
}

func TestMissingKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cfg  agentdir.Config
		want string
	}{
		{
			name: "google model without a key",
			cfg:  agentdir.Config{Harness: "opencode", Model: "google/gemini-3-flash-preview", Secrets: []string{"GITHUB_TOKEN"}},
			want: "no key for provider google: set one with `a2a-serve config set --secret GOOGLE_GENERATIVE_AI_API_KEY --restart`",
		},
		{
			name: "openrouter model without a key",
			cfg:  agentdir.Config{Harness: "opencode", Model: "openrouter/~google/gemini-flash-latest"},
			want: "no key for provider openrouter: set one with `a2a-serve config set --secret OPENROUTER_API_KEY --restart`",
		},
		{name: "google model with another google key", cfg: agentdir.Config{Harness: "opencode", Model: "google/gemini-3-flash-preview", Secrets: []string{"GEMINI_API_KEY"}}},
		{name: "key in env", cfg: agentdir.Config{Harness: "opencode", Model: "anthropic/claude-sonnet-4-5", Env: []string{"ANTHROPIC_API_KEY=sk"}}},
		{name: "unknown provider", cfg: agentdir.Config{Harness: "opencode", Model: "acme/model"}},
		{name: "no model", cfg: agentdir.Config{Harness: "opencode"}},
		{name: "claude", cfg: agentdir.Config{Harness: "claude", Model: "anthropic/claude-sonnet-4-5"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := MissingKey(&tc.cfg); got != tc.want {
				t.Fatalf("MissingKey() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestProfileNamesAreNotReserved(t *testing.T) {
	t.Parallel()
	for _, name := range agentprofile.ProfileNames() {
		if slices.Contains(agentdir.ReservedHarnesses, name) {
			t.Fatalf("agent.ProfileNames() = %v, want none of the reserved %v", agentprofile.ProfileNames(), agentdir.ReservedHarnesses)
		}
	}
}
