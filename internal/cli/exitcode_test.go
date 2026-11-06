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
	"slices"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/clierr"
)

func TestExitCodes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		args     []string
		noAgent  bool
		noDir    bool
		wantCode string
		wantExit int
	}{
		{name: "peers of a stopped agent", args: []string{"peers"}, wantCode: clierr.CodeNotRunning, wantExit: 6},
		{name: "schedule run on a stopped agent", args: []string{"schedule", "run", "brief"}, wantCode: clierr.CodeNotRunning, wantExit: 6},
		{name: "contact found by name on a stopped agent", args: []string{"contact", "add", "grok"}, wantCode: clierr.CodeNotRunning, wantExit: 6},
		{name: "channel rm of an unconfigured channel", args: []string{"channel", "rm", "telegram"}, wantCode: clierr.CodeUsage, wantExit: 2},
		{name: "status without an agent", args: []string{"status"}, noAgent: true, wantCode: clierr.CodeNoAgent, wantExit: 7},
		{name: "start without an agent", args: []string{"start"}, noAgent: true, wantCode: clierr.CodeNoAgent, wantExit: 7},
		{name: "mutually exclusive flags", args: []string{"pair", "list", "--joined", "--invited"}, wantCode: clierr.CodeUsage, wantExit: 2},
		{name: "pair get of an agent not joined", args: []string{"pair", "get", "https://other.example.com"}, wantCode: clierr.CodeUsage, wantExit: 2},
		{name: "pair rm of an agent not joined", args: []string{"pair", "rm", "https://other.example.com"}, wantCode: clierr.CodeUsage, wantExit: 2},
		{name: "contact add of an unreachable URL", args: []string{"contact", "add", "grok", "--url", "http://127.0.0.1:1"}, wantCode: clierr.CodeIO, wantExit: 3},
		{name: "backup now without a destination", args: []string{"backup", "now"}, wantCode: clierr.CodeUsage, wantExit: 2},
		{name: "backup list without a destination", args: []string{"backup", "list"}, wantCode: clierr.CodeUsage, wantExit: 2},
		{name: "backup restore into an agent", args: []string{"backup", "restore", "backups", "--snapshot", "coder-1", "--backup-key", "k"}, wantCode: clierr.CodeUsage, wantExit: 2},
		{name: "inspect on the public URL", args: []string{"inspect", "--url", "public"}, wantCode: clierr.CodeUsage, wantExit: 2},
		{name: "mailbox without a node token", args: []string{"mailbox", "list"}, noDir: true, wantCode: clierr.CodeUsage, wantExit: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if !tc.noAgent {
				dir = newTestAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode"}).Root
			}
			args := slices.Clone(tc.args)
			if !tc.noDir {
				args = append(args, "--dir", dir)
			}
			_, err := runCMD(t, args...)
			got := clierr.Classify(err)
			if got == nil {
				t.Fatalf("a2a-serve %s error = nil, want %s", strings.Join(args, " "), tc.wantCode)
			}
			if got.Code != tc.wantCode || got.Exit != tc.wantExit {
				t.Fatalf("a2a-serve %s error = %s (exit %d), want %s (exit %d)", strings.Join(args, " "), got.Code, got.Exit, tc.wantCode, tc.wantExit)
			}
		})
	}
}

func TestNotRunningSuggestsDetach(t *testing.T) {
	t.Parallel()
	d := newTestAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode"})

	_, err := runCMD(t, "contact", "add", "grok", "--dir", d.Root)

	if err == nil || !strings.Contains(classify(err, nil).Hint, "a2a-serve start --detach") {
		t.Fatalf("a2a-serve contact add error = %v, want a hint to start it with `a2a-serve start --detach`", err)
	}
}
