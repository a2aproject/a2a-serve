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
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestApplyInjectRules(t *testing.T) {
	t.Setenv("INJECT_TEST_GH", "ghp_env")
	cfg := &Config{Name: "coder", Harness: "opencode", Secrets: []string{"OPENROUTER_API_KEY"}}
	s := Settings{
		InjectHeaders:   []string{"API.GitHub.com:443=authorization: Bearer $INJECT_TEST_GH"},
		InjectModelKeys: []string{"openrouter=OPENROUTER_API_KEY"},
	}
	secrets, err := s.Apply(cfg, Dir{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("Settings.Apply() error = %v, want nil", err)
	}
	want := &Config{
		Name: "coder", Harness: "opencode", Secrets: []string{"OPENROUTER_API_KEY", "INJECT_TEST_GH"},
		Inject: []InjectRule{{Host: "api.github.com", Header: "Authorization: Bearer $INJECT_TEST_GH"}, {Model: "openrouter", Key: "OPENROUTER_API_KEY"}},
	}
	if diff := cmp.Diff(want, cfg); diff != "" {
		t.Fatalf("Settings.Apply() wrong config (-want +got) diff = %s", diff)
	}
	if diff := cmp.Diff(Secrets{"INJECT_TEST_GH": "ghp_env"}, secrets); diff != "" {
		t.Fatalf("Settings.Apply() wrong secrets (-want +got) diff = %s", diff)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Config.Validate() error = %v, want nil", err)
	}

	replaced := Settings{InjectHeaders: []string{"api.github.com=X-Token: $INJECT_TEST_GH"}}
	if _, err := replaced.Apply(cfg, Dir{Root: t.TempDir()}); err != nil {
		t.Fatalf("Settings.Apply() error = %v, want nil", err)
	}
	wantRules := []InjectRule{{Model: "openrouter", Key: "OPENROUTER_API_KEY"}, {Host: "api.github.com", Header: "X-Token: $INJECT_TEST_GH"}}
	if diff := cmp.Diff(wantRules, cfg.Inject); diff != "" {
		t.Fatalf("Settings.Apply() wrong rules (-want +got) diff = %s", diff)
	}
}

func TestApplyReplacedInjectRuleDropsItsSecret(t *testing.T) {
	t.Setenv("INJECT_TEST_NEW", "new")
	t.Setenv("INJECT_TEST_KEPT", "kept")
	tests := []struct {
		name        string
		s           Settings
		wantSecrets []string
	}{
		{
			name:        "replaced rule",
			s:           Settings{InjectHeaders: []string{"api.github.com=Authorization: Bearer $INJECT_TEST_NEW"}},
			wantSecrets: []string{"OTHER", "INJECT_TEST_NEW"},
		},
		{
			name:        "replaced rule with --secret for the old secret",
			s:           Settings{Secrets: []string{"OLD"}, SecretValues: Secrets{"OLD": "old"}, InjectHeaders: []string{"api.github.com=Authorization: Bearer $INJECT_TEST_KEPT"}},
			wantSecrets: []string{"OLD", "OTHER", "INJECT_TEST_KEPT"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{Secrets: []string{"OLD", "OTHER"}, Inject: []InjectRule{{Host: "api.github.com", Header: "Authorization: Bearer $OLD"}}}
			if _, err := tc.s.Apply(cfg, Dir{Root: t.TempDir()}); err != nil {
				t.Fatalf("Settings.Apply() error = %v, want nil", err)
			}
			if diff := cmp.Diff(tc.wantSecrets, cfg.Secrets); diff != "" {
				t.Fatalf("Settings.Apply() wrong secrets (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestApplyInjectRuleErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		s    Settings
		want string
	}{
		{name: "unset variable", s: Settings{InjectModelKeys: []string{"openrouter=INJECT_TEST_UNSET"}}, want: "the environment variable INJECT_TEST_UNSET is not set"},
		{name: "unknown provider", s: Settings{InjectModelKeys: []string{"mistral=K"}}, want: `unknown model provider "mistral": want anthropic, gemini, openai, openrouter`},
		{name: "expanded by the shell", s: Settings{InjectHeaders: []string{"api.github.com=Authorization: Bearer ghp_x"}}, want: "quote the rule in single quotes"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := tc.s.Apply(&Config{}, Dir{Root: t.TempDir()}); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Settings.Apply() error = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

func TestValidateInject(t *testing.T) {
	t.Parallel()
	gh := InjectRule{Host: "api.github.com", Header: "Authorization: Bearer $GH"}
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{name: "secret not stored", cfg: Config{Inject: []InjectRule{gh}}, want: "secret GH is not one of the agent's secrets"},
		{name: "two rules for a host", cfg: Config{Secrets: []string{"GH", "K"}, Inject: []InjectRule{gh, {Host: "api.github.com:443", Header: "X-Token: $K"}}}, want: "api.github.com already gets a header from inject-header"},
		{name: "model and header for a host", cfg: Config{Secrets: []string{"GH", "K"}, Inject: []InjectRule{{Model: "openrouter", Key: "K"}, {Host: "openrouter.ai", Header: "X-Token: $GH"}}}, want: "openrouter.ai already gets a header"},
		{name: "local sandbox", cfg: Config{Sandbox: SandboxLocal, Secrets: []string{"GH"}, Inject: []InjectRule{gh}}, want: "--inject-header needs the Docker sandbox"},
		{name: "unrestricted egress", cfg: Config{Egress: EgressConfig{Mode: "unrestricted"}, Secrets: []string{"GH"}, Inject: []InjectRule{gh}}, want: "--egress unrestricted turns off"},
		{name: "mixed rule", cfg: Config{Secrets: []string{"GH"}, Inject: []InjectRule{{Model: "openrouter", Key: "GH", Host: "x.example"}}}, want: "either host and header, or model and key"},
		{
			name: "sandbox rule renames an added secret",
			cfg:  Config{Secrets: []string{"GH"}, Inject: []InjectRule{gh}, Policy: PolicyConfig{Sandbox: []SandboxRule{{Match: []string{"guests"}, Secrets: []string{"TOKEN=secret:GH"}}}}},
			want: "the sandbox rule of guests passes GH as TOKEN, which would put it in the agent's environment",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.cfg.Name, tc.cfg.Harness = "coder", "opencode"
			if err := tc.cfg.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Config.Validate() error = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

func TestValidateInjectAllowsAnotherSecretUnderAnAddedName(t *testing.T) {
	t.Parallel()
	cfg := Config{
		Name: "coder", Harness: "opencode", Secrets: []string{"ANTHROPIC_API_KEY", "GUEST_KEY"},
		Inject: []InjectRule{{Model: "anthropic", Key: "ANTHROPIC_API_KEY"}},
		Policy: PolicyConfig{Sandbox: []SandboxRule{{Match: []string{"guests"}, Secrets: []string{"ANTHROPIC_API_KEY=secret:GUEST_KEY"}}}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Config.Validate() error = %v, want nil", err)
	}
}

func TestUnsetInjectRuleDropsItsSecret(t *testing.T) {
	t.Parallel()
	cfg := &Config{
		Secrets: []string{"OPENROUTER_API_KEY", "GH", "OTHER"},
		Inject:  []InjectRule{{Model: "openrouter", Key: "OPENROUTER_API_KEY"}, {Host: "api.github.com", Header: "Authorization: Bearer $GH"}},
	}
	deleted, err := (Dir{}).Unset(cfg, InjectHeaderSetting, []string{"api.github.com:443"})
	if err != nil {
		t.Fatalf("Dir.Unset() error = %v, want nil", err)
	}
	if diff := cmp.Diff([]string{"GH"}, deleted); diff != "" {
		t.Fatalf("Dir.Unset() wrong deleted secrets (-want +got) diff = %s", diff)
	}
	want := &Config{Secrets: []string{"OPENROUTER_API_KEY", "OTHER"}, Inject: []InjectRule{{Model: "openrouter", Key: "OPENROUTER_API_KEY"}}}
	if diff := cmp.Diff(want, cfg); diff != "" {
		t.Fatalf("Dir.Unset() wrong config (-want +got) diff = %s", diff)
	}
	if _, err := (Dir{}).Unset(cfg, InjectModelKeySetting, nil); err != nil {
		t.Fatalf("Dir.Unset() error = %v, want nil", err)
	}
	if diff := cmp.Diff(&Config{Secrets: []string{"OTHER"}}, cfg); diff != "" {
		t.Fatalf("Dir.Unset() wrong config (-want +got) diff = %s", diff)
	}
}

func TestUnsetInjectRefusals(t *testing.T) {
	t.Parallel()
	rule := InjectRule{Model: "openrouter", Key: "K"}
	tests := []struct {
		name    string
		cfg     *Config
		setting string
		entries []string
		wantErr string
	}{
		{
			name:    "secret of a rule",
			cfg:     &Config{Secrets: []string{"K"}, Inject: []InjectRule{rule}},
			setting: "secret", entries: []string{"K"},
			wantErr: "K is used by --inject-model-key openrouter; run `a2a-serve config unset inject-model-key openrouter` first",
		},
		{
			name:    "rule whose secret guests get",
			cfg:     &Config{Secrets: []string{"K"}, Inject: []InjectRule{rule}, Policy: PolicyConfig{Sandbox: []SandboxRule{{Match: []string{"guests"}, Secrets: []string{"K"}}}}},
			setting: InjectModelKeySetting, entries: []string{"openrouter"},
			wantErr: "K is used by the rule of guests; run `a2a-serve config unset guest-secrets` first",
		},
		{
			name:    "unknown rule",
			cfg:     &Config{Secrets: []string{"K"}, Inject: []InjectRule{rule}},
			setting: InjectModelKeySetting, entries: []string{"openai"},
			wantErr: "inject-model-key openai is not set",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := (Dir{}).Unset(tc.cfg, tc.setting, tc.entries); err == nil || err.Error() != tc.wantErr {
				t.Fatalf("Dir.Unset() error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestInjectRuleShow(t *testing.T) {
	t.Parallel()
	tests := []struct {
		rule InjectRule
		want string
	}{
		{rule: InjectRule{Model: "anthropic", Key: "CLAUDE_CODE_OAUTH_TOKEN"}, want: "api.anthropic.com gets CLAUDE_CODE_OAUTH_TOKEN"},
		{rule: InjectRule{Host: "git.example:8443", Header: "X-Auth: $A:$B"}, want: "git.example:8443 gets A, B"},
	}
	for _, tc := range tests {
		if got := tc.rule.Show(); got != tc.want {
			t.Errorf("InjectRule.Show() = %q, want %q", got, tc.want)
		}
	}
}
