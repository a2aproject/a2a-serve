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
	"crypto/rand"
	"database/sql"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
)

// newAgent creates an agent directory with a database, secrets, a harness
// home with a cache and a symlink, and a network identity.
func newAgent(t *testing.T) agentdir.Dir {
	t.Helper()
	return newAgentAt(t, t.TempDir())
}

func newAgentAt(t *testing.T, root string) agentdir.Dir {
	t.Helper()
	d, err := agentdir.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SaveConfig(&agentdir.Config{Name: "coder", Harness: "opencode"}); err != nil {
		t.Fatal(err)
	}
	if err := d.SaveSecrets(agentdir.Secrets{"BACKUP_KEY": "k"}); err != nil {
		t.Fatal(err)
	}
	u := url.URL{Scheme: "file", OmitHost: true, Path: d.DBPath()}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; CREATE TABLE notes (text TEXT); INSERT INTO notes VALUES ('remember me')`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("closing the database: %v", err)
		}
	})
	files := map[string]string{
		"state/ads.json": "{}",
		"state/home/.config/opencode/opencode.json": "{}",
		"state/home/.cache/big":                     "cache",
		"state/home/.npm/pkg":                       "cache",
		"state/homes/u1/.claude/settings.json":      "{}",
		"state/homes/u1/.cache/big":                 "cache",
		"state/tsnet/tailscaled.state":              "identity",
		"state/agentmesh/node.key":                  "identity",
	}
	for p, content := range files {
		full := filepath.Join(d.Path(), p)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("/src/app/.agents/skills/deploy", filepath.Join(d.Path(), "state/home/deploy")); err != nil {
		t.Fatal(err)
	}
	return d
}

// files lists the regular files and symlinks under root, with their
// contents or link targets.
func files(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if e.Type()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(p)
			out[rel] = "-> " + target
			return err
		}
		if strings.HasSuffix(rel, ".db") || strings.HasSuffix(rel, "secrets") || strings.HasSuffix(rel, "config.yaml") {
			out[rel] = "present"
			return nil
		}
		data, err := os.ReadFile(p)
		out[rel] = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestTakeAndRestore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	src := newAgent(t)
	dest, err := Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 2, 19, 35, 0, 0, time.UTC)
	snap, err := Take(ctx, src, dest, Config{Agent: "coder", Key: "k", Retention: time.Hour}, now)
	if err != nil {
		t.Fatalf("Take() error = %v", err)
	}
	if want := (Snapshot{Name: "coder-20261102T193500Z.a2abak", Agent: "coder", Taken: now}); snap != want {
		t.Fatalf("Take() = %+v, want %+v", snap, want)
	}

	tests := []struct {
		name     string
		takeOver bool
		want     map[string]string
	}{
		{
			name: "without the network identity",
			want: map[string]string{
				"config.yaml":       "present",
				"state/secrets":     "present",
				"state/server.db":   "present",
				"state/server.lock": "",
				"state/ads.json":    "{}",
				"state/home/.config/opencode/opencode.json": "{}",
				"state/home/deploy":                         "-> /src/app/.agents/skills/deploy",
				"state/homes/u1/.claude/settings.json":      "{}",
			},
		},
		{
			name:     "taking over",
			takeOver: true,
			want: map[string]string{
				"config.yaml":       "present",
				"state/secrets":     "present",
				"state/server.db":   "present",
				"state/server.lock": "",
				"state/ads.json":    "{}",
				"state/home/.config/opencode/opencode.json": "{}",
				"state/home/deploy":                         "-> /src/app/.agents/skills/deploy",
				"state/homes/u1/.claude/settings.json":      "{}",
				"state/tsnet/tailscaled.state":              "identity",
				"state/agentmesh/node.key":                  "identity",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			if err := Restore(ctx, dest, snap.Name, "k", root, tc.takeOver); err != nil {
				t.Fatalf("Restore() error = %v", err)
			}
			d, err := agentdir.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.want, files(t, d.Path())); diff != "" {
				t.Fatalf("Restore() wrong files (-want +got) diff = %s", diff)
			}
			db, err := sql.Open("sqlite", d.DBPath())
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := db.Close(); err != nil {
					t.Errorf("closing the database: %v", err)
				}
			}()
			var text string
			if err := db.QueryRow(`SELECT text FROM notes`).Scan(&text); err != nil || text != "remember me" {
				t.Fatalf("restored database note = %q, %v, want %q", text, err, "remember me")
			}
			cfg, err := d.LoadConfig()
			if err != nil || cfg.Name != "coder" {
				t.Fatalf("restored config = %+v, %v, want coder", cfg, err)
			}
			secrets, err := d.LoadSecrets()
			if err != nil || secrets["BACKUP_KEY"] != "k" {
				t.Fatalf("restored secrets = %v, %v, want BACKUP_KEY", secrets, err)
			}
		})
	}
}

func TestRestoreErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dest, err := Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := Take(ctx, newAgent(t), dest, Config{Agent: "coder", Key: "k", Retention: time.Hour}, time.Now())
	if err != nil {
		t.Fatalf("Take() error = %v", err)
	}

	empty := t.TempDir()
	if err := Restore(ctx, dest, snap.Name, "wrong", empty, false); err == nil {
		t.Fatalf("Restore() with the wrong key error = nil, want error")
	}
	if _, err := os.Stat(filepath.Join(empty, agentdir.Name)); !os.IsNotExist(err) {
		t.Fatalf("Restore() with the wrong key left %s: %v", agentdir.Name, err)
	}
	existing := newAgent(t)
	if err := Restore(ctx, dest, snap.Name, "k", existing.Root, false); err == nil {
		t.Fatalf("Restore() into an existing agent error = nil, want error")
	}
	other := filepath.Join(t.TempDir(), agentdir.Name)
	if err := os.Mkdir(other, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, other, map[string]string{"relay.yaml": "name: mesh"})
	if err := Restore(ctx, dest, snap.Name, "k", filepath.Dir(other), false); err == nil {
		t.Fatalf("Restore() into a non-empty %s error = nil, want error", agentdir.Name)
	}
	if diff := cmp.Diff(map[string]string{"relay.yaml": "name: mesh"}, files(t, other)); diff != "" {
		t.Fatalf("Restore() into a non-empty %s changed it (-want +got) diff = %s", agentdir.Name, diff)
	}
}

func TestRestoreRemovesLeftoverRestores(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dest, err := Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := Take(ctx, newAgent(t), dest, Config{Agent: "coder", Key: "k", Retention: time.Hour}, time.Now())
	if err != nil {
		t.Fatalf("Take() error = %v", err)
	}
	root := t.TempDir()
	d, err := agentdir.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	leftover := filepath.Join(root, agentdir.Name+".restore-1")
	if err := os.MkdirAll(filepath.Join(leftover, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, leftover, map[string]string{"state/secrets": "plaintext"})
	running := filepath.Join(root, agentdir.Name+".restore-2")
	release, err := lockRestored(d, running)
	if err != nil {
		t.Fatalf("lockRestored() error = %v", err)
	}
	t.Cleanup(func() {
		if err := release(); err != nil {
			t.Errorf("release() error = %v", err)
		}
	})

	if err := Restore(ctx, dest, snap.Name, "k", root, false); err != nil {
		t.Fatalf("Restore() error = %v", err)
	}

	got, err := filepath.Glob(filepath.Join(root, agentdir.Name+".restore-*"))
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{running}, got); diff != "" {
		t.Fatalf("Restore() wrong leftovers (-want +got) diff = %s", diff)
	}
}

func TestRestoreLeavesNoPartialAgent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	src := newAgent(t)
	blob := make([]byte, 256<<10)
	if _, err := rand.Read(blob); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src.HomeDir(), "blob"), blob, 0o600); err != nil {
		t.Fatal(err)
	}
	destDir := t.TempDir()
	dest, err := Open(destDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := Take(ctx, src, dest, Config{Agent: "coder", Key: "k", Retention: time.Hour}, time.Now())
	if err != nil {
		t.Fatalf("Take() error = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(destDir, snap.Name))
	if err != nil {
		t.Fatal(err)
	}
	damaged := "damaged" + Ext
	if err := os.WriteFile(filepath.Join(destDir, damaged), data[:len(data)-100], 0o600); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()

	tests := []struct {
		name     string
		ctx      context.Context
		snapshot string
		emptyDir bool
	}{
		{name: "damaged snapshot", ctx: ctx, snapshot: damaged},
		{name: "damaged snapshot into an empty directory", ctx: ctx, snapshot: damaged, emptyDir: true},
		{name: "canceled", ctx: canceled, snapshot: snap.Name},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			d, err := agentdir.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			var want []string
			if tc.emptyDir {
				if err := os.Mkdir(d.Path(), 0o700); err != nil {
					t.Fatal(err)
				}
				want = []string{d.Path()}
			}

			if err := Restore(tc.ctx, dest, tc.snapshot, "k", root, false); err == nil {
				t.Fatalf("Restore() error = nil, want error")
			}
			got, err := filepath.Glob(filepath.Join(root, agentdir.Name+"*"))
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Fatalf("Restore() wrong leftovers (-want +got) diff = %s", diff)
			}
			if tc.emptyDir {
				if diff := cmp.Diff(map[string]string{}, files(t, d.Path())); diff != "" {
					t.Fatalf("Restore() wrote into %s (-want +got) diff = %s", agentdir.Name, diff)
				}
			}
			if err := Restore(ctx, dest, snap.Name, "k", root, false); err != nil {
				t.Fatalf("Restore() after a failed restore error = %v", err)
			}
		})
	}
}

func TestPrune(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	dest, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	for _, name := range []string{
		"coder-20261101T120000Z.a2abak",
		"coder-20261105T120000Z.a2abak",
		"coder-20261109T120000Z.a2abak",
		"other-20261101T120000Z.a2abak",
		"notes.txt",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := Prune(ctx, dest, "coder", now, 7*24*time.Hour); err != nil {
		t.Fatalf("Prune() error = %v", err)
	}
	if err := Prune(ctx, dest, "other", now, 24*time.Hour); err != nil {
		t.Fatalf("Prune() error = %v", err)
	}
	names, err := dest.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"coder-20261105T120000Z.a2abak", "coder-20261109T120000Z.a2abak", "notes.txt", "other-20261101T120000Z.a2abak"}
	if diff := cmp.Diff(want, names); diff != "" {
		t.Fatalf("Prune() kept wrong snapshots (-want +got) diff = %s", diff)
	}
	latest, err := Latest(ctx, dest)
	if err != nil || latest.Name != "coder-20261109T120000Z.a2abak" {
		t.Fatalf("Latest() = %+v, %v, want the newest snapshot", latest, err)
	}
}
