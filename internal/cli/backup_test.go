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
	"strings"
	"testing"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/clierr"
)

func TestBackupAndRestore(t *testing.T) {
	t.Parallel()
	to := t.TempDir()
	d := newTestAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode", Backup: agentdir.BackupConfig{To: to, Key: "secret:BACKUP_KEY"}})
	if err := d.SaveSecrets(agentdir.Secrets{"BACKUP_KEY": "k"}); err != nil {
		t.Fatal(err)
	}

	if out, err := runCMD(t, "backup", "list", "--dir", d.Root); err != nil || !strings.Contains(out, "no snapshots yet") {
		t.Fatalf("a2a serve backup list = %q, %v, want no snapshots", out, err)
	}
	if out, err := runCMD(t, "backup", "now", "--dir", d.Root); err != nil || !strings.Contains(out, "took snapshot coder-") {
		t.Fatalf("a2a serve backup now = %q, %v, want a snapshot", out, err)
	}
	out, err := runCMD(t, "-o", "json", "backup", "list", "--dir", d.Root)
	if err != nil {
		t.Fatalf("a2a serve backup list error = %v", err)
	}
	var snaps []struct {
		Name  string `json:"name"`
		Agent string `json:"agent"`
	}
	if err := json.Unmarshal([]byte(out), &snaps); err != nil || len(snaps) != 1 || snaps[0].Agent != "coder" {
		t.Fatalf("a2a serve backup list -o json = %q, %v, want one snapshot of coder", out, err)
	}

	target := t.TempDir()
	if out, err := runCMD(t, "restore", to, "--dir", target, "--backup-key", "k"); err != nil || !strings.Contains(out, "restored "+snaps[0].Name) {
		t.Fatalf("a2a serve restore = %q, %v, want the snapshot restored", out, err)
	}
	restored, err := agentdir.Open(target)
	if err != nil {
		t.Fatal(err)
	}
	if conf, err := restored.LoadConfig(); err != nil || conf.Name != "coder" {
		t.Fatalf("restored config = %+v, %v, want coder", conf, err)
	}
	if _, err := runCMD(t, "restore", to, "--dir", target, "--backup-key", "k"); err == nil {
		t.Fatalf("a2a serve restore into an existing agent error = nil, want error")
	}
}

func TestRestoreUsageErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "no key", args: []string{"restore", t.TempDir()}, wantErr: "--backup-key is required"},
		{name: "stored secret", args: []string{"restore", t.TempDir(), "--backup-key", "secret:BACKUP_KEY"}, wantErr: "a restore has no stored secrets"},
		{name: "unknown destination", args: []string{"restore", "ftp://host/dir", "--backup-key", "k"}, wantErr: "unsupported backup destination"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := runCMD(t, tc.args...)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("a2a serve %s error = %v, want %q", strings.Join(tc.args, " "), err, tc.wantErr)
			}
			if got := clierr.Classify(err).Code; got != clierr.CodeUsage {
				t.Fatalf("a2a serve %s error code = %v, want %v", strings.Join(tc.args, " "), got, clierr.CodeUsage)
			}
		})
	}
}
