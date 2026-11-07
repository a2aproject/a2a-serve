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
	"runtime"
	"strings"
	"testing"
	"time"
)

// noImagesDocker has no images, fails every pull and docker info, and builds
// anything.
const noImagesDocker = `case "$1" in
build) cat >/dev/null; exit 0 ;;
*) echo "fake docker: $1 failed" >&2; exit 1 ;;
esac
`

// noImagesNoBuildDocker is noImagesDocker whose builds fail too.
const noImagesNoBuildDocker = `echo "fake docker: $1 failed" >&2
exit 1
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
	tests := []struct {
		name    string
		docker  string
		image   string
		want    string
		wantErr string
	}{
		{name: "profile default", docker: noImagesDocker, image: opencode{}.Image(), want: LocalImage("opencode")},
		{name: "empty means the profile default", docker: noImagesDocker, want: LocalImage("opencode")},
		{name: "user image", docker: noImagesDocker, image: "registry.example/custom:1", wantErr: "docker pull registry.example/custom:1: fake docker: pull failed"},
		{name: "build fails", docker: noImagesNoBuildDocker, wantErr: "fake docker: pull failed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fakeDocker(t, tc.docker)
			var progress strings.Builder
			got, _, err := EnsureImage(context.Background(), tc.image, opencode{}, &progress)
			if strings.Contains(progress.String(), "pull failed") {
				t.Fatalf("EnsureImage() progress = %q, want no pull error", progress.String())
			}
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
}

func TestCheckDockerHostUnreachable(t *testing.T) {
	fakeDocker(t, noImagesDocker)
	t.Setenv("DOCKER_HOST", "unix:///tmp/missing.sock")
	_, err := CheckDocker(context.Background())
	want := "docker is not reachable at unix:///tmp/missing.sock from DOCKER_HOST; fix or unset DOCKER_HOST"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("CheckDocker() error = %v, want %q", err, want)
	}
}

func TestCheckDockerNotRunning(t *testing.T) {
	fakeDocker(t, noImagesDocker)
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
			fakeDocker(t, "exec sleep 60")
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
	fakeDocker(t, "exec sleep 60")
	t.Setenv("DOCKER_HOST", "unix:///tmp/hang.sock")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := checkDocker(ctx, 100*time.Millisecond)
	if !errors.Is(err, ErrDockerNoAnswer) {
		t.Fatalf("checkDocker() error = %v, want %v", err, ErrDockerNoAnswer)
	}
}
