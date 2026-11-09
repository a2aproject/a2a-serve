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
)

func TestWritePolicy(t *testing.T) {
	t.Parallel()
	ask := []any{"Bash", "Edit", "Write", "NotebookEdit", "WebFetch", "WebSearch"}
	tests := []struct {
		name     string
		profile  Profile
		lockHome bool
		mcp      []string
		want     map[string]any
	}{
		{
			name:    "claude",
			profile: claude{},
			mcp:     []string{"github", "my server", "a__b", "github"},
			want: map[string]any{"permissions": map[string]any{
				"ask": append(slices.Clone(ask), "mcp__github", "mcp__my_server", "mcp__a"),
			}},
		},
		{
			name:     "claude with a locked home",
			profile:  claude{},
			lockHome: true,
			want: map[string]any{
				"permissions":                   map[string]any{"ask": ask},
				"strictPluginOnlyCustomization": []any{"mcp"},
			},
		},
		{
			name:    "agent of unknown kind",
			profile: custom{name: "acp", argv: []string{"fakeacp"}},
			want:    map[string]any{"permissions": map[string]any{"ask": ask}},
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

			if err := WritePolicy(tc.profile, dir, tc.lockHome, tc.mcp); err != nil {
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
	fakeDocker(t, runningDocker)
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
			if err := WritePolicy(tc.profile, policy, false, nil); err != nil {
				t.Fatalf("WritePolicy() error = %v", err)
			}

			p, err := Prepare(context.Background(), Spec{
				Profile:   tc.profile,
				Name:      "a2a-test",
				Mounts:    []agentdir.Mount{{Src: ws, Dst: ws}},
				Home:      t.TempDir(),
				Image:     "img:1",
				PolicyDir: policy,
			})
			if err != nil {
				t.Fatalf("Prepare() error = %v", err)
			}
			t.Cleanup(func() {
				if err := p.Stop(context.Background()); err != nil {
					t.Errorf("Stop() error = %v", err)
				}
			})

			if got := slices.Contains(bindMounts(p.Args), policy+":/etc/claude-code:ro"); got != tc.want {
				t.Fatalf("Prepare() mounts %q, want the policy mounted: %t", bindMounts(p.Args), tc.want)
			}
		})
	}
}

func TestPrepareDockerRefusesMissingPolicy(t *testing.T) {
	fakeDocker(t, runningDocker)
	policy := filepath.Join(t.TempDir(), "policy")
	ws := t.TempDir()

	_, err := Prepare(context.Background(), Spec{
		Profile:   claude{},
		Name:      "a2a-test",
		Mounts:    []agentdir.Mount{{Src: ws, Dst: ws}},
		Home:      t.TempDir(),
		PolicyDir: policy,
	})

	if want := filepath.Join(policy, "managed-settings.json"); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Prepare() error = %v, want it to name %s", err, want)
	}
}
