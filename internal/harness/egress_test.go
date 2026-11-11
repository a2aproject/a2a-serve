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
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strconv"
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
	fakeDocker(t, `echo "0.0.0.0:3000->3000/tcp, [::]:3000->3000/tcp"; echo "127.0.0.1:6379->6379/tcp"`)
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
		got, ok := hostGateway(tc.goos)(context.Background(), tc.port)
		if ok != tc.want {
			t.Fatalf("hostGateway(%q)(%d) = %v, %v, want found %v", tc.goos, tc.port, got, ok, tc.want)
		}
		if want := netip.MustParseAddrPort("127.0.0.1:" + strconv.Itoa(int(tc.port))); ok && got != want {
			t.Fatalf("hostGateway(%q)(%d) = %v, want %v", tc.goos, tc.port, got, want)
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
		{name: "relay image to pull", cfg: agentdir.Config{Egress: agentdir.EgressConfig{Mode: "allowlist", Allow: []string{"nas.lan"}}}, env: with(func(e *EgressEnv) {
			e.ForceRelay = true
			e.RelayImage = func() error { return &missingRelayImage{image: "img", pull: true} }
		}), boundary: true, warning: "the egress container's image img isn't on this machine"},
		{name: "relay image unknown", cfg: agentdir.Config{Egress: agentdir.EgressConfig{Mode: "allowlist", Allow: []string{"nas.lan"}}}, env: with(func(e *EgressEnv) {
			e.ForceRelay = true
			e.RelayImage = func() error { return errors.New("no commit") }
		}), warning: "--egress allowlist can't apply: no commit"},
		{name: "allowlist without a launcher", cfg: agentdir.Config{Egress: agentdir.EgressConfig{Mode: "allowlist", Allow: []string{"nas.lan"}}}, env: with(func(e *EgressEnv) { e.Launcher = noLauncher }), warning: "--egress allowlist can't apply: no launcher"},
		{name: "allowlist without rules", cfg: agentdir.Config{Egress: agentdir.EgressConfig{Mode: "allowlist"}}, env: linux, boundary: true, warning: "no --egress-allow rules"},
		{name: "inject without a launcher", cfg: agentdir.Config{Inject: []agentdir.InjectRule{{Host: "api.github.com", Header: "Authorization: Bearer $GH"}}}, env: with(func(e *EgressEnv) { e.Launcher = noLauncher }), warning: "--inject-header can't apply: no launcher. The agent doesn't start, so that it can't read GH."},
		{name: "unrestricted", cfg: agentdir.Config{Egress: agentdir.EgressConfig{Mode: "unrestricted"}}, env: with(func(e *EgressEnv) { e.Launcher = noLauncher })},
		{name: "local", cfg: agentdir.Config{Sandbox: SandboxLocal}, env: linux},
		{name: "local with rules", cfg: agentdir.Config{Sandbox: SandboxLocal, Egress: agentdir.EgressConfig{Allow: []string{"nas.lan"}}}, env: linux, warning: "--egress-allow has no effect with --sandbox local: " + UnrestrictedEffect + "; use --sandbox docker, or remove the rules with `a2a-serve config unset egress-allow`"},
		{name: "local with a mode", cfg: agentdir.Config{Sandbox: SandboxLocal, Egress: agentdir.EgressConfig{Mode: "allowlist"}}, env: linux, warning: "--egress has no effect with --sandbox local: " + UnrestrictedEffect + "; use --sandbox docker, or remove the setting with `a2a-serve config unset egress`"},
		{name: "local with a mode and rules", cfg: agentdir.Config{Sandbox: SandboxLocal, Egress: agentdir.EgressConfig{Mode: "allowlist", Allow: []string{"nas.lan"}}}, env: linux, warning: "--egress and --egress-allow have no effect with --sandbox local: " + UnrestrictedEffect + "; use --sandbox docker, or remove the settings with `a2a-serve config unset egress` and `a2a-serve config unset egress-allow`"},
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
			var missing *missingRelayImage
			if got := errors.As(err, &missing); got != tc.wantMissing {
				t.Fatalf("CheckRelayImage() error is missingRelayImage = %v, want %v", got, tc.wantMissing)
			}
		})
	}
}

func TestMissingRelayImage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		missing missingRelayImage
		want    string
	}{
		{name: "pull or build", missing: missingRelayImage{image: "img", pull: true, build: true}, want: "the egress container's image img isn't on this machine; the next start of the agent's container pulls it, or builds it from the a2a-serve repository"},
		{name: "pull only", missing: missingRelayImage{image: "img", pull: true}, want: "the egress container's image img isn't on this machine; the next start of the agent's container pulls it"},
		{name: "build", missing: missingRelayImage{image: "img", build: true}, want: "the egress container's image img is neither on this machine nor in the registry; the next start of the agent's container builds it from the a2a-serve repository, which takes a few minutes"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.missing.Error(); got != tc.want {
				t.Fatalf("missingRelayImage.Error() = %q, want %q", got, tc.want)
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
			log := fakeDocker(t, noRelayPullDocker)
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

func TestProbeFailed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cfg  agentdir.Config
		want string
	}{
		{name: "internet", want: "egress control is off: the agent's container can't set up its network: operation not permitted. The agent reaches every network"},
		{name: "allowlist", cfg: agentdir.Config{Egress: agentdir.EgressConfig{Mode: "allowlist"}}, want: "--egress allowlist can't apply: the agent's container can't set up its network: operation not permitted"},
		{
			name: "inject", cfg: agentdir.Config{Inject: []agentdir.InjectRule{{Model: "openrouter", Key: "OPENROUTER_API_KEY"}}},
			want: "--inject-model-key can't apply: the agent's container can't set up its network: operation not permitted. The agent doesn't start, so that it can't read OPENROUTER_API_KEY. Fix that, or put the keys in the agent's environment: remove the rules with `a2a-serve config unset inject-model-key`, then store the keys again with `a2a-serve config set --secret OPENROUTER_API_KEY=-`",
		},
	}
	two := probeFailed(&agentdir.Config{Inject: []agentdir.InjectRule{{Model: "openrouter", Key: "K"}, {Host: "api.github.com", Header: "Authorization: Bearer $GH"}}}, errors.New("x"))
	if want := "remove the rules with `a2a-serve config unset inject-model-key` and `a2a-serve config unset inject-header`, then store the keys again with `a2a-serve config set --secret K=-` and `a2a-serve config set --secret GH=-`"; !strings.Contains(two.Warning, want) {
		t.Fatalf("probeFailed() warning = %q, want it to contain %q", two.Warning, want)
	}
	for _, tc := range tests {
		got := probeFailed(&tc.cfg, errors.New("operation not permitted"))
		if got.Boundary || got.Problem == "" || !strings.HasPrefix(got.Warning, tc.want) {
			t.Fatalf("probeFailed(%s) = %+v, want no boundary and a warning starting with %q", tc.name, got, tc.want)
		}
	}
}

func TestCommandWithFallback(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// checks are what the successive checks do.
		checks     []string
		wantFailed []bool
		want       []string
		wantReason string
	}{
		{name: "check passes", checks: []string{"exit 0", "exit 0"}, wantFailed: []bool{false, false}, want: []string{"check", "egress", "check", "egress"}},
		{name: "launcher can't set up the network", checks: []string{launcherNoTUN}, wantFailed: []bool{false, false}, want: []string{"check", "fallback", "fallback"}, wantReason: "no tun"},
		{name: "runtime refuses the TUN device", checks: []string{tunMissing}, wantFailed: []bool{false, false}, want: []string{"check", "fallback", "fallback"}, wantReason: `docker: Error response from daemon: error gathering device information while adding custom device "/dev/net/tun": no such file or directory`},
		{name: "daemon error", checks: []string{daemonError, "exit 0"}, wantFailed: []bool{true, false}, want: []string{"check", "check", "egress"}},
		{name: "launcher crash naming the device", checks: []string{launcherCrashTUN, "exit 0"}, wantFailed: []bool{true, false}, want: []string{"check", "check", "egress"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			calls := filepath.Join(dir, "calls")
			var checks strings.Builder
			for i, c := range tc.checks {
				fmt.Fprintf(&checks, "%d) %s;;\n", i+1, c)
			}
			fakeDocker := "#!/bin/sh\n" +
				"[ \"$1\" = run ] || exit 0\n" +
				"case \"$*\" in *--check*) echo check >>" + calls + "\n" +
				"case $(grep -c check " + calls + ") in\n" + checks.String() + "esac;;\n" +
				"*sandbox-init*) echo egress >>" + calls + ";;\n" +
				"*) echo fallback >>" + calls + ";; esac\n"
			if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(fakeDocker), 0o700); err != nil {
				t.Fatal(err)
			}
			r := &DockerRunner{Name: "a2a-coder-1", Image: "img:1", Egress: &DockerEgress{Socket: "/s/egress.sock", Launcher: "/bin/a2a-serve"}}
			fallback := &DockerRunner{Name: "a2a-coder-1", Image: "img:1"}
			marker := filepath.Join(dir, "fallback")
			for i, wantFailed := range tc.wantFailed {
				cmd, err := r.CommandWithFallback(context.Background(), Command{Path: "agent", Args: []string{"it's"}}, fallback, marker)
				if err != nil {
					t.Fatalf("CommandWithFallback() error = %v", err)
				}
				cmd.Env = append(cmd.Env, "PATH="+dir+":/usr/bin:/bin")
				out, err := cmd.CombinedOutput()
				if failed := err != nil; failed != wantFailed {
					t.Fatalf("start %d of the container error = %v: %s, want failed %v", i+1, err, out, wantFailed)
				}
			}
			data, err := os.ReadFile(calls)
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.want, strings.Fields(string(data))); diff != "" {
				t.Fatalf("docker run calls wrong result (-want +got) diff = %s", diff)
			}
			_, statErr := os.Stat(marker)
			if tc.wantReason == "" {
				if statErr == nil {
					t.Fatalf("fallback marker exists after %q, want none: %s", tc.name, setupReason(marker))
				}
				return
			}
			if got := setupReason(marker); got != tc.wantReason {
				t.Fatalf("setupReason() = %q, want %q", got, tc.wantReason)
			}
		})
	}
}
