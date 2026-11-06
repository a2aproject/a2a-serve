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
	"bytes"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestEnvironOfProcessThatIsStarting(t *testing.T) {
	t.Parallel()
	const want = "A2A_SERVE_TEST=starting"
	for range 50 {
		cmd := exec.Command("sleep", "60")
		cmd.Env = append(os.Environ(), want)
		if err := cmd.Start(); err != nil {
			t.Fatalf("starting the process: %v", err)
		}

		got, err := environ(cmd.Process.Pid, time.Now().Add(5*time.Second))

		if err := cmd.Process.Kill(); err != nil {
			t.Fatalf("killing process %d: %v", cmd.Process.Pid, err)
		}
		if err := cmd.Wait(); err == nil {
			t.Fatalf("process %d exited on its own, want it killed", cmd.Process.Pid)
		}
		if err != nil {
			t.Fatalf("environ() error = %v", err)
		}
		if !bytes.Contains(got, []byte(want+"\x00")) {
			t.Fatalf("environ() = %q, want it to hold %s", got, want)
		}
	}
}

func TestProcessMarkKillSkipsProcessesWithoutEnvironment(t *testing.T) {
	t.Parallel()
	cmd := exec.Command("env", "-i", "sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the process: %v", err)
	}
	t.Cleanup(func() {
		if err := cmd.Process.Kill(); err != nil {
			t.Errorf("killing process %d: %v", cmd.Process.Pid, err)
		}
		if err := cmd.Wait(); err == nil {
			t.Errorf("process %d exited on its own, want it killed", cmd.Process.Pid)
		}
	})

	if err := NewProcessMark().Kill(); err != nil {
		t.Fatalf("ProcessMark.Kill() error = %v", err)
	}
}
