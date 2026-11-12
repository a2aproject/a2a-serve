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

package sandbox

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
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

		// Until the kernel switches the process to the new program, it can
		// show the environment of its parent.
		deadline := time.Now().Add(5 * time.Second)
		var got []byte
		var err error
		for {
			got, err = environ("/proc", cmd.Process.Pid, deadline)
			if err != nil || bytes.Contains(got, []byte(want+"\x00")) || time.Now().After(deadline) {
				break
			}
			time.Sleep(time.Millisecond)
		}

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

// fakeProc is a /proc tree with processes that start a program.
type fakeProc struct {
	t   *testing.T
	dir string

	mu       sync.Mutex
	signaled []int
}

func newFakeProc(t *testing.T) *fakeProc {
	return &fakeProc{t: t, dir: t.TempDir()}
}

// starting adds a process that shows an empty environment, as one in exec
// does before the kernel sets the address of its environment.
func (p *fakeProc) starting(pid int) {
	p.t.Helper()
	if err := os.Mkdir(p.pidDir(pid), 0o755); err != nil {
		p.t.Fatal(err)
	}
	p.write(pid, "environ", "")
	p.writeStat(pid, 0)
}

// started shows the environment of a process, in the order of the kernel:
// the environment first, then its address in stat.
func (p *fakeProc) started(pid int, env ...string) {
	p.t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.write(pid, "environ", strings.Join(env, "\x00")+"\x00")
	p.writeStat(pid, 1)
}

func (p *fakeProc) writeStat(pid, envEnd int) {
	p.t.Helper()
	// Fields from the state (field 3) to env_end (field 51).
	fields := make([]string, 49)
	for i := range fields {
		fields[i] = "0"
	}
	fields[0] = "S"
	fields[48] = strconv.Itoa(envEnd)
	p.write(pid, "stat", strconv.Itoa(pid)+" (sh) "+strings.Join(fields, " ")+"\n")
}

func (p *fakeProc) write(pid int, name, data string) {
	p.t.Helper()
	tmp := filepath.Join(p.pidDir(pid), name+".tmp")
	if err := os.WriteFile(tmp, []byte(data), 0o644); err != nil {
		p.t.Error(err)
		return
	}
	if err := os.Rename(tmp, filepath.Join(p.pidDir(pid), name)); err != nil {
		p.t.Error(err)
	}
}

func (p *fakeProc) pidDir(pid int) string { return filepath.Join(p.dir, strconv.Itoa(pid)) }

func (p *fakeProc) signal(pid int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.signaled = append(p.signaled, pid)
	return os.RemoveAll(p.pidDir(pid))
}

func (p *fakeProc) signaledPIDs() []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.signaled
}

func TestMarkScanRechecksProcessesThatStayInExec(t *testing.T) {
	t.Parallel()
	const pid = 4194305
	mark := NewProcessMark()
	tests := []struct {
		name         string
		env          []string
		wantSignaled []int
	}{
		{
			name: "unrelated process",
			env:  []string{"HOME=/home/user"},
		},
		{
			name:         "marked process",
			env:          []string{"HOME=/home/user", mark.Env()},
			wantSignaled: []int{pid},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			proc := newFakeProc(t)
			proc.starting(pid)
			done := make(chan struct{})
			go func() {
				defer close(done)
				time.Sleep(100 * time.Millisecond)
				proc.started(pid, tt.env...)
			}()
			s := markScan{proc: proc.dir, signal: proc.signal, environWait: 10 * time.Millisecond, startWait: 10 * time.Second}

			err := s.kill(append([]byte(mark.Env()), 0))
			<-done

			if err != nil {
				t.Fatalf("markScan.kill() error = %v", err)
			}
			if diff := cmp.Diff(tt.wantSignaled, proc.signaledPIDs()); diff != "" {
				t.Fatalf("markScan.kill() signaled wrong processes (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestMarkScanFailsForProcessThatNeverLeavesExec(t *testing.T) {
	t.Parallel()
	proc := newFakeProc(t)
	proc.starting(4194305)
	s := markScan{proc: proc.dir, signal: proc.signal, environWait: 10 * time.Millisecond, startWait: 50 * time.Millisecond}

	err := s.kill(append([]byte(NewProcessMark().Env()), 0))

	if !errors.Is(err, errStarting) {
		t.Fatalf("markScan.kill() error = %v, want %v", err, errStarting)
	}
}
