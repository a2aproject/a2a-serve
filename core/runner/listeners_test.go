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

package runner

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func TestWriteCredentialHelper(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("the helper is a POSIX shell script")
	}
	home := t.TempDir()
	env, err := writeCredentialHelper(home, "host.docker.internal", "k3y")
	if err != nil {
		t.Fatalf("writeCredentialHelper() error = %v", err)
	}
	if want := "A2ACLI_CREDENTIAL_HELPER=host.docker.internal=/home/agent/" + credentialHelperPath; env != want {
		t.Fatalf("writeCredentialHelper() = %q, want %q", env, want)
	}
	out, err := runHelper(filepath.Join(home, filepath.FromSlash(credentialHelperPath)), `{"uri":"http://host.docker.internal:4000/k3/coder"}`)
	if err != nil {
		t.Fatalf("credential helper error = %v", err)
	}
	var got struct {
		Headers map[string][]string `json:"headers"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("json.Unmarshal(%q) error = %v", out, err)
	}
	if diff := cmp.Diff(map[string][]string{"Authorization": {"Bearer k3y"}}, got.Headers); diff != "" {
		t.Fatalf("credential helper wrong result (-want +got) diff = %s", diff)
	}
}

// runHelper runs a just-written executable. A child forked meanwhile by a
// parallel test holds its write descriptor until exec, failing the run with
// ETXTBSY (golang.org/issue/22315), so the run is retried.
func runHelper(path, stdin string) ([]byte, error) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		cmd := exec.Command(path, "get")
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.Output()
		if !errors.Is(err, syscall.ETXTBSY) || time.Now().After(deadline) {
			return out, err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWriteCredentialHelperRefusesLinkedFolder(t *testing.T) {
	t.Parallel()
	home, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(home, ".local")); err != nil {
		t.Fatal(err)
	}
	if _, err := writeCredentialHelper(home, "host.docker.internal", "k3y"); err == nil {
		t.Fatalf("writeCredentialHelper() through a linked folder error = nil, want an error")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) > 0 {
		t.Fatalf("writeCredentialHelper() wrote %v outside the home, error = %v", entries, err)
	}
}
