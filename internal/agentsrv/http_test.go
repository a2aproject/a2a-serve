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

package agentsrv

import (
	"strings"
	"testing"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/network"
)

func TestUnauthorizedHint(t *testing.T) {
	t.Parallel()
	tests := []struct {
		kind      string
		wantFirst string
	}{
		{kind: network.ListenerLocal, wantFirst: "credential helper"},
		{kind: network.ListenerPrivate, wantFirst: "pairing code"},
	}
	for _, tc := range tests {
		t.Run(tc.kind, func(t *testing.T) {
			t.Parallel()
			got := unauthorizedHint(tc.kind)
			first, _, _ := strings.Cut(got, ";")
			if !strings.Contains(first, tc.wantFirst) || !strings.Contains(got, "credential helper") || !strings.Contains(got, "`a2a serve tokens add <agent-url> <code>`") {
				t.Fatalf("unauthorizedHint(%q) = %q, want both cases, %q first", tc.kind, got, tc.wantFirst)
			}
		})
	}
}

func TestCardDescription(t *testing.T) {
	t.Parallel()
	tests := []struct {
		harness string
		want    string
	}{
		{harness: "acp", want: "coder is an acp coding agent reached through `a2a serve`."},
		{harness: "opencode", want: "coder is an opencode coding agent reached through `a2a serve`."},
		{harness: "claude", want: "coder is a claude coding agent reached through `a2a serve`."},
	}
	for _, tc := range tests {
		t.Run(tc.harness, func(t *testing.T) {
			t.Parallel()
			s := &Server{cfg: &agentdir.Config{Name: "coder", Harness: tc.harness}}
			if got := s.card(network.ListenerPrivate, "http://127.0.0.1:9000").Description; got != tc.want {
				t.Fatalf("Server.card().Description = %q, want %q", got, tc.want)
			}
		})
	}
}
