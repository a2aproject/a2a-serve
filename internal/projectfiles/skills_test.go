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

package projectfiles

import "testing"

func TestIsAgentConfig(t *testing.T) {
	t.Parallel()
	tests := []struct {
		path string
		want bool
	}{
		{path: ".claude/settings.json", want: true},
		{path: "opencode.json", want: true},
		{path: ".opencode/agent/reviewer.md", want: true},
		{path: ".agents/hooks.json", want: true},
		{path: "_agent/mcp_config.json", want: true},
		{path: ".gemini/config.json", want: true},
		{path: ".agents/skills/triage/SKILL.md"},
		{path: ".opencode/skills/lint/SKILL.md"},
		{path: ".agents/skills"},
		{path: ".claude/skills/triage/SKILL.md"},
		{path: ".agentsx/hooks.json"},
		{path: "AGENTS.md"},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()
			if got := IsAgentConfig(tc.path); got != tc.want {
				t.Fatalf("IsAgentConfig(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}
