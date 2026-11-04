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

func fullConfig() *Config {
	return &Config{
		Name:        "coder",
		Description: "writes code",
		Harness:     "opencode",
		Workspace:   "/src/app",
		Mounts:      []string{"/docs:ro", "/data:/data"},
		Secrets:     []string{"KEY", "OTHER"},
		Env:         []string{"DEBUG=1", "MODE=fast"},
		Port:        4000,
		Network:     NetworkConfig{Private: "tailscale", Identities: map[string]string{"anna": "member"}},
		Channels: ChannelsConfig{
			Telegram: &TelegramConfig{Token: "secret:TELEGRAM_BOT_TOKEN"},
			Email:    &EmailConfig{Address: "a@x", IMAP: "i:993", SMTP: "s:465", Password: "secret:EMAIL_PASSWORD", TrustedAuthServ: "mx"},
		},
		Approvals: ApprovalsConfig{Allow: []string{"read", "search"}, OwnerChat: "telegram"},
		Limits:    LimitsConfig{Memory: "8g"},
	}
}

func TestUnset(t *testing.T) {
	t.Parallel()
	tests := []struct {
		setting     string
		entries     []string
		modify      func(*Config)
		wantDeleted []string
		wantErr     bool
	}{
		{setting: "description", modify: func(c *Config) { c.Description = "" }},
		{setting: "name", modify: func(c *Config) { c.Name = "app" }},
		{setting: "port", modify: func(c *Config) { c.Port = 0 }},
		{setting: "private", modify: func(c *Config) { c.Network.Private = "" }},
		{setting: "memory", modify: func(c *Config) { c.Limits.Memory = "" }},
		{setting: "workspace", modify: func(c *Config) { c.Workspace = "" }},
		{setting: "env", entries: []string{"DEBUG"}, modify: func(c *Config) { c.Env = []string{"MODE=fast"} }},
		{setting: "env", modify: func(c *Config) { c.Env = nil }},
		{setting: "mount", entries: []string{"/docs"}, modify: func(c *Config) { c.Mounts = []string{"/data:/data"} }},
		{setting: "allow-tools", entries: []string{"search"}, modify: func(c *Config) { c.Approvals.Allow = []string{"read"} }},
		{setting: "identity", entries: []string{"anna"}, modify: func(c *Config) { c.Network.Identities = nil }},
		{setting: "secret", entries: []string{"KEY"}, modify: func(c *Config) { c.Secrets = []string{"OTHER"} }, wantDeleted: []string{"KEY"}},
		{setting: "telegram", wantErr: true},
		{setting: "email", wantErr: true},
		{setting: "email-password", wantErr: true},
		{setting: "tailscale-authkey", modify: func(*Config) {}, wantDeleted: []string{SecretTailscaleKey}},
		{setting: "env", entries: []string{"MISSING"}, wantErr: true},
		{setting: "port", entries: []string{"1"}, wantErr: true},
		{setting: "agent", wantErr: true},
		{setting: "nope", wantErr: true},
	}
	d := Dir{Root: "/src/app"}
	for _, tc := range tests {
		t.Run(tc.setting, func(t *testing.T) {
			t.Parallel()
			got := fullConfig()
			deleted, err := d.Unset(got, tc.setting, tc.entries)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Unset() error = %v, want error %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			want := fullConfig()
			tc.modify(want)
			if diff := cmp.Diff(want, got); diff != "" {
				t.Fatalf("Unset() wrong result (-want +got) diff = %s", diff)
			}
			if diff := cmp.Diff(tc.wantDeleted, deleted); diff != "" {
				t.Fatalf("Unset() wrong deleted secrets (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestDiff(t *testing.T) {
	t.Parallel()
	old := fullConfig()
	next := old.Clone()
	next.Sandbox = "local"
	next.Description = "reviews code"
	next.Approvals.Allow = []string{"read", "edit"}
	next.Channels.Telegram = nil
	got := Diff(old, next)
	want := []Change{
		{Setting: "description", Old: "writes code", New: "reviews code", Live: true},
		{Setting: "sandbox", New: "local"},
		{Setting: "telegram", Old: "secret:TELEGRAM_BOT_TOKEN", Live: true},
		{Setting: "allow-tools", Added: []string{"edit"}, Removed: []string{"search"}, Live: true},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("Diff() wrong result (-want +got) diff = %s", diff)
	}
	if diff := cmp.Diff(fullConfig(), old); diff != "" {
		t.Fatalf("Clone() shares state with the original (-want +got) diff = %s", diff)
	}
}

func TestCommands(t *testing.T) {
	t.Parallel()
	cfg := &Config{
		Name:     "coder",
		Harness:  "acp",
		Command:  []string{"gemini", "--experimental-acp"},
		Secrets:  []string{"KEY"},
		Network:  NetworkConfig{Public: "ngrok"},
		Channels: ChannelsConfig{Telegram: &TelegramConfig{Token: "secret:BOT"}},
		Backup:   BackupConfig{To: "gs://bucket/coder", Key: "secret:BACKUP_KEY", Interval: "1h"},
	}
	var got []string
	for _, args := range cfg.Commands(Secrets{SecretNgrokToken: "n"}) {
		got = append(got, ShellJoin(args))
	}
	want := []string{
		"a2a-serve start acp --name coder --command 'gemini --experimental-acp' --secret KEY --public ngrok",
		"a2a-serve config set --backup gs://bucket/coder --backup-key env:BACKUP_KEY --backup-interval 1h --ngrok-authtoken env:NGROK_AUTHTOKEN",
		"a2a-serve channel add telegram --token env:BOT",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("Commands() wrong result (-want +got) diff = %s", diff)
	}
}

func TestDeleteSecrets(t *testing.T) {
	t.Parallel()
	d, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SaveSecrets(Secrets{"A": "1", "B": "2"}); err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteSecrets("A", "MISSING"); err != nil {
		t.Fatalf("DeleteSecrets() error = %v", err)
	}
	got, err := d.LoadSecrets()
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(Secrets{"B": "2"}, got); diff != "" {
		t.Fatalf("DeleteSecrets() wrong result (-want +got) diff = %s", diff)
	}
}
