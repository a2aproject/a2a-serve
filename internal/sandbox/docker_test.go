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
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-serve/internal/sandbox/sandboxtest"
	"github.com/google/go-cmp/cmp"
)

func TestDockerRunnerStopSurvivesCancelledContext(t *testing.T) {
	log := sandboxtest.FakeDocker(t, "exit 0")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	r := &DockerRunner{Name: "a2a-coder"}
	if err := r.Stop(ctx); err != nil {
		t.Fatalf("DockerRunner.Stop() error = %v", err)
	}

	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("os.ReadFile() error = %v", err)
	}
	if diff := cmp.Diff("rm -f a2a-coder\n", string(data)); diff != "" {
		t.Fatalf("DockerRunner.Stop() wrong docker calls (-want +got) diff = %s", diff)
	}
}

func TestDockerRunnerStopRemovesEnvFiles(t *testing.T) {
	sandboxtest.FakeDocker(t, "exit 0")
	envDir := t.TempDir()
	r := &DockerRunner{Name: "a2a-coder", Image: "img", EnvDir: envDir, Env: []string{"KEY=secret"}}
	if _, err := r.Command(context.Background(), Command{Path: "opencode"}); err != nil {
		t.Fatalf("DockerRunner.Command() error = %v", err)
	}
	if got := dirNames(t, envDir); len(got) != 1 {
		t.Fatalf("env directory has %v, want one env file", got)
	}

	if err := r.Stop(context.Background()); err != nil {
		t.Fatalf("DockerRunner.Stop() error = %v", err)
	}

	if diff := cmp.Diff([]string(nil), dirNames(t, envDir)); diff != "" {
		t.Fatalf("DockerRunner.Stop() left env files (-want +got) diff = %s", diff)
	}
}

func TestPruneEnvFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, name := range []string{"a2a-env-1", "a2a-env-2", "other"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("KEY=secret\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	n, err := PruneEnvFiles(dir)
	if err != nil {
		t.Fatalf("PruneEnvFiles() error = %v", err)
	}
	if n != 2 {
		t.Fatalf("PruneEnvFiles() = %d, want 2", n)
	}
	if diff := cmp.Diff([]string{"other"}, dirNames(t, dir)); diff != "" {
		t.Fatalf("PruneEnvFiles() left wrong files (-want +got) diff = %s", diff)
	}
}

func TestPruneEnvFilesWithoutDirectory(t *testing.T) {
	t.Parallel()
	n, err := PruneEnvFiles(filepath.Join(t.TempDir(), "missing"))
	if err != nil {
		t.Fatalf("PruneEnvFiles() error = %v", err)
	}
	if n != 0 {
		t.Fatalf("PruneEnvFiles() = %d, want 0", n)
	}
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("os.ReadDir() error = %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestPruneContainers(t *testing.T) {
	log := sandboxtest.FakeDocker(t, `case "$1" in
ps) printf 'abc\ndef\n' ;;
esac
`)

	n, err := PruneContainers(context.Background(), map[string]string{"a2a.dir": "/work/agent", "a2a.agent": "coder"})
	if err != nil {
		t.Fatalf("PruneContainers() error = %v", err)
	}
	if n != 2 {
		t.Fatalf("PruneContainers() = %d, want 2", n)
	}

	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("os.ReadFile() error = %v", err)
	}
	want := []string{
		"ps -aq --filter label=a2a.agent=coder --filter label=a2a.dir=/work/agent",
		"rm -f -v abc def",
	}
	if diff := cmp.Diff(want, strings.Split(strings.TrimSpace(string(data)), "\n")); diff != "" {
		t.Fatalf("PruneContainers() wrong docker calls (-want +got) diff = %s", diff)
	}
}

func TestPruneContainersWithoutMatches(t *testing.T) {
	log := sandboxtest.FakeDocker(t, "exit 0")

	n, err := PruneContainers(context.Background(), map[string]string{"a2a.dir": "/work/agent"})
	if err != nil {
		t.Fatalf("PruneContainers() error = %v", err)
	}
	if n != 0 {
		t.Fatalf("PruneContainers() = %d, want 0", n)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("os.ReadFile() error = %v", err)
	}
	if diff := cmp.Diff("ps -aq --filter label=a2a.dir=/work/agent\n", string(data)); diff != "" {
		t.Fatalf("PruneContainers() wrong docker calls (-want +got) diff = %s", diff)
	}
}

func TestPruneContainersWithoutAnswer(t *testing.T) {
	sandboxtest.FakeDocker(t, "exec sleep 60")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := pruneContainers(ctx, map[string]string{"a2a.dir": "/work/agent"}, 100*time.Millisecond)
	if !errors.Is(err, ErrDockerNoAnswer) {
		t.Fatalf("pruneContainers() error = %v, want %v", err, ErrDockerNoAnswer)
	}
}

func TestDockerRunnerProbe(t *testing.T) {
	flag := filepath.Join(t.TempDir(), "failed-once")
	tests := []struct {
		name      string
		run       string
		wantRuns  int
		wantSetup string
		wantErr   string
	}{
		{name: "setup works", run: "exit 0", wantRuns: 1},
		{name: "launcher can't set up the network", run: sandboxtest.LauncherNoTUN, wantRuns: 1, wantSetup: "no tun"},
		{name: "runtime refuses the TUN device", run: sandboxtest.TUNMissing, wantRuns: 1, wantSetup: `docker: Error response from daemon: error gathering device information while adding custom device "/dev/net/tun": no such file or directory`},
		{name: "runtime refuses NET_ADMIN", run: sandboxtest.NetAdminRefused, wantRuns: 1, wantSetup: "Error: requested capability NET_ADMIN is not allowed"},
		{name: "daemon error", run: sandboxtest.DaemonError, wantRuns: probeAttempts, wantErr: "failed to create task for container: EOF"},
		{name: "launcher crash naming the device", run: sandboxtest.LauncherCrashTUN, wantRuns: probeAttempts, wantErr: "panic: opening /dev/net/tun"},
		{name: "daemon error once", run: `[ -e ` + flag + ` ] && exit 0; touch ` + flag + `; ` + sandboxtest.DaemonError, wantRuns: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			log := sandboxtest.FakeDocker(t, `[ "$1" = run ] || exit 0`+"\n"+tc.run)
			r := &DockerRunner{Name: "a2a-coder-1", Image: "img:1", Egress: &DockerEgress{Socket: "/s/egress.sock", Launcher: "/bin/a2a-serve"}}
			err := r.Probe(context.Background())
			data, rerr := os.ReadFile(log)
			if rerr != nil {
				t.Fatal(rerr)
			}
			if got := strings.Count(string(data), "run --name a2a-coder-1-check "); got != tc.wantRuns {
				t.Fatalf("DockerRunner.Probe() ran %d checks, want %d", got, tc.wantRuns)
			}
			setupErr, isSetup := errors.AsType[*EgressSetupError](err)
			switch {
			case tc.wantSetup != "":
				if !isSetup || setupErr.Error() != tc.wantSetup {
					t.Fatalf("DockerRunner.Probe() error = %v, want an egress setup error %q", err, tc.wantSetup)
				}
			case tc.wantErr != "":
				if err == nil || isSetup || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("DockerRunner.Probe() error = %v, want a non-setup error containing %q", err, tc.wantErr)
				}
			case err != nil:
				t.Fatalf("DockerRunner.Probe() error = %v, want nil", err)
			}
		})
	}
}

func TestDockerRunnerCommand(t *testing.T) {
	t.Parallel()
	envDir := t.TempDir()
	r := &DockerRunner{
		Name:   "a2a-coder-123",
		Image:  "img:1",
		EnvDir: envDir,
		Mounts: []Mount{
			{Src: "/src/app", Dst: "/src/app"},
			{Src: "/home/u/docs", Dst: "/docs", ReadOnly: true},
		},
		Home: "/src/app/.a2a/state/home",
		Env:  []string{"OPENAI_API_KEY=sk-secret", "DOCKER_HOST=tcp://evil:2375", "HOME=/root"},
	}
	cmd, err := r.Command(context.Background(), Command{Path: "opencode", Args: []string{"acp"}, Dir: "/src/app", Env: []string{"X=1"}})
	if err != nil {
		t.Fatalf("Command() error = %v", err)
	}
	envFile := argAfter(cmd.Args, "--env-file")
	if got := filepath.Dir(envFile); got != envDir {
		t.Fatalf("env file directory = %q, want %q", got, envDir)
	}
	t.Cleanup(func() {
		if err := os.Remove(envFile); err != nil {
			t.Errorf("removing %s error = %v", envFile, err)
		}
	})
	want := []string{
		"sh", "-c", startScript, "a2a-coder-123",
		"--rm", "-i", "--init",
		"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--memory", "4g", "--cpus", "2", "--pids-limit", "512",
		"--add-host", "host.docker.internal:host-gateway",
		"-v", "/src/app:/src/app",
		"-v", "/home/u/docs:/docs:ro",
		"-v", "/src/app/.a2a/state/home:/home/agent",
		"--env-file", envFile,
		"-e", "HOME=/home/agent",
		"-w", "/src/app",
		"img:1", "opencode", "acp",
	}
	if diff := cmp.Diff(want, cmd.Args); diff != "" {
		t.Fatalf("Command() wrong result (-want +got) diff = %s", diff)
	}
	if diff := cmp.Diff(os.Environ(), cmd.Env); diff != "" {
		t.Fatalf("Command() wrong docker CLI environment (-want +got) diff = %s", diff)
	}
	data, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("reading the env file error = %v", err)
	}
	wantEnv := "OPENAI_API_KEY=sk-secret\nDOCKER_HOST=tcp://evil:2375\nHOME=/root\nX=1\n"
	if diff := cmp.Diff(wantEnv, string(data)); diff != "" {
		t.Fatalf("Command() wrong env file (-want +got) diff = %s", diff)
	}
	fi, err := os.Stat(envFile)
	if err != nil {
		t.Fatalf("os.Stat() error = %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("env file mode = %v, want %v", got, os.FileMode(0o600))
	}
}

func TestDockerRunnerCommandRequiresEnvDirForEnv(t *testing.T) {
	t.Parallel()
	r := &DockerRunner{Name: "n", Image: "img", Env: []string{"KEY=secret"}}
	if _, err := r.Command(context.Background(), Command{Path: "opencode"}); err == nil {
		t.Fatalf("DockerRunner.Command() error = nil, want an error")
	}
}

func TestDockerRunnerCommandRejectsMultilineEnv(t *testing.T) {
	t.Parallel()
	r := &DockerRunner{Name: "n", Image: "img", EnvDir: t.TempDir(), Env: []string{"KEY=line1\nline2"}}
	if _, err := r.Command(context.Background(), Command{Path: "opencode"}); err == nil {
		t.Fatalf("Command() error = nil, want an error")
	}
}

func argAfter(args []string, flag string) string {
	i := slices.Index(args, flag)
	if i < 0 || i+1 >= len(args) {
		return ""
	}
	return args[i+1]
}

func TestDockerRunnerHidden(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mounts []Mount
		hidden []string
		want   []string
	}{
		{
			name:   "inside the workspace",
			mounts: []Mount{{Src: "/src/app", Dst: "/src/app"}},
			hidden: []string{"/src/app/.a2a"},
			want:   []string{"type=tmpfs,dst=/src/app/.a2a,readonly"},
		},
		{
			name:   "under a mount with another destination",
			mounts: []Mount{{Src: "/src/app", Dst: "/src/app"}, {Src: "/home/u", Dst: "/u", ReadOnly: true}},
			hidden: []string{"/home/u/proj/.a2a"},
			want:   []string{"type=tmpfs,dst=/u/proj/.a2a,readonly"},
		},
		{
			name:   "the mount itself",
			mounts: []Mount{{Src: "/src/app/.a2a", Dst: "/cfg"}},
			hidden: []string{"/src/app/.a2a"},
			want:   []string{"type=tmpfs,dst=/cfg,readonly"},
		},
		{
			name:   "outside every mount",
			mounts: []Mount{{Src: "/src/app", Dst: "/src/app"}},
			hidden: []string{"/src/application/.a2a", "/other/.a2a"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := &DockerRunner{Name: "n", Image: "img", Mounts: tc.mounts, Hidden: tc.hidden}
			cmd, err := r.Command(context.Background(), Command{Path: "opencode"})
			if err != nil {
				t.Fatalf("Command() error = %v", err)
			}
			var got []string
			for i, a := range cmd.Args {
				if a == "--mount" {
					got = append(got, cmd.Args[i+1])
				}
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("Command() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestDockerRunnerWorkspaceSrc(t *testing.T) {
	t.Parallel()
	r := &DockerRunner{
		Name: "n", Image: "img", WorkspaceSrc: "/state/workspaces/u/own",
		Mounts: []Mount{{Src: "/src/app", Dst: "/src/app"}, {Src: "/docs", Dst: "/docs", ReadOnly: true}},
		Hidden: []string{"/src/app/.a2a"},
	}
	cmd, err := r.Command(context.Background(), Command{Path: "opencode"})
	if err != nil {
		t.Fatalf("Command() error = %v", err)
	}
	var got []string
	for i, a := range cmd.Args {
		if a == "-v" || a == "--mount" {
			got = append(got, cmd.Args[i+1])
		}
	}
	want := []string{"/state/workspaces/u/own:/src/app", "/docs:/docs:ro", "type=tmpfs,dst=/src/app/.a2a,readonly"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("Command() wrong result (-want +got) diff = %s", diff)
	}
}

func TestDockerRunnerHiddenThroughSymlinks(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	real := filepath.Join(tmp, "real")
	if err := os.MkdirAll(filepath.Join(real, ".a2a"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(tmp, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mounts []Mount
		hidden []string
		want   []string
	}{
		{
			name:   "root through a symlink, mount of the real path",
			mounts: []Mount{{Src: link, Dst: link}, {Src: real, Dst: "/real"}},
			hidden: []string{filepath.Join(link, ".a2a")},
			want:   []string{"type=tmpfs,dst=" + filepath.Join(link, ".a2a") + ",readonly", "type=tmpfs,dst=/real/.a2a,readonly"},
		},
		{
			name:   "real root, mount through a symlink",
			mounts: []Mount{{Src: link, Dst: "/w"}},
			hidden: []string{filepath.Join(real, ".a2a")},
			want:   []string{"type=tmpfs,dst=/w/.a2a,readonly"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := &DockerRunner{Name: "n", Image: "img", Mounts: tc.mounts, Hidden: tc.hidden}
			cmd, err := r.Command(context.Background(), Command{Path: "opencode"})
			if err != nil {
				t.Fatalf("Command() error = %v", err)
			}
			var got []string
			for i, a := range cmd.Args {
				if a == "--mount" {
					got = append(got, cmd.Args[i+1])
				}
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("Command() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestDockerRunnerCommandWithEgress(t *testing.T) {
	t.Parallel()
	for _, rootless := range []bool{false, true} {
		t.Run(fmt.Sprintf("rootless=%v", rootless), func(t *testing.T) {
			t.Parallel()
			r := &DockerRunner{
				Name:     "a2a-coder-123",
				Image:    "img:1",
				Mounts:   []Mount{{Src: "/src/app", Dst: "/src/app"}},
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
		Mounts: []Mount{{Src: "/src/app", Dst: "/src/app"}},
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

func TestDockerSandboxChecksBoundary(t *testing.T) {
	tests := []struct {
		name string
		// checks are what the successive checks do.
		checks []string
		// wantErr are the errors of the successive starts; "setup: " marks
		// an error that wraps ErrNoBoundary.
		wantErr []string
		want    []string
	}{
		{name: "check passes", checks: []string{"exit 0", "exit 0"}, wantErr: []string{"", ""}, want: []string{"check", "run", "check", "run"}},
		{name: "launcher can't set up the network", checks: []string{sandboxtest.LauncherNoTUN}, wantErr: []string{"setup: no tun"}, want: []string{"check"}},
		{name: "runtime refuses the TUN device", checks: []string{sandboxtest.TUNMissing}, wantErr: []string{`setup: docker: Error response from daemon: error gathering device information while adding custom device "/dev/net/tun": no such file or directory`}, want: []string{"check"}},
		{name: "daemon error", checks: []string{sandboxtest.DaemonError, "exit 0"}, wantErr: []string{"failed to create task for container: EOF", ""}, want: []string{"check", "check", "run"}},
		{name: "launcher crash naming the device", checks: []string{sandboxtest.LauncherCrashTUN, "exit 0"}, wantErr: []string{"panic: opening /dev/net/tun", ""}, want: []string{"check", "check", "run"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			calls := filepath.Join(t.TempDir(), "calls")
			var checks strings.Builder
			for i, c := range tc.checks {
				fmt.Fprintf(&checks, "%d) %s;;\n", i+1, c)
			}
			sandboxtest.FakeDocker(t, `[ "$1" = run ] || exit 0
case "$*" in *--check*) echo check >>`+calls+`
case $(grep -c check `+calls+`) in
`+checks.String()+`esac;;
*) echo run >>`+calls+`;; esac
`)
			r := &DockerRunner{Name: "a2a-coder-1", Image: "img:1", Egress: &DockerEgress{Socket: "/s/egress.sock", Launcher: "/bin/a2a-serve"}}
			sb, err := r.Sandbox(Command{Path: "agent", Args: []string{"it's"}}, true, Options{})
			if err != nil {
				t.Fatalf("DockerRunner.Sandbox() error = %v", err)
			}
			t.Cleanup(func() {
				if err := sb.Close(context.Background()); err != nil {
					t.Errorf("Sandbox.Close() error = %v", err)
				}
			})
			for i, wantErr := range tc.wantErr {
				rwc, err := sb.Start(context.Background())
				if err == nil {
					drain(t, rwc)
				}
				wantSetup, setup := strings.CutPrefix(wantErr, "setup: ")
				switch {
				case wantErr == "" && err != nil:
					t.Fatalf("start %d Sandbox.Start() error = %v, want nil", i+1, err)
				case setup && (!errors.Is(err, ErrNoBoundary) || err.Error() != wantSetup):
					t.Fatalf("start %d Sandbox.Start() error = %v, want %q wrapping %v", i+1, err, wantSetup, ErrNoBoundary)
				case wantErr != "" && !setup && (err == nil || errors.Is(err, ErrNoBoundary) || !strings.Contains(err.Error(), wantErr)):
					t.Fatalf("start %d Sandbox.Start() error = %v, want an error containing %q", i+1, err, wantErr)
				}
			}
			data, err := os.ReadFile(calls)
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.want, strings.Fields(string(data))); diff != "" {
				t.Fatalf("docker run calls wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestDockerSandboxRunsCommand(t *testing.T) {
	sandboxtest.FakeDocker(t, `case "$1" in run) shift; printf '%s\n' "$@" ;; esac`)
	checks := 0
	r := &DockerRunner{Name: "a2a-coder-1", Image: "img:1", EnvDir: t.TempDir(), Env: []string{"KEY=secret"}}
	sb, err := r.Sandbox(Command{Path: "agent", Args: []string{"acp"}, Dir: "/w"}, true, Options{Check: func() error {
		checks++
		if checks > 1 {
			return errors.New("refused")
		}
		return nil
	}})
	if err != nil {
		t.Fatalf("DockerRunner.Sandbox() error = %v", err)
	}
	rwc, err := sb.Start(context.Background())
	if err != nil {
		t.Fatalf("Sandbox.Start() error = %v", err)
	}
	got := strings.Fields(drain(t, rwc))
	if !slices.Equal(got[len(got)-3:], []string{"img:1", "agent", "acp"}) || !slices.Contains(got, "--env-file") {
		t.Fatalf("Sandbox.Start() ran docker run %q, want the command with an env file", got)
	}
	if _, err := sb.Start(context.Background()); err == nil || err.Error() != "refused" {
		t.Fatalf("Sandbox.Start() error = %v, want the error of the check", err)
	}
	envFile := argAfter(got, "--env-file")
	if err := sb.Close(context.Background()); err != nil {
		t.Fatalf("Sandbox.Close() error = %v", err)
	}
	if _, err := os.Stat(envFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("env file after Sandbox.Close() error = %v, want %v", err, os.ErrNotExist)
	}
}

func TestLocalSandbox(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	refuse := errors.New("refused")
	var checkErr error
	sb := NewLocal(Command{Path: "sh", Args: []string{"-c", `pwd; echo "$X"; cat`}, Dir: dir, Env: []string{"X=1"}}, Options{Check: func() error { return checkErr }})
	rwc, err := sb.Start(context.Background())
	if err != nil {
		t.Fatalf("Sandbox.Start() error = %v", err)
	}
	if _, err := io.WriteString(rwc, "in\n"); err != nil {
		t.Fatalf("writing to the process error = %v", err)
	}
	buf := make([]byte, 64)
	var out strings.Builder
	for !strings.HasSuffix(out.String(), "in\n") {
		n, err := rwc.Read(buf)
		if err != nil {
			t.Fatalf("reading the process error = %v, read %q", err, out.String())
		}
		out.Write(buf[:n])
	}
	if err := rwc.Close(); err != nil {
		t.Fatalf("closing the process error = %v", err)
	}
	if want := dir + "\n1\nin\n"; out.String() != want {
		t.Fatalf("Sandbox.Start() output = %q, want %q", out.String(), want)
	}
	checkErr = refuse
	if _, err := sb.Start(context.Background()); !errors.Is(err, refuse) {
		t.Fatalf("Sandbox.Start() error = %v, want %v", err, refuse)
	}
}

// drain reads rwc until the process exits, closes it and returns what it read.
func drain(t *testing.T, rwc io.ReadWriteCloser) string {
	t.Helper()
	data, err := io.ReadAll(rwc)
	if err != nil {
		t.Fatalf("reading the process error = %v", err)
	}
	if err := rwc.Close(); err != nil {
		t.Fatalf("closing the process error = %v", err)
	}
	return string(data)
}

func TestCheckDockerHostUnreachable(t *testing.T) {
	sandboxtest.FakeDocker(t, sandboxtest.NoImagesDocker)
	t.Setenv("DOCKER_HOST", "unix:///tmp/missing.sock")
	_, err := CheckDocker(context.Background())
	want := "docker is not reachable at unix:///tmp/missing.sock from DOCKER_HOST; fix or unset DOCKER_HOST"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("CheckDocker() error = %v, want %q", err, want)
	}
}

func TestCheckDockerNotRunning(t *testing.T) {
	sandboxtest.FakeDocker(t, sandboxtest.NoImagesDocker)
	t.Setenv("DOCKER_HOST", "")
	_, err := CheckDocker(context.Background())
	want := "start Docker or run a2a-serve config set --sandbox local, or run a2a-serve doctor"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("CheckDocker() error = %v, want %q", err, want)
	}
}

func TestDockerQueryWithoutAnswer(t *testing.T) {
	tests := []struct {
		name       string
		dockerHost string
		want       string
	}{
		{name: "DOCKER_HOST", dockerHost: "unix:///tmp/hang.sock", want: "docker does not answer within 100ms at unix:///tmp/hang.sock from DOCKER_HOST; fix or unset DOCKER_HOST"},
		{name: "default socket", want: "docker does not answer within 100ms; restart Docker or run a2a-serve config set --sandbox local"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sandboxtest.FakeDocker(t, "exec sleep 60")
			t.Setenv("DOCKER_HOST", tc.dockerHost)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			_, err := dockerQuery(ctx, 100*time.Millisecond, "info")
			if !errors.Is(err, ErrDockerNoAnswer) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("dockerQuery() error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestCheckDockerWithoutAnswer(t *testing.T) {
	sandboxtest.FakeDocker(t, "exec sleep 60")
	t.Setenv("DOCKER_HOST", "unix:///tmp/hang.sock")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := checkDocker(ctx, 100*time.Millisecond)
	if !errors.Is(err, ErrDockerNoAnswer) {
		t.Fatalf("checkDocker() error = %v, want %v", err, ErrDockerNoAnswer)
	}
}

func TestDockerQueryCancelled(t *testing.T) {
	sandboxtest.FakeDocker(t, "exec sleep 60")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := dockerQuery(ctx, time.Minute, "info")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("dockerQuery() error = %v, want %v", err, context.Canceled)
	}
}
