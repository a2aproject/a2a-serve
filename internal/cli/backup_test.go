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
	"os"
	"path/filepath"
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
		t.Fatalf("a2a-serve backup list = %q, %v, want no snapshots", out, err)
	}
	if out, err := runCMD(t, "backup", "now", "--dir", d.Root); err != nil || !strings.Contains(out, "took snapshot coder-") {
		t.Fatalf("a2a-serve backup now = %q, %v, want a snapshot", out, err)
	}
	out, err := runCMD(t, "-o", "json", "backup", "list", "--dir", d.Root)
	if err != nil {
		t.Fatalf("a2a-serve backup list error = %v", err)
	}
	var snaps []struct {
		Name  string `json:"name"`
		Agent string `json:"agent"`
	}
	if err := json.Unmarshal([]byte(out), &snaps); err != nil || len(snaps) != 1 || snaps[0].Agent != "coder" {
		t.Fatalf("a2a-serve backup list -o json = %q, %v, want one snapshot of coder", out, err)
	}

	target := t.TempDir()
	_, err = runCMD(t, "backup", "restore", to, "--snapshot", "nope", "--dir", target, "--backup-key", "k")
	if want := "omit --snapshot to restore the latest one, " + snaps[0].Name; clierr.Classify(err).Hint != want {
		t.Fatalf("a2a-serve backup restore --snapshot nope error = %v with hint %q, want hint %q", err, clierr.Classify(err).Hint, want)
	}
	if out, err := runCMD(t, "backup", "restore", to, "--dir", target, "--backup-key", "k"); err != nil || !strings.Contains(out, "restored "+snaps[0].Name) {
		t.Fatalf("a2a-serve backup restore = %q, %v, want the snapshot restored", out, err)
	}
	restored, err := agentdir.Open(target)
	if err != nil {
		t.Fatal(err)
	}
	if conf, err := restored.LoadConfig(); err != nil || conf.Name != "coder" {
		t.Fatalf("restored config = %+v, %v, want coder", conf, err)
	}
	if _, err := runCMD(t, "backup", "restore", to, "--dir", target, "--backup-key", "k"); err == nil {
		t.Fatalf("a2a-serve backup restore into an existing agent error = nil, want error")
	}
}

func TestRestoreUsageErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "no key", args: []string{"backup", "restore", t.TempDir()}, wantErr: "--backup-key is required"},
		{name: "stored secret", args: []string{"backup", "restore", t.TempDir(), "--backup-key", "secret:BACKUP_KEY"}, wantErr: "secret:NAME only applies to agent settings"},
		{name: "unknown destination", args: []string{"backup", "restore", "ftp://host/dir", "--backup-key", "k"}, wantErr: "unsupported backup destination"},
		{name: "missing snapshot", args: []string{"backup", "restore", t.TempDir(), "--snapshot", "nope", "--backup-key", "k", "--dir", t.TempDir()}, wantErr: "no snapshot nope"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := runCMD(t, tc.args...)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("a2a-serve %s error = %v, want %q", strings.Join(tc.args, " "), err, tc.wantErr)
			}
			if got := clierr.Classify(err).Code; got != clierr.CodeUsage {
				t.Fatalf("a2a-serve %s error code = %v, want %v", strings.Join(tc.args, " "), got, clierr.CodeUsage)
			}
		})
	}
}

func TestBackupRestoreHelpDescribesDir(t *testing.T) {
	t.Parallel()

	out, err := runCMD(t, "backup", "restore", "--help")
	if err != nil {
		t.Fatalf("a2a-serve backup restore --help error = %v", err)
	}

	if want := "Directory of the restored agent (default: the current directory)"; !strings.Contains(out, want) {
		t.Fatalf("a2a-serve backup restore --help = %q, want %q", out, want)
	}
	if bad := "nearest directory with .a2a"; strings.Contains(out, bad) {
		t.Fatalf("a2a-serve backup restore --help = %q, want no %q", out, bad)
	}
}

// TestBackupNowReportsFailedRetention can't run in parallel because of t.Setenv.
func TestBackupNowReportsFailedRetention(t *testing.T) {
	bin := t.TempDir()
	store := t.TempDir()
	script := `#!/bin/sh
case "$2" in
cp) cat > "$FAKE_BUCKET/$(basename "$4")" ;;
ls) ls "$FAKE_BUCKET" | sed "s|^|$3/|" ;;
rm) echo "AccessDeniedException: 403 no storage.objects.delete access" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "gcloud"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_BUCKET", store)
	if err := os.WriteFile(filepath.Join(store, "coder-20200101T000000Z.a2abak"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	d := newTestAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode", Backup: agentdir.BackupConfig{To: "gs://bucket/coder", Key: "secret:BACKUP_KEY"}})
	if err := d.SaveSecrets(agentdir.Secrets{"BACKUP_KEY": "k"}); err != nil {
		t.Fatal(err)
	}

	_, err := runCMD(t, "backup", "now", "--dir", d.Root)

	ce := clierr.Classify(err)
	if err == nil || ce.Exit != 1 {
		t.Fatalf("a2a-serve backup now error = %v with exit %d, want exit 1", err, ce.Exit)
	}
	for _, want := range []string{"took snapshot coder-", "AccessDeniedException"} {
		if !strings.Contains(ce.Message, want) {
			t.Fatalf("a2a-serve backup now error = %q, want it to contain %q", ce.Message, want)
		}
	}
	if !strings.Contains(ce.Hint, "permission to delete old snapshots") {
		t.Fatalf("a2a-serve backup now error hint = %q, want it to name the delete permission", ce.Hint)
	}
	entries, err := os.ReadDir(store)
	if err != nil || len(entries) != 2 {
		t.Fatalf("os.ReadDir(bucket) = %v, %v, want the new snapshot and the old one", entries, err)
	}
}
