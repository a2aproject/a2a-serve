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

package a2aserve_test

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/core/a2aserve"
	"github.com/a2aproject/a2a-serve/core/egress"
	"github.com/a2aproject/a2a-serve/core/fsys"
	"github.com/a2aproject/a2a-serve/core/sandbox"
	"github.com/a2aproject/a2a-serve/internal/cryptoutil"
	"github.com/a2aproject/a2a-serve/internal/testutil"
)

func TestNewRefusesLiveWithoutASandbox(t *testing.T) {
	t.Parallel()
	_, err := a2aserve.New(context.Background(), a2aserve.Config{
		Profile:  a2aserve.Command(fakeACP),
		Runtime:  sandbox.Local(),
		StateDir: testutil.ShortTempDir(t),
		Main:     a2aserve.Environment{Workspace: a2aserve.Live(fsys.Memory(), a2aserve.LiveOptions{}), Home: filepath.Join(t.TempDir(), "home")},
	})
	got, ok := errors.AsType[*sandbox.UnsupportedMountError](err)
	if !ok {
		t.Fatalf("a2aserve.New() with a live workspace on the host error = %v, want an UnsupportedMountError", err)
	}
	want := &sandbox.UnsupportedMountError{Backend: "local", Dst: "/workspace", Kind: sandbox.MountServed, Reason: "the command runs on the host, with no mount namespace"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("a2aserve.New() wrong error (-want +got) diff = %s", diff)
	}
}

func TestLiveRefusesAttach(t *testing.T) {
	t.Parallel()
	tests := map[string]a2aserve.Workspace{
		"outside an Agent": a2aserve.Live(fsys.Memory(), a2aserve.LiveOptions{}),
		"relative path":    a2aserve.Live(fsys.Memory(), a2aserve.LiveOptions{At: "workspace"}),
		"no tree":          a2aserve.Live(nil, a2aserve.LiveOptions{}),
	}
	for name, w := range tests {
		if _, err := w.Attach(context.Background(), "k"); err == nil {
			t.Fatalf("%s: Workspace.Attach() error = nil, want one", name)
		}
	}
}

// TestLiveAgentWorksOnTheTree runs fakeacp in Docker on a live workspace:
// the caller sees what the agent writes when its turn ends, and the agent
// sees the caller's files and the masks of its workspace.
func TestLiveAgentWorksOnTheTree(t *testing.T) {
	t.Parallel()
	rt, image := liveDocker(t)
	caps, err := rt.Probe(context.Background())
	if err != nil {
		t.Fatalf("Docker().Probe() error = %v", err)
	}
	if !slices.Contains(caps.Mounts, sandbox.MountServed) {
		t.Skipf("served mounts can't work here: %s", caps.MountProblems[sandbox.MountServed])
	}
	b, err := egress.New(egress.Config{Policy: func() egress.Policy { return egress.Policy{Mode: egress.ModeAllowlist} }, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("egress.New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := b.Close(); err != nil {
			t.Errorf("Boundary.Close() error = %v", err)
		}
	})
	tree := fsys.Memory()
	put(t, tree, "host.txt", "from the host", 0o644)
	put(t, tree, ".mcp.json", `{"mcpServers":{"evil":{}}}`, 0o644)
	put(t, tree, "sub/.a2a/secret", "secret", 0o600)
	put(t, tree, "sub/code.go", "package sub", 0o644)
	a := newDockerAgent(t, a2aserve.Config{
		Profile: a2aserve.Command("/fakeacp"),
		Runtime: rt,
		Egress:  b,
		Main: a2aserve.Environment{
			Workspace: a2aserve.Live(tree, a2aserve.LiveOptions{}),
			Home:      filepath.Join(t.TempDir(), "home"),
			Image:     image,
			Name:      "a2aserve-live-test-" + cryptoutil.RandomString(12, "abcdefghijklmnopqrstuvwxyz"),
		},
	})
	c := client(t, a)

	if got := send(t, c, "", "write out.txt"); !strings.Contains(got, "write ok") {
		t.Fatalf("reply to write = %q, want write ok", got)
	}
	if got := snapshot(t, tree)["out.txt"]; got != "-rw-r--r-- written by fakeacp\n" {
		t.Fatalf("the tree's out.txt after the turn = %q, want what the agent wrote", got)
	}
	tests := []struct{ prompt, want string }{
		{"read host.txt", "from the host"},
		{"read .mcp.json", `{"mcpServers": {}}`},
		{"ls sub", "code.go"},
	}
	for _, tc := range tests {
		if got := send(t, c, "", tc.prompt); !strings.Contains(got, tc.want) || strings.Contains(got, ".a2a") {
			t.Fatalf("reply to %q = %q, want %q", tc.prompt, got, tc.want)
		}
	}
}

// newDockerAgent starts an agent with cfg and its own state directory, and
// closes it at the end of the test.
func newDockerAgent(t *testing.T, cfg a2aserve.Config) *a2aserve.Agent {
	t.Helper()
	cfg.StateDir = testutil.ShortTempDir(t)
	a, err := a2aserve.New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("a2aserve.New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := a.Close(context.Background()); err != nil {
			t.Errorf("Agent.Close() error = %v", err)
		}
	})
	return a
}

// liveDocker returns the Docker runtime with a launcher built from this
// tree, and an image of fakeacp, or skips the test where Docker can't run.
func liveDocker(t *testing.T) (sandbox.Runtime, string) {
	t.Helper()
	if runtime.GOOS != "linux" || os.Getenv("A2A_SERVE_SANDBOX_RELAY") == "1" {
		t.Skip("needs the egress boundary of Linux without an egress container")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("needs the docker CLI")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skipf("docker isn't available: %v", err)
	}
	dir := t.TempDir()
	launcher := filepath.Join(dir, "launcher")
	for _, b := range []struct{ out, pkg string }{{launcher, "../sandbox/testdata/launcher"}, {filepath.Join(dir, "fakeacp"), "../../internal/agentsrv/testdata/fakeacp"}} {
		build := exec.Command("go", "build", "-o", b.out, b.pkg)
		build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux")
		if out, err := build.CombinedOutput(); err != nil {
			t.Fatalf("go build %s error = %v\n%s", b.pkg, err, out)
		}
	}
	image := "a2aserve-live-test:" + cryptoutil.RandomString(12, "abcdefghijklmnopqrstuvwxyz")
	build := exec.Command("docker", "build", "-q", "-t", image, "-f", "-", dir)
	build.Stdin = strings.NewReader("FROM scratch\nCOPY fakeacp /fakeacp\n")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("docker build error = %v\n%s", err, out)
	}
	t.Cleanup(func() {
		if out, err := exec.Command("docker", "rmi", "-f", image).CombinedOutput(); err != nil {
			t.Errorf("docker rmi %s error = %v: %s", image, err, out)
		}
	})
	return sandbox.Docker(sandbox.DockerOptions{Launcher: launcher, StateDir: testutil.ShortTempDir(t)}), image
}
