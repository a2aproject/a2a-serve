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

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
)

func TestEditorCommand(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		env      map[string]string
		terminal bool
		want     []string
		wantErr  bool
	}{
		{name: "default on a terminal", terminal: true, want: []string{"vi"}},
		{name: "default without a terminal", wantErr: true},
		{name: "EDITOR without a terminal", env: map[string]string{"EDITOR": "ed -s"}, want: []string{"ed", "-s"}},
		{name: "VISUAL wins over EDITOR", env: map[string]string{"VISUAL": "code -w", "EDITOR": "ed"}, terminal: true, want: []string{"code", "-w"}},
		{name: "unbalanced quote", env: map[string]string{"EDITOR": "ed 'x"}, terminal: true, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := editorCommand(func(k string) string { return tc.env[k] }, tc.terminal)
			if (err != nil) != tc.wantErr {
				t.Fatalf("editorCommand() error = %v, want error %v", err, tc.wantErr)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("editorCommand() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestConfigEditWithoutTerminalFails(t *testing.T) {
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "")
	d := newTestAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode"})

	_, err := runCMD(t, "config", "edit", "--dir", d.Root)

	if !isUsage(err) || !strings.Contains(err.Error(), "no terminal") {
		t.Fatalf("a2a-serve config edit without a terminal error = %v, want a usage error about the missing terminal", err)
	}
}
