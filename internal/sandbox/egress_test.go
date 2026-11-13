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

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-serve/internal/sandbox/sandboxtest"
)

func TestPublishes(t *testing.T) {
	t.Parallel()
	ports := "80/tcp, 0.0.0.0:3000->3000/tcp, [::]:3000->3000/tcp\n127.0.0.1:4317->4317/tcp\n[::1]:6379->6379/tcp\n[::]:9000->9000/tcp\n192.168.1.5:8080->80/tcp, 0.0.0.0:5353->53/udp"
	tests := []struct {
		name     string
		port     uint16
		loopback bool
		want     bool
	}{
		{name: "all addresses", port: 3000, want: true},
		{name: "all IPv6 addresses", port: 9000, want: true},
		{name: "loopback", port: 4317},
		{name: "IPv6 loopback", port: 6379},
		{name: "loopback with Docker Desktop", port: 4317, loopback: true, want: true},
		{name: "IPv6 loopback with Docker Desktop", port: 6379, loopback: true, want: true},
		{name: "all addresses with Docker Desktop", port: 3000, loopback: true, want: true},
		{name: "not published", port: 80},
		{name: "another address", port: 8080, loopback: true},
		{name: "UDP", port: 5353, loopback: true},
		{name: "unknown", port: 22, loopback: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := publishes(ports, tc.port, tc.loopback); got != tc.want {
				t.Fatalf("publishes(%d, %v) = %v, want %v", tc.port, tc.loopback, got, tc.want)
			}
		})
	}
}

func TestHostGateway(t *testing.T) {
	sandboxtest.FakeDocker(t, `echo "0.0.0.0:3000->3000/tcp, [::]:3000->3000/tcp"; echo "127.0.0.1:6379->6379/tcp"`)
	tests := []struct {
		goos string
		port uint16
		want bool
	}{
		{goos: "linux", port: 3000, want: true},
		{goos: "linux", port: 6379},
		{goos: "darwin", port: 3000, want: true},
		{goos: "darwin", port: 6379, want: true},
		{goos: "windows", port: 6379, want: true},
	}
	for _, tc := range tests {
		got, ok := HostGateway(tc.goos)(context.Background(), tc.port)
		if ok != tc.want {
			t.Fatalf("HostGateway(%q)(%d) = %v, %v, want found %v", tc.goos, tc.port, got, ok, tc.want)
		}
		if want := netip.MustParseAddrPort("127.0.0.1:" + strconv.Itoa(int(tc.port))); ok && got != want {
			t.Fatalf("HostGateway(%q)(%d) = %v, want %v", tc.goos, tc.port, got, want)
		}
	}
}

func TestRelayImageRef(t *testing.T) {
	t.Parallel()
	rev := "f871ae2e3f61aa00bb11cc22dd33ee44ff556677"
	tests := []struct {
		name      string
		info      *debug.BuildInfo
		wantImage string
		wantRef   string
	}{
		{name: "release", info: &debug.BuildInfo{Main: debug.Module{Version: "v0.2.0"}}, wantImage: RelayImageRepo + ":v0.2.0", wantRef: "v0.2.0"},
		{name: "work tree", info: &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}, Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: rev}}}, wantImage: RelayImageRepo + ":sha-f871ae2e3f61", wantRef: rev},
		{name: "go install of a commit", info: &debug.BuildInfo{Main: debug.Module{Version: "v0.0.0-20261108175548-f871ae2e3f61"}}, wantImage: RelayImageRepo + ":sha-f871ae2e3f61", wantRef: "f871ae2e3f61"},
		{name: "unknown", info: &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			image, ref, err := relayImageRef(tc.info)
			if tc.wantImage == "" {
				if err == nil {
					t.Fatalf("relayImageRef() error = nil, want an error")
				}
				return
			}
			if err != nil || image != tc.wantImage || ref != tc.wantRef {
				t.Fatalf("relayImageRef() = %q, %q, %v, want %q, %q", image, ref, err, tc.wantImage, tc.wantRef)
			}
		})
	}
}

func TestCheckRelayImage(t *testing.T) {
	t.Setenv(EnvRelayImage, "example.com/egress:test")
	tests := []struct {
		name        string
		have        []string
		pull        bool
		wantErr     string
		wantMissing bool
	}{
		{name: "on this machine", have: []string{"image"}},
		{name: "missing", wantErr: "isn't on this machine; the next start of the agent's container pulls it", wantMissing: true},
		{name: "in the registry", have: []string{"manifest"}, pull: true},
		{name: "nowhere", pull: true, wantErr: "is neither on this machine nor in the registry"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			run := func(_ context.Context, _ string, args ...string) ([]byte, error) {
				if slices.Contains(tc.have, args[0]) {
					return nil, nil
				}
				return nil, errors.New("not found")
			}
			err := CheckRelayImage(context.Background(), run, tc.pull)
			if tc.wantErr == "" && err != nil || tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("CheckRelayImage() error = %v, want %q", err, tc.wantErr)
			}
			var missing *MissingRelayImage
			if got := errors.As(err, &missing); got != tc.wantMissing {
				t.Fatalf("CheckRelayImage() error is MissingRelayImage = %v, want %v", got, tc.wantMissing)
			}
		})
	}
}

func TestMissingRelayImage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		missing MissingRelayImage
		want    string
	}{
		{name: "pull or build", missing: MissingRelayImage{Image: "img", Pull: true, Build: true}, want: "the egress container's image img isn't on this machine; the next start of the agent's container pulls it, or builds it from the a2a-serve repository"},
		{name: "pull only", missing: MissingRelayImage{Image: "img", Pull: true}, want: "the egress container's image img isn't on this machine; the next start of the agent's container pulls it"},
		{name: "build", missing: MissingRelayImage{Image: "img", Build: true}, want: "the egress container's image img is neither on this machine nor in the registry; the next start of the agent's container builds it from the a2a-serve repository, which takes a few minutes"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.missing.Error(); got != tc.want {
				t.Fatalf("MissingRelayImage.Error() = %q, want %q", got, tc.want)
			}
		})
	}
}

// noRelayPullDocker has no images and can't pull, but builds.
const noRelayPullDocker = `case "$1" in
build) ;;
*) echo "fake docker: $1 failed" >&2; exit 1 ;;
esac
`

func TestEnsureRelayImageBuilds(t *testing.T) {
	full := "f871ae2e3f61aa00bb11cc22dd33ee44ff556677"
	resolve := func(_ context.Context, short string) (string, error) {
		if !strings.HasPrefix(full, short) {
			return "", fmt.Errorf("unknown commit %s", short)
		}
		return full, nil
	}
	tests := []struct {
		name      string
		ref       string
		wantBuild string
	}{
		{name: "release", ref: "v0.2.0", wantBuild: "--build-arg VERSION=0.2.0 " + relaySource + "#v0.2.0"},
		{name: "work tree", ref: full, wantBuild: "--build-arg VERSION=" + full + " " + relaySource + "#" + full},
		{name: "go install of a commit", ref: "f871ae2e3f61", wantBuild: "--build-arg VERSION=" + full + " " + relaySource + "#" + full},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			log := sandboxtest.FakeDocker(t, noRelayPullDocker)
			if _, err := ensureRelayImage(context.Background(), io.Discard, "example.com/egress:test", tc.ref, resolve); err != nil {
				t.Fatalf("ensureRelayImage() error = %v", err)
			}
			calls, err := os.ReadFile(log)
			if err != nil {
				t.Fatalf("os.ReadFile() error = %v", err)
			}
			want := "build -t example.com/egress:test -f " + relayDockerfile + " " + tc.wantBuild
			if !strings.Contains(string(calls), want) {
				t.Fatalf("ensureRelayImage() docker calls = %q, want them to contain %q", calls, want)
			}
		})
	}
}

func TestResolveCommit(t *testing.T) {
	t.Parallel()
	full := "f871ae2e3f61aa00bb11cc22dd33ee44ff556677"
	tests := []struct {
		name    string
		status  int
		body    string
		want    string
		wantErr string
	}{
		{name: "found", status: http.StatusOK, body: full + "\n", want: full},
		{name: "unknown", status: http.StatusUnprocessableEntity, body: `{"message":"No commit found"}`, wantErr: "422"},
		{name: "another commit", status: http.StatusOK, body: "0000000000000000000000000000000000000000", wantErr: "isn't the commit"},
		{name: "not a commit", status: http.StatusOK, body: "<html>", wantErr: "isn't the commit"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/commits/f871ae2e3f61" || r.Header.Get("Accept") != "application/vnd.github.sha" {
					http.NotFound(w, r)
					return
				}
				w.WriteHeader(tc.status)
				if _, err := io.WriteString(w, tc.body); err != nil {
					t.Errorf("writing the response: %v", err)
				}
			}))
			t.Cleanup(srv.Close)
			got, err := resolveCommit(context.Background(), srv.Client(), srv.URL+"/commits/", "f871ae2e3f61")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("resolveCommit() error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("resolveCommit() = %q, %v, want %q", got, err, tc.want)
			}
		})
	}
}

func TestEgressEnvProbe(t *testing.T) {
	t.Parallel()
	noTUN := errors.New("no tun")
	noLauncher := errors.New("no launcher")
	missing := &MissingRelayImage{Image: "img", Pull: true}
	ok := func() error { return nil }
	launcher := func() (string, error) { return "/usr/bin/a2a-serve", nil }
	linux := EgressEnv{GOOS: "linux", Launcher: launcher, TUN: ok}
	tests := []struct {
		name         string
		env          EgressEnv
		wantBoundary string
		wantRelay    bool
		wantImage    error
	}{
		{name: "linux", env: linux},
		{name: "no TUN device", env: EgressEnv{GOOS: "linux", Launcher: launcher, TUN: func() error { return noTUN }}, wantBoundary: "no tun"},
		{name: "no launcher", env: EgressEnv{GOOS: "linux", Launcher: func() (string, error) { return "", noLauncher }, TUN: ok}, wantBoundary: "no launcher"},
		{name: "image for another architecture", env: EgressEnv{GOOS: "linux", Launcher: launcher, TUN: ok, ImageArch: "s390x"}, wantBoundary: "the agent's image is for s390x"},
		{name: "docker desktop", env: EgressEnv{GOOS: "darwin", RelayImage: func() error { return missing }}, wantRelay: true, wantImage: missing},
		{name: "forced relay", env: EgressEnv{GOOS: "linux", ForceRelay: true}, wantRelay: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tc.env.Probe()
			if tc.wantBoundary == "" && got.Boundary != nil {
				t.Fatalf("EgressEnv.Probe().Boundary = %v, want nil", got.Boundary)
			}
			if tc.wantBoundary != "" && (!errors.Is(got.Boundary, ErrNoBoundary) || !strings.HasPrefix(got.Boundary.Error(), tc.wantBoundary)) {
				t.Fatalf("EgressEnv.Probe().Boundary = %v, want %q wrapping %v", got.Boundary, tc.wantBoundary, ErrNoBoundary)
			}
			if got.EgressRelay != tc.wantRelay || got.RelayImage != tc.wantImage {
				t.Fatalf("EgressEnv.Probe() = relay %v, image %v, want relay %v, image %v", got.EgressRelay, got.RelayImage, tc.wantRelay, tc.wantImage)
			}
		})
	}
}
