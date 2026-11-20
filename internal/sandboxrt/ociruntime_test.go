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

package sandboxrt

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-serve/internal/sandboxrt/sandboxtest"
)

func TestOCIRuntimeHostSocketsProblem(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		rt   ociRuntime
		want bool
	}{
		{name: "host-uds open", rt: ociRuntime{Name: "runsc", Path: "/usr/bin/runsc", Args: []string{"--host-uds=open"}}},
		{name: "host-uds all as two words", rt: ociRuntime{Name: "runsc", Path: "/usr/bin/runsc", Args: []string{"--debug", "--host-uds", "all"}}},
		{name: "deprecated flag", rt: ociRuntime{Name: "runsc", Path: "/usr/bin/runsc", Args: []string{"--fsgofer-host-uds"}}},
		{name: "containerd shim", rt: ociRuntime{Name: "runsc", Type: "io.containerd.runsc.v1"}},
		{name: "no flags", rt: ociRuntime{Name: "runsc", Path: "/usr/bin/runsc"}, want: true},
		{name: "host-uds create", rt: ociRuntime{Name: "runsc", Path: "/usr/bin/runsc", Args: []string{"--host-uds=create"}}, want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.rt.hostSocketsProblem()
			if got := err != nil; got != tc.want {
				t.Fatalf("ociRuntime.hostSocketsProblem() = %v, want a problem %v", err, tc.want)
			}
			if err != nil && (!errors.Is(err, ErrNoBoundary) || !strings.Contains(err.Error(), "--host-uds=open")) {
				t.Fatalf("ociRuntime.hostSocketsProblem() = %v, want one that names --host-uds=open and wraps ErrNoBoundary", err)
			}
		})
	}
}

func TestOCIRuntimeGVisor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		rt   ociRuntime
		want bool
	}{
		{rt: ociRuntime{Name: "runsc", Path: "runsc"}, want: true},
		{rt: ociRuntime{Name: "gvisor", Path: "/usr/local/bin/runsc"}, want: true},
		{rt: ociRuntime{Name: "sandboxed", Type: "io.containerd.runsc.v1"}, want: true},
		{rt: ociRuntime{Name: "crun", Path: "/usr/bin/crun"}},
		{},
	}
	for _, tc := range tests {
		if got := tc.rt.gVisor(); got != tc.want {
			t.Fatalf("%+v.gVisor() = %v, want %v", tc.rt, got, tc.want)
		}
	}
}

func TestDockerRunnerRuntime(t *testing.T) {
	t.Parallel()
	for _, eg := range []*DockerEgress{nil, {Socket: "/state/abc/egress.sock", Launcher: "/usr/bin/a2a-serve"}} {
		for _, check := range []bool{false, true} {
			r := &DockerRunner{Name: "a2a-coder-123", Image: "img:1", Egress: eg, Runtime: "runsc"}
			if args := r.runArgs(Command{Path: "agent"}, "", check); !containsRun(args, []string{"--runtime", "runsc"}) {
				t.Fatalf("DockerRunner.runArgs(check %v) = %q, want --runtime runsc", check, args)
			}
			r.Runtime = ""
			if args := r.runArgs(Command{Path: "agent"}, "", check); slices.Contains(args, "--runtime") {
				t.Fatalf("DockerRunner.runArgs(check %v) without a runtime = %q, want no --runtime", check, args)
			}
		}
	}
}

// runtimesDocker is a fake docker whose daemon has the runtimes of the
// JSON runtimes, as docker info prints them.
func runtimesDocker(runtimes string) string {
	return `case "$1 $2 $3" in
"info --format {{json .Runtimes}}") echo '` + runtimes + `' ;;
"info --format "*) echo "[name=seccomp,profile=builtin]" ;;
*) echo "fake docker: $1 failed" >&2; exit 1 ;;
esac
`
}

func TestDockerProbeChecksRuntime(t *testing.T) {
	const (
		withoutHostUDS = `{"runc":{"path":"runc"},"runsc":{"path":"/usr/bin/runsc"}}`
		withHostUDS    = `{"runc":{"path":"runc"},"runsc":{"path":"/usr/bin/runsc","runtimeArgs":["--host-uds=open"]}}`
	)
	tests := []struct {
		name         string
		runtimes     string
		runtime      string
		relay        bool
		wantErr      string
		wantBoundary string
		wantServed   string
	}{
		{name: "missing runsc", runtimes: `{"runc":{"path":"runc"}}`, runtime: "runsc", wantErr: "docker has no runtime runsc; install gVisor and register runsc with Docker, as " + GVisorInstallDoc},
		{name: "missing other runtime", runtimes: `{"runc":{"path":"runc"},"crun":{"path":"crun"}}`, runtime: "kata", wantErr: "docker has no runtime kata; pick one of crun, runc"},
		{name: "runsc without host sockets", runtimes: withoutHostUDS, runtime: "runsc", wantBoundary: "without --host-uds=open", wantServed: "gVisor's FUSE"},
		{name: "runsc with host sockets", runtimes: withHostUDS, runtime: "runsc", wantServed: "gVisor's FUSE"},
		{name: "runsc with an egress container", runtimes: withHostUDS, runtime: "runsc", relay: true, wantBoundary: "doesn't work with the runtime runsc", wantServed: "gVisor's FUSE"},
	}
	if err := CheckTUN(); err != nil {
		t.Skip(err)
	}
	launcher := filepath.Join(t.TempDir(), "launcher")
	build := exec.Command("go", "build", "-o", launcher, "./testdata/launcher")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build ./testdata/launcher error = %v\n%s", err, out)
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sandboxtest.FakeDocker(t, runtimesDocker(tc.runtimes))
			t.Setenv(EnvForceRelay, "")
			if tc.relay {
				t.Setenv(EnvForceRelay, "1")
			}
			c, err := NewDocker(DockerConfig{Launcher: func() string { return launcher }, Runtime: tc.runtime}).Probe(context.Background())
			if tc.wantErr != "" {
				if _, ok := errors.AsType[*DockerError](err); !ok || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Docker.Probe() error = %v, want a DockerError with %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Docker.Probe() error = %v", err)
			}
			if tc.wantBoundary != "" && (c.Boundary == nil || !strings.Contains(c.Boundary.Error(), tc.wantBoundary)) {
				t.Fatalf("Docker.Probe() boundary problem = %v, want %q", c.Boundary, tc.wantBoundary)
			}
			if tc.wantBoundary == "" && c.Boundary != nil {
				t.Fatalf("Docker.Probe() boundary problem = %v, want none", c.Boundary)
			}
			if c.EgressRelay {
				t.Fatalf("Docker.Probe() = %+v, want no egress container with a runtime", c)
			}
			if c.Served == nil || !strings.Contains(c.Served.Error(), tc.wantServed) {
				t.Fatalf("Docker.Probe() served problem = %v, want %q", c.Served, tc.wantServed)
			}
		})
	}
}

func TestDockerPrepareRefusesServedUnderGVisor(t *testing.T) {
	sandboxtest.FakeDocker(t, runtimesDocker(`{"runsc":{"path":"/usr/bin/runsc","runtimeArgs":["--host-uds=open"]}}`))
	_, err := NewDocker(DockerConfig{EnvDir: t.TempDir(), Runtime: "runsc"}).Prepare(context.Background(), Spec{
		Name:    "a2a-test",
		Image:   "img:1",
		Command: Command{Path: "agent"},
		Served:  []ServedMount{{Dst: "/workspace"}},
		Network: Network{Boundary: func(HostGatewayFunc, bool) (EgressEndpoint, error) {
			return EgressEndpoint{}, errors.New("unexpected boundary")
		}},
	})
	if sme, ok := errors.AsType[*ServedMountError](err); !ok || sme.Dst != "/workspace" || !strings.Contains(sme.Reason, "gVisor") {
		t.Fatalf("Docker.Prepare() with a served mount under gVisor error = %v, want a ServedMountError for gVisor", err)
	}
}
