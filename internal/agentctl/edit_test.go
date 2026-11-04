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

package agentctl

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
)

func TestEdit(t *testing.T) {
	t.Parallel()
	const (
		valid   = "name: coder\nagent: opencode\nsandbox: local # edited\n"
		invalid = "name: coder\nagent: opencode\nsandbox: vm\n"
	)
	tests := []struct {
		name        string
		edits       []string
		retry       bool
		wantErr     bool
		wantSandbox string
		wantFile    string
		wantChanges int
	}{
		{name: "valid", edits: []string{valid}, wantSandbox: "local", wantFile: valid, wantChanges: 1},
		{name: "invalid, then fixed", edits: []string{invalid, valid}, retry: true, wantSandbox: "local", wantFile: valid, wantChanges: 1},
		{name: "invalid, given up", edits: []string{invalid}, wantErr: true},
		{name: "unchanged", edits: []string{""}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode"}, nil)
			orig, err := os.ReadFile(d.ConfigPath())
			if err != nil {
				t.Fatal(err)
			}
			var opened []string
			edit := func(path string) error {
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				opened = append(opened, string(data))
				next := tc.edits[len(opened)-1]
				if next == "" {
					return nil
				}
				return os.WriteFile(path, []byte(next), 0o600)
			}
			res, err := Edit(t.Context(), d, edit, func(error) (bool, error) { return tc.retry, nil })
			var se *agentdir.SettingsError
			if tc.wantErr != errors.As(err, &se) {
				t.Fatalf("Edit() error = %v, want a SettingsError %v", err, tc.wantErr)
			}
			if len(opened) != len(tc.edits) {
				t.Fatalf("Edit() opened the editor %d times, want %d", len(opened), len(tc.edits))
			}
			if len(opened) > 1 && !strings.HasPrefix(opened[1], `# ERROR: invalid --sandbox "vm"`) {
				t.Fatalf("Edit() reopened %q, want the error on top", opened[1])
			}
			got, err := os.ReadFile(d.ConfigPath())
			if err != nil {
				t.Fatal(err)
			}
			wantFile := tc.wantFile
			if wantFile == "" {
				wantFile = string(orig)
			}
			if diff := cmp.Diff(wantFile, string(got)); diff != "" {
				t.Fatalf("config.yaml wrong result (-want +got) diff = %s", diff)
			}
			if tc.wantErr {
				return
			}
			if len(res.Changes) != tc.wantChanges {
				t.Fatalf("Edit() changes = %+v, want %d", res.Changes, tc.wantChanges)
			}
		})
	}
}
