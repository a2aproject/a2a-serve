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
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"gopkg.in/yaml.v3"
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
		Network:     NetworkConfig{Private: "tailscale"},
		Policy:      PolicyConfig{PrivateAllowIncoming: AccessPaired, AllowEmails: []string{"mom@example.com", "*@example.com"}},
		Messengers: MessengersConfig{
			Telegram: &TelegramConfig{Token: "secret:TELEGRAM_BOT_TOKEN"},
			Email:    &EmailConfig{Address: "a@x", IMAP: "i:993", SMTP: "s:465", Password: "secret:EMAIL_PASSWORD", TrustedAuthServ: "mx"},
		},
		Approvals: ApprovalsConfig{Allow: []string{"read", "search"}, OwnerChat: "telegram"},
		Limits:    LimitsConfig{Memory: "8g"},
		MCP:       MCPConfig{Disabled: []string{"github", "kubernetes"}},
		Card: CardConfig{Skills: []CardSkill{
			{ID: "release-notes", Name: "Release notes", Description: "Writes release notes.", Tags: []string{"git"}},
			{ID: "review"},
		}},
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
		{setting: "private-allow-incoming", modify: func(c *Config) { c.Policy.PrivateAllowIncoming = "" }},
		{setting: "allow-email", entries: []string{"Mom@example.com"}, modify: func(c *Config) { c.Policy.AllowEmails = []string{"*@example.com"} }},
		{setting: "ngrok-authtoken", wantErr: true},
		{setting: "memory", modify: func(c *Config) { c.Limits.Memory = "" }},
		{setting: "workspace", modify: func(c *Config) { c.Workspace = "" }},
		{setting: "env", entries: []string{"DEBUG"}, modify: func(c *Config) { c.Env = []string{"MODE=fast"} }},
		{setting: "env", modify: func(c *Config) { c.Env = nil }},
		{setting: "mount", entries: []string{"/docs"}, modify: func(c *Config) { c.Mounts = []string{"/data:/data"} }},
		{setting: "allow-tools", entries: []string{"search"}, modify: func(c *Config) { c.Approvals.Allow = []string{"read"} }},
		{setting: "disable-mcp", entries: []string{"github"}, modify: func(c *Config) { c.MCP.Disabled = []string{"kubernetes"} }},
		{setting: "disable-mcp", modify: func(c *Config) { c.MCP.Disabled = nil }},
		{setting: "card-skills", entries: []string{"review"}, modify: func(c *Config) { c.Card.Skills = c.Card.Skills[:1] }},
		{setting: "card-skills", modify: func(c *Config) { c.Card.Skills = nil }},
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
	next.Messengers.Telegram = nil
	next.Policy.AllowEmails = append(next.Policy.AllowEmails, "ben@example.org")
	next.Card.Skills[1].Description = "Reviews code."
	got := Diff(old, next)
	want := []Change{
		{Setting: "description", Old: "writes code", New: "reviews code", Live: true},
		{Setting: "card-skills", Added: []string{"review"}, Removed: []string{"review"}, Live: true},
		{Setting: "sandbox", New: "local"},
		{Setting: "allow-email", Added: []string{"ben@example.org"}, Live: true},
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

func TestDiffTreatsDefaultAsUnset(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		old  *Config
		next *Config
		want []Change
	}{
		{
			name: "explicit default over unset",
			old:  &Config{},
			next: &Config{Policy: PolicyConfig{PrivateAllowIncoming: AccessEveryone}, Sandbox: SandboxDocker, Limits: LimitsConfig{PIDs: 512}},
		},
		{
			name: "unset over explicit default",
			old:  &Config{Policy: PolicyConfig{PublicAllowIncoming: AccessPaired}},
			next: &Config{},
		},
		{
			name: "other value over default",
			old:  &Config{Policy: PolicyConfig{PrivateAllowIncoming: AccessEveryone}},
			next: &Config{Policy: PolicyConfig{PrivateAllowIncoming: AccessPaired}},
			want: []Change{{Setting: "private-allow-incoming", Old: AccessEveryone, New: AccessPaired, Live: true}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := Diff(tc.old, tc.next)

			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("Diff() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestCommands(t *testing.T) {
	t.Parallel()
	cfg := &Config{
		Name:       "coder",
		Harness:    "acp",
		Command:    []string{"gemini", "--experimental-acp"},
		Secrets:    []string{"KEY"},
		Network:    NetworkConfig{Public: "tailscale"},
		Policy:     PolicyConfig{PublicAllowIncoming: AccessAnyone, AllowEmails: []string{"*@example.com"}},
		Messengers: MessengersConfig{Telegram: &TelegramConfig{Token: "secret:BOT"}},
		Backup:     BackupConfig{To: "gs://bucket/coder", Key: "secret:BACKUP_KEY", Interval: "1h"},
	}
	var got []string
	for _, args := range cfg.Commands(Secrets{SecretTailscaleKey: "k"}) {
		got = append(got, ShellJoin(args))
	}
	want := []string{
		"a2a-serve start acp --name coder --command 'gemini --experimental-acp' --secret KEY --public tailscale --public-allow-incoming anyone --allow-email '*@example.com'",
		"a2a-serve config set --tailscale-authkey env:TS_AUTHKEY --backup gs://bucket/coder --backup-key env:BACKUP_KEY --backup-interval 1h",
		"a2a-serve messenger add telegram --token env:BOT",
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

func TestLiveSettings(t *testing.T) {
	t.Parallel()
	want := []string{"description", "card-skills", "private-allow-incoming", "public-allow-incoming", "private-allow-outgoing", "allow-email", "guest-storage", "guest-secrets", "sandbox-rules", "allow-tools", "deny-tools", "disable-mcp", "owner-chat", "egress-allow", "contact-refresh"}
	got := LiveSettings()
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("LiveSettings() wrong result (-want +got) diff = %s", diff)
	}
	for _, name := range got {
		if !IsLive(name) {
			t.Fatalf("IsLive(%q) = false, want true for a live setting", name)
		}
	}
}

func TestCopyLive(t *testing.T) {
	t.Parallel()
	src := fullConfig()
	src.Description = "reviews code"
	src.Policy.PublicAllowIncoming = AccessAnyone
	src.Approvals.Deny = []string{"execute"}
	dst := &Config{Name: "other", Harness: "claude"}
	CopyLive(dst, src)
	var got []string
	for _, c := range Diff(src, dst) {
		if slices.Contains(LiveSettings(), c.Setting) {
			got = append(got, c.Setting)
		}
	}
	if len(got) > 0 {
		t.Fatalf("CopyLive() left live settings %v uncopied", got)
	}
	src.MCP.Disabled[0] = "changed"
	if dst.MCP.Disabled[0] == "changed" {
		t.Fatalf("CopyLive() shares lists with the source")
	}
}

func TestFieldsHaveScope(t *testing.T) {
	t.Parallel()
	for _, f := range fields {
		if f.scope == scopeUnset {
			t.Fatalf("setting %s has no scope", f.flag)
		}
	}
}

func TestAgentSettings(t *testing.T) {
	t.Parallel()
	cfg := fullConfig()
	cfg.Command = []string{"gemini", "--experimental-acp", "--model", "a b"}
	cfg.Approvals.Deny = []string{"execute"}
	got := cfg.AgentSettings()
	want := &Config{
		Description: "writes code",
		Harness:     "opencode",
		Command:     []string{"gemini", "--experimental-acp", "--model", "a b"},
		Mounts:      []string{"/docs:ro", "/data:/data"},
		Secrets:     []string{"KEY", "OTHER"},
		Env:         []string{"DEBUG=1", "MODE=fast"},
		Approvals:   ApprovalsConfig{Allow: []string{"read", "search"}, Deny: []string{"execute"}},
		Limits:      LimitsConfig{Memory: "8g"},
		Card:        fullConfig().Card,
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("Config.AgentSettings() wrong result (-want +got) diff = %s", diff)
	}
	got.Mounts[0] = "/changed"
	if cfg.Mounts[0] == "/changed" {
		t.Fatalf("Config.AgentSettings() shares mounts with the original")
	}
}

func TestOwnerSettings(t *testing.T) {
	t.Parallel()
	got := fullConfig().OwnerSettings()
	want := []string{"name", "workspace", "port", "private", "private-allow-incoming", "allow-email", "telegram", "email-address", "email-imap", "email-smtp", "email-password", "email-trusted-authserv", "disable-mcp", "owner-chat"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("Config.OwnerSettings() wrong result (-want +got) diff = %s", diff)
	}
}

func TestIsAgentSetting(t *testing.T) {
	t.Parallel()
	tests := []struct {
		setting string
		want    bool
	}{
		{setting: "agent", want: true},
		{setting: "command", want: true},
		{setting: "deny-tools", want: true},
		{setting: "pids", want: true},
		{setting: "name", want: false},
		{setting: "public-allow-incoming", want: false},
		{setting: "backup", want: false},
		{setting: "telegram", want: false},
		{setting: "nope", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.setting, func(t *testing.T) {
			t.Parallel()
			if got := IsAgentSetting(tc.setting); got != tc.want {
				t.Fatalf("IsAgentSetting(%q) = %v, want %v", tc.setting, got, tc.want)
			}
		})
	}
}

func TestFlagsSetTheirKeys(t *testing.T) {
	t.Parallel()
	const all = AgentFlag | CoreFlags | OperationalFlags | EmailFlags
	docs := map[string]SettingDoc{}
	for _, d := range SettingDocs() {
		docs[d.Flag] = d
	}
	for _, fl := range (&Settings{}).Flags(all) {
		t.Run(fl.Name, func(t *testing.T) {
			t.Parallel()
			s := Settings{SecretValues: Secrets{"V": "value"}}
			flags := s.Flags(all)
			switch v := flags[slices.IndexFunc(flags, func(f Flag) bool { return f.Name == fl.Name })].Value.(type) {
			case *string:
				*v = "V"
			case *int:
				*v = 7
			case *[]string:
				*v = []string{"V"}
			default:
				t.Fatalf("Settings.Flags() value of --%s = %T, want *string, *int or *[]string", fl.Name, v)
			}
			cfg := &Config{}
			secrets, err := s.Apply(cfg, Dir{Root: "/x"})
			if err != nil {
				t.Fatalf("Settings.Apply() error = %v", err)
			}
			doc := docs[fl.Name]
			if doc.Key == "" {
				if secrets[doc.Secret] != "V" {
					t.Fatalf("Settings.Apply() secrets = %v, want %s stored", secrets, doc.Secret)
				}
				return
			}
			data, err := MarshalConfig(cfg)
			if err != nil {
				t.Fatal(err)
			}
			var root yaml.Node
			if err := yaml.Unmarshal(data, &root); err != nil {
				t.Fatal(err)
			}
			var got []string
			setKeys(root.Content[0], "", &got)
			got = slices.DeleteFunc(got, func(k string) bool { return k == "name" && fl.Name != "name" })
			if diff := cmp.Diff([]string{doc.Key}, got); diff != "" {
				t.Fatalf("--%s sets wrong config.yaml keys (-want +got) diff = %s", fl.Name, diff)
			}
		})
	}
}

// setKeys appends the dotted keys of n with values.
func setKeys(n *yaml.Node, prefix string, out *[]string) {
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			setKeys(n.Content[i+1], prefix+n.Content[i].Value+".", out)
		}
	case yaml.SequenceNode:
		if len(n.Content) > 0 {
			*out = append(*out, strings.TrimSuffix(prefix, "."))
		}
	case yaml.ScalarNode:
		if n.Value != "" {
			*out = append(*out, strings.TrimSuffix(prefix, "."))
		}
	}
}
