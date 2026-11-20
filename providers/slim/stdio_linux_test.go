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

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestProviderIgnoresSIGPIPE checks that the kernel ignores SIGPIPE for the
// provider. The SLIM library writes to the node's socket from threads of its
// own, and a SIGPIPE there kills a process that doesn't ignore it.
func TestProviderIgnoresSIGPIPE(t *testing.T) {
	t.Parallel()
	cmd := exec.Command(providerBin, "serve")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("Cmd.StdinPipe() error = %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Cmd.Start() error = %v", err)
	}
	t.Cleanup(func() {
		if err := stdin.Close(); err != nil {
			t.Errorf("closing the provider's stdin: %v", err)
		}
		if err := cmd.Wait(); err != nil {
			t.Errorf("Cmd.Wait() error = %v", err)
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		ignored, err := ignoresSignal(cmd.Process.Pid, unix.SIGPIPE)
		if err != nil {
			t.Fatalf("reading the provider's signals: %v", err)
		}
		if ignored {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the provider doesn't ignore SIGPIPE")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func ignoresSignal(pid int, sig unix.Signal) (bool, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return false, err
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		mask, ok := strings.CutPrefix(line, "SigIgn:")
		if !ok {
			continue
		}
		bits, err := strconv.ParseUint(strings.TrimSpace(mask), 16, 64)
		if err != nil {
			return false, err
		}
		return bits&(1<<(uint(sig)-1)) != 0, nil
	}
	return false, fmt.Errorf("no SigIgn in /proc/%d/status", pid)
}
