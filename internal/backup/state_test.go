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

package backup

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// newCommandState gives the agent in root the state/ that commands such as
// `config set` and `pair list` create: a new database and the backup key.
func newCommandState(t *testing.T, root string, change func(*store.Store)) agentdir.Dir {
	t.Helper()
	d, err := agentdir.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SaveConfig(&agentdir.Config{Name: "coder", Harness: "opencode"}); err != nil {
		t.Fatal(err)
	}
	if err := d.SaveSecrets(agentdir.Secrets{agentdir.SecretBackupKey: "k"}); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(d.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	if change != nil {
		change(st)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestRestoreMovesAsideStateWithoutAgentData(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dest, err := Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := takeFast(ctx, newAgent(t), dest, Config{Agent: "coder", Key: "k", Retention: time.Hour}, time.Now())
	if err != nil {
		t.Fatalf("Take() error = %v", err)
	}
	root := t.TempDir()
	d := newCommandState(t, root, nil)

	notes, err := restoreFast(ctx, dest, snap.Name, "k", root, false)
	if err != nil {
		t.Fatalf("Restore() error = %v", err)
	}

	aside, err := filepath.Glob(filepath.Join(d.Path(), "state.before-restore-*"))
	if err != nil || len(aside) != 1 {
		t.Fatalf("moved-aside state = %v, %v, want one", aside, err)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], filepath.Join(agentdir.Name, filepath.Base(aside[0]))) {
		t.Fatalf("Restore() notes = %q, want one naming %s", notes, aside[0])
	}
	if got := files(t, aside[0]); got["server.db"] != "present" || got["secrets"] != "present" || got[".gitignore"] != "*\n" {
		t.Fatalf("moved-aside state files = %v, want the database, the secrets and a .gitignore of *", got)
	}
	if got := files(t, d.StateDir()); got["home/.config/opencode/opencode.json"] != "{}" {
		t.Fatalf("restored state files = %v, want the snapshot's", got)
	}
}

func TestRestoreRefusesStateWithAgentData(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dest, err := Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := takeFast(ctx, newAgent(t), dest, Config{Agent: "coder", Key: "k", Retention: time.Hour}, time.Now())
	if err != nil {
		t.Fatalf("Take() error = %v", err)
	}

	tests := []struct {
		name    string
		setup   func(t *testing.T, root string) (release func() error)
		wantErr string
	}{
		{
			name: "contact",
			setup: func(t *testing.T, root string) func() error {
				newCommandState(t, root, func(st *store.Store) {
					if err := st.PutContact(context.Background(), store.Contact{Name: "bob", URL: "http://bob", Via: "direct"}); err != nil {
						t.Fatal(err)
					}
				})
				return nil
			},
			wantErr: "already has an agent",
		},
		{
			name: "secret other than the backup key",
			setup: func(t *testing.T, root string) func() error {
				d := newCommandState(t, root, nil)
				if err := d.SaveSecrets(agentdir.Secrets{"TELEGRAM_TOKEN": "x"}); err != nil {
					t.Fatal(err)
				}
				return nil
			},
			wantErr: "already has an agent",
		},
		{
			name: "running",
			setup: func(t *testing.T, root string) func() error {
				release, err := newCommandState(t, root, nil).Lock()
				if err != nil {
					t.Fatal(err)
				}
				return release
			},
			wantErr: "a2a-serve stop --dir ",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			if release := tc.setup(t, root); release != nil {
				t.Cleanup(func() {
					if err := release(); err != nil {
						t.Errorf("release() error = %v", err)
					}
				})
			}
			before := paths(t, root)

			_, err := restoreFast(ctx, dest, snap.Name, "k", root, false)

			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Restore() error = %v, want %q", err, tc.wantErr)
			}
			if diff := cmp.Diff(before, paths(t, root)); diff != "" {
				t.Fatalf("Restore() changed the directory (-want +got) diff = %s", diff)
			}
		})
	}
}
