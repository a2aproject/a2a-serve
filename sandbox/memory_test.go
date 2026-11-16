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
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/cryptoutil"
	"github.com/a2aproject/a2a-serve/sandbox/egress"
	"github.com/a2aproject/a2a-serve/sandbox/sandboxnet"
)

func TestMemoryFilesAbs(t *testing.T) {
	t.Parallel()
	f := &memoryFiles{dir: "/workspace"}
	tests := []struct {
		name, want string
		wantErr    bool
	}{
		{name: "a.txt", want: "/workspace/a.txt"},
		{name: "sub/../b.txt", want: "/workspace/b.txt"},
		{name: "/workspace/c", want: "/workspace/c"},
		{name: ".", want: "/workspace"},
		{name: "../etc/passwd", wantErr: true},
		{name: "/etc/passwd", wantErr: true},
		{name: "/workspacex/a", wantErr: true},
	}
	for _, tc := range tests {
		got, err := f.abs(tc.name)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Fatalf("memoryFiles.abs(%q) = %q, %v, want %q, error %v", tc.name, got, err, tc.want, tc.wantErr)
		}
	}
}

func TestLocalRefusesMemory(t *testing.T) {
	t.Parallel()
	if _, err := (Local{}).Prepare(context.Background(), Spec{Memory: &Memory{Dir: "/w"}}); err == nil {
		t.Fatal("Local.Prepare() with Memory error = nil, want an error")
	}
}

// staticInterceptor answers the requests to virtual.test itself and leaves
// the rest to the policy.
type staticInterceptor struct{}

func (staticInterceptor) Connect(_ context.Context, host string, _ uint16) sandboxnet.ConnectAction {
	if host == "virtual.test" {
		return sandboxnet.ConnectInspect
	}
	return sandboxnet.ConnectPolicy
}

func (staticInterceptor) Request(_ context.Context, r *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("answered " + r.URL.Path))}, nil
}

// TestDockerMemoryFiles runs a command in a container whose workspace is
// in memory, behind a boundary whose interceptor answers a virtual host.
func TestDockerMemoryFiles(t *testing.T) {
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
	b := NewBoundaries(filepath.Join(t.TempDir(), "boundaries"), 12, "abcdefghijklmnopqrstuvwxyz", &sandboxnet.Blocks{})
	t.Cleanup(func() {
		if err := b.Close(context.Background()); err != nil {
			t.Errorf("Boundaries.Close() error = %v", err)
		}
	})
	policy, err := egress.NewPolicy(egress.ModeAllowlist, nil)
	if err != nil {
		t.Fatalf("egress.NewPolicy() error = %v", err)
	}
	name := "a2a-sandbox-test-" + cryptoutil.RandomString(12, "abcdefghijklmnopqrstuvwxyz")
	var stderr bytes.Buffer
	sb, err := NewDocker(t.TempDir(), func() string { return launcher }).Prepare(ctx, Spec{
		Name:    name,
		Image:   image,
		Command: Command{Path: "/probe", Args: []string{"read:/workspace/seed/hello.txt", "write:/workspace/out.txt", "http:virtual.test/tool"}, Dir: "/workspace"},
		Memory:  &Memory{Dir: "/workspace", Size: "16m", Files: map[string][]byte{"seed/hello.txt": []byte("seeded")}},
		Labels:  map[string]string{"a2a.test": "sandbox"},
		Network: Network{Boundary: b.Opener(name, BoundaryRules{Policy: func() egress.Policy { return policy }, Interceptor: staticInterceptor{}})},
		Stderr:  &stderr,
	})
	if err != nil {
		t.Fatalf("Docker.Prepare() error = %v", err)
	}
	t.Cleanup(func() {
		if err := sb.Close(context.Background()); err != nil {
			t.Errorf("Sandbox.Close() error = %v", err)
		}
		if out, err := exec.Command("docker", "volume", "inspect", memoryVolume(name)).CombinedOutput(); err == nil {
			t.Errorf("volume %s after Sandbox.Close() exists: %s", memoryVolume(name), out)
		}
	})
	files, ok := sb.(Files)
	if !ok {
		t.Fatalf("Docker.Prepare() = %T, want a sandbox with Files", sb)
	}
	rwc, err := sb.Start(ctx)
	if err != nil {
		t.Fatalf("Sandbox.Start() error = %v", err)
	}
	got := strings.Split(strings.TrimSpace(drain(t, rwc)), "\n")
	want := []string{"read:/workspace/seed/hello.txt seeded", "write:/workspace/out.txt ok", "http:virtual.test/tool answered /tool"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("Sandbox.Start() output wrong result (-want +got) diff = %s\nstderr: %s", diff, stderr.String())
	}
	data, err := files.ReadFile(ctx, "out.txt")
	if err != nil || string(data) != "written" {
		t.Fatalf("Files.ReadFile(out.txt) = %q, %v, want %q", data, err, "written")
	}
	if err := files.WriteFile(ctx, "notes/plan.md", []byte("plan")); err != nil {
		t.Fatalf("Files.WriteFile() error = %v", err)
	}
	names, err := files.List(ctx, ".")
	if err != nil {
		t.Fatalf("Files.List() error = %v", err)
	}
	slices.Sort(names)
	if diff := cmp.Diff([]string{"notes/plan.md", "out.txt", "seed/hello.txt"}, names); diff != "" {
		t.Fatalf("Files.List() wrong result (-want +got) diff = %s", diff)
	}
	if _, err := files.ReadFile(ctx, "/etc/hostname"); err == nil {
		t.Fatal("Files.ReadFile(/etc/hostname) error = nil, want an error for a path outside the file system")
	}

	if _, err := files.Exec(ctx, []string{"/probe", "read:out.txt"}, nil); err == nil {
		t.Fatal("Files.Exec() without a running command error = nil, want an error")
	}

	sb.(*memorySandbox).runArgs = append(sb.(*memorySandbox).runArgs[:len(sb.(*memorySandbox).runArgs)-3], "wait:")
	rwc, err = sb.Start(ctx)
	if err != nil {
		t.Fatalf("Sandbox.Start() again error = %v", err)
	}
	var res ExecResult
	for range 100 {
		if res, err = files.Exec(ctx, []string{"/probe", "read:notes/plan.md", "dial:" + net.JoinHostPort(egress.HostGateway, "1")}, nil); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("Files.Exec() error = %v", err)
	}
	if got, want := string(res.Stdout), "read:notes/plan.md plan\ndial:host.docker.internal:1 refused\n"; got != want || res.ExitCode != 0 {
		t.Fatalf("Files.Exec() = %q, exit %d, want %q, exit 0\nstderr: %s", got, res.ExitCode, want, res.Stderr)
	}
	tctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if _, err := files.Exec(tctx, []string{"/probe", "sleep:5m"}, nil); err == nil {
		t.Fatal("Files.Exec() past its timeout error = nil, want an error")
	}
	if out, err := exec.Command("docker", "top", name).CombinedOutput(); err != nil || strings.Contains(string(out), "sleep:5m") {
		t.Fatalf("docker top %s after the timeout = %s, %v, want no sleeping command", name, out, err)
	}
	if err := rwc.Close(); err != nil {
		t.Fatalf("closing the command error = %v", err)
	}

	if out, err := exec.Command("docker", "kill", memoryVolume(name)).CombinedOutput(); err != nil {
		t.Fatalf("docker kill the holder error = %v: %s", err, out)
	}
	if _, err := sb.Start(ctx); err == nil || !strings.Contains(err.Error(), "the file system in memory is gone") {
		t.Fatalf("Sandbox.Start() after the holder died error = %v, want the file system gone", err)
	}
}

func TestRemoveVolumeRemovesStoppedUsers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	if _, err := CheckDocker(ctx); err != nil {
		t.Skipf("docker isn't available: %v", err)
	}
	_, image := buildTestContainer(t)
	name := "a2a-sandbox-test-" + cryptoutil.RandomString(12, "abcdefghijklmnopqrstuvwxyz")
	if err := createVolume(ctx, name, &Memory{Size: "1m"}, nil); err != nil {
		t.Fatalf("createVolume() error = %v", err)
	}
	if out, err := exec.Command("docker", "run", "--name", name, "-v", name+":/w", image, "/probe", "write:/w/a").CombinedOutput(); err != nil {
		t.Fatalf("docker run error = %v: %s", err, out)
	}
	t.Cleanup(func() {
		if err := RemoveContainer(context.Background(), name); err != nil {
			t.Errorf("RemoveContainer() error = %v", err)
		}
	})
	if err := removeVolume(ctx, name); err != nil {
		t.Fatalf("removeVolume() with a stopped container using it error = %v", err)
	}
	if err := exec.Command("docker", "volume", "inspect", name).Run(); err == nil {
		t.Fatalf("volume %s exists after removeVolume()", name)
	}
}
