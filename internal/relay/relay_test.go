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

package relay

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/relay/meshadmin"
	"github.com/a2aproject/a2a-serve/internal/relaydir"
)

func TestMeshArgsAndEnv(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		config   MeshConfig
		wantArgs []string
		wantEnv  []string
	}{
		{
			name:   "public URL and SQLite",
			config: MeshConfig{URL: "https://relay.example.com", Port: 4100, DataDir: "/r/agentmesh", AdminToken: "tok"},
			wantArgs: []string{
				"--bind-address", "127.0.0.1", "--port", "4100", "--external-url", "https://relay.example.com",
				"--data-dir", "/r/agentmesh", "--no-join-token", "--enroll-qr=false", "--router-allow-loopback=false",
			},
			wantEnv: []string{"HOME=/home/me", "AGENTMESH_ADMIN_TOKEN=tok"},
		},
		{
			name:   "loopback URL and PostgreSQL",
			config: MeshConfig{URL: "http://127.0.0.1:8443", Port: 4100, DataDir: "/r/agentmesh", AdminToken: "tok", Database: "postgres://db"},
			wantArgs: []string{
				"--bind-address", "127.0.0.1", "--port", "4100", "--external-url", "http://127.0.0.1:8443",
				"--data-dir", "/r/agentmesh", "--no-join-token", "--enroll-qr=false", "--router-allow-loopback=true",
				"--db-driver", "postgres",
			},
			wantEnv: []string{"HOME=/home/me", "AGENTMESH_ADMIN_TOKEN=tok", "AGENTMESH_DB_DSN=postgres://db"},
		},
		{
			name:   "sign-in",
			config: MeshConfig{URL: "https://relay.example.com", Port: 4100, DataDir: "/r/agentmesh", AdminToken: "tok", Issuer: "https://relay.example.com/a2a-serve/signin"},
			wantArgs: []string{
				"--bind-address", "127.0.0.1", "--port", "4100", "--external-url", "https://relay.example.com",
				"--data-dir", "/r/agentmesh", "--no-join-token", "--enroll-qr=false", "--router-allow-loopback=false",
				"--issuer", "https://relay.example.com/a2a-serve/signin", "--workload-issuer", "https://relay.example.com/a2a-serve/signin",
				"--oidc-client-id", "a2a-serve", "--allowed-audiences", "a2a-serve", "--control-plane-workload-session-ttl", "24h0m0s",
			},
			wantEnv: []string{"HOME=/home/me", "AGENTMESH_ADMIN_TOKEN=tok"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(tt.wantArgs, MeshArgs(tt.config)); diff != "" {
				t.Fatalf("MeshArgs() wrong result (-want +got) diff = %s", diff)
			}
			base := []string{"HOME=/home/me", "AGENTMESH_TOKEN=stray", "AGENTMESH_ADMIN_TOKEN=stray"}
			if diff := cmp.Diff(tt.wantEnv, MeshEnv(tt.config, base)); diff != "" {
				t.Fatalf("MeshEnv() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestPolicyLetsNodesServeA2AOnly(t *testing.T) {
	t.Parallel()
	p := Policy(false)
	if len(p.Bindings) != 0 {
		t.Fatalf("Policy(false) bindings = %v, want none: invited agents get their role from the token", p.Bindings)
	}
	for _, r := range p.Roles {
		if r.Name != "mesh:role:node" {
			continue
		}
		if diff := cmp.Diff([]string{"a2a://*", "system://mesh.catalog"}, r.AllowedServices); diff != "" {
			t.Fatalf("Policy() node role wrong services (-want +got) diff = %s", diff)
		}
		for _, list := range [][]string{r.AllowedPublish, r.AllowedIngress[0].Services} {
			if diff := cmp.Diff([]string{"a2a://*"}, list); diff != "" {
				t.Fatalf("Policy() node role wrong published services (-want +got) diff = %s", diff)
			}
		}
		if diff := cmp.Diff([]string{"INGRESS_AUTH_MODE_PASSTHROUGH"}, r.AllowedIngress[0].AllowedAuthModes); diff != "" {
			t.Fatalf("Policy() node role wrong ingress modes (-want +got) diff = %s", diff)
		}
		return
	}
	t.Fatal("Policy() has no node role")
}

func TestPolicyWithSignInLetsSignedInAgentsJoin(t *testing.T) {
	t.Parallel()
	want := []meshadmin.Binding{{Role: "mesh:role:node", Members: []string{"mesh:system:authenticated"}}}
	if diff := cmp.Diff(want, Policy(true).Bindings); diff != "" {
		t.Fatalf("Policy(true) bindings wrong result (-want +got) diff = %s", diff)
	}
}

func newRelayDir(t *testing.T) relaydir.Dir {
	t.Helper()
	d := relaydir.Dir{Root: t.TempDir()}
	if _, err := d.SaveSettings(relaydir.Settings{URL: "https://relay.example.com"}); err != nil {
		t.Fatalf("SaveSettings() error = %v", err)
	}
	return d
}

func writeMeshDB(t *testing.T, d relaydir.Dir) {
	t.Helper()
	if err := os.MkdirAll(d.MeshDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	u := url.URL{Scheme: "file", OmitHost: true, Path: d.MeshDBPath()}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE nodes (peer_id TEXT); INSERT INTO nodes VALUES ('12D3coder')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareData(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		noDB       bool
		recorded   string
		pin        string
		postgres   bool
		want       UpgradeResult
		wantRecord string
		wantBackup string
		wantErr    string
	}{
		{name: "first run", noDB: true, pin: "v0.2.0", want: UpgradeResult{To: "v0.2.0"}, wantRecord: "v0.2.0"},
		{name: "data without a record", pin: "v0.2.0", want: UpgradeResult{To: "v0.2.0"}, wantRecord: "v0.2.0", wantBackup: "agentmesh-unknown-20261105T090000Z.db"},
		{name: "same version", recorded: "v0.2.0", pin: "v0.2.0", want: UpgradeResult{From: "v0.2.0", To: "v0.2.0"}, wantRecord: "v0.2.0"},
		{name: "upgrade backs up first", recorded: "v0.2.0", pin: "v0.3.0", want: UpgradeResult{From: "v0.2.0", To: "v0.3.0"}, wantRecord: "v0.3.0", wantBackup: "agentmesh-v0.2.0-20261105T090000Z.db"},
		{name: "upgrade on PostgreSQL", recorded: "v0.2.0", pin: "v0.3.0", postgres: true, want: UpgradeResult{From: "v0.2.0", To: "v0.3.0"}, wantRecord: "v0.3.0"},
		{name: "downgrade", recorded: "v0.3.0", pin: "v0.2.0", wantRecord: "v0.3.0", wantErr: "last used by Agent Mesh v0.3.0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d := newRelayDir(t)
			if !tt.noDB {
				writeMeshDB(t, d)
			}
			if tt.recorded != "" {
				if err := d.RecordVersion(tt.recorded); err != nil {
					t.Fatal(err)
				}
			}

			got, err := PrepareData(context.Background(), d, tt.pin, tt.postgres, now)

			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("PrepareData() error = %v, want one containing %q", err, tt.wantErr)
				}
			} else if err != nil {
				t.Fatalf("PrepareData() error = %v", err)
			}
			if rec, err := d.RecordedVersion(); err != nil || rec != tt.wantRecord {
				t.Fatalf("RecordedVersion() = %q, %v, want %q", rec, err, tt.wantRecord)
			}
			if tt.wantBackup != "" {
				want := filepath.Join(d.BackupsDir(), tt.wantBackup)
				if got.Backup != want {
					t.Fatalf("PrepareData() backup = %q, want %q", got.Backup, want)
				}
				checkBackup(t, got.Backup)
				got.Backup = ""
			}
			if tt.wantErr == "" {
				if diff := cmp.Diff(tt.want, got); diff != "" {
					t.Fatalf("PrepareData() wrong result (-want +got) diff = %s", diff)
				}
			}
		})
	}
}

func checkBackup(t *testing.T, path string) {
	t.Helper()
	u := url.URL{Scheme: "file", OmitHost: true, Path: path}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	var peer string
	if err := db.QueryRow(`SELECT peer_id FROM nodes`).Scan(&peer); err != nil || peer != "12D3coder" {
		t.Fatalf("the backup has %q, %v, want the database's rows", peer, err)
	}
}

func TestPrepareDataKeepsTheLastThreeBackups(t *testing.T) {
	t.Parallel()
	d := newRelayDir(t)
	writeMeshDB(t, d)
	if err := d.RecordVersion("v0.2.0"); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	for i, pin := range []string{"v0.3.0", "v0.4.0", "v0.5.0", "v0.6.0"} {
		res, err := PrepareData(context.Background(), d, pin, false, now.Add(time.Duration(i)*time.Hour))
		if err != nil {
			t.Fatalf("PrepareData(%s) error = %v", pin, err)
		}
		// Backups are ordered by their time on disk.
		mod := now.Add(time.Duration(i) * time.Hour)
		if err := os.Chtimes(res.Backup, mod, mod); err != nil {
			t.Fatal(err)
		}
	}

	backups, err := listBackups(d)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, b := range backups {
		names = append(names, filepath.Base(b))
	}
	want := []string{"agentmesh-v0.3.0-20261105T100000Z.db", "agentmesh-v0.4.0-20261105T110000Z.db", "agentmesh-v0.5.0-20261105T120000Z.db"}
	if diff := cmp.Diff(want, names); diff != "" {
		t.Fatalf("backups wrong (-want +got) diff = %s", diff)
	}
}

func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func zipped(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func TestFetcher(t *testing.T) {
	t.Parallel()
	linux := tarGz(t, map[string]string{"README.md": "readme", "agentmesh-one": "linux binary"})
	windows := zipped(t, map[string]string{"agentmesh-one.exe": "windows binary"})
	tests := []struct {
		name         string
		goos         string
		archive      []byte
		hash         string
		override     string
		wantBinary   string
		wantErr      string
		wantDownload bool
	}{
		{name: "tar.gz", goos: "linux", archive: linux, hash: sum(linux), wantBinary: "linux binary", wantDownload: true},
		{name: "zip", goos: "windows", archive: windows, hash: sum(windows), wantBinary: "windows binary", wantDownload: true},
		{name: "wrong hash", goos: "linux", archive: linux, hash: sum([]byte("other")), wantErr: "SHA-256"},
		{name: "override", goos: "linux", override: "/opt/agentmesh-one"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			downloads := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				downloads++
				if _, err := w.Write(tt.archive); err != nil {
					t.Errorf("writing: %v", err)
				}
			}))
			t.Cleanup(srv.Close)
			name := map[string]string{"linux": "agentmesh_Linux_x86_64.tar.gz", "windows": "agentmesh_Windows_x86_64.zip"}[tt.goos]
			f := &Fetcher{
				cacheDir: t.TempDir(), override: tt.override, hc: srv.Client(), goos: tt.goos, goarch: "amd64",
				archives: map[string]string{name: tt.hash}, url: func(n string) string { return srv.URL + "/" + n },
			}

			path, err := f.Get(context.Background())

			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Fetcher.Get() error = %v, want one containing %q", err, tt.wantErr)
				}
				if _, err := os.Stat(f.Path()); !os.IsNotExist(err) {
					t.Fatalf("Stat(%s) error = %v, want no binary after a failed check", f.Path(), err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Fetcher.Get() error = %v", err)
			}
			if tt.override != "" {
				if path != tt.override || downloads != 0 {
					t.Fatalf("Fetcher.Get() = %s after %d downloads, want the override and none", path, downloads)
				}
				return
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != tt.wantBinary {
				t.Fatalf("the fetched binary = %q, %v, want %q", data, err, tt.wantBinary)
			}
			if _, err := f.Get(context.Background()); err != nil || downloads != 1 {
				t.Fatalf("a second Fetcher.Get() = %v after %d downloads, want the cached binary", err, downloads)
			}
		})
	}
}
