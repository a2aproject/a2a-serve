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

package agentdir

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestMerge3(t *testing.T) {
	t.Parallel()
	base := &Config{
		Name: "o11y", Harness: "opencode", Description: "v1", Port: 9000,
		Mounts:    []string{"/a:/kube:ro", "/data:/data"},
		Env:       []string{"LEVEL=info", "MODE=a"},
		Approvals: ApprovalsConfig{Allow: []string{"read"}},
		Limits:    LimitsConfig{Memory: "4g"},
	}
	tests := []struct {
		name          string
		local         func(c *Config)
		upstream      func(c *Config)
		want          func(c *Config)
		wantConflicts []Conflict
	}{
		{
			name:     "upstream change",
			upstream: func(c *Config) { c.Description = "v2"; c.Limits.Memory = "8g" },
			want:     func(c *Config) { c.Description = "v2"; c.Limits.Memory = "8g" },
		},
		{
			name:  "local change",
			local: func(c *Config) { c.Description = "mine" },
			want:  func(c *Config) { c.Description = "mine" },
		},
		{
			name:     "same change on both sides",
			local:    func(c *Config) { c.Sandbox = SandboxLocal },
			upstream: func(c *Config) { c.Sandbox = SandboxLocal },
			want:     func(c *Config) { c.Sandbox = SandboxLocal },
		},
		{
			name:          "conflict keeps local",
			local:         func(c *Config) { c.Description = "mine" },
			upstream:      func(c *Config) { c.Description = "v2" },
			want:          func(c *Config) { c.Description = "mine" },
			wantConflicts: []Conflict{{Setting: "description"}},
		},
		{
			name:     "upstream removes a scalar",
			upstream: func(c *Config) { c.Limits.Memory = "" },
			want:     func(c *Config) { c.Limits.Memory = "" },
		},
		{
			name: "list entries by key",
			local: func(c *Config) {
				c.Mounts = []string{"/home/me/kube:/kube:ro", "/data:/data"}
				c.Env = []string{"LEVEL=debug", "MODE=a"}
			},
			upstream: func(c *Config) { c.Mounts = []string{"/a:/kube:ro"}; c.Env = []string{"LEVEL=info", "MODE=b", "NEW=1"} },
			want: func(c *Config) {
				c.Mounts = []string{"/home/me/kube:/kube:ro"}
				c.Env = []string{"LEVEL=debug", "MODE=b", "NEW=1"}
			},
		},
		{
			name:          "list entry conflict",
			local:         func(c *Config) { c.Env = []string{"LEVEL=debug", "MODE=a"} },
			upstream:      func(c *Config) { c.Env = []string{"LEVEL=warn", "MODE=a"} },
			want:          func(c *Config) { c.Env = []string{"LEVEL=debug", "MODE=a"} },
			wantConflicts: []Conflict{{Setting: "env", Key: "LEVEL"}},
		},
		{
			name:     "entry removed locally stays removed",
			local:    func(c *Config) { c.Approvals.Allow = nil },
			upstream: func(c *Config) { c.Approvals.Allow = []string{"read", "search"} },
			want:     func(c *Config) { c.Approvals.Allow = []string{"search"} },
		},
		{
			name:  "entry moved to deny-tools stays there",
			local: func(c *Config) { c.Approvals.Allow, c.Approvals.Deny = nil, []string{"read"} },
			upstream: func(c *Config) {
				c.Approvals.Allow = []string{"read", "tool:grafana.*"}
				c.Approvals.Deny = []string{"execute"}
			},
			want: func(c *Config) {
				c.Approvals.Allow, c.Approvals.Deny = []string{"tool:grafana.*"}, []string{"read", "execute"}
			},
		},
		{
			name:          "entry moved locally and removed upstream",
			local:         func(c *Config) { c.Approvals.Allow, c.Approvals.Deny = []string{"command:git *"}, []string{"read"} },
			upstream:      func(c *Config) { c.Approvals.Allow = []string{"command:git *"} },
			want:          func(c *Config) { c.Approvals.Allow, c.Approvals.Deny = []string{"command:git *"}, []string{"read"} },
			wantConflicts: []Conflict{{Setting: "deny-tools", Key: "read"}},
		},
		{
			name:     "owner settings stay local",
			local:    func(c *Config) { c.Port = 9100; c.Network.Private = "tailscale" },
			upstream: func(c *Config) { c.Port = 9200; c.Name = "other"; c.Network.Public = "tailscale" },
			want:     func(c *Config) { c.Port = 9100; c.Network.Private = "tailscale" },
		},
		{
			name:     "command",
			upstream: func(c *Config) { c.Command = []string{"agent", "--acp", "a b"} },
			want:     func(c *Config) { c.Command = []string{"agent", "--acp", "a b"} },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			apply := func(edit func(*Config)) *Config {
				c := base.Clone()
				if edit != nil {
					edit(c)
				}
				return c
			}
			got, conflicts := Merge3(base, apply(tc.local), apply(tc.upstream))
			if diff := cmp.Diff(apply(tc.want), got); diff != "" {
				t.Fatalf("Merge3() wrong result (-want +got) diff = %s", diff)
			}
			if diff := cmp.Diff(tc.wantConflicts, conflicts); diff != "" {
				t.Fatalf("Merge3() wrong conflicts (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestUnusedSecrets(t *testing.T) {
	t.Parallel()
	old := &Config{Secrets: []string{"A", "B"}, Messengers: MessengersConfig{Telegram: &TelegramConfig{Token: "secret:TG"}}}
	next := &Config{Secrets: []string{"B", "C"}}
	if diff := cmp.Diff([]string{"A", "TG"}, UnusedSecrets(old, next)); diff != "" {
		t.Fatalf("UnusedSecrets() wrong result (-want +got) diff = %s", diff)
	}
}
