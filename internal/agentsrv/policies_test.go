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
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
)

func TestPoliciesKeepServersOfEarlierSessions(t *testing.T) {
	t.Parallel()
	dir := agentdir.Dir{Root: t.TempDir()}
	cfg := &agentdir.Config{Harness: "claude", Approvals: agentdir.ApprovalsConfig{Deny: []string{"command:git push *"}}}
	p := newPolicies(dir)

	for _, w := range []struct {
		lockHome bool
		servers  []string
	}{{false, []string{"github"}}, {true, []string{"grafana"}}, {false, nil}} {
		if _, err := p.write(cfg, w.lockHome, w.servers); err != nil {
			t.Fatalf("policies.write() error = %v", err)
		}
	}

	tests := []struct {
		lockHome   bool
		wantStrict bool
	}{{lockHome: false}, {lockHome: true, wantStrict: true}}
	for _, tc := range tests {
		data, err := os.ReadFile(filepath.Join(dir.PolicyDir(tc.lockHome), "managed-settings.json"))
		if err != nil {
			t.Fatalf("os.ReadFile() error = %v", err)
		}
		var got struct {
			Permissions struct {
				Ask []string `json:"ask"`
			} `json:"permissions"`
			Strict []string `json:"strictPluginOnlyCustomization"`
		}
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("json.Unmarshal(%s) error = %v", data, err)
		}
		want := []string{"Bash", "Edit", "Write", "NotebookEdit", "WebFetch", "WebSearch", "mcp__github", "mcp__grafana"}
		if diff := cmp.Diff(want, got.Permissions.Ask); diff != "" {
			t.Fatalf("policy of lockHome=%t ask rules wrong result (-want +got) diff = %s", tc.lockHome, diff)
		}
		if strict := len(got.Strict) > 0; strict != tc.wantStrict {
			t.Fatalf("policy of lockHome=%t restricts MCP servers = %t, want %t", tc.lockHome, strict, tc.wantStrict)
		}
	}
}
