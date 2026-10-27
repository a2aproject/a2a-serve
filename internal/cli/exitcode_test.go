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
	"strings"
	"testing"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/clierr"
)

func TestExitCodes(t *testing.T) {
	t.Parallel()
	stopped := newTestAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode"}).Root
	empty := t.TempDir()
	tests := []struct {
		name     string
		args     []string
		wantCode string
		wantExit int
	}{
		{name: "contact list on a stopped agent", args: []string{"contact", "list", "--dir", stopped}, wantCode: clierr.CodeNotRunning, wantExit: 6},
		{name: "pair invite on a stopped agent", args: []string{"pair", "invite", "anna", "--dir", stopped}, wantCode: clierr.CodeNotRunning, wantExit: 6},
		{name: "restart of a stopped agent", args: []string{"restart", "--dir", stopped}, wantCode: clierr.CodeNotRunning, wantExit: 6},
		{name: "status without an agent", args: []string{"status", "--dir", empty}, wantCode: clierr.CodeNoAgent, wantExit: 7},
		{name: "start without an agent", args: []string{"--dir", empty}, wantCode: clierr.CodeNoAgent, wantExit: 7},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := runCMD(t, tc.args...)
			got := clierr.Classify(err)
			if got == nil {
				t.Fatalf("a2a serve %s error = nil, want %s", strings.Join(tc.args, " "), tc.wantCode)
			}
			if got.Code != tc.wantCode || got.Exit != tc.wantExit {
				t.Fatalf("a2a serve %s error = %s (exit %d), want %s (exit %d)", strings.Join(tc.args, " "), got.Code, got.Exit, tc.wantCode, tc.wantExit)
			}
		})
	}
}

func TestNotRunningSuggestsDetach(t *testing.T) {
	t.Parallel()
	d := newTestAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode"})

	_, err := runCMD(t, "contact", "add", "grok", "--dir", d.Root)

	if err == nil || !strings.Contains(err.Error(), "a2a serve --detach") {
		t.Fatalf("a2a serve contact add error = %v, want a hint to start it with `a2a serve --detach`", err)
	}
}
