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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func TestDockerRunnerStopSurvivesCancelledContext(t *testing.T) {
	log := fakeDocker(t, "exit 0")
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
	fakeDocker(t, "exit 0")
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
	log := fakeDocker(t, `case "$1" in
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
	log := fakeDocker(t, "exit 0")

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
	fakeDocker(t, "exec sleep 60")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := pruneContainers(ctx, map[string]string{"a2a.dir": "/work/agent"}, 100*time.Millisecond)
	if !errors.Is(err, ErrDockerNoAnswer) {
		t.Fatalf("pruneContainers() error = %v, want %v", err, ErrDockerNoAnswer)
	}
}

const (
	tunMissing       = `echo 'docker: Error response from daemon: error gathering device information while adding custom device "/dev/net/tun": no such file or directory' >&2; exit 127`
	netAdminRefused  = `echo 'Error: requested capability NET_ADMIN is not allowed' >&2; exit 125`
	launcherNoTUN    = `echo "a2a-init: setting up the sandbox's network: no tun" >&2; exit 121`
	daemonError      = `echo 'docker: Error response from daemon: failed to create task for container: EOF' >&2; exit 125`
	launcherCrashTUN = `echo 'panic: opening /dev/net/tun' >&2; exit 2`
)

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
		{name: "launcher can't set up the network", run: launcherNoTUN, wantRuns: 1, wantSetup: "no tun"},
		{name: "runtime refuses the TUN device", run: tunMissing, wantRuns: 1, wantSetup: `docker: Error response from daemon: error gathering device information while adding custom device "/dev/net/tun": no such file or directory`},
		{name: "runtime refuses NET_ADMIN", run: netAdminRefused, wantRuns: 1, wantSetup: "Error: requested capability NET_ADMIN is not allowed"},
		{name: "daemon error", run: daemonError, wantRuns: probeAttempts, wantErr: "failed to create task for container: EOF"},
		{name: "launcher crash naming the device", run: launcherCrashTUN, wantRuns: probeAttempts, wantErr: "panic: opening /dev/net/tun"},
		{name: "daemon error once", run: `[ -e ` + flag + ` ] && exit 0; touch ` + flag + `; ` + daemonError, wantRuns: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			log := fakeDocker(t, `[ "$1" = run ] || exit 0`+"\n"+tc.run)
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
