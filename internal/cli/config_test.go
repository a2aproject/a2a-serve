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
	"encoding/json"
	"errors"
	"io"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/spf13/pflag"
	"gopkg.in/yaml.v3"

	"github.com/a2aproject/a2a-serve/internal/agentctl"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/clierr"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/secretref"
)

func newTestAgent(t *testing.T, cfg *agentdir.Config) agentdir.Dir {
	t.Helper()
	d, err := agentdir.Open(t.TempDir())
	if err != nil {
		t.Fatalf("agentdir.Open() error = %v", err)
	}
	if cfg == nil {
		return d
	}
	if err := d.SaveConfig(cfg); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}
	return d
}

func isUsage(err error) bool {
	var ce *clierr.Error
	return errors.As(clierr.Classify(err), &ce) && ce.Code == clierr.CodeUsage
}

func TestConfigCommands(t *testing.T) {
	t.Parallel()
	base := func() *agentdir.Config {
		return &agentdir.Config{Name: "coder", Harness: "opencode", Env: []string{"DEBUG=1"}, Approvals: agentdir.ApprovalsConfig{Allow: []string{"read"}}}
	}
	tests := []struct {
		name       string
		args       []string
		wantOut    []string
		wantUsage  bool
		wantConfig func(*agentdir.Config)
	}{
		{
			name:    "set",
			args:    []string{"config", "set", "--sandbox", "local", "--allow-tools", "read,search", "--env", "DEBUG=0", "--memory", "8g"},
			wantOut: []string{"sandbox      (default) → local", "allow-tools  + search", "env          − DEBUG=1, + DEBUG=0", "memory       (default) → 8g", "✓ saved"},
			wantConfig: func(c *agentdir.Config) {
				c.Sandbox, c.Approvals.Allow, c.Env, c.Limits.Memory = "local", []string{"read", "search"}, []string{"DEBUG=0"}, "8g"
			},
		},
		{
			name:    "set dry run",
			args:    []string{"config", "set", "--sandbox", "local", "--dry-run"},
			wantOut: []string{"sandbox      (default) → local", "(dry run: nothing saved)"},
		},
		{name: "set invalid", args: []string{"config", "set", "--private", "tailscal"}, wantUsage: true},
		{
			name:       "set owner chat",
			args:       []string{"config", "set", "--owner-chat", "telegram:anna"},
			wantOut:    []string{"owner-chat   (default) → telegram:anna"},
			wantConfig: func(c *agentdir.Config) { c.Approvals.OwnerChat = "telegram:anna" },
		},
		{name: "set invalid owner chat", args: []string{"config", "set", "--owner-chat", "fax"}, wantUsage: true},
		{name: "approver removed", args: []string{"config", "set", "--approver", "pair:anna"}, wantUsage: true},
		{name: "set missing mount", args: []string{"config", "set", "--mount", "/nonexistent-a2a:/x"}, wantUsage: true},
		{
			name:       "set agent",
			args:       []string{"config", "set", "--agent", "claude"},
			wantOut:    []string{"agent        opencode → claude"},
			wantConfig: func(c *agentdir.Config) { c.Harness = "claude" },
		},
		{
			name:       "unset entry",
			args:       []string{"config", "unset", "env", "DEBUG"},
			wantOut:    []string{"env          − DEBUG=1", "✓ saved"},
			wantConfig: func(c *agentdir.Config) { c.Env = nil },
		},
		{name: "unset unknown entry", args: []string{"config", "unset", "allow-tools", "edit"}, wantUsage: true},
		{name: "unset unknown setting", args: []string{"config", "unset", "nope"}, wantUsage: true},
		{name: "unset agent", args: []string{"config", "unset", "agent"}, wantUsage: true},
		{name: "show", args: []string{"config"}, wantOut: []string{"coder (opencode)", "sandbox      docker (default)", "approvals    allow read", "limits       memory 4g, cpus 2, pids 512 (defaults)"}},
		{name: "show defaults", args: []string{"config", "show", "--defaults"}, wantOut: []string{"description  none", "channels     none"}},
		{name: "show as command", args: []string{"config", "show", "--as-command"}, wantOut: []string{"a2a-serve start opencode --name coder --env DEBUG=1 --allow-tools read\n"}},
		{name: "yaml only for show", args: []string{"-o", "yaml", "config", "set", "--sandbox", "local"}, wantUsage: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newTestAgent(t, base())
			out, err := runCMD(t, append(tc.args, "--dir", d.Root)...)
			if tc.wantUsage {
				if !isUsage(err) {
					t.Fatalf("a2a-serve %s error = %v, want a usage error", strings.Join(tc.args, " "), err)
				}
			} else if err != nil {
				t.Fatalf("a2a-serve %s error = %v", strings.Join(tc.args, " "), err)
			}
			for _, want := range tc.wantOut {
				if !strings.Contains(out, want) {
					t.Fatalf("a2a-serve %s = %q, want %q", strings.Join(tc.args, " "), out, want)
				}
			}
			want := base()
			if tc.wantConfig != nil {
				tc.wantConfig(want)
			}
			got, err := d.ReadConfig()
			if err != nil {
				t.Fatalf("ReadConfig() error = %v", err)
			}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Fatalf("a2a-serve %s wrong config (-want +got) diff = %s", strings.Join(tc.args, " "), diff)
			}
		})
	}
}

func TestConfigShowMachineReadable(t *testing.T) {
	t.Parallel()
	d := newTestAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode", Secrets: []string{"KEY"}})
	tests := []struct {
		format    string
		unmarshal func([]byte, any) error
	}{
		{format: "json", unmarshal: json.Unmarshal},
		{format: "yaml", unmarshal: yaml.Unmarshal},
	}
	for _, tc := range tests {
		t.Run(tc.format, func(t *testing.T) {
			t.Parallel()
			out, err := runCMD(t, "-o", tc.format, "config", "show", "--defaults", "--dir", d.Root)
			if err != nil {
				t.Fatalf("a2a-serve config show -o %s error = %v", tc.format, err)
			}
			var got agentctl.View
			if err := tc.unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("parsing %q error = %v", out, err)
			}
			if got.Config.Name != "coder" || got.Secrets["KEY"] != agentctl.SecretStateMissing || got.Defaults == nil || got.Defaults.Sandbox != "docker" {
				t.Fatalf("a2a-serve config show -o %s = %+v, want coder with KEY missing and the default sandbox", tc.format, got)
			}
			if !strings.Contains(got.Invalid, "secret KEY is not stored") {
				t.Fatalf("a2a-serve config show -o %s invalid = %q, want the missing secret", tc.format, got.Invalid)
			}
		})
	}
}

func TestHarnessCommandValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		existing *agentdir.Config
		args     []string
		wantErr  string
	}{
		{name: "invalid setting on the first run", args: []string{"start", "opencode", "--private", "tailscal"}, wantErr: `invalid --private "tailscal"`},
		{name: "acp without a command", args: []string{"start", "acp"}, wantErr: "agent acp needs a command"},
		{
			name:     "acp command from the config",
			existing: &agentdir.Config{Name: "coder", Harness: "acp", Command: []string{"fakeacp"}},
			args:     []string{"start", "acp", "--private", "tailscal"},
			wantErr:  `invalid --private "tailscal"`,
		},
		{
			name:     "other harness",
			existing: &agentdir.Config{Name: "coder", Harness: "opencode"},
			args:     []string{"start", "claude"},
			wantErr:  "this agent runs opencode; switch with `a2a-serve config set --agent claude`",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newTestAgent(t, tc.existing)
			_, err := runCMD(t, append(tc.args, "--dir", d.Root)...)
			if !isUsage(err) || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("a2a-serve %s error = %v, want a usage error %q", strings.Join(tc.args, " "), err, tc.wantErr)
			}
			if tc.existing != nil {
				return
			}
			if d.Exists() {
				t.Fatalf("a2a-serve %s saved an invalid config", strings.Join(tc.args, " "))
			}
		})
	}
}

// pipeWith returns the read end of a pipe which holds text.
func pipeWith(t *testing.T, text string) *os.File {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	if _, err := w.WriteString(text); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestSecretReader(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		flags settingsFlags
		got   func(agentdir.Settings) string
	}{
		{name: "tailscale-authkey", flags: settingsFlags{s: agentdir.Settings{TailscaleAuthKey: "-"}}, got: func(s agentdir.Settings) string { return s.TailscaleAuthKey }},
		{name: "google-client-secret", flags: settingsFlags{s: agentdir.Settings{GoogleClientSecret: "-"}}, got: func(s agentdir.Settings) string { return s.GoogleClientSecret }},
		{name: "relay-token", flags: settingsFlags{s: agentdir.Settings{RelayToken: "-"}}, got: func(s agentdir.Settings) string { return s.RelayToken }},
		{name: "backup-key", flags: settingsFlags{s: agentdir.Settings{BackupKey: "-"}}, got: func(s agentdir.Settings) string { return s.BackupKey }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sr := secretref.NewReader(pipeWith(t, "123:abc\n"), io.Discard)

			got, err := tc.flags.settings(sr)
			if err != nil {
				t.Fatalf("settings() error = %v", err)
			}
			if tc.got(got) != "123:abc" {
				t.Fatalf("settings() --%s = %q, want %q", tc.name, tc.got(got), "123:abc")
			}
			if _, err := tc.flags.settings(sr); !isUsage(err) {
				t.Fatalf("settings() reading stdin twice error = %v, want a usage error", err)
			}
		})
	}
}

// TestConfigRunningAgent runs serially, like TestServerPairAndSend.
func TestConfigRunningAgent(t *testing.T) {
	fake := buildFakeACP(t)
	dir, err := os.MkdirTemp("", "a2a")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("removing %s: %v", dir, err)
		}
	})
	d, err := agentdir.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := runCMD(t, "start", "acp", "--dir", dir, "--name", "coder", "--command", fake, "--sandbox", "local")
		done <- err
	}()
	client := controlapi.NewClient(d)
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := client.Status(t.Context()); err == nil {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("a2a-serve start acp error = %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("agent did not start")
		}
		time.Sleep(100 * time.Millisecond)
	}

	out, err := runCMD(t, "start", "acp", "--dir", dir, "--description", "reviews code")
	if err != nil {
		t.Fatalf("a2a-serve start acp on the running agent error = %v", err)
	}
	for _, want := range []string{"description  (default) → reviews code", "✓ applied to the running agent: description", "✓ coder is already running"} {
		if !strings.Contains(out, want) {
			t.Fatalf("a2a-serve start acp on the running agent = %q, want %q", out, want)
		}
	}
	running, err := client.Config(t.Context())
	if err != nil {
		t.Fatalf("Config() error = %v", err)
	}
	if running.Description != "reviews code" {
		t.Fatalf("running description = %q, want %q", running.Description, "reviews code")
	}

	out, err = runCMD(t, "config", "set", "--dir", dir, "--env", "DEBUG=1")
	if err != nil || !strings.Contains(out, "! coder is running; restart to apply env: a2a-serve restart") {
		t.Fatalf("a2a-serve config set --env = %q, %v, want a restart notice", out, err)
	}
	out, err = runCMD(t, "-o", "json", "config", "set", "--dir", dir, "--env", "TRACE=1")
	if err != nil {
		t.Fatalf("a2a-serve config set --env -o json error = %v", err)
	}
	var set struct {
		Restart []string `json:"restart"`
	}
	if err := json.Unmarshal([]byte(out), &set); err != nil {
		t.Fatalf("json.Unmarshal(%q) error = %v", out, err)
	}
	if diff := cmp.Diff([]string{"env"}, set.Restart); diff != "" {
		t.Fatalf("a2a-serve config set --env -o json restart wrong result (-want +got) diff = %s", diff)
	}
	out, err = runCMD(t, "config", "--dir", dir)
	if err != nil || !strings.Contains(out, "! restart to apply env: a2a-serve restart") {
		t.Fatalf("a2a-serve config = %q, %v, want env pending", out, err)
	}
	out, err = runCMD(t, "status", "--dir", dir)
	if err != nil || !strings.Contains(out, "pending  env (a2a-serve restart)") {
		t.Fatalf("a2a-serve status = %q, %v, want env pending", out, err)
	}
	if _, err := runCMD(t, "restart", "--dir", dir); err == nil || !strings.Contains(err.Error(), "press Ctrl-C there") {
		t.Fatalf("a2a-serve restart of a foreground agent error = %v, want a refusal", err)
	}

	if _, err := runCMD(t, "stop", "--dir", dir); err != nil {
		t.Fatalf("a2a-serve stop error = %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a2a-serve start acp error = %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("agent did not stop")
	}
	saved, err := d.ReadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{"DEBUG=1", "TRACE=1"}, saved.Env); diff != "" {
		t.Fatalf("saved env wrong result (-want +got) diff = %s", diff)
	}
}

func TestConfigShowAsCommandHelp(t *testing.T) {
	t.Parallel()
	out, err := runCMD(t, "config", "show", "--help")
	if err != nil {
		t.Fatalf("a2a-serve config show --help error = %v", err)
	}
	if !regexp.MustCompile(`--as-command\s+Print the a2a-serve commands which`).MatchString(out) {
		t.Fatalf("a2a-serve config show --help = %q, want --as-command without a value name", out)
	}
}

func TestConfigInvalidOutput(t *testing.T) {
	t.Parallel()
	_, err := runCMD(t, "-o", "xml", "config")
	if !isUsage(err) || !strings.Contains(err.Error(), "want text, json, jsonl or yaml") {
		t.Fatalf("a2a-serve -o xml config error = %v, want a usage error listing jsonl", err)
	}
}

func TestSettingsFlagPlacement(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "operational on run", args: []string{"start", "opencode", "--memory", "8g"}, wantErr: "unknown flag: --memory"},
		{name: "channel on run", args: []string{"start", "opencode", "--telegram", "x"}, wantErr: "unknown flag: --telegram"},
		{name: "channel on config set", args: []string{"config", "set", "--telegram", "x"}, wantErr: "unknown flag: --telegram"},
		{name: "email on config set", args: []string{"config", "set", "--email-address", "a@example.com"}, wantErr: "unknown flag: --email-address"},
		{name: "quiet on config set", args: []string{"config", "set", "--quiet"}, wantErr: "unknown flag: --quiet"},
		{name: "unset a channel", args: []string{"config", "unset", "telegram"}, wantErr: "a2a-serve channel rm telegram"},
		{name: "unknown tool kind", args: []string{"config", "set", "--allow-tools", "reed"}, wantErr: `invalid --allow-tools "reed"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newTestAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode"})
			_, err := runCMD(t, append(tc.args, "--dir", d.Root)...)
			if !isUsage(err) || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("a2a-serve %s error = %v, want a usage error %q", strings.Join(tc.args, " "), err, tc.wantErr)
			}
		})
	}
}

func TestSettingsFlagsMatchSettings(t *testing.T) {
	t.Parallel()
	root := newRootCmd(&globalConfig{})
	set, _, err := root.Find([]string{"config", "set"})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	set.LocalFlags().VisitAll(func(f *pflag.Flag) {
		if len(f.Annotations[sectionAnnotation]) > 0 {
			got = append(got, f.Name)
		}
	})
	if diff := cmp.Diff(agentdir.SettingNames(), got); diff != "" {
		t.Fatalf("a2a-serve config set flags wrong result (-want +got) diff = %s", diff)
	}
	start, _, err := root.Find([]string{"start", "opencode"})
	if err != nil {
		t.Fatal(err)
	}
	start.LocalFlags().VisitAll(func(f *pflag.Flag) {
		if s := f.Annotations[sectionAnnotation]; len(s) > 0 && s[0] != sectionRun && set.LocalFlags().Lookup(f.Name) == nil {
			t.Fatalf("a2a-serve start opencode --%s is a setting that config set doesn't take", f.Name)
		}
	})
}

func TestConfigUnsetValidatesSettingBeforeDirectory(t *testing.T) {
	t.Parallel()
	empty := t.TempDir()
	tests := []struct {
		setting string
		wantErr string
	}{
		{setting: "nope", wantErr: `unknown setting "nope"`},
		{setting: "agent", wantErr: "the agent can't be unset"},
		{setting: "telegram", wantErr: "a2a-serve channel rm telegram"},
	}
	for _, tc := range tests {
		t.Run(tc.setting, func(t *testing.T) {
			t.Parallel()
			_, err := runCMD(t, "config", "unset", tc.setting, "--dir", empty)
			if !isUsage(err) || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("a2a-serve config unset %s error = %v, want a usage error %q", tc.setting, err, tc.wantErr)
			}
		})
	}
}

func TestToolKindsAreRepeatable(t *testing.T) {
	t.Parallel()
	d := newTestAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode"})
	if _, err := runCMD(t, "config", "set", "--allow-tools", "read,search", "--allow-tools", "fetch", "--deny-tools", "exec*", "--dir", d.Root); err != nil {
		t.Fatalf("a2a-serve config set --allow-tools error = %v", err)
	}
	got, err := d.ReadConfig()
	if err != nil {
		t.Fatalf("ReadConfig() error = %v", err)
	}
	want := agentdir.ApprovalsConfig{Allow: []string{"read", "search", "fetch"}, Deny: []string{"exec*"}}
	if diff := cmp.Diff(want, got.Approvals); diff != "" {
		t.Fatalf("a2a-serve config set --allow-tools wrong approvals (-want +got) diff = %s", diff)
	}
}

func TestSettingsHelpSections(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		args   []string
		want   []string
		absent []string
	}{
		{
			name:   "run harness",
			args:   []string{"start", "opencode", "--help"},
			want:   []string{"General:", "Workspace:", "Sandbox:", "Network:", "Approvals:", "Run (not saved):", "--detach", "--image", "(repeatable)"},
			absent: []string{"--memory", "--telegram", "--agent"},
		},
		{
			name:   "config set",
			args:   []string{"config", "set", "--help"},
			want:   []string{"General:", "--agent", "--memory", "--backup-key", "Flags:", "--dry-run"},
			absent: []string{"Run (not saved):", "--telegram", "--quiet"},
		},
		{
			name:   "local sandbox",
			args:   []string{"config", "set", "--sandbox", "local", "--help"},
			want:   []string{"--sandbox"},
			absent: []string{"--image"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newTestAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode"})
			out, err := runCMD(t, append(tc.args, "--dir", d.Root)...)
			if err != nil {
				t.Fatalf("a2a-serve %s error = %v", strings.Join(tc.args, " "), err)
			}
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Fatalf("a2a-serve %s = %q, want %q", strings.Join(tc.args, " "), out, want)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(out, absent) {
					t.Fatalf("a2a-serve %s = %q, want no %q", strings.Join(tc.args, " "), out, absent)
				}
			}
		})
	}
}

func TestConfigSetCreatesAgentWithHarness(t *testing.T) {
	t.Parallel()
	d := newTestAgent(t, nil)

	if _, err := runCMD(t, "config", "set", "--dir", d.Root, "--agent", "opencode", "--name", "coder", "--memory", "8g"); err != nil {
		t.Fatalf("a2a-serve config set --agent in a new directory error = %v", err)
	}

	got, err := d.ReadConfig()
	if err != nil {
		t.Fatalf("ReadConfig() error = %v", err)
	}
	want := &agentdir.Config{Name: "coder", Harness: "opencode", Limits: agentdir.LimitsConfig{Memory: "8g"}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("a2a-serve config set --agent wrong config (-want +got) diff = %s", diff)
	}
	if _, err := runCMD(t, "config", "set", "--dir", t.TempDir(), "--memory", "8g"); !errors.Is(err, agentdir.ErrNotFound) {
		t.Fatalf("a2a-serve config set without --agent in a new directory error = %v, want %v", err, agentdir.ErrNotFound)
	}
}

func TestConfigSetSecretValueForms(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		secret  string
		stdin   string
		want    agentdir.Secrets
		wantErr string
	}{
		{name: "value", secret: "KEY=sk-123", want: agentdir.Secrets{"KEY": "sk-123", "OLD": "stored"}},
		{name: "value with equals signs", secret: "KEY=a=b", want: agentdir.Secrets{"KEY": "a=b", "OLD": "stored"}},
		{name: "other variable", secret: "KEY=env:HOME", want: agentdir.Secrets{"KEY": os.Getenv("HOME"), "OLD": "stored"}},
		{name: "stdin", secret: "KEY=-", stdin: "piped\n", want: agentdir.Secrets{"KEY": "piped", "OLD": "stored"}},
		{name: "stored value", secret: "KEY=secret:OLD", want: agentdir.Secrets{"KEY": "stored", "OLD": "stored"}},
		{name: "unknown stored value", secret: "KEY=secret:MISSING", wantErr: "--secret KEY: no stored secret MISSING"},
		{name: "empty variable", secret: "KEY=env:A2A_SERVE_TEST_UNSET", wantErr: "--secret KEY: environment variable A2A_SERVE_TEST_UNSET is empty"},
		{name: "empty value", secret: "KEY=", wantErr: "--secret KEY: empty secret"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newTestAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode", Secrets: []string{"OLD"}})
			if err := d.SaveSecrets(agentdir.Secrets{"OLD": "stored"}); err != nil {
				t.Fatalf("SaveSecrets() error = %v", err)
			}

			_, err := runCMDWithStdin(t, pipeWith(t, tc.stdin), "config", "set", "--secret", tc.secret, "--dir", d.Root)

			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("a2a-serve config set --secret %s error = %v, want %q", tc.secret, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("a2a-serve config set --secret %s error = %v", tc.secret, err)
			}
			got, err := d.LoadSecrets()
			if err != nil {
				t.Fatalf("LoadSecrets() error = %v", err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("a2a-serve config set --secret %s wrong secrets (-want +got) diff = %s", tc.secret, diff)
			}
			conf, err := d.LoadConfig()
			if err != nil {
				t.Fatalf("LoadConfig() error = %v", err)
			}
			if diff := cmp.Diff([]string{"OLD", "KEY"}, conf.Secrets); diff != "" {
				t.Fatalf("a2a-serve config set --secret %s wrong config secrets (-want +got) diff = %s", tc.secret, diff)
			}
		})
	}
}
