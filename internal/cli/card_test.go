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
	"encoding/json"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/clierr"
)

func TestCardStopped(t *testing.T) {
	t.Parallel()
	d := newTestAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode", Port: 4100, Card: agentdir.CardConfig{Skills: []agentdir.CardSkill{
		{ID: "release-notes", Name: "Release notes", Description: "Writes release notes from merged pull requests.", Tags: []string{"git", "docs"}},
	}}})
	empty := t.TempDir()
	tests := []struct {
		name     string
		args     []string
		wantExit int
	}{
		{name: "furthest", args: []string{"card", "--dir", d.Root}},
		{name: "local", args: []string{"card", "--url=local", "--dir", d.Root}},
		{name: "json", args: []string{"-o", "json", "card", "--url", "local", "--dir", d.Root}},
		{name: "private needs the server", args: []string{"card", "--url", "private", "--dir", d.Root}, wantExit: 6},
		{name: "unknown scope", args: []string{"card", "--url", "bogus", "--dir", empty}, wantExit: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, err := runCMD(t, tc.args...)
			gotExit := 0
			if err != nil {
				gotExit = clierr.Classify(err).Exit
			}
			if gotExit != tc.wantExit {
				t.Fatalf("a2a-serve %s error = %v (exit %d), want exit %d", strings.Join(tc.args, " "), err, gotExit, tc.wantExit)
			}
			if tc.wantExit != 0 {
				return
			}
			var card a2a.AgentCard
			if err := json.Unmarshal([]byte(out), &card); err != nil {
				t.Fatalf("json.Unmarshal(%q) error = %v", out, err)
			}
			if got := card.SupportedInterfaces[0].URL; got != "http://127.0.0.1:4100" {
				t.Fatalf("a2a-serve card interface URL = %q, want %q", got, "http://127.0.0.1:4100")
			}
			want := []a2a.AgentSkill{{ID: "release-notes", Name: "Release notes", Description: "Writes release notes from merged pull requests.", Tags: []string{"git", "docs"}}}
			if diff := cmp.Diff(want, card.Skills); diff != "" {
				t.Fatalf("a2a-serve card skills wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}
