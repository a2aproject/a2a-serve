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

package doctor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentmesh"
	"github.com/a2aproject/a2a-serve/internal/relay/control"
	"github.com/a2aproject/a2a-serve/internal/relaydir"
)

// fakeRelay serves /readyz with ready, and the version route with info, or
// 404 when info is nil.
func fakeRelay(t *testing.T, ready bool, info *agentmesh.Info) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/readyz":
			if !ready {
				w.WriteHeader(http.StatusServiceUnavailable)
			}
		case agentmesh.InfoPath:
			if info == nil {
				http.NotFound(w, r)
				return
			}
			if err := json.NewEncoder(w).Encode(info); err != nil {
				t.Errorf("writing: %v", err)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestRelayOnline(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		ready bool
		info  *agentmesh.Info
		want  Status
	}{
		{name: "same version", ready: true, info: &agentmesh.Info{AgentMesh: agentmesh.Version}, want: StatusOK},
		{name: "another patch version", ready: true, info: &agentmesh.Info{AgentMesh: "v0.2.9"}, want: StatusWarn},
		{name: "another minor version", ready: true, info: &agentmesh.Info{AgentMesh: "v0.3.0"}, want: StatusFail},
		{name: "no version route", ready: true, want: StatusWarn},
		{name: "not ready", info: &agentmesh.Info{AgentMesh: agentmesh.Version}, want: StatusFail},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := relayOnline(context.Background(), http.DefaultClient, fakeRelay(t, tt.ready, tt.info))
			if diff := cmp.Diff([]check{{"relay", tt.want}}, checks(got)); diff != "" {
				t.Fatalf("relayOnline() wrong result (-want +got) diff = %s\n%+v", diff, got)
			}
		})
	}
}

func newTestRelayDir(t *testing.T, url string) relaydir.Dir {
	t.Helper()
	d := relaydir.Dir{Root: t.TempDir()}
	if _, err := d.SaveSettings(relaydir.Settings{URL: url}); err != nil {
		t.Fatalf("SaveSettings() error = %v", err)
	}
	if _, err := d.Resolve(); err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	return d
}

func TestRunRelay(t *testing.T) {
	t.Parallel()
	relayURL := fakeRelay(t, true, &agentmesh.Info{AgentMesh: agentmesh.Version})
	tests := []struct {
		name    string
		prepare func(t *testing.T, d relaydir.Dir)
		status  *control.Status
		online  bool
		want    []check
	}{
		{
			name: "fresh relay, stopped",
			want: []check{{"relay-config", StatusOK}, {"agentmesh-one", StatusOK}, {"relay-data", StatusOK}, {"relay-running", StatusWarn}},
		},
		{
			name:   "running and online",
			status: &control.Status{URL: relayURL, PID: 7},
			online: true,
			want:   []check{{"relay-config", StatusOK}, {"agentmesh-one", StatusOK}, {"relay-data", StatusOK}, {"relay-running", StatusOK}, {"relay", StatusOK}},
		},
		{
			name: "state open to others",
			prepare: func(t *testing.T, d relaydir.Dir) {
				if err := os.Chmod(d.SecretsPath(), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want: []check{{"relay-config", StatusOK}, {"state-permissions", StatusFail}, {"agentmesh-one", StatusOK}, {"relay-data", StatusOK}, {"relay-running", StatusWarn}},
		},
		{
			name: "data of an older Agent Mesh",
			prepare: func(t *testing.T, d relaydir.Dir) {
				if err := d.RecordVersion("v0.1.0"); err != nil {
					t.Fatal(err)
				}
			},
			want: []check{{"relay-config", StatusOK}, {"agentmesh-one", StatusOK}, {"relay-data", StatusWarn}, {"relay-running", StatusWarn}},
		},
		{
			name: "data of a newer Agent Mesh",
			prepare: func(t *testing.T, d relaydir.Dir) {
				if err := d.RecordVersion("v9.0.0"); err != nil {
					t.Fatal(err)
				}
			},
			want: []check{{"relay-config", StatusOK}, {"agentmesh-one", StatusOK}, {"relay-data", StatusFail}, {"relay-running", StatusWarn}},
		},
		{
			name: "secret missing",
			prepare: func(t *testing.T, d relaydir.Dir) {
				if err := os.Remove(d.SecretsPath()); err != nil {
					t.Fatal(err)
				}
			},
			want: []check{{"relay-config", StatusFail}, {"agentmesh-one", StatusOK}, {"relay-data", StatusOK}, {"relay-running", StatusWarn}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d := newTestRelayDir(t, relayURL)
			if tt.prepare != nil {
				tt.prepare(t, d)
			}
			env := newFakeMachine(t).Env()
			env.RelayStatus = func(context.Context, relaydir.Dir) (*control.Status, error) {
				if tt.status == nil {
					return nil, control.ErrNotRunning
				}
				return tt.status, nil
			}

			got := relayChecks(context.Background(), env, RelayOptions{Dir: d, Binary: filepath.Join(t.TempDir(), "agentmesh-one"), Online: tt.online})

			if diff := cmp.Diff(tt.want, checks(got)); diff != "" {
				t.Fatalf("relayChecks() wrong result (-want +got) diff = %s\n%+v", diff, got)
			}
		})
	}
}

func TestRelayStatePermissionsFix(t *testing.T) {
	t.Parallel()
	d := newTestRelayDir(t, "https://relay.example.com")
	if err := os.Chmod(d.StateDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	env := newFakeMachine(t).Env()

	for _, r := range relayChecks(context.Background(), env, RelayOptions{Dir: d}) {
		if r.Fixable() {
			if _, err := r.Apply(); err != nil {
				t.Fatalf("Result.Apply() error = %v", err)
			}
		}
	}

	info, err := os.Stat(d.StateDir())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("state directory mode after the fix = %v, want 0700", info.Mode().Perm())
	}
}
