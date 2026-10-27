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

// fakeDocker puts a docker on PATH which has no images, fails every pull and
// docker info, and builds anything. Tests using it can't run in parallel.
func fakeDocker(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake docker is a shell script")
	}
	dir := t.TempDir()
	script := `#!/bin/sh
case "$1" in
build) cat >/dev/null; exit 0 ;;
*) echo "fake docker: $1 failed" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestEnsureImage(t *testing.T) {
	fakeDocker(t)
	version := a2acli.Version(context.Background())
	tests := []struct {
		name    string
		image   string
		want    string
		wantErr string
	}{
		{name: "profile default", image: opencode{}.Image(), want: LocalImage("opencode", version)},
		{name: "empty means the profile default", want: LocalImage("opencode", version)},
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
}

func TestCheckDockerNotRunning(t *testing.T) {
	fakeDocker(t)
	_, err := CheckDocker(context.Background())
	want := "start Docker or run a2a serve config set --sandbox local, or run a2a serve doctor"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("CheckDocker() error = %v, want %q", err, want)
	}
}
