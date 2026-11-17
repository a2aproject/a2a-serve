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
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/backup"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/jsonhttp"
)

// serveControl answers the control socket of d with h until the test ends.
func serveControl(t *testing.T, d agentdir.Dir, h http.Handler) {
	t.Helper()
	if err := d.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	ln, err := net.Listen("unix", d.SocketPath())
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Serve() error = %v", err)
		}
	}()
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
}

func TestTakeBackup(t *testing.T) {
	t.Parallel()
	serverSnap := backup.Snapshot{Name: "coder-20261110T120000Z.a2abak", Agent: "coder", Taken: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)}
	tests := []struct {
		name string
		// running is the backup config of the running server, nil when
		// it doesn't run.
		running *agentdir.BackupConfig
		// backupsOff makes the server report that it takes no backups.
		backupsOff bool
		// backups answers POST /backups, nil for a server without it.
		backups    http.HandlerFunc
		wantServer bool
		wantErr    string
	}{
		{name: "stopped"},
		{
			name:    "running",
			running: &agentdir.BackupConfig{},
			backups: func(w http.ResponseWriter, _ *http.Request) {
				jsonhttp.Write(w, http.StatusOK, controlapi.BackupResponse{Snapshot: serverSnap})
			},
			wantServer: true,
		},
		{
			name:    "running with a failed retention",
			running: &agentdir.BackupConfig{},
			backups: func(w http.ResponseWriter, _ *http.Request) {
				jsonhttp.Write(w, http.StatusOK, controlapi.BackupResponse{Snapshot: serverSnap, RetentionError: "AccessDenied"})
			},
			wantServer: true,
			wantErr:    "took snapshot " + serverSnap.Name + ", but removing old snapshots failed: AccessDenied",
		},
		{name: "running an older server", running: &agentdir.BackupConfig{}},
		{name: "running without backups", running: &agentdir.BackupConfig{}, backupsOff: true},
		{name: "running with other backup settings", running: &agentdir.BackupConfig{To: "gs://old/coder"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			to := t.TempDir()
			bcfg := agentdir.BackupConfig{To: to, Key: "secret:BACKUP_KEY"}
			d := newAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode", Backup: bcfg}, agentdir.Secrets{"BACKUP_KEY": "k"})
			if tc.running != nil {
				running := *tc.running
				if running == (agentdir.BackupConfig{}) {
					running = bcfg
				}
				mux := http.NewServeMux()
				mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
					st := controlapi.Status{Name: "coder"}
					if !tc.backupsOff {
						st.Backup = &controlapi.BackupStatus{To: running.To}
					}
					jsonhttp.Write(w, http.StatusOK, st)
				})
				mux.HandleFunc("GET /config", func(w http.ResponseWriter, _ *http.Request) {
					jsonhttp.Write(w, http.StatusOK, agentdir.Config{Name: "coder", Harness: "opencode", Backup: running})
				})
				if tc.backups != nil {
					mux.HandleFunc("POST /backups", tc.backups)
				}
				serveControl(t, d, mux)
			}

			snap, err := TakeBackup(t.Context(), d, controlapi.NewClient(d))

			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(agenterr.HintText(err), "permission to delete") {
					t.Fatalf("TakeBackup() error = %v with hint %q, want %q and the delete permission hint", err, agenterr.HintText(err), tc.wantErr)
				}
			} else if err != nil {
				t.Fatalf("TakeBackup() error = %v", err)
			}
			dest, err := backup.Open(to, nil)
			if err != nil {
				t.Fatal(err)
			}
			local, err := backup.List(t.Context(), dest, "coder")
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantServer {
				if diff := cmp.Diff(serverSnap, snap); diff != "" || len(local) != 0 {
					t.Fatalf("TakeBackup() = %v with local snapshots %v, want the server's snapshot only (-want +got) diff = %s", snap, local, diff)
				}
				return
			}
			if len(local) != 1 || local[0].Name != snap.Name {
				t.Fatalf("TakeBackup() = %v with local snapshots %v, want a local snapshot", snap, local)
			}
		})
	}
}
