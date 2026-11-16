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

package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/a2aproject/a2a-serve/internal/output"
	"github.com/a2aproject/a2a-serve/internal/testutil"
)

// envInstallProvider makes the test binary install the network provider
// it names with --yes, and exit.
const envInstallProvider = "A2A_TEST_INSTALL_PROVIDER"

func runInstallProvider(spec string) int {
	cfg := &globalConfig{Printer: output.NewPrinter(os.Stdout, output.ModeText)}
	if _, err := installNetworkProvider(context.Background(), cfg, spec, true); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func TestInterruptedInstallOfANetworkProviderLeavesNoCopy(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("interrupts are signals on unix")
	}
	dir := t.TempDir()
	tmp := filepath.Join(dir, "tmp")
	if err := os.Mkdir(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	started := filepath.Join(dir, "started")
	provider := filepath.Join(dir, "provider")
	testutil.WriteExecutable(t, provider, []byte("#!/bin/sh\ntouch "+started+"\ncat >/dev/null\n"))
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), envInstallProvider+"="+provider, "TMPDIR="+tmp, "XDG_DATA_HOME="+filepath.Join(dir, "data"), "GORACE=atexit_sleep_ms=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the install: %v", err)
	}
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the provider didn't start; stderr: %s", stderr.String())
		}
	}

	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("interrupting the install: %v", err)
	}
	var ee *exec.ExitError
	if err := cmd.Wait(); err != nil && !errors.As(err, &ee) {
		t.Fatalf("waiting for the install: %v", err)
	}

	left, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) > 0 {
		t.Fatalf("the interrupted install left %s in TMPDIR; stderr: %s", left[0].Name(), stderr.String())
	}
}
