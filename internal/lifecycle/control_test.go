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

package lifecycle

import (
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
	"github.com/a2aproject/a2a-serve/internal/jsonhttp"
)

// fakeControl answers the control socket of a running agent.
type fakeControl struct {
	pid   int
	mu    sync.Mutex
	calls []string
}

func (f *fakeControl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	f.mu.Unlock()
	jsonhttp.Write(w, http.StatusOK, controlapi.Status{Name: "coder", PID: f.pid})
}

func (f *fakeControl) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func newAgent(t *testing.T) agentdir.Dir {
	t.Helper()
	dir, err := agentdir.Open(t.TempDir())
	if err != nil {
		t.Fatalf("agentdir.Open() error = %v", err)
	}
	if err := dir.SaveConfig(&agentdir.Config{Name: "coder", Harness: "opencode"}); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}
	if err := dir.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	return dir
}

func serveControl(t *testing.T, dir agentdir.Dir, h http.Handler) {
	t.Helper()
	ln, err := net.Listen("unix", dir.SocketPath())
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Serve() error = %v", err)
		}
	}()
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
}

// fakeProcess is a process that a PID file names.
type fakeProcess struct {
	pid  int
	done chan struct{}
}

// exited reports whether the process ended within a few seconds.
func (p *fakeProcess) exited() bool {
	select {
	case <-p.done:
		return true
	case <-time.After(5 * time.Second):
		return false
	}
}

// startingServer simulates a server that holds the lock of dir and has
// written its PID file, but doesn't serve the control socket yet.
func startingServer(t *testing.T, dir agentdir.Dir) *fakeProcess {
	t.Helper()
	release, err := fsutil.TryLock(dir.LockPath())
	if err != nil {
		t.Fatalf("fsutil.TryLock() error = %v", err)
	}
	t.Cleanup(func() {
		if err := release(); err != nil {
			t.Errorf("release() error = %v", err)
		}
	})
	return pidFileProcess(t, dir)
}

// pidFileProcess starts a process that exits on a signal and writes its PID
// to the PID file of dir.
func pidFileProcess(t *testing.T, dir agentdir.Dir) *fakeProcess {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting sleep: %v", err)
	}
	p := &fakeProcess{pid: cmd.Process.Pid, done: make(chan struct{})}
	go func() {
		defer close(p.done)
		var ee *exec.ExitError
		if err := cmd.Wait(); err != nil && !errors.As(err, &ee) {
			t.Errorf("waiting for sleep: %v", err)
		}
	}()
	t.Cleanup(func() {
		if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Errorf("killing sleep: %v", err)
		}
		<-p.done
	})
	if err := os.WriteFile(dir.PIDPath(), []byte(strconv.Itoa(p.pid)), 0o600); err != nil {
		t.Fatalf("writing the PID file: %v", err)
	}
	return p
}
