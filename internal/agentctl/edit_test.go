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

package agentctl

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
)

func TestEdit(t *testing.T) {
	t.Parallel()
	const (
		valid   = "name: coder\nagent: opencode\nsandbox: local # edited\n"
		invalid = "name: coder\nagent: opencode\nsandbox: vm\n"
	)
	tests := []struct {
		name        string
		edits       []string
		retry       bool
		wantErr     bool
		wantSandbox string
		wantFile    string
		wantChanges int
	}{
		{name: "valid", edits: []string{valid}, wantSandbox: "local", wantFile: valid, wantChanges: 1},
		{name: "invalid, then fixed", edits: []string{invalid, valid}, retry: true, wantSandbox: "local", wantFile: valid, wantChanges: 1},
		{name: "invalid, given up", edits: []string{invalid}, wantErr: true},
		{name: "unchanged", edits: []string{""}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode"}, nil)
			orig, err := os.ReadFile(d.ConfigPath())
			if err != nil {
				t.Fatal(err)
			}
			var opened []string
			edit := func(path string) error {
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				opened = append(opened, string(data))
				next := tc.edits[len(opened)-1]
				if next == "" {
					return nil
				}
				return os.WriteFile(path, []byte(next), 0o600)
			}
			res, editErr := Edit(t.Context(), d, controlapi.NewClient(d), edit, func(error) (bool, error) { return tc.retry, nil })
			var se *agentdir.SettingsError
			if tc.wantErr != errors.As(editErr, &se) {
				t.Fatalf("Edit() error = %v, want a SettingsError %v", editErr, tc.wantErr)
			}
			if len(opened) != len(tc.edits) {
				t.Fatalf("Edit() opened the editor %d times, want %d", len(opened), len(tc.edits))
			}
			if len(opened) > 1 && !strings.HasPrefix(opened[1], `# ERROR: config.yaml: sandbox has "vm"`) {
				t.Fatalf("Edit() reopened %q, want the error on top", opened[1])
			}
			got, err := os.ReadFile(d.ConfigPath())
			if err != nil {
				t.Fatal(err)
			}
			wantFile := tc.wantFile
			if wantFile == "" {
				wantFile = string(orig)
			}
			if diff := cmp.Diff(wantFile, string(got)); diff != "" {
				t.Fatalf("config.yaml wrong result (-want +got) diff = %s", diff)
			}
			if tc.wantErr {
				if got, want := agenterr.HintText(editErr), "fix it with `a2a-serve config edit`"; got != want {
					t.Fatalf("agenterr.HintText() = %q, want %q", got, want)
				}
				return
			}
			if len(res.Changes) != tc.wantChanges {
				t.Fatalf("Edit() changes = %+v, want %d", res.Changes, tc.wantChanges)
			}
		})
	}
}

func TestEditKeepsRejectedDraft(t *testing.T) {
	t.Parallel()
	const (
		valid   = "name: coder\nagent: opencode\nsandbox: local\n"
		invalid = "name: coder\nagent: opencode\nsandbox: vm\n"
	)
	d := newAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode"}, nil)
	write := func(content string) func(string) error {
		return func(path string) error { return os.WriteFile(path, []byte(content), 0o600) }
	}
	giveUp := func(error) (bool, error) { return false, nil }

	_, err := Edit(t.Context(), d, controlapi.NewClient(d), write(invalid), giveUp)
	if err == nil || !strings.Contains(err.Error(), "config.yaml was not changed; your edit is saved in "+d.ConfigDraftPath()) {
		t.Fatalf("Edit() error = %v, want it to name the saved draft %s", err, d.ConfigDraftPath())
	}
	got, err := os.ReadFile(d.ConfigDraftPath())
	if err != nil {
		t.Fatalf("reading the draft: %v", err)
	}
	if diff := cmp.Diff(invalid, string(got)); diff != "" {
		t.Fatalf("draft wrong result (-want +got) diff = %s", diff)
	}

	if _, err := Edit(t.Context(), d, controlapi.NewClient(d), write(valid), giveUp); err != nil {
		t.Fatalf("Edit() error = %v", err)
	}
	if _, err := os.Stat(d.ConfigDraftPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("os.Stat(draft) error = %v, want it removed after a valid edit", err)
	}
}

func TestEditReportsUnknownKeys(t *testing.T) {
	t.Parallel()
	const typo = "name: coder\nagent: opencode\nsandbx: local\n"
	tests := []struct {
		name string
		orig string
		edit string
	}{
		{name: "typo added", orig: "name: coder\nagent: opencode\n", edit: typo},
		{name: "typo kept", orig: typo},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode"}, nil)
			if err := os.WriteFile(d.ConfigPath(), []byte(tc.orig), 0o644); err != nil {
				t.Fatal(err)
			}
			edit := func(path string) error {
				if tc.edit == "" {
					return nil
				}
				return os.WriteFile(path, []byte(tc.edit), 0o600)
			}
			res, err := Edit(t.Context(), d, controlapi.NewClient(d), edit, func(error) (bool, error) { return false, nil })
			if err != nil {
				t.Fatalf("Edit() error = %v", err)
			}
			want := []string{"config.yaml has an unknown key sandbx: did you mean sandbox?"}
			if diff := cmp.Diff(want, res.Warnings); diff != "" {
				t.Fatalf("Edit() warnings wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestEditRepairsFileThatDoesNotParse(t *testing.T) {
	t.Parallel()
	d := newAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode"}, nil)
	if err := os.WriteFile(d.ConfigPath(), []byte("name: coder\nagent: [\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	const fixed = "name: coder\nagent: opencode\n"
	edit := func(path string) error { return os.WriteFile(path, []byte(fixed), 0o600) }

	if _, err := Edit(t.Context(), d, controlapi.NewClient(d), edit, func(error) (bool, error) { return false, nil }); err != nil {
		t.Fatalf("Edit() error = %v", err)
	}

	got, err := os.ReadFile(d.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(fixed, string(got)); diff != "" {
		t.Fatalf("config.yaml wrong result (-want +got) diff = %s", diff)
	}
}

func TestEditStoresSecretValues(t *testing.T) {
	t.Parallel()
	d := newAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode"}, nil)
	edit := func(path string) error {
		return os.WriteFile(path, []byte("name: coder\nagent: opencode\nbackup:\n    to: /backups\n    key: hunter2 # the passphrase\n"), 0o600)
	}

	res, err := Edit(t.Context(), d, controlapi.NewClient(d), edit, func(error) (bool, error) { return false, nil })

	if err != nil {
		t.Fatalf("Edit() error = %v", err)
	}
	if diff := cmp.Diff([]SecretChange{{Name: agentdir.SecretBackupKey, Action: SecretStored}}, res.Secrets); diff != "" {
		t.Fatalf("Edit() secrets wrong result (-want +got) diff = %s", diff)
	}
	for _, c := range res.Changes {
		if c.Old == "hunter2" || c.New == "hunter2" {
			t.Fatalf("Edit() changes = %+v, want no secret value", res.Changes)
		}
	}
	saved, err := os.ReadFile(d.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if want := "key: secret:BACKUP_KEY # the passphrase"; !strings.Contains(string(saved), want) || strings.Contains(string(saved), "hunter2") {
		t.Fatalf("config.yaml = %q, want %q and no secret value", saved, want)
	}
	secrets, err := d.LoadSecrets()
	if err != nil {
		t.Fatalf("LoadSecrets() error = %v", err)
	}
	if diff := cmp.Diff(agentdir.Secrets{agentdir.SecretBackupKey: "hunter2"}, secrets); diff != "" {
		t.Fatalf("stored secrets wrong result (-want +got) diff = %s", diff)
	}
}

func TestEditNormalizesName(t *testing.T) {
	t.Parallel()
	d := newAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode"}, nil)
	edit := func(path string) error {
		return os.WriteFile(path, []byte("name: My Agent # shown to peers\nagent: opencode\n"), 0o600)
	}

	res, err := Edit(t.Context(), d, controlapi.NewClient(d), edit, func(error) (bool, error) { return false, nil })

	if err != nil {
		t.Fatalf("Edit() error = %v", err)
	}
	if diff := cmp.Diff([]agentdir.Change{{Setting: "name", Old: "coder", New: "my-agent"}}, res.Changes); diff != "" {
		t.Fatalf("Edit() changes wrong result (-want +got) diff = %s", diff)
	}
	if res.Name != "my-agent" {
		t.Fatalf("Edit() name = %q, want %q", res.Name, "my-agent")
	}
	saved, err := os.ReadFile(d.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if want := "name: my-agent # shown to peers"; !strings.Contains(string(saved), want) {
		t.Fatalf("config.yaml = %q, want %q", saved, want)
	}
	cfg, err := d.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if cfg.Name != "my-agent" {
		t.Fatalf("saved name = %q, want %q", cfg.Name, "my-agent")
	}
}

func TestEditRefusesMessengers(t *testing.T) {
	t.Parallel()
	const refused = "# ERROR: messengers are changed with a2a-serve messenger add and a2a-serve messenger rm\n"
	telegram := &agentdir.TelegramConfig{Token: "secret:" + agentdir.SecretTelegram}
	tests := []struct {
		name     string
		telegram *agentdir.TelegramConfig
		edit     string
	}{
		{name: "added", edit: "name: coder\nagent: opencode\nmessengers:\n    telegram:\n        token: secret:TOK\n"},
		{name: "changed", telegram: telegram, edit: "name: coder\nagent: opencode\nmessengers:\n    telegram:\n        token: secret:TOK\n"},
		{name: "removed", telegram: telegram, edit: "name: coder\nagent: opencode\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode", Messengers: agentdir.MessengersConfig{Telegram: tc.telegram}}, agentdir.Secrets{agentdir.SecretTelegram: "t", "TOK": "t2"})
			orig, err := os.ReadFile(d.ConfigPath())
			if err != nil {
				t.Fatal(err)
			}
			var opened []string
			edit := func(path string) error {
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				opened = append(opened, string(data))
				if len(opened) > 1 {
					return nil
				}
				return os.WriteFile(path, []byte(tc.edit), 0o600)
			}
			retries := 0
			_, err = Edit(t.Context(), d, controlapi.NewClient(d), edit, func(error) (bool, error) { retries++; return retries == 1, nil })
			var se *agentdir.SettingsError
			if !errors.As(err, &se) {
				t.Fatalf("Edit() error = %v, want a SettingsError", err)
			}
			if len(opened) != 2 || !strings.HasPrefix(opened[1], refused) {
				t.Fatalf("Edit() opened %q, want the file reopened with %q", opened, refused)
			}
			got, err := os.ReadFile(d.ConfigPath())
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(string(orig), string(got)); diff != "" {
				t.Fatalf("config.yaml wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestEditReopensFileChangedMeanwhile(t *testing.T) {
	t.Parallel()
	const edited = "name: coder\nagent: opencode\nsandbox: local\n"
	d := newAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode"}, nil)
	var opened []string
	var draft []byte
	edit := func(path string) error {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		opened = append(opened, string(data))
		if len(opened) > 1 {
			if draft, err = os.ReadFile(d.ConfigDraftPath()); err != nil {
				return err
			}
			return os.WriteFile(path, append(stripErrors(data), "sandbox: local\n"...), 0o600)
		}
		if err := d.UpdateConfig(func(cfg *agentdir.Config) error {
			cfg.Description = "saved meanwhile"
			return nil
		}); err != nil {
			return err
		}
		return os.WriteFile(path, []byte(edited), 0o600)
	}
	var retried []error
	res, err := Edit(t.Context(), d, controlapi.NewClient(d), edit, func(err error) (bool, error) {
		retried = append(retried, err)
		return true, nil
	})
	if err != nil {
		t.Fatalf("Edit() error = %v", err)
	}
	if len(retried) != 1 || !errors.Is(retried[0], ErrConfigChanged) {
		t.Fatalf("Edit() asked to retry with %v, want one %v", retried, ErrConfigChanged)
	}
	if len(opened) != 2 || !strings.HasPrefix(opened[1], "# ERROR: config.yaml changed while you edited it\n") || !strings.Contains(opened[1], "saved meanwhile") {
		t.Fatalf("Edit() opened %q, want the current file reopened with the conflict on top", opened)
	}
	if diff := cmp.Diff(edited, string(draft)); diff != "" {
		t.Fatalf("draft while editing again wrong result (-want +got) diff = %s", diff)
	}
	cfg := loadConfig(t, d)
	if cfg.Description != "saved meanwhile" || cfg.Sandbox != "local" {
		t.Fatalf("saved config = %+v, want the change saved meanwhile and the edit", cfg)
	}
	if diff := cmp.Diff([]agentdir.Change{{Setting: "sandbox", Old: "", New: "local"}}, res.Changes); diff != "" {
		t.Fatalf("Edit() changes wrong result (-want +got) diff = %s", diff)
	}
}

func TestEditKeepsEditWhenFileChangedMeanwhile(t *testing.T) {
	t.Parallel()
	const edited = "name: coder\nagent: opencode\nsandbox: local\n"
	d := newAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode"}, nil)
	edit := func(path string) error {
		if err := d.UpdateConfig(func(cfg *agentdir.Config) error {
			cfg.Description = "saved meanwhile"
			return nil
		}); err != nil {
			return err
		}
		return os.WriteFile(path, []byte(edited), 0o600)
	}

	_, err := Edit(t.Context(), d, controlapi.NewClient(d), edit, func(error) (bool, error) { return false, nil })

	if !errors.Is(err, ErrConfigChanged) || !strings.Contains(err.Error(), "your edit is saved in "+d.ConfigDraftPath()) {
		t.Fatalf("Edit() error = %v, want %v naming the draft %s", err, ErrConfigChanged, d.ConfigDraftPath())
	}
	if cfg := loadConfig(t, d); cfg.Description != "saved meanwhile" || cfg.Sandbox != "" {
		t.Fatalf("saved config = %+v, want only the change saved meanwhile", cfg)
	}
	draft, err := os.ReadFile(d.ConfigDraftPath())
	if err != nil {
		t.Fatalf("reading the draft: %v", err)
	}
	if diff := cmp.Diff(edited, string(draft)); diff != "" {
		t.Fatalf("draft wrong result (-want +got) diff = %s", diff)
	}
}

func TestEditStoresNoSecretWhenFileChangedMeanwhile(t *testing.T) {
	t.Parallel()
	d := newAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode"}, nil)
	edit := func(path string) error {
		if err := d.UpdateConfig(func(cfg *agentdir.Config) error {
			cfg.Description = "saved meanwhile"
			return nil
		}); err != nil {
			return err
		}
		return os.WriteFile(path, []byte("name: coder\nagent: opencode\nbackup:\n    to: /backups\n    key: hunter2\n"), 0o600)
	}

	_, err := Edit(t.Context(), d, controlapi.NewClient(d), edit, func(error) (bool, error) { return false, nil })

	if !errors.Is(err, ErrConfigChanged) {
		t.Fatalf("Edit() error = %v, want %v", err, ErrConfigChanged)
	}
	secrets, err := d.LoadSecrets()
	if err != nil {
		t.Fatalf("LoadSecrets() error = %v", err)
	}
	if len(secrets) != 0 {
		t.Fatalf("stored secrets = %v, want none", secrets)
	}
}

func TestEditResumesDraft(t *testing.T) {
	t.Parallel()
	const (
		valid   = "name: coder\nagent: opencode\nsandbox: local\n"
		invalid = "name: coder\nagent: opencode\nsandbox: vm\n"
	)
	d := newAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode"}, nil)
	giveUp := func(error) (bool, error) { return false, nil }
	if _, err := Edit(t.Context(), d, controlapi.NewClient(d), func(path string) error { return os.WriteFile(path, []byte(invalid), 0o600) }, giveUp); err == nil {
		t.Fatalf("Edit() error = nil, want the invalid edit rejected")
	}

	var opened string
	keep := func(path string) error {
		data, err := os.ReadFile(path)
		opened = string(data)
		return err
	}
	if _, err := Edit(t.Context(), d, controlapi.NewClient(d), keep, giveUp); err == nil {
		t.Fatalf("Edit() of the unchanged draft error = nil, want it rejected again")
	}
	wantNote := "# NOTE: this is your edit saved in " + d.ConfigDraftPath() + "; to discard it, quit, run `rm " + d.ConfigDraftPath() + "`, and run `a2a-serve config edit` again\n"
	if diff := cmp.Diff(wantNote+invalid, opened); diff != "" {
		t.Fatalf("Edit() opened wrong file (-want +got) diff = %s", diff)
	}
	if _, err := os.Stat(d.ConfigDraftPath()); err != nil {
		t.Fatalf("os.Stat(draft) error = %v, want the draft kept", err)
	}

	fix := func(path string) error {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(path, []byte(strings.Replace(string(data), "sandbox: vm", "sandbox: local", 1)), 0o600)
	}
	if _, err := Edit(t.Context(), d, controlapi.NewClient(d), fix, giveUp); err != nil {
		t.Fatalf("Edit() of the fixed draft error = %v", err)
	}
	got, err := os.ReadFile(d.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(valid, string(got)); diff != "" {
		t.Fatalf("config.yaml wrong result (-want +got) diff = %s", diff)
	}
	if _, err := os.Stat(d.ConfigDraftPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("os.Stat(draft) error = %v, want it removed after a valid edit", err)
	}
}

func TestEditRefusesDraftOfOlderFile(t *testing.T) {
	t.Parallel()
	const invalid = "name: coder\nagent: opencode\nsandbox: vm\n"
	d := newAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode"}, nil)
	giveUp := func(error) (bool, error) { return false, nil }
	if _, err := Edit(t.Context(), d, controlapi.NewClient(d), func(path string) error { return os.WriteFile(path, []byte(invalid), 0o600) }, giveUp); err == nil {
		t.Fatalf("Edit() error = nil, want the invalid edit rejected")
	}
	if err := d.UpdateConfig(func(cfg *agentdir.Config) error {
		cfg.Description = "saved since"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	fix := func(path string) error {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(path, []byte(strings.Replace(string(data), "sandbox: vm", "sandbox: local", 1)), 0o600)
	}

	_, err := Edit(t.Context(), d, controlapi.NewClient(d), fix, giveUp)

	if !errors.Is(err, ErrConfigChanged) {
		t.Fatalf("Edit() of a draft of an older config.yaml error = %v, want %v", err, ErrConfigChanged)
	}
	if cfg := loadConfig(t, d); cfg.Description != "saved since" || cfg.Sandbox != "" {
		t.Fatalf("saved config = %+v, want only the change saved since", cfg)
	}
}
