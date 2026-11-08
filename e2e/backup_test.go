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

//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/go-cmp/cmp"
)

// restoreAgent restores the latest snapshot of dest into a new agent
// directory and starts the agent.
func (u *user) restoreAgent(name, dest, key string) *agent {
	u.t.Helper()
	dir := filepath.Join(u.home, name)
	if err := os.Mkdir(dir, 0o700); err != nil {
		u.t.Fatalf("os.Mkdir() error = %v", err)
	}
	a := &agent{u: u, name: name, dir: dir, stopped: true}
	u.t.Cleanup(a.cleanup)
	a.serve("backup", "restore", dest, "--backup-key", key)
	a.serve("start", "--detach")
	a.stopped = false
	a.url = strings.TrimSpace(a.serve("status", "--url=local"))
	return a
}

func TestBackupRestoreKeepsConversationAndPairings(t *testing.T) {
	t.Parallel()
	owner, anna := newUser(t), newUser(t)
	coder := owner.startAgent("coder")
	anna.pairJoin(coder.url, coder.invite("anna"))
	task := owner.mustSend(coder.url, "remember banana")
	dest := filepath.Join(owner.home, "backups")

	coder.serve("config", "set", "--backup", dest, "--backup-key", "e2e-backup-key")
	coder.serve("backup", "now")
	coder.stop()
	restored := owner.restoreAgent("restored", dest, "e2e-backup-key")

	got := resultOf(owner.mustGetTask(restored.url, string(task.ID)))
	if diff := cmp.Diff(taskResult{State: a2a.TaskStateCompleted, Text: "OK."}, got); diff != "" {
		t.Fatalf("a2a task get on the restored agent wrong result (-want +got) diff = %s", diff)
	}
	var paired pairing
	restored.serveJSON(&paired, "pair", "get", "anna")
	if paired.Kind != "person" || paired.Role != "member" || len(paired.Tokens) != 1 {
		t.Fatalf("a2a-serve pair get anna on the restored agent = %+v, want anna's pairing with her token", paired)
	}
	if got := resultOf(anna.mustSend(restored.url, "hello")); got.Text != "Hello from fakeacp." {
		t.Fatalf("a2a send by anna to the restored agent = %+v, want the agent's answer", got)
	}
}

func TestBackupRestoreIntoNewDirectory(t *testing.T) {
	t.Parallel()
	owner := newUser(t)
	coder := owner.startAgent("coder")
	dest := filepath.Join(owner.home, "backups")
	coder.serve("config", "set", "--backup", dest, "--backup-key", "e2e-backup-key")
	coder.serve("backup", "now")
	dir := filepath.Join(owner.home, "restored", "app")

	if _, err := owner.exec(owner.home, "a2a", "serve", "backup", "restore", dest, "--dir", dir, "--backup-key", "wrong-key"); err == nil {
		t.Fatalf("a2a-serve backup restore with the wrong key error = nil, want error")
	}
	if _, err := os.Stat(filepath.Join(owner.home, "restored")); !os.IsNotExist(err) {
		t.Fatalf("a2a-serve backup restore with the wrong key left %s: %v", filepath.Join(owner.home, "restored"), err)
	}
	owner.run(owner.home, "a2a", "serve", "backup", "restore", dest, "--dir", dir, "--backup-key", "e2e-backup-key")
	got, err := os.ReadFile(filepath.Join(dir, ".a2a", ".gitignore"))
	if err != nil || string(got) != "state/\n" {
		t.Fatalf("restored .a2a/.gitignore = %q, %v, want %q", got, err, "state/\n")
	}
}
