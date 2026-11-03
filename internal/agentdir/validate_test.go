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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestValidate(t *testing.T) {
	t.Parallel()
	valid := func() *Config { return &Config{Name: "coder", Harness: "opencode"} }
	tests := []struct {
		name    string
		modify  func(*Config)
		wantErr string
	}{
		{name: "valid", modify: func(*Config) {}},
		{name: "acp with a command", modify: func(c *Config) { c.Harness, c.Command = "acp", []string{"fakeacp"} }},
		{name: "unknown harness with a command", modify: func(c *Config) { c.Harness, c.Command = "gemini", []string{"gemini"} }},
		{name: "no harness", modify: func(c *Config) { c.Harness = "" }, wantErr: "no harness"},
		{name: "acp without a command", modify: func(c *Config) { c.Harness = "acp" }, wantErr: "harness acp needs a command"},
		{name: "unknown harness", modify: func(c *Config) { c.Harness = "gemini" }, wantErr: `unknown harness "gemini"`},
		{name: "reserved harness with a command", modify: func(c *Config) { c.Harness, c.Command = "script", []string{"fakeacp"} }, wantErr: `invalid harness "script"`},
		{name: "sandbox", modify: func(c *Config) { c.Sandbox = "vm" }, wantErr: `invalid --sandbox "vm"`},
		{name: "private", modify: func(c *Config) { c.Network.Private = "tailscal" }, wantErr: `invalid --private "tailscal": want tailscale, lan or none`},
		{name: "public", modify: func(c *Config) { c.Network.Public = "lan" }, wantErr: `invalid --public "lan"`},
		{name: "mount", modify: func(c *Config) { c.Mounts = []string{"/a:rel"} }, wantErr: "--mount: invalid mount"},
		{name: "workspace", modify: func(c *Config) { c.Workspace = "/a:/b:/c" }, wantErr: "--workspace: invalid mount"},
		{name: "secret name", modify: func(c *Config) { c.Secrets = []string{"NOT-A-NAME"} }, wantErr: "invalid --secret"},
		{name: "env", modify: func(c *Config) { c.Env = []string{"DEBUG"} }, wantErr: `invalid --env "DEBUG"`},
		{name: "port", modify: func(c *Config) { c.Port = 70000 }, wantErr: "invalid --port 70000"},
		{name: "identity role", modify: func(c *Config) { c.Network.Identities = map[string]string{"anna": "admin"} }, wantErr: "invalid --identity anna=admin"},
		{name: "memory", modify: func(c *Config) { c.Limits.Memory = "lots" }, wantErr: "invalid --memory"},
		{name: "cpus", modify: func(c *Config) { c.Limits.CPUs = "-1" }, wantErr: "invalid --cpus"},
		{name: "tool kinds and globs", modify: func(c *Config) { c.Approvals.Allow, c.Approvals.Deny = []string{"read", "switch_mode"}, []string{"*"} }},
		{name: "unknown allowed tool kind", modify: func(c *Config) { c.Approvals.Allow = []string{"reed"} }, wantErr: `invalid --allow-tools "reed"`},
		{name: "unknown denied tool kind", modify: func(c *Config) { c.Approvals.Deny = []string{"shell"} }, wantErr: `invalid --deny-tools "shell"`},
		{name: "telegram without a token", modify: func(c *Config) { c.Channels.Telegram = &TelegramConfig{} }, wantErr: "needs --telegram"},
		{name: "sign-in", modify: func(c *Config) {
			c.Auth = AuthConfig{AllowEmails: []string{"mom@example.com", "*@example.com"}, GoogleClientID: "id", GoogleClientSecret: "secret:GOOGLE_CLIENT_SECRET"}
		}},
		{name: "allow-email domain without star", modify: func(c *Config) { c.Auth.AllowEmails = []string{"example.com"} }, wantErr: `invalid --allow-email "example.com"`},
		{name: "allow-email wildcard domain", modify: func(c *Config) { c.Auth.AllowEmails = []string{"*@*.com"} }, wantErr: `invalid --allow-email "*@*.com"`},
		{name: "google client without secret", modify: func(c *Config) { c.Auth.GoogleClientID = "id" }, wantErr: "go together"},
		{name: "relay", modify: func(c *Config) {
			c.Network = NetworkConfig{Private: "https://relay.example.com", Public: "https://relay.example.com/"}
		}},
		{name: "relay with a path", modify: func(c *Config) { c.Network.Private = "https://relay.example.com/x" }, wantErr: `invalid --private "https://relay.example.com/x"`},
		{name: "two relays", modify: func(c *Config) {
			c.Network = NetworkConfig{Private: "https://a.example.com", Public: "https://b.example.com"}
		}, wantErr: "different relays"},
		{name: "backup", modify: func(c *Config) {
			c.Backup = BackupConfig{To: "s3://bucket/coder", Key: "secret:BACKUP_KEY", Interval: "1h", Retention: "14d"}
		}},
		{name: "backup without a key", modify: func(c *Config) { c.Backup.To = "/backups" }, wantErr: "--backup needs --backup-key"},
		{name: "backup to an unknown scheme", modify: func(c *Config) { c.Backup = BackupConfig{To: "ftp://x", Key: "k"} }, wantErr: `invalid --backup "ftp://x"`},
		{name: "backup interval", modify: func(c *Config) { c.Backup.Interval = "10s" }, wantErr: "--backup-interval 10s: want at least 1m"},
		{name: "backup retention", modify: func(c *Config) { c.Backup.Retention = "a week" }, wantErr: "--backup-retention: invalid duration"},
		{
			name:    "incomplete email",
			modify:  func(c *Config) { c.Channels.Email = &EmailConfig{Address: "a@x", IMAP: "imap:993"} },
			wantErr: "the email channel needs --email-password, --email-smtp, --email-trusted-authserv",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := valid()
			tc.modify(cfg)
			err := cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestCheckConfig(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	docs := filepath.Join(root, "docs")
	if err := os.Mkdir(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	d, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		cfg     Config
		secrets Secrets
		wantErr string
	}{
		{
			name: "valid",
			cfg: Config{
				Name: "coder", Harness: "opencode", Mounts: []string{docs + ":/docs:ro"}, Secrets: []string{"KEY"},
				Channels: ChannelsConfig{Telegram: &TelegramConfig{Token: "secret:BOT"}},
			},
			secrets: Secrets{"KEY": "k", "BOT": "b"},
		},
		{name: "relative workspace", cfg: Config{Name: "coder", Harness: "opencode", Workspace: "docs"}},
		{name: "invalid", cfg: Config{Name: "coder"}, wantErr: "no harness"},
		{name: "missing mount source", cfg: Config{Name: "coder", Harness: "opencode", Mounts: []string{"/nonexistent-a2a:/x"}}, wantErr: "the source does not exist"},
		{name: "duplicate destination", cfg: Config{Name: "coder", Harness: "opencode", Mounts: []string{docs + ":" + root}}, wantErr: "already uses the destination"},
		{name: "missing secret", cfg: Config{Name: "coder", Harness: "opencode", Secrets: []string{"KEY"}}, wantErr: "secret KEY is not stored"},
		{
			name:    "missing channel secret",
			cfg:     Config{Name: "coder", Harness: "opencode", Channels: ChannelsConfig{Discord: &DiscordConfig{Token: "secret:BOT"}}},
			wantErr: "--discord: secret BOT is not set",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := d.CheckConfig(&tc.cfg, tc.secrets)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("CheckConfig() error = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("CheckConfig() error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestSaveSettingsValidatesBeforeSaving(t *testing.T) {
	t.Parallel()
	d, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	err = d.SaveSettings(Settings{Harness: "opencode", Private: "tailscal"})
	var se *SettingsError
	if !errors.As(err, &se) {
		t.Fatalf("SaveSettings(--private tailscal) error = %v, want a SettingsError", err)
	}
	if d.Exists() {
		t.Fatalf("SaveSettings() saved an invalid config")
	}
}

func TestApplySettingsACPCommand(t *testing.T) {
	t.Parallel()
	existing, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := existing.SaveConfig(&Config{Name: "x", Harness: "acp", Command: []string{"fakeacp"}}); err != nil {
		t.Fatal(err)
	}
	empty, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		dir     Dir
		wantErr bool
	}{
		{name: "command in the saved config", dir: existing},
		{name: "no command", dir: empty, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := tc.dir.ApplySettings(Settings{Harness: "acp", Description: "x"})
			if (err != nil) != tc.wantErr {
				t.Fatalf("ApplySettings() error = %v, want error %v", err, tc.wantErr)
			}
		})
	}
}

func TestLoadConfigValidates(t *testing.T) {
	t.Parallel()
	d, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SaveConfig(&Config{Name: "coder", Harness: "opencode", Network: NetworkConfig{Private: "tailscal"}}); err != nil {
		t.Fatal(err)
	}
	var se *SettingsError
	if _, err := d.LoadConfig(); !errors.As(err, &se) {
		t.Fatalf("LoadConfig() error = %v, want a SettingsError", err)
	}
	if _, err := d.ReadConfig(); err != nil {
		t.Fatalf("ReadConfig() error = %v", err)
	}
}

func TestParseConfigWorkspace(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		yaml          string
		wantWorkspace string
		wantMounts    []string
	}{
		{name: "default", yaml: "harness: opencode\n"},
		{name: "first mount is the workspace", yaml: "mounts: [/src/app, /docs:/docs:ro]\n", wantWorkspace: "/src/app", wantMounts: []string{"/docs:/docs:ro"}},
		{name: "only the workspace mount", yaml: "mounts: [/src/app]\n", wantWorkspace: "/src/app"},
		{name: "default workspace with mounts", yaml: "workspace: .\nmounts: [/docs]\n", wantMounts: []string{"/docs"}},
		{name: "workspace", yaml: "workspace: /src/app\nmounts: [/docs]\n", wantWorkspace: "/src/app", wantMounts: []string{"/docs"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseConfig([]byte(tc.yaml))
			if err != nil {
				t.Fatalf("ParseConfig() error = %v", err)
			}
			if got.Workspace != tc.wantWorkspace {
				t.Fatalf("ParseConfig().Workspace = %q, want %q", got.Workspace, tc.wantWorkspace)
			}
			if diff := cmp.Diff(tc.wantMounts, got.Mounts); diff != "" {
				t.Fatalf("ParseConfig().Mounts wrong result (-want +got) diff = %s", diff)
			}
			data, err := MarshalConfig(got)
			if err != nil {
				t.Fatalf("MarshalConfig() error = %v", err)
			}
			again, err := ParseConfig(data)
			if err != nil {
				t.Fatalf("ParseConfig(MarshalConfig()) error = %v", err)
			}
			if diff := cmp.Diff(got, again); diff != "" {
				t.Fatalf("ParseConfig(MarshalConfig()) wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestMounts(t *testing.T) {
	t.Parallel()
	d := Dir{Root: "/src/app"}
	got, err := d.Mounts(&Config{Workspace: "sub:/work", Mounts: []string{"/docs:ro"}})
	if err != nil {
		t.Fatalf("Mounts() error = %v", err)
	}
	want := []Mount{{Src: "/src/app/sub", Dst: "/work"}, {Src: "/docs", Dst: "/docs", ReadOnly: true}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("Mounts() wrong result (-want +got) diff = %s", diff)
	}
}

func TestCheckHarness(t *testing.T) {
	t.Parallel()
	cfg := &Config{Harness: "opencode"}
	if err := cfg.CheckHarness("opencode"); err != nil {
		t.Fatalf("CheckHarness(opencode) error = %v, want nil", err)
	}
	want := "this agent runs opencode; switch with `a2a serve config set --harness claude`"
	if err := cfg.CheckHarness("claude"); err == nil || err.Error() != want {
		t.Fatalf("CheckHarness(claude) error = %v, want %q", err, want)
	}
}
