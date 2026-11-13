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

package netinstall

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/testutil"
)

var loopnet string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "netinstall")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	loopnet = filepath.Join(dir, "loopnet")
	if out, err := exec.Command("go", "build", "-o", loopnet, "github.com/a2aproject/a2a-serve/netprovider/internal/loopnet").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "building loopnet: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	if err := os.RemoveAll(dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	os.Exit(code)
}

func TestCheck(t *testing.T) {
	t.Parallel()
	tests := []struct {
		spec    string
		wantErr string
	}{
		{spec: ""},
		{spec: "agentmesh"},
		{spec: "slim"},
		{spec: "https://example.com/p_linux_amd64.tar.gz"},
		{spec: "./bin/provider"},
		{spec: "/opt/provider"},
		{spec: "nebula", wantErr: "unknown network provider"},
		{spec: "http://example.com/p.tar.gz", wantErr: "must use https"},
	}
	for _, tc := range tests {
		t.Run(tc.spec, func(t *testing.T) {
			t.Parallel()
			err := Check(tc.spec)
			if tc.wantErr == "" && err != nil {
				t.Fatalf("Check(%q) error = %v, want nil", tc.spec, err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("Check(%q) error = %v, want %q", tc.spec, err, tc.wantErr)
			}
		})
	}
}

func newInstaller(t *testing.T) *Installer {
	t.Helper()
	bin := t.TempDir()
	return &Installer{Dir: t.TempDir(), Executable: filepath.Join(bin, "a2a-serve"), GOOS: "linux", GOARCH: "amd64"}
}

func TestFetchRunsNothing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	src := filepath.Join(dir, "provider")
	testutil.WriteExecutable(t, src, []byte("#!/bin/sh\ntouch "+marker+"\n"))
	if _, err := newInstaller(t).Fetch(t.Context(), src); err != nil {
		t.Fatalf("Installer.Fetch() error = %v", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the provider ran before the user confirmed it: %v", err)
	}
}

func TestTrustRefusesTakenNames(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tests := []struct {
		name    string
		wantErr string
	}{
		{name: "agentmesh", wantErr: "calls itself \"agentmesh\""},
		{name: "tailscale", wantErr: "calls itself \"tailscale\""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			src := filepath.Join(dir, tc.name)
			script := "#!/bin/sh\nread -r line\nprintf '%s\\n' '{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"name\":\"" + tc.name + "\",\"version\":\"v1\",\"protocol\":1,\"hosts\":[]}}'\ncat >/dev/null\n"
			testutil.WriteExecutable(t, src, []byte(script))
			inst := newInstaller(t)
			c, err := inst.Fetch(t.Context(), src)
			if err != nil {
				t.Fatalf("Installer.Fetch() error = %v", err)
			}
			if _, _, err := inst.Trust(t.Context(), c); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Installer.Trust() error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestResolveBundled(t *testing.T) {
	t.Parallel()
	inst := newInstaller(t)
	bundled := filepath.Join(filepath.Dir(inst.Executable), "netprovider-agentmesh")
	testutil.WriteExecutable(t, bundled, []byte("#!/bin/sh\n"))
	got, err := inst.Resolve(t.Context(), "")
	if err != nil {
		t.Fatalf("Installer.Resolve() error = %v", err)
	}
	if diff := cmp.Diff(Provider{Name: "agentmesh", Path: bundled}, got); diff != "" {
		t.Fatalf("Installer.Resolve() wrong result (-want +got) diff = %s", diff)
	}
}

func TestResolveWithoutReleaseSaysHowToBuild(t *testing.T) {
	t.Parallel()
	inst := newInstaller(t)
	_, err := inst.Resolve(t.Context(), "slim")
	want := "(cd providers/slim && go run github.com/agntcy/slim-bindings-go/v3/cmd/slim-bindings-setup && go build -o " + filepath.Join(filepath.Dir(inst.Executable), "netprovider-slim") + " .)"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Installer.Resolve(slim) error = %v, want %q", err, want)
	}
	if _, err := inst.Find("slim"); err == nil || errors.Is(err, ErrDownload) {
		t.Fatalf("Installer.Find(slim) error = %v, want the build hint", err)
	}
}

func TestResolveDownloadsPinnedRelease(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte("#!/bin/sh\n")
	if err := tw.WriteHeader(&tar.Header{Name: "netprovider-slim", Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatalf("tar.WriteHeader() error = %v", err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatalf("tar.Write() error = %v", err)
	}
	if err := errors.Join(tw.Close(), gz.Close()); err != nil {
		t.Fatalf("closing the archive: %v", err)
	}
	sum := sha256.Sum256(buf.Bytes())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := w.Write(buf.Bytes()); err != nil {
			t.Errorf("writing the archive: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	tests := []struct {
		name    string
		sum     string
		wantErr string
	}{
		{name: "pinned", sum: hex.EncodeToString(sum[:])},
		{name: "wrong checksum", sum: strings.Repeat("0", 64), wantErr: "not running it"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			inst := newInstaller(t)
			inst.HTTP = srv.Client()
			inst.Pins = map[string]map[string]Archive{"slim": {"linux/amd64": {URL: srv.URL + "/slim.tar.gz", SHA256: tc.sum}}}
			if _, err := inst.Find("slim"); !errors.Is(err, ErrDownload) {
				t.Fatalf("Installer.Find(slim) error = %v, want %v", err, ErrDownload)
			}
			got, err := inst.Resolve(t.Context(), "slim")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Installer.Resolve(slim) error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Installer.Resolve(slim) error = %v", err)
			}
			data, err := os.ReadFile(got.Path)
			if err != nil || !bytes.Equal(data, body) || got.Path != inst.Installed(WellKnown[1]) {
				t.Fatalf("Installer.Resolve(slim) = %+v (%q, %v), want the binary installed", got, data, err)
			}
		})
	}
}

func TestCustomProviderTrust(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	inst := newInstaller(t)
	src := filepath.Join(t.TempDir(), "loopnet")
	data, err := os.ReadFile(loopnet)
	if err != nil {
		t.Fatalf("os.ReadFile() error = %v", err)
	}
	testutil.WriteExecutable(t, src, data)

	if _, err := inst.Resolve(ctx, src); !errors.Is(err, ErrNotTrusted) {
		t.Fatalf("Installer.Resolve() before trust error = %v, want %v", err, ErrNotTrusted)
	}
	c, err := inst.Fetch(ctx, src)
	if err != nil {
		t.Fatalf("Installer.Fetch() error = %v", err)
	}
	if c.Trusted || c.Changed || len(c.SHA256) != 64 {
		t.Fatalf("Installer.Fetch() = %+v, want an untrusted provider with a fingerprint", c)
	}
	installed, version, err := inst.Trust(ctx, c)
	if err != nil {
		t.Fatalf("Installer.Trust() error = %v", err)
	}
	if installed.Name != "loopnet" || version != "v0.0.0" {
		t.Fatalf("Installer.Trust() = %+v, %q, want loopnet v0.0.0", installed, version)
	}
	got, err := inst.Resolve(ctx, src)
	if err != nil {
		t.Fatalf("Installer.Resolve() error = %v", err)
	}
	if diff := cmp.Diff(installed, got); diff != "" {
		t.Fatalf("Installer.Resolve() wrong result (-want +got) diff = %s", diff)
	}
	if c, err := inst.Fetch(ctx, src); err != nil || !c.Trusted {
		t.Fatalf("Installer.Fetch() again = %+v, %v, want trusted", c, err)
	}

	testutil.WriteExecutable(t, src, append(data, 0))
	if c, err := inst.Fetch(ctx, src); err != nil || !c.Changed {
		t.Fatalf("Installer.Fetch() after a change = %+v, %v, want changed", c, err)
	}

	testutil.WriteExecutable(t, installed.Path, []byte("tampered"))
	if _, err := inst.Resolve(ctx, src); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("Installer.Resolve() after tampering error = %v, want changed", err)
	}
	testutil.WriteExecutable(t, src, data)
	if c, err := inst.Fetch(ctx, src); err != nil || c.Trusted || c.Changed {
		t.Fatalf("Installer.Fetch() of a trusted source with a tampered copy = %+v, %v, want it installed again without a change", c, err)
	}

	upgrade := filepath.Join(t.TempDir(), "loopnet")
	testutil.WriteExecutable(t, upgrade, data)
	c, err = inst.Fetch(ctx, upgrade)
	if err != nil {
		t.Fatalf("Installer.Fetch(upgrade) error = %v", err)
	}
	if _, _, err := inst.Trust(ctx, c); err != nil {
		t.Fatalf("Installer.Trust(upgrade) error = %v, want it to replace the provider of the same name", err)
	}
	if _, err := inst.Resolve(ctx, src); !errors.Is(err, ErrNotTrusted) {
		t.Fatalf("Installer.Resolve(old source) error = %v, want %v after the upgrade", err, ErrNotTrusted)
	}
}

func TestBundledNextToTheSymlink(t *testing.T) {
	t.Parallel()
	inst := newInstaller(t)
	inst.Link = filepath.Join(t.TempDir(), "a2a-serve")
	got := inst.Bundled(WellKnown[0])
	if want := filepath.Join(filepath.Dir(inst.Link), "netprovider-agentmesh"); got[0] != want {
		t.Fatalf("Installer.Bundled() = %v, want %s first", got, want)
	}
}
