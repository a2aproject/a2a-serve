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
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
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

func TestEgressWarning(t *testing.T) {
	t.Parallel()
	noLauncher := func() (string, error) { return "", errors.New("no launcher") }
	launcher := func() (string, error) { return "/usr/bin/a2a-serve", nil }
	tests := []struct {
		name     string
		cfg      agentdir.Config
		launcher func() (string, error)
		want     string
	}{
		{name: "docker", launcher: launcher},
		{name: "docker without a launcher", launcher: noLauncher, want: "egress control is off: no launcher. The agent reaches every network"},
		{name: "allowlist without a launcher", cfg: agentdir.Config{Egress: agentdir.EgressConfig{Mode: "allowlist", Allow: []string{"nas.lan"}}}, launcher: noLauncher, want: "--egress allowlist can't apply: no launcher"},
		{name: "allowlist without rules", cfg: agentdir.Config{Egress: agentdir.EgressConfig{Mode: "allowlist"}}, launcher: launcher, want: "no --egress-allow rules"},
		{name: "unrestricted", cfg: agentdir.Config{Egress: agentdir.EgressConfig{Mode: "unrestricted"}}, launcher: noLauncher},
		{name: "local", cfg: agentdir.Config{Sandbox: SandboxLocal}, launcher: noLauncher},
		{name: "local with rules", cfg: agentdir.Config{Sandbox: SandboxLocal, Egress: agentdir.EgressConfig{Allow: []string{"nas.lan"}}}, launcher: launcher, want: "have no effect with --sandbox local"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := EgressWarning(&tc.cfg, tc.launcher)
			if tc.want == "" && got != "" || !strings.Contains(got, tc.want) {
				t.Fatalf("EgressWarning() = %q, want it to contain %q", got, tc.want)
			}
		})
	}
}
