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

// Package sandboxtest fakes the docker CLI for tests.
package sandboxtest

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// NoImagesDocker is a [FakeDocker] script that has no images, fails every
// pull and docker info, and builds anything.
const NoImagesDocker = `case "$1" in
build) cat >/dev/null; exit 0 ;;
*) echo "fake docker: $1 failed" >&2; exit 1 ;;
esac
`

// Scripts of a [FakeDocker] docker run that fails as the egress check
// does when the runtime refuses the TUN device or NET_ADMIN, when the
// launcher can't set up the network, when the daemon fails, and when the
// launcher crashes with output that names the device.
const (
	TUNMissing       = `echo 'docker: Error response from daemon: error gathering device information while adding custom device "/dev/net/tun": no such file or directory' >&2; exit 127`
	NetAdminRefused  = `echo 'Error: requested capability NET_ADMIN is not allowed' >&2; exit 125`
	LauncherNoTUN    = `echo "a2a-init: setting up the sandbox's network: no tun" >&2; exit 121`
	DaemonError      = `echo 'docker: Error response from daemon: failed to create task for container: EOF' >&2; exit 125`
	LauncherCrashTUN = `echo 'panic: opening /dev/net/tun' >&2; exit 2`
)

// FakeDocker puts a docker on PATH which runs script and logs its arguments,
// one call per line, to the returned file. Tests using it can't run in
// parallel.
func FakeDocker(t *testing.T, script string) string {
	t.Helper()
	log := filepath.Join(t.TempDir(), "docker.log")
	PutOnPath(t, "docker", `echo "$*" >>`+log+"\n"+script)
	return log
}

// PutOnPath puts a shell script named name first on PATH.
func PutOnPath(t *testing.T, name, script string) {
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
