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
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestSetSandboxRule(t *testing.T) {
	t.Parallel()
	ben := SandboxRule{Match: []string{"pairing:ben"}, Storage: StorageOwn}
	anyone := SandboxRule{Match: []string{"guests"}, Storage: StorageReadOnly}
	tests := []struct {
		name  string
		rules []SandboxRule
		set   SandboxRule
		want  []SandboxRule
	}{
		{name: "adds", rules: []SandboxRule{anyone}, set: ben, want: []SandboxRule{anyone, ben}},
		{name: "replaces", rules: []SandboxRule{ben, anyone}, set: SandboxRule{Match: []string{"pairing:ben"}, Secrets: []string{"all"}}, want: []SandboxRule{{Match: []string{"pairing:ben"}, Secrets: []string{"all"}}, anyone}},
		{name: "removes an empty rule", rules: []SandboxRule{ben, anyone}, set: SandboxRule{Match: []string{"pairing:ben"}}, want: []SandboxRule{anyone}},
		{name: "removes the last rule", rules: []SandboxRule{ben}, set: SandboxRule{Match: []string{"pairing:ben"}}},
		{name: "keeps a rule with more matchers", rules: []SandboxRule{{Match: []string{"pairing:ben", "private"}}}, set: ben, want: []SandboxRule{{Match: []string{"pairing:ben", "private"}}, ben}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := PolicyConfig{Sandbox: tt.rules}
			p.SetSandboxRule(tt.set)
			if diff := cmp.Diff(tt.want, p.Sandbox); diff != "" {
				t.Fatalf("SetSandboxRule() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestRemoveSandboxMatcher(t *testing.T) {
	t.Parallel()
	p := PolicyConfig{Sandbox: []SandboxRule{
		{Match: []string{"pairing:ben"}, Storage: StorageOwn},
		{Match: []string{"private", "pairing:ben"}, Storage: StorageShared},
		{Match: []string{"guests"}},
	}}
	if !p.RemoveSandboxMatcher("pairing:ben") {
		t.Fatalf("RemoveSandboxMatcher() = false, want true")
	}
	want := []SandboxRule{{Match: []string{"private"}, Storage: StorageShared}, {Match: []string{"guests"}}}
	if diff := cmp.Diff(want, p.Sandbox); diff != "" {
		t.Fatalf("RemoveSandboxMatcher() wrong result (-want +got) diff = %s", diff)
	}
	if p.RemoveSandboxMatcher("pairing:ben") {
		t.Fatalf("RemoveSandboxMatcher() = true, want false")
	}
}

func TestParseSecretGrant(t *testing.T) {
	t.Parallel()
	tests := []struct {
		entry      string
		name, from string
		ok         bool
	}{
		{entry: "KEY", name: "KEY", from: "KEY", ok: true},
		{entry: "KEY=secret:GUEST", name: "KEY", from: "GUEST", ok: true},
		{entry: "KEY=GUEST"},
		{entry: "KEY=secret:"},
		{entry: "not-a-name"},
	}
	for _, tt := range tests {
		t.Run(tt.entry, func(t *testing.T) {
			t.Parallel()
			name, from, ok := ParseSecretGrant(tt.entry)
			if ok != tt.ok || ok && (name != tt.name || from != tt.from) {
				t.Fatalf("ParseSecretGrant(%q) = %q, %q, %v, want %q, %q, %v", tt.entry, name, from, ok, tt.name, tt.from, tt.ok)
			}
		})
	}
}

func TestGuestSettings(t *testing.T) {
	t.Parallel()
	cfg := &Config{Name: "coder", Harness: "opencode", Secrets: []string{"KEY"}, Policy: PolicyConfig{Sandbox: []SandboxRule{{Match: []string{"pairing:ben"}, Storage: StorageOwn}}}}
	next := cfg.Clone()
	f, _ := lookupField("guest-storage")
	f.put(next, []string{StorageShared})
	f, _ = lookupField("guest-secrets")
	f.put(next, []string{"all"})
	want := []SandboxRule{{Match: []string{"pairing:ben"}, Storage: StorageOwn}, {Match: []string{"guests"}, Storage: StorageShared, Secrets: []string{"all"}}}
	if diff := cmp.Diff(want, next.Policy.Sandbox); diff != "" {
		t.Fatalf("put() wrong result (-want +got) diff = %s", diff)
	}
	wantChanges := []Change{
		{Setting: "guest-storage", New: StorageShared, Live: true},
		{Setting: "guest-secrets", Added: []string{"all"}, Live: true},
	}
	if diff := cmp.Diff(wantChanges, Diff(cfg, next)); diff != "" {
		t.Fatalf("Diff() wrong result (-want +got) diff = %s", diff)
	}
	if _, err := (Dir{}).Unset(next, "guest-storage", nil); err != nil {
		t.Fatalf("Unset() error = %v, want nil", err)
	}
	if _, err := (Dir{}).Unset(next, "guest-secrets", nil); err != nil {
		t.Fatalf("Unset() error = %v, want nil", err)
	}
	if diff := cmp.Diff(cfg.Policy.Sandbox, next.Policy.Sandbox); diff != "" {
		t.Fatalf("Unset() wrong result (-want +got) diff = %s", diff)
	}
}

func TestCopyLiveSandboxRules(t *testing.T) {
	t.Parallel()
	running := &Config{Policy: PolicyConfig{Sandbox: []SandboxRule{{Match: []string{"pairing:ben"}, Storage: StorageOwn}}}}
	saved := &Config{Policy: PolicyConfig{Sandbox: []SandboxRule{
		{Match: []string{"pairing:cy"}, Storage: StorageEmpty},
		{Match: []string{"guests"}, Storage: StorageShared, Secrets: []string{"all"}},
	}}}
	CopyLive(running, saved)
	if diff := cmp.Diff(saved.Policy.Sandbox, running.Policy.Sandbox); diff != "" {
		t.Fatalf("CopyLive() wrong rules (-want +got) diff = %s", diff)
	}
}

func TestGuestSecretsReplaceTheList(t *testing.T) {
	t.Parallel()
	cfg := &Config{Secrets: []string{"KEY", "GUEST"}}
	for _, v := range [][]string{{"KEY", "GUEST"}, {"none"}} {
		if _, err := (Settings{GuestSecrets: v}).Apply(cfg, Dir{Root: t.TempDir()}); err != nil {
			t.Fatalf("Apply() error = %v", err)
		}
	}
	want := []SandboxRule{{Match: []string{"guests"}, Secrets: []string{"none"}}}
	if diff := cmp.Diff(want, cfg.Policy.Sandbox); diff != "" {
		t.Fatalf("Apply() wrong rules (-want +got) diff = %s", diff)
	}
}

func TestGuestSettingsMoveAnyoneOutOfASharedRule(t *testing.T) {
	t.Parallel()
	cfg := &Config{Policy: PolicyConfig{Sandbox: []SandboxRule{{Match: []string{"guests", "private"}, Storage: StorageOwn}}}}
	if _, err := (Settings{GuestSecrets: []string{"none"}}).Apply(cfg, Dir{Root: t.TempDir()}); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	want := []SandboxRule{{Match: []string{"private"}, Storage: StorageOwn}, {Match: []string{"guests"}, Storage: StorageOwn, Secrets: []string{"none"}}}
	if diff := cmp.Diff(want, cfg.Policy.Sandbox); diff != "" {
		t.Fatalf("Apply() wrong rules (-want +got) diff = %s", diff)
	}
}

func TestShowSandboxRule(t *testing.T) {
	t.Parallel()
	tests := []struct {
		rule SandboxRule
		want string
	}{
		{rule: SandboxRule{Match: []string{"pairing:ben"}, Storage: StorageOwn, Secrets: []string{"KEY"}}, want: "pairing:ben: own, secrets KEY"},
		{rule: SandboxRule{Match: []string{"guests"}, Secrets: []string{"none"}}, want: "guests: default storage, secrets none"},
	}
	for _, tt := range tests {
		if got := ShowSandboxRule(tt.rule); got != tt.want {
			t.Fatalf("ShowSandboxRule(%v) = %q, want %q", tt.rule, got, tt.want)
		}
	}
}

func TestCopyLiveKeepsARuleThatNamesASecretWaitingForRestart(t *testing.T) {
	t.Parallel()
	running := &Config{Secrets: []string{"KEY"}}
	saved := &Config{Secrets: []string{"KEY", "GUEST"}, Policy: PolicyConfig{Sandbox: []SandboxRule{{Match: []string{"guests"}, Secrets: []string{"KEY=secret:GUEST"}}}}}

	CopyLive(running, saved)

	if len(running.Policy.Sandbox) != 0 {
		t.Fatalf("CopyLive() rules = %v, want none until the restart loads GUEST", running.Policy.Sandbox)
	}
	if diff := Diff(running, saved); !slices.ContainsFunc(diff, func(c Change) bool { return c.Setting == "guest-secrets" }) {
		t.Fatalf("Diff() = %v, want guest-secrets waiting", diff)
	}
}

func TestUnsetSecretThatARuleUses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		rule    SandboxRule
		wantErr string
	}{
		{name: "anonymous", rule: SandboxRule{Match: []string{"guests"}, Secrets: []string{"KEY=secret:G2"}}, wantErr: "G2 is used by the rule of guests; run `a2a-serve config unset guest-secrets` first"},
		{name: "pairing", rule: SandboxRule{Match: []string{"pairing:ben"}, Secrets: []string{"G2"}}, wantErr: "G2 is used by ben's sandbox rule; change it with `a2a-serve pair set ben --secrets` first"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := &Config{Secrets: []string{"KEY", "G2"}, Policy: PolicyConfig{Sandbox: []SandboxRule{tt.rule}}}
			if _, err := (Dir{}).Unset(cfg, "secret", []string{"G2"}); err == nil || err.Error() != tt.wantErr {
				t.Fatalf("Unset() error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}
