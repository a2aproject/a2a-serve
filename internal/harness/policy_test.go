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

package harness

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/sandbox/sandboxtest"
)

func TestWritePolicy(t *testing.T) {
	t.Parallel()
	ask := []any{"Edit", "Write", "NotebookEdit", "WebFetch", "WebSearch"}
	tests := []struct {
		name    string
		profile Profile
		policy  Policy
		want    map[string]any
	}{
		{
			name:    "claude",
			profile: claude{},
			policy:  Policy{MCPServers: []string{"github", "my server", "a__b", "docs.", "github"}},
			want: map[string]any{
				"permissions":           map[string]any{"ask": append(slices.Clone(ask), "mcp__github", "mcp__my_server", "mcp__a", "mcp__docs")},
				"allowManagedHooksOnly": true,
			},
		},
		{
			name:    "claude with commands denied",
			profile: claude{},
			policy:  Policy{AskCommands: true},
			want: map[string]any{
				"permissions":           map[string]any{"ask": append([]any{"Bash"}, ask...)},
				"allowManagedHooksOnly": true,
			},
		},
		{
			name:    "claude with a locked home",
			profile: claude{},
			policy:  Policy{LockHome: true},
			want: map[string]any{
				"permissions":                   map[string]any{"ask": ask},
				"allowManagedHooksOnly":         true,
				"strictPluginOnlyCustomization": []any{"mcp"},
			},
		},
		{
			name:    "agent of unknown kind",
			profile: custom{name: "acp", argv: []string{"fakeacp"}},
			want: map[string]any{
				"permissions":           map[string]any{"ask": ask},
				"allowManagedHooksOnly": true,
			},
		},
		{
			name:    "opencode",
			profile: opencode{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := filepath.Join(t.TempDir(), "policy")

			if err := WritePolicy(tc.profile, dir, tc.policy); err != nil {
				t.Fatalf("WritePolicy() error = %v", err)
			}

			data, err := os.ReadFile(filepath.Join(dir, "managed-settings.json"))
			if tc.want == nil {
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("WritePolicy() wrote %q, want no policy", data)
				}
				return
			}
			if err != nil {
				t.Fatalf("os.ReadFile() error = %v", err)
			}
			var got map[string]any
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("json.Unmarshal(%s) error = %v", data, err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("WritePolicy() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestPrepareDockerMountsPolicy(t *testing.T) {
	sandboxtest.FakeDocker(t, runningDocker)
	tests := []struct {
		name    string
		profile Profile
		want    bool
	}{
		{name: "claude", profile: claude{}, want: true},
		{name: "agent of unknown kind", profile: custom{name: "acp", argv: []string{"fakeacp"}}, want: true},
		{name: "opencode", profile: opencode{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ws, policy := t.TempDir(), filepath.Join(t.TempDir(), "policy")
			if err := WritePolicy(tc.profile, policy, Policy{}); err != nil {
				t.Fatalf("WritePolicy() error = %v", err)
			}

			p, err := Prepare(context.Background(), Spec{
				Profile:   tc.profile,
				Name:      "a2a-test",
				Mounts:    []agentdir.Mount{{Src: ws, Dst: ws}},
				Home:      t.TempDir(),
				EnvDir:    t.TempDir(),
				Image:     "img:1",
				PolicyDir: policy,
			})
			if err != nil {
				t.Fatalf("Prepare() error = %v", err)
			}
			t.Cleanup(func() {
				if err := p.Close(context.Background()); err != nil {
					t.Errorf("Prepared.Close() error = %v", err)
				}
			})

			if got := slices.Contains(bindMounts(output(t, p)), policy+":/etc/claude-code:ro"); got != tc.want {
				t.Fatalf("Prepare() mounts %q, want the policy mounted: %t", bindMounts(output(t, p)), tc.want)
			}
		})
	}
}

func TestPrepareDockerRefusesMissingPolicy(t *testing.T) {
	sandboxtest.FakeDocker(t, runningDocker)
	policy := filepath.Join(t.TempDir(), "policy")
	ws := t.TempDir()

	_, err := Prepare(context.Background(), Spec{
		Profile:   claude{},
		Name:      "a2a-test",
		Mounts:    []agentdir.Mount{{Src: ws, Dst: ws}},
		Home:      t.TempDir(),
		EnvDir:    t.TempDir(),
		PolicyDir: policy,
	})

	if want := filepath.Join(policy, "managed-settings.json"); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Prepare() error = %v, want it to name %s", err, want)
	}
}
