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
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/cryptoutil"
	"github.com/a2aproject/a2a-serve/internal/egress"
	"github.com/a2aproject/a2a-serve/internal/sandbox/sandboxtest"
	"github.com/a2aproject/a2a-serve/internal/sandboxnet"
)

func TestDockerProbeOnce(t *testing.T) {
	t.Parallel()
	setupErr := &EgressSetupError{Reason: "no tun"}
	tests := []struct {
		name      string
		first     error
		wantCalls int
		wantErr   error
	}{
		{name: "check passes", wantCalls: 1},
		{name: "setup fails", first: setupErr, wantCalls: 1, wantErr: setupErr},
		{name: "docker fails", first: errors.New("docker: Error response from daemon: EOF"), wantCalls: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := NewDocker(t.TempDir(), nil)
			calls := 0
			probe := func() error {
				calls++
				if calls == 1 {
					return tc.first
				}
				return nil
			}
			if err := d.probeOnce(probe); !errors.Is(err, tc.first) {
				t.Fatalf("Docker.probeOnce() error = %v, want %v", err, tc.first)
			}
			if err := d.probeOnce(probe); !errors.Is(err, tc.wantErr) {
				t.Fatalf("second Docker.probeOnce() error = %v, want %v", err, tc.wantErr)
			}
			if calls != tc.wantCalls {
				t.Fatalf("Docker.probeOnce() ran the check %d times, want %d", calls, tc.wantCalls)
			}
		})
	}
}

func TestWithoutEgressCA(t *testing.T) {
	t.Parallel()
	got := withoutEgressCA([]string{"A=1", sandboxnet.EnvEgressCA + "=from-a-recipe", "B=2"})
	if diff := cmp.Diff([]string{"A=1", "B=2"}, got); diff != "" {
		t.Fatalf("withoutEgressCA() wrong result (-want +got) diff = %s", diff)
	}
}

func TestDockerPrepareWithoutBoundary(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("other systems get an egress container")
	}
	sandboxtest.FakeDocker(t, `case "$1 $2" in
"info "*) echo "[name=seccomp,profile=builtin]" ;;
"image inspect") echo "amd64 null" ;;
*) echo "fake docker: $1 failed" >&2; exit 1 ;;
esac
`)
	t.Setenv(EnvForceRelay, "")
	opened := false
	d := NewDocker(t.TempDir(), func() string { return filepath.Join(t.TempDir(), "missing") })
	_, err := d.Prepare(context.Background(), Spec{
		Name:    "a2a-test",
		Image:   "img:1",
		Command: Command{Path: "agent"},
		Network: Network{Boundary: func(HostGatewayFunc, bool) (EgressEndpoint, error) {
			opened = true
			return EgressEndpoint{}, errors.New("unexpected boundary")
		}},
	})
	if !errors.Is(err, ErrNoBoundary) || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("Docker.Prepare() error = %v, want the missing launcher wrapping %v", err, ErrNoBoundary)
	}
	if opened {
		t.Fatalf("Docker.Prepare() opened the boundary, want no boundary on this host")
	}
}

// TestDockerRunsCommandBehindBoundary runs a command in a container with
// a mount, both kinds of mask and an allowlist boundary.
func TestDockerRunsCommandBehindBoundary(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	if runtime.GOOS != "linux" || os.Getenv(EnvForceRelay) == "1" {
		t.Skip("needs the egress boundary of Linux without an egress container")
	}
	if _, err := CheckDocker(ctx); err != nil {
		t.Skipf("docker isn't available: %v", err)
	}
	if err := CheckTUN(); err != nil {
		t.Skip(err)
	}
	launcher, image := buildTestContainer(t)

	data := t.TempDir()
	writeFile(t, filepath.Join(data, "hello.txt"), "hello")
	writeFile(t, filepath.Join(data, "secret.txt"), "secret")
	writeFile(t, filepath.Join(data, "private", "key"), "key")
	mask := filepath.Join(t.TempDir(), "mask")
	writeFile(t, mask, "masked")
	allowed, other := listen(t), listen(t)

	b := NewBoundaries(filepath.Join(t.TempDir(), "boundaries"), 12, "abcdefghijklmnopqrstuvwxyz", &sandboxnet.Blocks{})
	t.Cleanup(func() {
		if err := b.Close(context.Background()); err != nil {
			t.Errorf("Boundaries.Close() error = %v", err)
		}
	})
	policy, err := egress.NewPolicy(egress.ModeAllowlist, []string{net.JoinHostPort(egress.HostGateway, allowed)})
	if err != nil {
		t.Fatalf("egress.NewPolicy() error = %v", err)
	}
	name := "a2a-sandbox-test-" + cryptoutil.RandomString(12, "abcdefghijklmnopqrstuvwxyz")
	var stderr bytes.Buffer
	sb, err := NewDocker(t.TempDir(), func() string { return launcher }).Prepare(ctx, Spec{
		Name:  name,
		Image: image,
		Command: Command{Path: "/probe", Args: []string{
			"read:/data/hello.txt", "read:/data/secret.txt", "ls:/data/private",
			"dial:" + net.JoinHostPort(egress.HostGateway, allowed), "dial:" + net.JoinHostPort(egress.HostGateway, other),
		}},
		Mounts:  []Mount{{Src: data, Dst: "/data", ReadOnly: true}},
		Masks:   []Mask{{Src: mask, Dst: "/data/secret.txt"}, {Dst: "/data/private"}},
		Labels:  map[string]string{"a2a.test": "sandbox"},
		Network: Network{Boundary: b.Opener(name, BoundaryRules{Policy: func() egress.Policy { return policy }})},
		Stderr:  &stderr,
	})
	if err != nil {
		t.Fatalf("Docker.Prepare() error = %v", err)
	}
	t.Cleanup(func() {
		if err := sb.Close(context.Background()); err != nil {
			t.Errorf("Sandbox.Close() error = %v", err)
		}
	})
	if info := sb.Info(); !info.Boundary || info.Relay {
		t.Fatalf("Sandbox.Info() = %+v, want the boundary without an egress container", info)
	}
	rwc, err := sb.Start(ctx)
	if err != nil {
		t.Fatalf("Sandbox.Start() error = %v", err)
	}
	got := strings.Split(strings.TrimSpace(drain(t, rwc)), "\n")
	want := []string{
		"read:/data/hello.txt hello",
		"read:/data/secret.txt masked",
		"ls:/data/private 0 entries",
		"dial:" + net.JoinHostPort(egress.HostGateway, allowed) + " ok",
		"dial:" + net.JoinHostPort(egress.HostGateway, other) + " refused",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("Sandbox.Start() output wrong result (-want +got) diff = %s\nstderr: %s", diff, stderr.String())
	}
}

// buildTestContainer builds the launcher and an image whose /probe is the
// probe program, and returns the launcher's path and the image.
func buildTestContainer(t *testing.T) (launcher, image string) {
	t.Helper()
	dir := t.TempDir()
	launcher = filepath.Join(dir, "launcher")
	ctxDir := filepath.Join(dir, "image")
	for _, b := range []struct{ out, pkg string }{{launcher, "./testdata/launcher"}, {filepath.Join(ctxDir, "probe"), "./testdata/probe"}} {
		build := exec.Command("go", "build", "-o", b.out, b.pkg)
		build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux")
		if out, err := build.CombinedOutput(); err != nil {
			t.Fatalf("go build %s error = %v\n%s", b.pkg, err, out)
		}
	}
	image = "a2a-sandbox-test-probe:" + cryptoutil.RandomString(12, "abcdefghijklmnopqrstuvwxyz")
	build := exec.Command("docker", "build", "-q", "-t", image, "-f", "-", ctxDir)
	build.Stdin = strings.NewReader("FROM scratch\nCOPY probe /probe\n")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("docker build error = %v\n%s", err, out)
	}
	t.Cleanup(func() {
		if out, err := exec.Command("docker", "rmi", "-f", image).CombinedOutput(); err != nil {
			t.Errorf("docker rmi %s error = %v: %s", image, err, out)
		}
	})
	return launcher, image
}

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

// listen accepts connections on this host's loopback and returns the port.
func listen(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	t.Cleanup(func() {
		if err := ln.Close(); err != nil {
			t.Errorf("closing the listener error = %v", err)
		}
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			if err := c.Close(); err != nil {
				return
			}
		}
	}()
	return strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
}
