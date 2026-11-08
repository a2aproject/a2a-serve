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

package harness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/sandboxnet"
)

func TestDockerRunnerCommandWithEgress(t *testing.T) {
	t.Parallel()
	for _, rootless := range []bool{false, true} {
		t.Run(fmt.Sprintf("rootless=%v", rootless), func(t *testing.T) {
			t.Parallel()
			r := &DockerRunner{
				Name:     "a2a-coder-123",
				Image:    "img:1",
				Mounts:   []agentdir.Mount{{Src: "/src/app", Dst: "/src/app"}},
				Rootless: rootless,
				Egress: &DockerEgress{
					Socket:     "/state/harness/abc/egress.sock",
					Launcher:   "/usr/bin/a2a-serve",
					Entrypoint: []string{"docker-entrypoint.sh"},
				},
			}
			cmd, err := r.Command(context.Background(), Command{Path: "opencode", Args: []string{"acp"}, Dir: "/src/app"})
			if err != nil {
				t.Fatalf("Command() error = %v", err)
			}
			uid, gid := fmt.Sprint(os.Getuid()), fmt.Sprint(os.Getgid())
			if rootless {
				uid, gid = "0", "0"
			}
			want := []string{
				"sh", "-c", startScript, "a2a-coder-123",
				"--rm", "-i",
				"--user", "0:0",
				"--cap-drop", "ALL",
				"--cap-add", "NET_ADMIN", "--cap-add", "SETPCAP", "--cap-add", "SETUID", "--cap-add", "SETGID",
				"--network", "none", "--device", "/dev/net/tun",
				"--entrypoint", "/run/a2a-init",
				"-v", "/usr/bin/a2a-serve:/run/a2a-init:ro",
				"-v", "/state/harness/abc:/run/a2a-egress:ro",
				"--security-opt", "no-new-privileges",
				"--memory", "4g", "--cpus", "2", "--pids-limit", "512",
				"-v", "/src/app:/src/app",
				"-e", "HOME=/home/agent",
				"-w", "/src/app",
				"img:1", "sandbox-init", "/run/a2a-egress/egress.sock", uid, gid,
				"docker-entrypoint.sh", "opencode", "acp",
			}
			if diff := cmp.Diff(want, cmd.Args); diff != "" {
				t.Fatalf("Command() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestDockerRunnerCommandWithRelay(t *testing.T) {
	t.Parallel()
	r := &DockerRunner{
		Name:   "a2a-coder-123",
		Image:  "img:1",
		Mounts: []agentdir.Mount{{Src: "/src/app", Dst: "/src/app"}},
		Egress: &DockerEgress{Relay: "a2a-coder-123-relay"},
	}
	cmd, err := r.Command(context.Background(), Command{Path: "opencode", Args: []string{"acp"}, Dir: "/src/app"})
	if err != nil {
		t.Fatalf("Command() error = %v", err)
	}
	want := []string{
		"sh", "-c", startScript, "a2a-coder-123",
		"--rm", "-i",
		"--user", "0:0",
		"--cap-drop", "ALL",
		"--cap-add", "NET_ADMIN", "--cap-add", "SETPCAP", "--cap-add", "SETUID", "--cap-add", "SETGID",
		"--network", "none", "--device", "/dev/net/tun",
		"--entrypoint", "/opt/a2a/a2a-serve", "--volumes-from", "a2a-coder-123-relay:ro",
		"--security-opt", "no-new-privileges",
		"--memory", "4g", "--cpus", "2", "--pids-limit", "512",
		"-v", "/src/app:/src/app",
		"-e", "HOME=/home/agent",
		"-w", "/src/app",
		"img:1", "sandbox-init", "/run/a2a-egress/egress.sock", fmt.Sprint(os.Getuid()), fmt.Sprint(os.Getgid()),
		"opencode", "acp",
	}
	if diff := cmp.Diff(want, cmd.Args); diff != "" {
		t.Fatalf("Command() wrong result (-want +got) diff = %s", diff)
	}
}

func TestPublishes(t *testing.T) {
	t.Parallel()
	ports := "80/tcp, 0.0.0.0:3000->3000/tcp, [::]:3000->3000/tcp\n127.0.0.1:4317->4317/tcp\n192.168.1.5:8080->80/tcp, 0.0.0.0:5353->53/udp"
	tests := []struct {
		port uint16
		want bool
	}{
		{port: 3000, want: true},
		{port: 4317, want: true},
		{port: 80},
		{port: 8080},
		{port: 5353},
		{port: 22},
	}
	for _, tc := range tests {
		if got := publishes(ports, tc.port); got != tc.want {
			t.Errorf("publishes(%d) = %v, want %v", tc.port, got, tc.want)
		}
	}
}

func TestDecideEgress(t *testing.T) {
	t.Parallel()
	fail := func() error { return errors.New("no tun") }
	ok := func() error { return nil }
	noLauncher := func() (string, error) { return "", errors.New("no launcher") }
	launcher := func() (string, error) { return "/usr/bin/a2a-serve", nil }
	linux := EgressEnv{GOOS: "linux", Launcher: launcher, TUN: ok}
	with := func(f func(*EgressEnv)) EgressEnv { e := linux; f(&e); return e }
	tests := []struct {
		name     string
		cfg      agentdir.Config
		env      EgressEnv
		boundary bool
		warning  string
	}{
		{name: "docker", env: linux, boundary: true},
		{name: "docker without a launcher", env: with(func(e *EgressEnv) { e.Launcher = noLauncher }), warning: "egress control is off: no launcher. The agent reaches every network"},
		{name: "docker without a TUN device", env: with(func(e *EgressEnv) { e.TUN = fail }), warning: "egress control is off: no tun"},
		{name: "image for another architecture", env: with(func(e *EgressEnv) { e.ImageArch = "s390x" }), warning: "the agent's image is for s390x"},
		{name: "docker desktop", env: with(func(e *EgressEnv) { e.GOOS = "darwin"; e.Launcher = noLauncher; e.TUN = fail }), boundary: true},
		{name: "forced relay", env: with(func(e *EgressEnv) { e.ForceRelay = true; e.Launcher = noLauncher }), boundary: true},
		{name: "allowlist without a launcher", cfg: agentdir.Config{Egress: agentdir.EgressConfig{Mode: "allowlist", Allow: []string{"nas.lan"}}}, env: with(func(e *EgressEnv) { e.Launcher = noLauncher }), warning: "--egress allowlist can't apply: no launcher"},
		{name: "allowlist without rules", cfg: agentdir.Config{Egress: agentdir.EgressConfig{Mode: "allowlist"}}, env: linux, boundary: true, warning: "no --egress-allow rules"},
		{name: "unrestricted", cfg: agentdir.Config{Egress: agentdir.EgressConfig{Mode: "unrestricted"}}, env: with(func(e *EgressEnv) { e.Launcher = noLauncher })},
		{name: "local", cfg: agentdir.Config{Sandbox: SandboxLocal}, env: linux},
		{name: "local with rules", cfg: agentdir.Config{Sandbox: SandboxLocal, Egress: agentdir.EgressConfig{Allow: []string{"nas.lan"}}}, env: linux, warning: "have no effect with --sandbox local"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := DecideEgress(&tc.cfg, tc.env)
			if got.Boundary != tc.boundary {
				t.Fatalf("DecideEgress().Boundary = %v, want %v", got.Boundary, tc.boundary)
			}
			if tc.warning == "" && got.Warning != "" || !strings.Contains(got.Warning, tc.warning) {
				t.Fatalf("DecideEgress().Warning = %q, want it to contain %q", got.Warning, tc.warning)
			}
		})
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

func TestProbeFailed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		mode, want string
	}{
		{mode: "", want: "egress control is off: the agent's container can't set up its network: operation not permitted. The agent reaches every network"},
		{mode: "allowlist", want: "--egress allowlist can't apply: the agent's container can't set up its network: operation not permitted"},
	}
	for _, tc := range tests {
		got := probeFailed(tc.mode, errors.New("operation not permitted"))
		if got.Boundary || got.Problem == "" || !strings.HasPrefix(got.Warning, tc.want) {
			t.Fatalf("probeFailed(%q) = %+v, want no boundary and a warning starting with %q", tc.mode, got, tc.want)
		}
	}
}

func TestCommandWithFallback(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		checkFail bool
		want      []string
	}{
		{name: "check passes", want: []string{"check", "egress", "check", "egress"}},
		{name: "check fails", checkFail: true, want: []string{"check", "fallback", "fallback"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			calls := filepath.Join(dir, "calls")
			fail := "exit 0"
			if tc.checkFail {
				fail = "echo \"a2a-init: setting up the sandbox's network: no tun\" >&2; exit " + fmt.Sprint(sandboxnet.SetupFailedExit)
			}
			fakeDocker := "#!/bin/sh\n" +
				"[ \"$1\" = run ] || exit 0\n" +
				"case \"$*\" in *--check*) echo check >>" + calls + "; " + fail + ";;\n" +
				"*sandbox-init*) echo egress >>" + calls + ";;\n" +
				"*) echo fallback >>" + calls + ";; esac\n"
			if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(fakeDocker), 0o700); err != nil {
				t.Fatal(err)
			}
			r := &DockerRunner{Name: "a2a-coder-1", Image: "img:1", Egress: &DockerEgress{Socket: "/s/egress.sock", Launcher: "/bin/a2a-serve"}}
			fallback := &DockerRunner{Name: "a2a-coder-1", Image: "img:1"}
			marker := filepath.Join(dir, "fallback")
			for range 2 {
				cmd, err := r.CommandWithFallback(context.Background(), Command{Path: "agent", Args: []string{"it's"}}, fallback, marker)
				if err != nil {
					t.Fatalf("CommandWithFallback() error = %v", err)
				}
				cmd.Env = append(cmd.Env, "PATH="+dir+":/usr/bin:/bin")
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("starting the container error = %v: %s", err, out)
				}
			}
			data, err := os.ReadFile(calls)
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.want, strings.Fields(string(data))); diff != "" {
				t.Fatalf("docker run calls wrong result (-want +got) diff = %s", diff)
			}
			if tc.checkFail {
				if got := setupReason(marker); got != "no tun" {
					t.Fatalf("setupReason() = %q, want %q", got, "no tun")
				}
			}
		})
	}
}
