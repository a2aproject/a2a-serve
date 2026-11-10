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

//go:build unix

package testutil

import (
	"os"
	"os/exec"
	"testing"
)

func TestProcessRunning(t *testing.T) {
	t.Parallel()
	if !processRunning(os.Getpid()) {
		t.Errorf("processRunning(%d) = false for the test process, want true", os.Getpid())
	}
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("cmd.Run() error = %v", err)
	}
	if processRunning(cmd.Process.Pid) {
		t.Errorf("processRunning(%d) = true for a finished process, want false", cmd.Process.Pid)
	}
}
