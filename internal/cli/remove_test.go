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
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/credstore"
	"github.com/a2aproject/a2a-serve/internal/output"
)

func TestRemoveCommandsJSON(t *testing.T) {
	t.Parallel()
	const agentURL = "http://127.0.0.1:9000"
	tests := []struct {
		name    string
		args    []string
		inAgent bool
		want    map[string]any
	}{
		{
			name:    "channel rm",
			args:    []string{"channel", "rm", "telegram"},
			inAgent: true,
			want:    map[string]any{"removed": true, "kind": "channel", "name": "telegram"},
		},
		{
			name: "pair rm of a joined agent",
			args: []string{"pair", "rm", agentURL},
			want: map[string]any{"removed": true, "kind": "joined", "name": agentURL},
		},
		{
			name:    "stop",
			args:    []string{"stop"},
			inAgent: true,
			want:    map[string]any{"name": "coder", "stopped": false, "serviceInstalled": false, "serviceRemoved": false},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := &agentdir.Config{Name: "coder", Harness: "opencode"}
			if err := cfg.SetChannel("telegram", "secret:TELEGRAM_TOKEN", nil); err != nil {
				t.Fatal(err)
			}
			d := newTestAgent(t, cfg)
			creds := &credstore.Store{Path: filepath.Join(t.TempDir(), "credentials.json")}
			if err := creds.Put(credstore.Credential{URL: agentURL, Token: "tok"}); err != nil {
				t.Fatal(err)
			}

			args := append([]string{"-o", "json"}, tc.args...)
			if tc.inAgent {
				args = append(args, "--dir", d.Root)
			}
			out, err := runCMDWithCreds(t, creds, args...)
			if err != nil {
				t.Fatalf("a2a-serve %v error = %v", tc.args, err)
			}

			var got map[string]any
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("json.Unmarshal(%q) error = %v", out, err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("a2a-serve %v wrong result (-want +got) diff = %s", tc.args, diff)
			}
		})
	}
}

func TestRemoveCommandsHaveDeleteAlias(t *testing.T) {
	t.Parallel()
	root := newRootCmd(&globalConfig{Printer: output.NewPrinter(&strings.Builder{}, output.ModeText)})
	for _, group := range []string{"channel", "contact", "pair", "schedule"} {
		cmd, _, err := root.Find([]string{group, "delete"})
		if err != nil || cmd.Name() != "rm" {
			t.Fatalf("a2a-serve %s delete = %v, %v, want the rm command", group, cmd, err)
		}
	}
}
