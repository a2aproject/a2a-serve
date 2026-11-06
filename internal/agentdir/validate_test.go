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
	"slices"
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
		{name: "model", modify: func(c *Config) { c.Model = "openrouter/~google/gemini-flash-latest" }},
		{name: "model with a profile command", modify: func(c *Config) { c.Harness, c.Command, c.Model = "claude", []string{"my-claude"}, "sonnet" }},
		{name: "model of acp", modify: func(c *Config) { c.Harness, c.Command, c.Model = "acp", []string{"fakeacp"}, "m" }, wantErr: `invalid --model "m": needs the agent claude or opencode`},
		{name: "no agent", modify: func(c *Config) { c.Harness = "" }, wantErr: "no agent"},
		{name: "acp without a command", modify: func(c *Config) { c.Harness = "acp" }, wantErr: "agent acp needs a command"},
		{name: "unknown harness", modify: func(c *Config) { c.Harness = "gemini" }, wantErr: `unknown agent "gemini"`},
		{name: "reserved agent with a command", modify: func(c *Config) { c.Harness, c.Command = "script", []string{"fakeacp"} }, wantErr: `invalid agent "script"`},
		{name: "sandbox", modify: func(c *Config) { c.Sandbox = "vm" }, wantErr: `invalid --sandbox "vm"`},
		{name: "private", modify: func(c *Config) { c.Network.Private = "tailscal" }, wantErr: `invalid --private "tailscal": want tailscale or none`},
		{name: "removed lan", modify: func(c *Config) { c.Network.Private = "lan" }, wantErr: `invalid --private "lan": lan was removed`},
		{name: "public", modify: func(c *Config) { c.Network.Public = "lan" }, wantErr: `invalid --public "lan"`},
		{name: "mount", modify: func(c *Config) { c.Mounts = []string{"/a:rel"} }, wantErr: `invalid --mount "/a:rel": destination must be absolute`},
		{name: "workspace", modify: func(c *Config) { c.Workspace = "/a:/b:/c" }, wantErr: `invalid --workspace "/a:/b:/c": want src[:dst][:ro]`},
		{name: "secret name", modify: func(c *Config) { c.Secrets = []string{"NOT-A-NAME"} }, wantErr: "invalid --secret"},
		{name: "env", modify: func(c *Config) { c.Env = []string{"DEBUG"} }, wantErr: `invalid --env "DEBUG"`},
		{name: "port", modify: func(c *Config) { c.Port = 70000 }, wantErr: `invalid --port "70000"`},
		{name: "memory", modify: func(c *Config) { c.Limits.Memory = "lots" }, wantErr: "invalid --memory"},
		{name: "cpus", modify: func(c *Config) { c.Limits.CPUs = "-1" }, wantErr: "invalid --cpus"},
		{name: "tool kinds and globs", modify: func(c *Config) { c.Approvals.Allow, c.Approvals.Deny = []string{"read", "switch_mode"}, []string{"*"} }},
		{name: "unknown allowed tool kind", modify: func(c *Config) { c.Approvals.Allow = []string{"reed"} }, wantErr: `invalid --allow-tools "reed"`},
		{name: "unknown denied tool kind", modify: func(c *Config) { c.Approvals.Deny = []string{"shell"} }, wantErr: `invalid --deny-tools "shell"`},
		{name: "disabled MCP server", modify: func(c *Config) { c.MCP.Disabled = []string{"github"} }},
		{name: "empty disabled MCP server", modify: func(c *Config) { c.MCP.Disabled = []string{""} }, wantErr: `invalid --disable-mcp ""`},
		{name: "owner chat", modify: func(c *Config) { c.Approvals.OwnerChat = "telegram:anna" }},
		{name: "owner chat channel", modify: func(c *Config) { c.Approvals.OwnerChat = "fax" }, wantErr: `invalid --owner-chat "fax"`},
		{name: "owner chat without who", modify: func(c *Config) { c.Approvals.OwnerChat = "telegram:" }, wantErr: `invalid --owner-chat "telegram:"`},
		{name: "removed approvers", modify: func(c *Config) { c.Approvals.RemovedApprovers = []string{"pair:anna"} }, wantErr: "approvals.approvers was removed"},
		{name: "telegram without a token", modify: func(c *Config) { c.Channels.Telegram = &TelegramConfig{} }, wantErr: "the telegram channel needs a token"},
		{name: "sign-in", modify: func(c *Config) {
			c.Policy.AllowEmails = []string{"mom@example.com", "*@example.com"}
			c.Auth = AuthConfig{GoogleClientID: "id", GoogleClientSecret: "secret:GOOGLE_CLIENT_SECRET"}
		}},
		{name: "allow-email domain without star", modify: func(c *Config) { c.Policy.AllowEmails = []string{"example.com"} }, wantErr: `invalid --allow-email "example.com"`},
		{name: "allow-email wildcard domain", modify: func(c *Config) { c.Policy.AllowEmails = []string{"*@*.com"} }, wantErr: `invalid --allow-email "*@*.com"`},
		{name: "access", modify: func(c *Config) {
			c.Policy = PolicyConfig{PrivateAllowIncoming: AccessPaired, PublicAllowIncoming: AccessAnyone, PrivateAllowOutgoing: AccessContacts}
		}},
		{name: "private-allow-incoming", modify: func(c *Config) { c.Policy.PrivateAllowIncoming = AccessAnyone }, wantErr: `invalid --private-allow-incoming "anyone": want everyone or paired`},
		{name: "public-allow-incoming", modify: func(c *Config) { c.Policy.PublicAllowIncoming = AccessEveryone }, wantErr: `invalid --public-allow-incoming "everyone": want paired or anyone`},
		{name: "private-allow-outgoing", modify: func(c *Config) { c.Policy.PrivateAllowOutgoing = AccessPaired }, wantErr: `invalid --private-allow-outgoing "paired": want everyone or contacts`},
		{name: "public ngrok", modify: func(c *Config) { c.Network.Public = "ngrok" }, wantErr: `invalid --public "ngrok": want tailscale or none`},
		{name: "google client without secret", modify: func(c *Config) {
			c.Policy.AllowEmails = []string{"mom@example.org"}
			c.Auth.GoogleClientID = "id"
		}, wantErr: "go together"},
		{name: "google client without allowed emails", modify: func(c *Config) {
			c.Auth = AuthConfig{GoogleClientID: "id", GoogleClientSecret: "secret"}
		}, wantErr: "--google-client-id needs --allow-email"},
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
		{name: "backup interval", modify: func(c *Config) { c.Backup.Interval = "10s" }, wantErr: `invalid --backup-interval "10s": want at least 1m`},
		{name: "backup retention", modify: func(c *Config) { c.Backup.Retention = "a week" }, wantErr: `invalid --backup-retention "a week": want a number of days`},
		{name: "task ttl", modify: func(c *Config) { c.TaskTTL = "a month" }, wantErr: `invalid --task-ttl "a month": want a number of days`},
		{name: "task ttl too short", modify: func(c *Config) { c.TaskTTL = "10m" }, wantErr: `invalid --task-ttl "10m": want at least 1h`},
		{
			name: "email with OAuth",
			modify: func(c *Config) {
				c.Channels.Email = &EmailConfig{
					Address: "a@x", IMAP: "imap:993", SMTP: "smtp:465", TrustedAuthServ: "mx",
					OAuthTokenURL: "https://login.example.com/token", OAuthClientID: "id", OAuthRefreshToken: "secret:EMAIL_OAUTH_REFRESH_TOKEN",
				}
			},
		},
		{
			name: "incomplete email OAuth",
			modify: func(c *Config) {
				c.Channels.Email = &EmailConfig{Address: "a@x", IMAP: "imap:993", SMTP: "smtp:465", TrustedAuthServ: "mx", OAuthRefreshToken: "r"}
			},
			wantErr: "the email channel needs --email-oauth-client-id, --email-oauth-token-url",
		},
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
		{name: "invalid", cfg: Config{Name: "coder"}, wantErr: "no agent"},
		{name: "missing mount source", cfg: Config{Name: "coder", Harness: "opencode", Mounts: []string{"/nonexistent-a2a:/x"}}, wantErr: "the source does not exist"},
		{name: "duplicate destination", cfg: Config{Name: "coder", Harness: "opencode", Mounts: []string{docs + ":" + root}}, wantErr: "already uses the destination"},
		{name: "missing secret", cfg: Config{Name: "coder", Harness: "opencode", Secrets: []string{"KEY"}}, wantErr: "secret KEY is not stored"},
		{
			name:    "secret with a line break",
			cfg:     Config{Name: "coder", Harness: "opencode", Secrets: []string{"MULTI"}},
			secrets: Secrets{"MULTI": "line1\nline2"},
			wantErr: "the value of MULTI has a line break, which docker can't pass to the container",
		},
		{
			name:    "env with a line break",
			cfg:     Config{Name: "coder", Harness: "opencode", Sandbox: SandboxDocker, Env: []string{"MULTI=line1\r\nline2"}},
			wantErr: "the value of MULTI has a line break, which docker can't pass to the container",
		},
		{
			name:    "secret with a line break without docker",
			cfg:     Config{Name: "coder", Harness: "opencode", Sandbox: SandboxLocal, Secrets: []string{"MULTI"}, Env: []string{"E=a\nb"}},
			secrets: Secrets{"MULTI": "line1\nline2"},
		},
		{
			name:    "missing channel secret",
			cfg:     Config{Name: "coder", Harness: "opencode", Channels: ChannelsConfig{Discord: &DiscordConfig{Token: "secret:BOT"}}},
			wantErr: "discord token (a2a-serve channel add discord --token): secret BOT is not set",
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

func TestCheckConfigBroadMounts(t *testing.T) {
	t.Parallel()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	project := t.TempDir()
	tests := []struct {
		name    string
		root    string
		cfg     Config
		wantErr string
	}{
		{name: "project directory", root: project, cfg: Config{Workspace: project}},
		{name: "agent in home", root: home, wantErr: "the workspace " + home + " includes your home directory"},
		{name: "home workspace", root: project, cfg: Config{Workspace: home}, wantErr: "--workspace " + home + " includes your home directory"},
		{name: "root workspace", root: project, cfg: Config{Workspace: "/"}, wantErr: "--workspace / includes your home directory"},
		{name: "root mount", root: project, cfg: Config{Mounts: []string{"/:/host:ro"}}, wantErr: "--mount /:/host:ro includes your home directory"},
		{name: "home allowed", root: project, cfg: Config{Workspace: home, AllowBroadMounts: []string{home}}},
		{name: "another path allowed", root: project, cfg: Config{Workspace: "/", AllowBroadMounts: []string{home}}, wantErr: "--allow-broad-mount /"},
		{name: "local sandbox", root: home, cfg: Config{Sandbox: SandboxLocal}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, err := Open(tc.root)
			if err != nil {
				t.Fatal(err)
			}
			tc.cfg.Name, tc.cfg.Harness = "coder", "opencode"
			err = d.CheckConfig(&tc.cfg, nil)
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
		{name: "default", yaml: "agent: opencode\n"},
		{name: "mounts without workspace", yaml: "mounts: [/src/app, /docs:/docs:ro]\n", wantMounts: []string{"/src/app", "/docs:/docs:ro"}},
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

func TestUnknownKeys(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		data string
		want []UnknownKey
	}{
		{
			name: "known keys",
			data: "name: coder\nagent: opencode\nnetwork:\n  private: tailscale\nchannels:\n  telegram:\n    token: secret:T\napprovals:\n  allow: [read]\n",
		},
		{
			name: "unknown keys at any depth",
			data: "name: coder\ncard: {}\nnetwork:\n  allowlist: [example.com]\nchannels:\n  telegram:\n    chat: x\n",
			want: []UnknownKey{{Path: "card"}, {Path: "channels.telegram.chat"}, {Path: "network.allowlist"}},
		},
		{
			name: "typos",
			data: "sandbx: local\nTaskTTL: 7d\nnetwork:\n  privat: tailscale\n",
			want: []UnknownKey{{Path: "TaskTTL", Suggestion: "taskTTL"}, {Path: "network.privat", Suggestion: "network.private"}, {Path: "sandbx", Suggestion: "sandbox"}},
		},
		{
			name: "removed keys are not suggested",
			data: "harnes: opencode\n",
			want: []UnknownKey{{Path: "harnes"}},
		},
		{
			name: "legacy keys",
			data: "harness: opencode\napprovals:\n  approvers: [owner]\n",
		},
		{
			name: "snake_case keys",
			data: "task_ttl: 7d\npolicy:\n  allow_emails: [a@example.com]\nchannels:\n  email:\n    trusted_authserv: mx\n",
		},
		{
			name: "snake_case key besides its camelCase key",
			data: "taskTTL: 7d\ntask_ttl: 1d\n",
			want: []UnknownKey{{Path: "task_ttl", Suggestion: "taskTTL"}},
		},
		{
			name: "empty",
			data: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := UnknownKeys([]byte(tc.data))
			if err != nil {
				t.Fatalf("UnknownKeys() error = %v", err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("UnknownKeys() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestUnknownKeyString(t *testing.T) {
	t.Parallel()
	tests := []struct {
		key  UnknownKey
		want string
	}{
		{key: UnknownKey{Path: "sandbx", Suggestion: "sandbox"}, want: "config.yaml has an unknown key sandbx: did you mean sandbox?"},
		{key: UnknownKey{Path: "card"}, want: "config.yaml has an unknown key card, which the server ignores"},
	}
	for _, tc := range tests {
		if got := tc.key.String(); got != tc.want {
			t.Fatalf("UnknownKey.String() = %q, want %q", got, tc.want)
		}
	}
}

func TestUpdateConfigKeepsCommentsAndUnknownKeys(t *testing.T) {
	t.Parallel()
	d, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Init(); err != nil {
		t.Fatal(err)
	}
	const orig = `# my agent
name: coder # the name
agent: opencode
sandbx: local
# who may call
policy:
    privateAllowIncoming: paired
    future: true
approvals:
    allow: [read] # safe
`
	if err := os.WriteFile(d.ConfigPath(), []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	err = d.UpdateConfig(func(c *Config) error {
		c.Name = "writer"
		c.Policy.PrivateAllowIncoming = ""
		c.Model = "m"
		return nil
	})
	if err != nil {
		t.Fatalf("UpdateConfig() error = %v", err)
	}
	got, err := os.ReadFile(d.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	const want = `# my agent
name: writer # the name
agent: opencode
sandbx: local
# who may call
policy:
    future: true
approvals:
    allow: [read] # safe
model: m
`
	if diff := cmp.Diff(want, string(got)); diff != "" {
		t.Fatalf("UpdateConfig() wrong file (-want +got) diff = %s", diff)
	}
}

func TestParseConfigSnakeCaseKeys(t *testing.T) {
	t.Parallel()
	data := `agent: opencode
task_ttl: 7d
network:
  tailscale_control_url: https://headscale.example.com
policy:
  private_access: paired
  public_access: anyone
  allow_emails: [a@example.com]
approvals:
  owner_chat: telegram
auth:
  google_client_id: id
  google_client_secret: secret:GOOGLE_CLIENT_SECRET
channels:
  email:
    trusted_authserv: mx
    oauth_token_url: https://login.example.com/token
    oauth_client_id: client
    oauth_client_secret: secret:EMAIL_OAUTH_CLIENT_SECRET
    oauth_refresh_token: secret:EMAIL_OAUTH_REFRESH_TOKEN
`
	camel := `agent: opencode
taskTTL: 7d
network:
  tailscaleControlURL: https://headscale.example.com
policy:
  privateAllowIncoming: paired
  publicAllowIncoming: anyone
  allowEmails: [a@example.com]
approvals:
  ownerChat: telegram
auth:
  googleClientID: id
  googleClientSecret: secret:GOOGLE_CLIENT_SECRET
channels:
  email:
    trustedAuthServ: mx
    oauthTokenURL: https://login.example.com/token
    oauthClientID: client
    oauthClientSecret: secret:EMAIL_OAUTH_CLIENT_SECRET
    oauthRefreshToken: secret:EMAIL_OAUTH_REFRESH_TOKEN
`
	want, err := ParseConfig([]byte(camel))
	if err != nil {
		t.Fatalf("ParseConfig() error = %v", err)
	}
	got, err := ParseConfig([]byte(data))
	if err != nil {
		t.Fatalf("ParseConfig() error = %v", err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("ParseConfig() wrong result (-want +got) diff = %s", diff)
	}
	if want.TaskTTL != "7d" || want.Channels.Email == nil || want.Channels.Email.OAuthRefreshToken == "" {
		t.Fatalf("ParseConfig() = %+v, want the camelCase keys read", want)
	}
	legacy, err := LegacyKeys([]byte(data))
	if err != nil {
		t.Fatalf("LegacyKeys() error = %v", err)
	}
	wantLegacy := []string{
		"auth.google_client_id", "auth.google_client_secret", "approvals.owner_chat",
		"channels.email.oauth_client_id", "channels.email.oauth_client_secret", "channels.email.oauth_refresh_token",
		"channels.email.oauth_token_url", "channels.email.trusted_authserv",
		"network.tailscale_control_url", "policy.allow_emails", "policy.private_access", "policy.public_access", "task_ttl",
	}
	slices.Sort(wantLegacy)
	if diff := cmp.Diff(wantLegacy, legacy); diff != "" {
		t.Fatalf("LegacyKeys() wrong result (-want +got) diff = %s", diff)
	}
	saved, err := MarshalConfig(got)
	if err != nil {
		t.Fatalf("MarshalConfig() error = %v", err)
	}
	if legacy, err := LegacyKeys(saved); err != nil || len(legacy) > 0 {
		t.Fatalf("LegacyKeys(MarshalConfig()) = %v, %v, want no snake_case keys", legacy, err)
	}
}

func TestParseConfigRenamedKeys(t *testing.T) {
	t.Parallel()
	data := "name: coder\nagent: opencode\npolicy:\n  privateAccess: paired\n  publicAccess: anyone\n"
	got, err := ParseConfig([]byte(data))
	if err != nil {
		t.Fatalf("ParseConfig() error = %v", err)
	}
	want := PolicyConfig{PrivateAllowIncoming: AccessPaired, PublicAllowIncoming: AccessAnyone}
	if diff := cmp.Diff(want, got.Policy); diff != "" {
		t.Fatalf("ParseConfig() wrong policy (-want +got) diff = %s", diff)
	}
	legacy, err := LegacyKeys([]byte(data))
	if err != nil {
		t.Fatalf("LegacyKeys() error = %v", err)
	}
	if diff := cmp.Diff([]string{"policy.privateAccess", "policy.publicAccess"}, legacy); diff != "" {
		t.Fatalf("LegacyKeys() wrong result (-want +got) diff = %s", diff)
	}
	merged, err := mergeConfig([]byte(data), got)
	if err != nil {
		t.Fatalf("mergeConfig() error = %v", err)
	}
	wantSaved := "name: coder\nagent: opencode\npolicy:\n    privateAllowIncoming: paired\n    publicAllowIncoming: anyone\n"
	if diff := cmp.Diff(wantSaved, string(merged)); diff != "" {
		t.Fatalf("mergeConfig() wrong file (-want +got) diff = %s", diff)
	}
}

func TestParseConfigAgent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		yaml string
	}{
		{name: "agent", yaml: "agent: opencode\n"},
		{name: "legacy harness key", yaml: "harness: opencode\n"},
		{name: "agent wins over the legacy key", yaml: "agent: opencode\nharness: claude\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseConfig([]byte(tc.yaml))
			if err != nil {
				t.Fatalf("ParseConfig() error = %v", err)
			}
			if got.Harness != "opencode" {
				t.Fatalf("ParseConfig().Harness = %q, want %q", got.Harness, "opencode")
			}
			data, err := MarshalConfig(got)
			if err != nil {
				t.Fatalf("MarshalConfig() error = %v", err)
			}
			if !strings.Contains(string(data), "\nagent: opencode\n") || strings.Contains(string(data), "harness:") {
				t.Fatalf("MarshalConfig() = %q, want only the agent key", data)
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
	want := "this agent runs opencode; switch with `a2a-serve config set --agent claude`"
	if err := cfg.CheckHarness("claude"); err == nil || err.Error() != want {
		t.Fatalf("CheckHarness(claude) error = %v, want %q", err, want)
	}
}

func TestAccessWordAliases(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   PolicyConfig
		want PolicyConfig
	}{
		{name: "anyone for the private network", in: PolicyConfig{PrivateAllowIncoming: "anyone"}, want: PolicyConfig{PrivateAllowIncoming: AccessEveryone}},
		{name: "everyone for the public URL", in: PolicyConfig{PublicAllowIncoming: "everyone"}, want: PolicyConfig{PublicAllowIncoming: AccessAnyone}},
		{name: "anyone for calls on the private network", in: PolicyConfig{PrivateAllowOutgoing: "anyone"}, want: PolicyConfig{PrivateAllowOutgoing: AccessEveryone}},
		{name: "canonical words", in: PolicyConfig{PrivateAllowIncoming: AccessPaired, PublicAllowIncoming: AccessAnyone}, want: PolicyConfig{PrivateAllowIncoming: AccessPaired, PublicAllowIncoming: AccessAnyone}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := &Config{}
			s := Settings{Harness: "opencode", PrivateAllowIncoming: tc.in.PrivateAllowIncoming, PublicAllowIncoming: tc.in.PublicAllowIncoming, PrivateAllowOutgoing: tc.in.PrivateAllowOutgoing}
			if _, err := s.Apply(cfg, Dir{Root: "/x"}); err != nil {
				t.Fatalf("Settings.Apply() error = %v", err)
			}
			if diff := cmp.Diff(tc.want, cfg.Policy); diff != "" {
				t.Fatalf("Settings.Apply() wrong policy (-want +got) diff = %s", diff)
			}

			parsed, err := ParseConfig([]byte("agent: opencode\npolicy:\n  private_access: " + tc.in.PrivateAllowIncoming + "\n  public_access: " + tc.in.PublicAllowIncoming + "\n  privateAllowOutgoing: " + tc.in.PrivateAllowOutgoing + "\n"))
			if err != nil {
				t.Fatalf("ParseConfig() error = %v", err)
			}
			if diff := cmp.Diff(tc.want, parsed.Policy); diff != "" {
				t.Fatalf("ParseConfig() wrong policy (-want +got) diff = %s", diff)
			}
		})
	}
}
