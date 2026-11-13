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

package download_test

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-serve/internal/download"
)

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
			rel := download.Release{
				Program: "agentmesh-one", Version: "v0.2.0", CacheDir: "agentmesh-v0.2.0",
				Binary: func(goos string) string {
					if goos == "windows" {
						return "agentmesh-one.exe"
					}
					return "agentmesh-one"
				},
				Archive:  func(goos, _ string) (string, error) { return name, nil },
				Archives: map[string]string{name: tt.hash},
				URL:      func(n string) string { return srv.URL + "/" + n },
			}
			f := download.NewFetcher(rel, t.TempDir(), tt.override, srv.Client(), tt.goos, "amd64", nil)

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
