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
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/core/egress"
	"github.com/a2aproject/a2a-serve/core/fsys"
	"github.com/a2aproject/a2a-serve/core/sandbox"
)

const noCaps = "CapInh=0000000000000000 CapPrm=0000000000000000 CapEff=0000000000000000 CapBnd=0000000000000000 CapAmb=0000000000000000"

// TestBubblewrapRunsCommandBehindEgress runs a command with host
// directories read-only and writable, a hidden directory, tmpfs mounts, a
// home, a socket of the host, and an endpoint whose router sends one host
// to a server on this machine and refuses the rest.
func TestBubblewrapRunsCommandBehindEgress(t *testing.T) {
	t.Parallel()
	rt, probe := bubblewrapRuntime(t, sandbox.MountHostDir)

	ro, rw, home := t.TempDir(), t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(ro, "hello.txt"), "hello")
	writeFile(t, filepath.Join(ro, "private", "key"), "key")
	server := listen(t)
	router := egress.RouterFunc(func(_ context.Context, f egress.Flow) (egress.Route, error) {
		if f.Host != "allowed.test" {
			return egress.Refuse(http.StatusForbidden, "not allowed"), nil
		}
		return egress.Dial(func(ctx context.Context) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", server))
		}), nil
	})
	sockDir := t.TempDir()
	sock, err := net.Listen("unix", filepath.Join(sockDir, "api.sock"))
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	t.Cleanup(func() {
		if err := sock.Close(); err != nil {
			t.Errorf("closing the socket error = %v", err)
		}
	})
	var stderr bytes.Buffer
	sb := create(t, rt, sandbox.Spec{
		Command: sandbox.Command{Path: probe, Dir: "/data", Args: []string{
			"read:/data/hello.txt", "write:/data/new.txt", "ls:/data/private", "write:/rw/new.txt",
			"write:/scratch/new.txt", "write:/empty/new.txt", "write:/home/agent/new.txt", "env:HOME", "env:SSL_CERT_FILE",
			"read:/etc/shadow", "ls:/root", "unix:/run/a2a/api.sock",
			"dial:allowed.test:80", "dial:refused.test:80", "dial:127.0.0.1:" + server, "dial:1.1.1.1:80",
			"caps:", "unshare:user", "userns:" + probe,
		}},
		Mounts: []sandbox.Mount{
			{Dst: "/data", Src: sandbox.HostDir(ro), ReadOnly: true},
			{Dst: "/rw", Src: sandbox.HostDir(rw)},
			{Dst: "/scratch", Src: sandbox.Tmpfs()},
			{Dst: "/empty", Src: sandbox.Tmpfs(), ReadOnly: true},
		},
		Hide:    []string{filepath.Join(ro, "private")},
		Home:    home,
		Sockets: []sandbox.Socket{{Listener: sock, Env: "API_SOCKET"}},
		Egress:  routerEndpoint(t, router),
		Stderr:  &stderr,
	})
	got := strings.Split(strings.TrimSpace(run(t, sb)), "\n")
	want := []string{
		"read:/data/hello.txt hello",
		"write:/data/new.txt error",
		"ls:/data/private 0 entries",
		"write:/rw/new.txt ok",
		"write:/scratch/new.txt ok",
		"write:/empty/new.txt error",
		"write:/home/agent/new.txt ok",
		"env:HOME /home/agent",
		"env:SSL_CERT_FILE /run/a2a-egress-ca-bundle.pem",
		"read:/etc/shadow error",
		"ls:/root error",
		"unix:/run/a2a/api.sock ok",
		"dial:allowed.test:80 ok",
		"dial:refused.test:80 refused",
		"dial:127.0.0.1:" + server + " refused",
		"dial:1.1.1.1:80 refused",
		"caps: " + noCaps,
		"unshare:user EPERM",
		"userns:" + probe + " EPERM",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("Sandbox.Start() output wrong result (-want +got) diff = %s\nstderr: %s", diff, stderr.String())
	}
	for _, p := range []string{filepath.Join(rw, "new.txt"), filepath.Join(home, "new.txt")} {
		if data, err := os.ReadFile(p); err != nil || string(data) != "written" {
			t.Fatalf("os.ReadFile(%q) = %q, %v, want what the command wrote", p, data, err)
		}
	}
}

// TestBubblewrapServesLiveWorkspace mounts a tree in memory, whose
// changes by the command the tree sees.
func TestBubblewrapServesLiveWorkspace(t *testing.T) {
	t.Parallel()
	rt, probe := bubblewrapRuntime(t, sandbox.MountServed)

	tree := fsys.Memory()
	put(t, tree, "host.txt", "from host")
	var stderr bytes.Buffer
	sb := create(t, rt, sandbox.Spec{
		Command: sandbox.Command{Path: probe, Dir: "/ws", Args: []string{
			"read:/ws/host.txt", "write:/ws/new.txt", "mkdir:/ws/dir", "mv:/ws/new.txt=/ws/dir/moved.txt", "caps:", "unshare:user",
		}},
		Mounts: []sandbox.Mount{{Dst: "/ws", Src: sandbox.Served(tree)}},
		Egress: openEndpoint(t, egress.Policy{Mode: egress.ModeAllowlist}),
		Stderr: &stderr,
	})
	got := strings.Split(strings.TrimSpace(run(t, sb)), "\n")
	want := []string{
		"read:/ws/host.txt from host",
		"write:/ws/new.txt ok",
		"mkdir:/ws/dir ok",
		"mv:/ws/new.txt=/ws/dir/moved.txt ok",
		"caps: " + noCaps,
		"unshare:user EPERM",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("Sandbox.Start() output wrong result (-want +got) diff = %s\nstderr: %s", diff, stderr.String())
	}
	if got := get(t, tree, "dir/moved.txt"); got != "written" {
		t.Fatalf("the tree's dir/moved.txt = %q, want what the command wrote", got)
	}
}

// TestBubblewrapWithoutEgressSharesNetwork runs a command without an
// endpoint, which reaches this machine's loopback.
func TestBubblewrapWithoutEgressSharesNetwork(t *testing.T) {
	t.Parallel()
	rt, probe := bubblewrapRuntime(t, sandbox.MountHostDir)

	server := listen(t)
	var stderr bytes.Buffer
	sb := create(t, rt, sandbox.Spec{
		Command: sandbox.Command{Path: probe, Args: []string{"dial:127.0.0.1:" + server, "ls:/root"}},
		Stderr:  &stderr,
	})
	got := strings.Split(strings.TrimSpace(run(t, sb)), "\n")
	want := []string{"dial:127.0.0.1:" + server + " ok", "ls:/root error"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("Sandbox.Start() output wrong result (-want +got) diff = %s\nstderr: %s", diff, stderr.String())
	}
}

func TestBubblewrapRefusesMissingProgram(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		opts sandbox.BubblewrapOptions
		cmd  string
		want string
	}{
		"command": {cmd: "a2a-no-such-agent", want: "a2a-no-such-agent"},
		"program": {opts: sandbox.BubblewrapOptions{Programs: []string{"a2a-no-such-cli"}}, cmd: "true", want: "a2a-no-such-cli"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			opts := tc.opts
			opts.StateDir = t.TempDir()
			rt := sandbox.Bubblewrap(opts)
			if _, err := rt.Probe(context.Background()); err != nil {
				t.Skipf("bubblewrap isn't available: %v", err)
			}
			_, err := rt.Create(context.Background(), sandbox.Spec{Command: sandbox.Command{Path: tc.cmd}})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Runtime.Create() error = %v, want one that names %s", err, tc.want)
			}
		})
	}
}

// bubblewrapRuntime returns the bubblewrap runtime with a launcher built
// from this tree, and the probe program, or skips the test where the
// runtime can't mount kind or route through the boundary.
func bubblewrapRuntime(t *testing.T, kind sandbox.MountKind) (sandbox.Runtime, string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("bubblewrap runs only on Linux")
	}
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("needs bwrap")
	}
	dir := t.TempDir()
	launcher, probe := filepath.Join(dir, "launcher"), filepath.Join(dir, "probe")
	for _, b := range []struct{ out, pkg string }{{launcher, "./testdata/launcher"}, {probe, "../../internal/sandboxrt/testdata/probe"}} {
		build := exec.Command("go", "build", "-o", b.out, b.pkg)
		build.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := build.CombinedOutput(); err != nil {
			t.Fatalf("go build %s error = %v\n%s", b.pkg, err, out)
		}
	}
	rt := sandbox.Bubblewrap(sandbox.BubblewrapOptions{Launcher: launcher, StateDir: t.TempDir()})
	caps, err := rt.Probe(context.Background())
	if err != nil {
		t.Skipf("bubblewrap isn't available: %v", err)
	}
	if !caps.Boundary {
		t.Skipf("the boundary can't work here: %s", caps.Problem)
	}
	if !slices.Contains(caps.Mounts, kind) {
		t.Skipf("%s mounts can't work here: %s", kind, caps.MountProblems[kind])
	}
	return rt, probe
}

func routerEndpoint(t *testing.T, r egress.Router) *egress.Endpoint {
	t.Helper()
	b, err := egress.New(egress.Config{Router: r, Log: slog.New(slog.DiscardHandler)})
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
