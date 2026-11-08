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

package cli

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentctl"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
)

func fullConfig() *agentdir.Config {
	return &agentdir.Config{
		Name: "coder", Description: "reviews code", Harness: "opencode",
		Command: []string{"opencode", "acp"}, Model: "openrouter/model-x", Image: "img:x", Sandbox: "local",
		Workspace: "/ws-x", Mounts: []string{"/docs-x:/docs-x:ro"}, AllowBroadMounts: []string{"/broad-x"},
		Secrets: []string{"API_KEY"}, Env: []string{"DEBUG=1"}, Port: 4711,
		TaskTTL: "48h", ContactRefresh: "20m",
		Card: agentdir.CardConfig{Skills: []agentdir.CardSkill{{ID: "review-x"}}},
		Network: agentdir.NetworkConfig{
			Private: "tailscale", Public: "https://relay-x.example.com", TailscaleControlURL: "https://headscale-x.example.com",
		},
		Messengers: agentdir.MessengersConfig{
			Telegram: &agentdir.TelegramConfig{Token: "secret:telegram"},
			Discord:  &agentdir.DiscordConfig{Token: "secret:discord"},
			Email: &agentdir.EmailConfig{
				Address: "bot-x@example.com", IMAP: "imap-x.example.com:993", SMTP: "smtp-x.example.com:465",
				Username: "user-x", Password: "secret:email-password", Mailbox: "mailbox-x", TrustedAuthServ: "authserv-x",
				OAuthTokenURL: "https://token-x.example.com", OAuthClientID: "client-x",
				OAuthClientSecret: "secret:email-client", OAuthRefreshToken: "secret:email-oauth",
			},
		},
		Policy: agentdir.PolicyConfig{
			PrivateAllowIncoming: "paired", PublicAllowIncoming: "anyone",
			AllowEmails: []string{"anna-x@example.com"}, PrivateAllowOutgoing: "everyone",
		},
		Approvals: agentdir.ApprovalsConfig{Allow: []string{"read-x"}, Deny: []string{"execute-x"}, OwnerChat: "telegram:anna-x"},
		Limits:    agentdir.LimitsConfig{Memory: "8g-x", CPUs: "3-x", PIDs: 1234},
		Auth:      agentdir.AuthConfig{GoogleClientID: "google-id-x", GoogleClientSecret: "secret:google-client"},
		Backup:    agentdir.BackupConfig{To: "/backups-x", Key: "secret:backup-key", Interval: "10m", Retention: "3d"},
		MCP:       agentdir.MCPConfig{Disabled: []string{"github-x"}},
		Egress:    agentdir.EgressConfig{Mode: "allowlist", Allow: []string{"nas-x.lan"}},
	}
}

func TestRenderConfigListsEverySetting(t *testing.T) {
	t.Parallel()
	cfg := fullConfig()
	changes := agentdir.Diff(&agentdir.Config{}, cfg)
	var reported []string
	for _, c := range changes {
		reported = append(reported, c.Setting)
	}
	var want []string
	for _, v := range cfg.SettingValues() {
		want = append(want, v.Setting)
	}
	slices.Sort(reported)
	slices.Sort(want)
	if diff := cmp.Diff(want, reported); diff != "" {
		t.Fatalf("the settings Diff reports for a full config wrong result (-want +got) diff = %s", diff)
	}
	secrets := map[string]string{}
	for _, v := range cfg.SettingValues() {
		for _, e := range v.Values {
			if name, ok := strings.CutPrefix(e, agentdir.SecretPrefix); ok {
				secrets[name] = agentctl.SecretStateStored
			}
		}
	}
	secrets["API_KEY"] = agentctl.SecretStateStored
	v := &agentctl.View{Path: "/a/.a2a/config.yaml", Config: cfg, Defaults: &agentdir.Config{}, Secrets: secrets, Pending: reported}
	for _, all := range []bool{false, true} {
		out := renderConfig(v, all)
		for _, c := range changes {
			var values []string
			if c.New != "" {
				values = append(values, c.New)
			}
			values = append(values, c.Added...)
			for _, e := range values {
				if strings.HasPrefix(e, agentdir.SecretPrefix) {
					continue
				}
				if !strings.Contains(out, strings.TrimSuffix(e, ":ro")) && !strings.Contains(out, formatMount(e)) {
					t.Errorf("renderConfig(all = %v) has no line for %s %q:\n%s", all, c.Setting, e, out)
				}
			}
		}
	}
}

func TestRenderConfigSecretSettingsShowState(t *testing.T) {
	t.Parallel()
	cfg := &agentdir.Config{
		Name: "coder", Harness: "opencode",
		Auth:   agentdir.AuthConfig{GoogleClientSecret: "secret:google-client"},
		Backup: agentdir.BackupConfig{To: "/backups", Key: "literal-passphrase"},
	}
	v := &agentctl.View{Path: "p", Config: cfg, Defaults: &agentdir.Config{}, Secrets: map[string]string{"google-client": agentctl.SecretStateStored}}
	out := renderConfig(v, false)
	if strings.Contains(out, "literal-passphrase") {
		t.Fatalf("renderConfig() = %q, want no secret value", out)
	}
	for _, want := range []string{`google-client-secret +stored`, `backup-key +in config\.yaml`} {
		if !regexp.MustCompile(want).MatchString(out) {
			t.Errorf("renderConfig() = %q, want it to match %q", out, want)
		}
	}
}

func TestRenderConfigImageOfLocalSandbox(t *testing.T) {
	t.Parallel()
	v := &agentctl.View{Path: "p", Config: &agentdir.Config{Name: "coder", Harness: "opencode", Sandbox: "local", Image: "img:x"}, Defaults: &agentdir.Config{}}
	if out := renderConfig(v, false); !strings.Contains(out, "image img:x") {
		t.Fatalf("renderConfig() = %q, want the image that is set", out)
	}
}

func TestRenderConfigUnsetSettings(t *testing.T) {
	t.Parallel()
	v := &agentctl.View{Path: "p", Config: &agentdir.Config{Name: "coder", Harness: "opencode"}, Defaults: &agentdir.Config{}}
	if out := renderConfig(v, false); strings.Contains(out, "task-ttl") || strings.Contains(out, "model") {
		t.Fatalf("renderConfig(all = false) = %q, want no line for unset settings", out)
	}
	out := renderConfig(v, true)
	for _, want := range []string{"model", "task-ttl", "backup", "disable-mcp", "allow-broad-mount", "contact-refresh", "google-client-id"} {
		if !strings.Contains(out, want) {
			t.Errorf("renderConfig(all = true) = %q, want a line for %s", out, want)
		}
	}
	if strings.Contains(out, "email-") {
		t.Errorf("renderConfig(all = true) = %q, want no email settings without the messenger", out)
	}
}
