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
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/core/egress"
	"github.com/a2aproject/a2a-serve/core/fsys"
	"github.com/a2aproject/a2a-serve/core/sandbox"
	"github.com/a2aproject/a2a-serve/internal/cryptoutil"
	"github.com/a2aproject/a2a-serve/internal/fsysmask"
	"github.com/a2aproject/a2a-serve/internal/sandboxrt/sandboxtest"
	"github.com/a2aproject/a2a-serve/internal/testutil"
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
	launcher := cachedLauncher(t)
	tests := []struct {
		args     []string
		wantCode int
		wantErr  string
	}{
		{args: []string{"sandbox-init"}, wantCode: 2, wantErr: "requires at least 3 arg(s), only received 0"},
		{args: []string{"sandbox-init", "--check", "s", "x", "1"}, wantCode: 1, wantErr: `invalid uid "x"`},
		{args: []string{"sandbox-init", "s", "1", "2"}, wantCode: 1, wantErr: "want <socket> <uid> <gid> and a command, or --check"},
		{args: []string{"sandbox-init", "--check", "--fs", "workspace=/s", "s", "1", "2"}, wantCode: 1, wantErr: `invalid --fs "workspace=/s": want <dst>=<socket>[,ro] with absolute paths`},
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
	rt, image := dockerRuntime(t)
	caps, err := rt.Probe(ctx)
	if err != nil {
		t.Fatalf("Docker().Probe() error = %v", err)
	}
	if !caps.Boundary {
		sandboxtest.Skip(t, "the boundary can't work here: %s", caps.Problem)
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
			"caps:", "unshare:user",
		}},
		Mounts: []sandbox.Mount{{Dst: "/data", Src: sandbox.HostDir(data), ReadOnly: true}, {Dst: "/scratch", Src: sandbox.Tmpfs()}, {Dst: "/empty", Src: sandbox.Tmpfs(), ReadOnly: true}},
		Hide:   []string{filepath.Join(data, "private")},
		Labels: map[string]string{"a2a.test": "sandbox"},
		Egress: ep,
		Stderr: &stderr,
	})
	got := strings.Split(strings.TrimSpace(run(t, sb)), "\n")
	for i, line := range got {
		if sets, ok := strings.CutPrefix(line, "caps: "); ok && noCapabilities(sets) {
			got[i] = "caps: none"
		}
	}
	want := []string{
		"read:/data/hello.txt hello",
		"write:/data/new.txt error",
		"ls:/data/private 0 entries",
		"write:/scratch/new.txt ok",
		"read:/scratch/new.txt written",
		"write:/empty/new.txt error",
		"dial:" + net.JoinHostPort("host.docker.internal", allowed) + " ok",
		"dial:" + net.JoinHostPort("host.docker.internal", other) + " refused",
		"caps: none",
		"unshare:user EPERM",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("Sandbox.Start() output wrong result (-want +got) diff = %s\nstderr: %s", diff, stderr.String())
	}
}

// TestDockerServesLiveWorkspace mounts a tree in memory in a container,
// whose command changes it in every way that the tree supports, behind a
// mask that hides an agent directory and replaces .mcp.json.
func TestDockerServesLiveWorkspace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rt, image := dockerRuntime(t)
	caps, err := rt.Probe(ctx)
	if err != nil {
		t.Fatalf("Docker().Probe() error = %v", err)
	}
	if sandboxtest.Runtime() == "runsc" {
		checkServedRefused(t, rt, image, caps)
		return
	}
	if !slices.Contains(caps.Mounts, sandbox.MountServed) {
		if profile := os.Getenv(envLiveAppArmor); profile != "" {
			t.Fatalf("Docker(ServedAppArmor: %s).Probe() doesn't support served mounts: %s", profile, caps.MountProblems[sandbox.MountServed])
		}
		sandboxtest.Skip(t, "served mounts can't work here: %s", caps.MountProblems[sandbox.MountServed])
	}

	tree := fsys.Memory()
	put(t, tree, "host.txt", "from host")
	put(t, tree, "delete.txt", "x")
	put(t, tree, ".mcp.json", `{"mcpServers":{"evil":{}}}`)
	put(t, tree, "sub/.a2a/secret", "secret")
	masked := fsysmask.New(tree, fsysmask.Rules{
		Files:  map[string][]byte{".mcp.json": []byte(`{"mcpServers": {}}`)},
		Hidden: func(name string) bool { return name == ".a2a" },
	})
	var stderr bytes.Buffer
	sb := create(t, rt, sandbox.Spec{
		Name:  "a2a-sandbox-test-" + cryptoutil.RandomString(12, "abcdefghijklmnopqrstuvwxyz"),
		Image: image,
		Command: sandbox.Command{Path: "/probe", Args: []string{
			"read:/ws/host.txt", "write:/ws/new.txt", "mkdir:/ws/dir", "mv:/ws/new.txt=/ws/dir/moved.txt",
			"cp:/probe=/ws/tool", "chmod:/ws/tool=755", "mode:/ws/tool", "exec:/ws/tool",
			"ln:tool=/ws/link", "readlink:/ws/link", "rm:/ws/delete.txt",
			"read:/ws/.mcp.json", "ls:/ws/sub", "write:/ws/.mcp.json", "mkdir:/ws/.a2a",
			"unshare:user", "userns:/probe",
		}},
		Mounts: []sandbox.Mount{{Dst: "/ws", Src: sandbox.Served(masked)}},
		Labels: map[string]string{"a2a.test": "sandbox"},
		Egress: openEndpoint(t, egress.Policy{Mode: egress.ModeAllowlist}),
		Stderr: &stderr,
	})
	got := strings.Split(strings.TrimSpace(run(t, sb)), "\n")
	want := []string{
		"read:/ws/host.txt from host",
		"write:/ws/new.txt ok",
		"mkdir:/ws/dir ok",
		"mv:/ws/new.txt=/ws/dir/moved.txt ok",
		"cp:/probe=/ws/tool ok",
		"chmod:/ws/tool=755 ok",
		"mode:/ws/tool 0755",
		"exec:/ws/tool ok",
		"ln:tool=/ws/link ok",
		"readlink:/ws/link tool",
		"rm:/ws/delete.txt ok",
		`read:/ws/.mcp.json {"mcpServers": {}}`,
		"ls:/ws/sub 0 entries",
		"write:/ws/.mcp.json error",
		"mkdir:/ws/.a2a error",
		"unshare:user EPERM",
		"userns:/probe EPERM",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("Sandbox.Start() output wrong result (-want +got) diff = %s\nstderr: %s", diff, stderr.String())
	}

	if got := get(t, tree, "dir/moved.txt"); got != "written" {
		t.Fatalf("the tree's dir/moved.txt = %q, want what the command wrote", got)
	}
	for p, want := range map[string]fsys.Info{"tool": {Mode: 0o755}, "link": {Mode: fs.ModeSymlink | 0o777, Target: "tool"}} {
		info, err := tree.Stat(ctx, p)
		if err != nil {
			t.Fatalf("FS.Stat(%q) error = %v", p, err)
		}
		if info.Mode != want.Mode || info.Target != want.Target {
			t.Fatalf("FS.Stat(%q) = %v %q, want %v %q", p, info.Mode, info.Target, want.Mode, want.Target)
		}
	}
	for _, p := range []string{"delete.txt", "new.txt"} {
		if _, err := tree.Stat(ctx, p); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("FS.Stat(%q) error = %v, want %v", p, err, fs.ErrNotExist)
		}
	}
	if got := get(t, tree, ".mcp.json"); got != `{"mcpServers":{"evil":{}}}` {
		t.Fatalf("the tree's .mcp.json = %q, want it unchanged", got)
	}
}

func TestServedNeedsBoundary(t *testing.T) {
	t.Parallel()
	mount := sandbox.Mount{Dst: "/ws", Src: sandbox.Served(fsys.Memory())}
	tests := map[string]struct {
		rt   sandbox.Runtime
		spec sandbox.Spec
	}{
		"local":                   {rt: sandbox.Local()},
		"docker without boundary": {rt: sandbox.Docker(sandbox.DockerOptions{StateDir: t.TempDir()})},
		"docker unrestricted":     {rt: sandbox.Docker(sandbox.DockerOptions{StateDir: t.TempDir()}), spec: sandbox.Spec{Egress: openEndpoint(t, egress.Policy{Mode: egress.ModeUnrestricted})}},
		"bwrap without boundary":  {rt: sandbox.Bubblewrap(sandbox.BubblewrapOptions{StateDir: t.TempDir()})},
		"bwrap unrestricted":      {rt: sandbox.Bubblewrap(sandbox.BubblewrapOptions{StateDir: t.TempDir()}), spec: sandbox.Spec{Egress: openEndpoint(t, egress.Policy{Mode: egress.ModeUnrestricted})}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			spec := tc.spec
			spec.Command, spec.Mounts = sandbox.Command{Path: "true"}, []sandbox.Mount{mount}
			_, err := tc.rt.Create(context.Background(), spec)
			got, ok := errors.AsType[*sandbox.UnsupportedMountError](err)
			if !ok || got.Kind != sandbox.MountServed || got.Dst != "/ws" {
				t.Fatalf("Runtime.Create() with a served mount error = %v, want an UnsupportedMountError of it", err)
			}
		})
	}
}

// envLiveAppArmor names the AppArmor profile that the containers with a
// live workspace get in the tests, which then fail where it can't mount.
const envLiveAppArmor = "A2A_SERVE_TEST_LIVE_APPARMOR"

// checkServedRefused checks that rt, whose runtime is gVisor's and which
// found caps, refuses a served mount, since gVisor's FUSE can't map files.
func checkServedRefused(t *testing.T, rt sandbox.Runtime, image string, caps sandbox.Capabilities) {
	t.Helper()
	if slices.Contains(caps.Mounts, sandbox.MountServed) || !strings.Contains(caps.MountProblems[sandbox.MountServed], "gVisor") {
		t.Fatalf("Docker().Probe() under gVisor = %+v, want served mounts refused for gVisor", caps)
	}
	_, err := rt.Create(context.Background(), sandbox.Spec{
		Name:    "a2a-sandbox-test-" + cryptoutil.RandomString(12, "abcdefghijklmnopqrstuvwxyz"),
		Image:   image,
		Command: sandbox.Command{Path: "/probe"},
		Mounts:  []sandbox.Mount{{Dst: "/ws", Src: sandbox.Served(fsys.Memory())}},
		Egress:  openEndpoint(t, egress.Policy{Mode: egress.ModeAllowlist}),
	})
	if got, ok := errors.AsType[*sandbox.UnsupportedMountError](err); !ok || got.Kind != sandbox.MountServed || !strings.Contains(got.Reason, "gVisor") {
		t.Fatalf("Docker().Create() with a served mount under gVisor error = %v, want an UnsupportedMountError for gVisor", err)
	}
}

// TestLauncherUnderGVisor runs the launcher under runsc do, which needs
// --host-uds=open to reach the egress socket, and an agent as uid 0, which
// gets no capability and can't make namespaces.
func TestLauncherUnderGVisor(t *testing.T) {
	t.Parallel()
	runsc := os.Getenv(sandboxtest.EnvRunsc)
	if runsc == "" {
		sandboxtest.Skip(t, "needs a runsc binary in %s", sandboxtest.EnvRunsc)
	}
	launcher, probe := cachedLauncher(t), cachedProbe(t)
	sock := filepath.Join(testutil.ShortTempDir(t), "egress.sock")
	ln, err := net.Listen("unix", sock)
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
	tests := []struct {
		name       string
		flags      []string
		init       []string
		wantCode   int
		wantStdout []string
		wantStderr string
	}{
		{name: "check with host sockets open", flags: []string{"--host-uds=open"}, init: []string{"--check", sock, "0", "0"}},
		{name: "check with host sockets refused", init: []string{"--check", sock, "0", "0"}, wantCode: 121, wantStderr: "unless runsc runs with --host-uds=open"},
		{
			name:       "agent as root",
			flags:      []string{"--host-uds=open"},
			init:       []string{sock, "0", "0", probe, "caps:", "unshare:user", "userns:" + probe},
			wantStdout: []string{"caps: none", "unshare:user EPERM", "userns:" + probe + " EPERM"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			args := append([]string{"--root", t.TempDir(), "--rootless", "--network=none", "--ignore-cgroups"}, tc.flags...)
			args = append(append(args, "do", launcher, "sandbox-init"), tc.init...)
			cmd := exec.Command(runsc, args...)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			code := 0
			if ee, ok := errors.AsType[*exec.ExitError](err); ok {
				code = ee.ExitCode()
			} else if err != nil {
				t.Fatalf("runsc do error = %v", err)
			}
			if code != tc.wantCode || !strings.Contains(stderr.String(), tc.wantStderr) {
				t.Fatalf("runsc do launcher exit = %d, stderr %q, want %d and %q", code, stderr.String(), tc.wantCode, tc.wantStderr)
			}
			var got []string
			if out := strings.TrimSpace(stdout.String()); out != "" {
				got = strings.Split(out, "\n")
			}
			for i, line := range got {
				if sets, ok := strings.CutPrefix(line, "caps: "); ok && noCapabilities(sets) {
					got[i] = "caps: none"
				}
			}
			if diff := cmp.Diff(tc.wantStdout, got); diff != "" {
				t.Fatalf("runsc do launcher output wrong result (-want +got) diff = %s\nstderr: %s", diff, stderr.String())
			}
		})
	}
}

// noCapabilities reports whether sets, the capability sets that the probe
// prints, list the bounding set and are all empty. Releases of gVisor
// without ambient capabilities leave out CapAmb.
func noCapabilities(sets string) bool {
	bounding := false
	for set := range strings.FieldsSeq(sets) {
		name, v, _ := strings.Cut(set, "=")
		if strings.Trim(v, "0") != "" {
			return false
		}
		bounding = bounding || name == "CapBnd"
	}
	return bounding
}

// dockerRuntime returns the Docker runtime with a launcher built from this
// tree and the image of the probe, or skips the test where it can't run.
// Its containers run with the runtime of sandboxtest.EnvRuntime.
func dockerRuntime(t *testing.T) (sandbox.Runtime, string) {
	t.Helper()
	if runtime.GOOS != "linux" || os.Getenv("A2A_SERVE_SANDBOX_RELAY") == "1" {
		sandboxtest.Skip(t, "needs the egress boundary of Linux without an egress container")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		sandboxtest.Skip(t, "needs the docker CLI")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		sandboxtest.Skip(t, "docker isn't available: %v", err)
	}
	launcher, image := buildTestContainer(t)
	rt := sandbox.Docker(sandbox.DockerOptions{Launcher: launcher, StateDir: t.TempDir(), ServedAppArmor: os.Getenv(envLiveAppArmor), Runtime: sandboxtest.Runtime()})
	return rt, image
}

func put(t *testing.T, tree fsys.FS, p, data string) {
	t.Helper()
	if _, err := tree.Write(context.Background(), p, strings.NewReader(data), 0o644, ""); err != nil {
		t.Fatalf("FS.Write(%q) error = %v", p, err)
	}
}

func get(t *testing.T, tree fsys.FS, p string) string {
	t.Helper()
	rc, err := tree.Open(context.Background(), p)
	if err != nil {
		t.Fatalf("FS.Open(%q) error = %v", p, err)
	}
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("reading %q error = %v", p, err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("closing %q error = %v", p, err)
	}
	return string(data)
}

var (
	testArtifactDir string

	launcherOnce sync.Once
	launcherPath string
	launcherErr  error

	linuxLauncherOnce sync.Once
	linuxLauncherPath string
	linuxLauncherErr  error

	probeOnce sync.Once
	probePath string
	probeErr  error

	probeImageOnce sync.Once
	probeImage     string
	probeImageErr  error
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "a2a-sandbox-test-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	testArtifactDir = dir
	code := m.Run()
	if probeImage != "" {
		if out, err := exec.Command("docker", "rmi", "-f", probeImage).CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "docker rmi %s error = %v: %s\n", probeImage, err, out)
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	os.Exit(code)
}

func cachedLauncher(t *testing.T) string {
	t.Helper()
	launcherOnce.Do(func() {
		p := filepath.Join(testArtifactDir, "launcher")
		build := exec.Command("go", "build", "-o", p, "./testdata/launcher")
		build.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := build.CombinedOutput(); err != nil {
			launcherErr = fmt.Errorf("go build ./testdata/launcher error = %v\n%s", err, out)
			return
		}
		launcherPath = p
	})
	if launcherErr != nil {
		t.Fatal(launcherErr)
	}
	return launcherPath
}

func cachedLinuxLauncher(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "linux" {
		return cachedLauncher(t)
	}
	linuxLauncherOnce.Do(func() {
		p := filepath.Join(testArtifactDir, "launcher-linux")
		build := exec.Command("go", "build", "-o", p, "./testdata/launcher")
		build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux")
		if out, err := build.CombinedOutput(); err != nil {
			linuxLauncherErr = fmt.Errorf("go build ./testdata/launcher error = %v\n%s", err, out)
			return
		}
		linuxLauncherPath = p
	})
	if linuxLauncherErr != nil {
		t.Fatal(linuxLauncherErr)
	}
	return linuxLauncherPath
}

func cachedProbe(t *testing.T) string {
	t.Helper()
	probeOnce.Do(func() {
		ctxDir := filepath.Join(testArtifactDir, "probe-image")
		if err := os.MkdirAll(ctxDir, 0o755); err != nil {
			probeErr = err
			return
		}
		p := filepath.Join(ctxDir, "probe")
		build := exec.Command("go", "build", "-o", p, "../../internal/sandboxrt/testdata/probe")
		build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux")
		if out, err := build.CombinedOutput(); err != nil {
			probeErr = fmt.Errorf("go build ../../internal/sandboxrt/testdata/probe error = %v\n%s", err, out)
			return
		}
		probePath = p
	})
	if probeErr != nil {
		t.Fatal(probeErr)
	}
	return probePath
}

// buildTestContainer builds the launcher and an image whose /probe is the
// probe program of the internal sandbox tests, and returns the launcher's
// path and the image.
func buildTestContainer(t *testing.T) (launcher, image string) {
	t.Helper()
	launcher = cachedLinuxLauncher(t)
	probe := cachedProbe(t)
	probeImageOnce.Do(func() {
		img := "a2a-sandbox-test-probe:" + cryptoutil.RandomString(12, "abcdefghijklmnopqrstuvwxyz")
		build := exec.Command("docker", "build", "-q", "-t", img, "-f", "-", filepath.Dir(probe))
		build.Stdin = strings.NewReader("FROM scratch\nCOPY probe /probe\n")
		if out, err := build.CombinedOutput(); err != nil {
			probeImageErr = fmt.Errorf("docker build error = %v\n%s", err, out)
			return
		}
		probeImage = img
	})
	if probeImageErr != nil {
		t.Fatal(probeImageErr)
	}
	return launcher, probeImage
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
