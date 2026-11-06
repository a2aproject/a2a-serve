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

package agentsrv

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/a2aproject/a2a-serve/internal/auth"
)

func TestLocalAgentProcessesDontOutliveAgent(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" {
		t.Skip("processes are found through /proc")
	}
	ts := startServer(t, serverOpts{})
	c := ts.client(ts.pair("me", auth.RoleOwner))
	pidFile := filepath.Join(t.TempDir(), "pid")

	task := send(t, c, userText("orphan "+pidFile, "", ""))
	if task.Status.State != a2a.TaskStateFailed {
		t.Fatalf("task state = %s, want failed", task.Status.State)
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("reading the PID of the orphan: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("parsing the PID of the orphan %q: %v", data, err)
	}
	t.Cleanup(func() {
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			t.Errorf("killing the orphan: %v", err)
		}
	})

	deadline := time.Now().Add(10 * time.Second)
	for alive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("process %d that the agent started outlived it", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// alive reports whether the process runs, which a zombie doesn't.
func alive(pid int) bool {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	_, rest, ok := strings.Cut(string(data), ") ")
	return ok && !strings.HasPrefix(rest, "Z")
}
