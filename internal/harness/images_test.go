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
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-serve/internal/a2acli"
)

// noImagesDocker has no images, fails every pull and docker info, and builds
// anything.
const noImagesDocker = `case "$1" in
build) cat >/dev/null; exit 0 ;;
*) echo "fake docker: $1 failed" >&2; exit 1 ;;
esac
`

// fakeDocker puts a docker on PATH which runs script and logs its arguments,
// one call per line, to the returned file. Tests using it can't run in
// parallel.
func fakeDocker(t *testing.T, script string) string {
	t.Helper()
	log := filepath.Join(t.TempDir(), "docker.log")
	putOnPath(t, "docker", `echo "$*" >>`+log+"\n"+script)
	return log
}

// putOnPath puts a shell script named name first on PATH.
func putOnPath(t *testing.T, name, script string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake commands are shell scripts")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestEnsureImage(t *testing.T) {
	log := fakeDocker(t, noImagesDocker)
	putOnPath(t, a2acli.Binary, `echo '{"version":"0.3.1"}'`)
	tests := []struct {
		name    string
		image   string
		want    string
		wantErr string
	}{
		{name: "profile default", image: opencode{}.Image(), want: LocalImage("opencode")},
		{name: "empty means the profile default", want: LocalImage("opencode")},
		{name: "user image", image: "registry.example/custom:1", wantErr: "docker pull registry.example/custom:1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := EnsureImage(context.Background(), tc.image, opencode{}, io.Discard)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("EnsureImage() error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("EnsureImage() error = %v", err)
			}
			if got != tc.want {
				t.Fatalf("EnsureImage() = %q, want %q", got, tc.want)
			}
		})
	}
	calls, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("os.ReadFile() error = %v", err)
	}
	if want := "--build-arg A2A_CLI_VERSION=" + a2acli.PinnedVersion + " "; !strings.Contains(string(calls), want) {
		t.Fatalf("EnsureImage() ran docker with:\n%s\nwant a build with %q", calls, want)
	}
}

func TestCheckDockerNotRunning(t *testing.T) {
	fakeDocker(t, noImagesDocker)
	_, err := CheckDocker(context.Background())
	want := "start Docker or run a2a serve config set --sandbox local, or run a2a serve doctor"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("CheckDocker() error = %v, want %q", err, want)
	}
}
