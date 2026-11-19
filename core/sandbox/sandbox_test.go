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

package sandbox_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/core/egress"
	"github.com/a2aproject/a2a-serve/core/sandbox"
	"github.com/a2aproject/a2a-serve/internal/cryptoutil"
)

func create(t *testing.T, rt sandbox.Runtime, spec sandbox.Spec) sandbox.Sandbox {
	t.Helper()
	sb, err := rt.Create(context.Background(), spec)
	if err != nil {
		t.Fatalf("Runtime.Create() error = %v", err)
	}
	t.Cleanup(func() {
		if err := sb.Close(context.Background()); err != nil {
			t.Errorf("Sandbox.Close() error = %v", err)
		}
	})
	return sb
}

// run starts sb and returns its output once the command exits.
func run(t *testing.T, sb sandbox.Sandbox) string {
	t.Helper()
	rwc, err := sb.Start(context.Background())
	if err != nil {
		t.Fatalf("Sandbox.Start() error = %v", err)
	}
	data, err := io.ReadAll(rwc)
	if err != nil {
		t.Fatalf("reading the command's output error = %v", err)
	}
	if err := rwc.Close(); err != nil {
		t.Fatalf("closing the command error = %v", err)
	}
	return string(data)
}

func openEndpoint(t *testing.T, policy egress.Policy) *egress.Endpoint {
	t.Helper()
	b, err := egress.New(egress.Config{Policy: func() egress.Policy { return policy }, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("egress.New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := b.Close(); err != nil {
			t.Errorf("Boundary.Close() error = %v", err)
		}
	})
	ep, err := b.Open("test", "")
	if err != nil {
		t.Fatalf("Boundary.Open() error = %v", err)
	}
	return ep
}

func TestLocalRunsCommand(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("the command is a shell script")
	}
	dir := t.TempDir()
	ln, err := net.Listen("unix", filepath.Join(dir, "s.sock"))
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	t.Cleanup(func() {
		if err := ln.Close(); err != nil {
			t.Errorf("closing the listener error = %v", err)
		}
	})
	sb := create(t, sandbox.Local(), sandbox.Spec{
		Command: sandbox.Command{Path: "sh", Args: []string{"-c", `pwd; echo "$X"; echo "$S"`}, Dir: dir, Env: []string{"X=1"}},
		Sockets: []sandbox.Socket{{Listener: ln, Env: "S"}},
	})
	want := dir + "\n1\n" + ln.Addr().String() + "\n"
	if got := run(t, sb); got != want {
		t.Fatalf("Sandbox.Start() output = %q, want %q", got, want)
	}
	if got := run(t, sb); got != want {
		t.Fatalf("Sandbox.Start() again output = %q, want %q", got, want)
	}
}

func TestLocalRefusesEgress(t *testing.T) {
	t.Parallel()
	ep := openEndpoint(t, egress.Policy{})
	_, err := sandbox.Local().Create(context.Background(), sandbox.Spec{Command: sandbox.Command{Path: "true"}, Egress: ep})
	if !errors.Is(err, sandbox.ErrNoBoundary) {
		t.Fatalf("Local().Create() with an egress endpoint error = %v, want %v", err, sandbox.ErrNoBoundary)
	}
	caps, err := sandbox.Local().Probe(context.Background())
	if err != nil {
		t.Fatalf("Local().Probe() error = %v", err)
	}
	if caps.Boundary || caps.Problem == "" {
		t.Fatalf("Local().Probe() = %+v, want no boundary and a problem", caps)
	}
}

func TestLocalMounts(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tests := []struct {
		name    string
		mount   sandbox.Mount
		wantErr error
	}{
		{name: "host dir in place", mount: sandbox.Mount{Dst: dir, Src: sandbox.HostDir(dir)}},
		{name: "read-only host dir in place", mount: sandbox.Mount{Dst: dir + "/", Src: sandbox.HostDir(dir), ReadOnly: true}},
		{
			name:    "host dir elsewhere",
			mount:   sandbox.Mount{Dst: "/work", Src: sandbox.HostDir(dir)},
			wantErr: &sandbox.UnsupportedMountError{Backend: "local", Dst: "/work", Kind: sandbox.MountHostDir, Reason: "the command runs on the host, where " + dir + " stays at its own path"},
		},
		{
			name:    "tmpfs",
			mount:   sandbox.Mount{Dst: "/scratch", Src: sandbox.Tmpfs()},
			wantErr: &sandbox.UnsupportedMountError{Backend: "local", Dst: "/scratch", Kind: sandbox.MountTmpfs, Reason: "the command runs on the host, with no mount namespace"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sb, err := sandbox.Local().Create(context.Background(), sandbox.Spec{Command: sandbox.Command{Path: "true"}, Mounts: []sandbox.Mount{tc.mount}})
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("Local().Create() error = %v, want nil", err)
				}
				if err := sb.Close(context.Background()); err != nil {
					t.Fatalf("Sandbox.Close() error = %v", err)
				}
				return
			}
			got, ok := errors.AsType[*sandbox.UnsupportedMountError](err)
			if !ok {
				t.Fatalf("Local().Create() error = %v, want an UnsupportedMountError", err)
			}
			if diff := cmp.Diff(tc.wantErr, got); diff != "" {
				t.Fatalf("Local().Create() wrong error (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestCreateRefusesMountWithoutSource(t *testing.T) {
	t.Parallel()
	tests := map[string]sandbox.Runtime{
		"local":  sandbox.Local(),
		"docker": sandbox.Docker(sandbox.DockerOptions{StateDir: t.TempDir()}),
	}
	for name, rt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := rt.Create(context.Background(), sandbox.Spec{Command: sandbox.Command{Path: "true"}, Mounts: []sandbox.Mount{{Dst: "/data"}}})
			if err == nil || !strings.Contains(err.Error(), "has no source") {
				t.Fatalf("Runtime.Create() with a mount without a source error = %v, want one that says so", err)
			}
		})
	}
}

func TestDockerRefusesRelativeMountDestination(t *testing.T) {
	t.Parallel()
	rt := sandbox.Docker(sandbox.DockerOptions{StateDir: t.TempDir()})
	_, err := rt.Create(context.Background(), sandbox.Spec{Command: sandbox.Command{Path: "true"}, Mounts: []sandbox.Mount{{Dst: "data", Src: sandbox.Tmpfs()}}})
	if err == nil || !strings.Contains(err.Error(), "isn't an absolute path") {
		t.Fatalf("Docker().Create() with a relative destination error = %v, want one that says so", err)
	}
}

func TestProbeReportsMountKinds(t *testing.T) {
	t.Parallel()
	caps, err := sandbox.Local().Probe(context.Background())
	if err != nil {
		t.Fatalf("Local().Probe() error = %v", err)
	}
	if diff := cmp.Diff([]sandbox.MountKind{sandbox.MountHostDir}, caps.Mounts); diff != "" {
		t.Fatalf("Local().Probe() wrong mounts (-want +got) diff = %s", diff)
	}
}

func TestMainIgnoresOtherArgs(t *testing.T) {
	t.Parallel()
	tests := [][]string{nil, {}, {"serve"}, {"sandbox"}, {"--sandbox-init"}}
	for _, args := range tests {
		if code, ok := sandbox.Main(args); ok || code != 0 {
			t.Fatalf("sandbox.Main(%q) = %d, %v, want 0, false", args, code, ok)
		}
	}
}

func TestMainRunsEntryPoints(t *testing.T) {
	t.Parallel()
	launcher := filepath.Join(t.TempDir(), "launcher")
	build := exec.Command("go", "build", "-o", launcher, "./testdata/launcher")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build ./testdata/launcher error = %v\n%s", err, out)
	}
	tests := []struct {
		args     []string
		wantCode int
		wantErr  string
	}{
		{args: []string{"sandbox-init"}, wantCode: 2, wantErr: "requires at least 3 arg(s), only received 0"},
		{args: []string{"sandbox-init", "--check", "s", "x", "1"}, wantCode: 1, wantErr: `invalid uid "x"`},
		{args: []string{"sandbox-init", "s", "1", "2"}, wantCode: 1, wantErr: "want <socket> <uid> <gid> and a command, or --check"},
		{args: []string{"sandbox-relay"}, wantCode: 2, wantErr: "--egress-listen and --egress-to are required"},
		{args: []string{"sandbox-relay", "--egress-listen", "a", "--egress-to", "b"}, wantCode: 1, wantErr: "A2A_SERVE_RELAY_KEY is not set"},
	}
	for _, tc := range tests {
		cmd := exec.Command(launcher, tc.args...)
		cmd.Env = append(os.Environ(), "A2A_SERVE_RELAY_KEY=")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		err := cmd.Run()
		code := 0
		if ee, ok := errors.AsType[*exec.ExitError](err); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatalf("running the launcher with %q error = %v", tc.args, err)
		}
		if code != tc.wantCode || !strings.Contains(stderr.String(), tc.wantErr) {
			t.Fatalf("launcher %q exit = %d, stderr %q, want %d and %q", tc.args, code, stderr.String(), tc.wantCode, tc.wantErr)
		}
	}
}

// TestDockerRunsCommandBehindEgress runs a command in a container with a
// read-only mount, a hidden directory, writable and read-only tmpfs mounts
// and an allowlist endpoint.
func TestDockerRunsCommandBehindEgress(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	if runtime.GOOS != "linux" || os.Getenv("A2A_SERVE_SANDBOX_RELAY") == "1" {
		t.Skip("needs the egress boundary of Linux without an egress container")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("needs the docker CLI")
	}
	launcher := filepath.Join(t.TempDir(), "launcher")
	rt := sandbox.Docker(sandbox.DockerOptions{Launcher: launcher, StateDir: t.TempDir()})
	if err := exec.CommandContext(ctx, "docker", "info").Run(); err != nil {
		t.Skipf("docker isn't available: %v", err)
	}
	image := buildTestContainer(t, launcher)
	caps, err := rt.Probe(ctx)
	if err != nil {
		t.Fatalf("Docker().Probe() error = %v", err)
	}
	if !caps.Boundary {
		t.Skipf("the boundary can't work here: %s", caps.Problem)
	}

	data := t.TempDir()
	writeFile(t, filepath.Join(data, "hello.txt"), "hello")
	writeFile(t, filepath.Join(data, "private", "key"), "key")
	allowed, other := listen(t), listen(t)
	ep := openEndpoint(t, egress.Policy{Mode: egress.ModeAllowlist, Allow: []string{net.JoinHostPort("host.docker.internal", allowed)}})
	var stderr bytes.Buffer
	sb := create(t, rt, sandbox.Spec{
		Name:  "a2a-sandbox-test-" + cryptoutil.RandomString(12, "abcdefghijklmnopqrstuvwxyz"),
		Image: image,
		Command: sandbox.Command{Path: "/probe", Args: []string{
			"read:/data/hello.txt", "write:/data/new.txt", "ls:/data/private", "write:/scratch/new.txt", "read:/scratch/new.txt", "write:/empty/new.txt",
			"dial:" + net.JoinHostPort("host.docker.internal", allowed), "dial:" + net.JoinHostPort("host.docker.internal", other),
		}},
		Mounts: []sandbox.Mount{{Dst: "/data", Src: sandbox.HostDir(data), ReadOnly: true}, {Dst: "/scratch", Src: sandbox.Tmpfs()}, {Dst: "/empty", Src: sandbox.Tmpfs(), ReadOnly: true}},
		Hide:   []string{filepath.Join(data, "private")},
		Labels: map[string]string{"a2a.test": "sandbox"},
		Egress: ep,
		Stderr: &stderr,
	})
	got := strings.Split(strings.TrimSpace(run(t, sb)), "\n")
	want := []string{
		"read:/data/hello.txt hello",
		"write:/data/new.txt error",
		"ls:/data/private 0 entries",
		"write:/scratch/new.txt ok",
		"read:/scratch/new.txt written",
		"write:/empty/new.txt error",
		"dial:" + net.JoinHostPort("host.docker.internal", allowed) + " ok",
		"dial:" + net.JoinHostPort("host.docker.internal", other) + " refused",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("Sandbox.Start() output wrong result (-want +got) diff = %s\nstderr: %s", diff, stderr.String())
	}
}

// buildTestContainer builds the launcher at launcher and an image whose
// /probe is the probe program of the internal sandbox tests.
func buildTestContainer(t *testing.T, launcher string) string {
	t.Helper()
	ctxDir := t.TempDir()
	for _, b := range []struct{ out, pkg string }{{launcher, "./testdata/launcher"}, {filepath.Join(ctxDir, "probe"), "../../internal/sandboxrt/testdata/probe"}} {
		build := exec.Command("go", "build", "-o", b.out, b.pkg)
		build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux")
		if out, err := build.CombinedOutput(); err != nil {
			t.Fatalf("go build %s error = %v\n%s", b.pkg, err, out)
		}
	}
	image := "a2a-sandbox-test-probe:" + cryptoutil.RandomString(12, "abcdefghijklmnopqrstuvwxyz")
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
	return image
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
