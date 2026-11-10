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

package lifecycle

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
)

// sessionLeaderProcess starts a process in its own session, as a detached
// server runs, and returns its PID.
func sessionLeaderProcess(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting sleep: %v", err)
	}
	t.Cleanup(func() {
		if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Errorf("killing sleep: %v", err)
		}
		var ee *exec.ExitError
		if err := cmd.Wait(); err != nil && !errors.As(err, &ee) {
			t.Errorf("waiting for sleep: %v", err)
		}
	})
	return cmd.Process.Pid
}

func TestRestartMode(t *testing.T) {
	t.Parallel()
	foreground, detached := os.Getpid(), sessionLeaderProcess(t)
	errQuery := errors.New("no service manager")
	tests := []struct {
		name     string
		services fakeServices
		pid      int
		want     Mode
		wantErr  error
	}{
		{name: "stopped", want: Detached},
		{name: "stopped with a service", services: fakeServices{installed: true}, want: Service},
		{name: "detached", pid: detached, want: Detached},
		{name: "foreground", pid: foreground, wantErr: ErrForeground},
		{name: "service", services: fakeServices{installed: true, mainPID: detached}, pid: detached, want: Service},
		{name: "detached with a stopped service", services: fakeServices{installed: true}, pid: detached, want: Detached},
		{name: "foreground with a stopped service", services: fakeServices{installed: true}, pid: foreground, wantErr: ErrForeground},
		{name: "foreground while the service runs another server", services: fakeServices{installed: true, mainPID: detached}, pid: foreground, wantErr: ErrForeground},
		{name: "service manager fails", services: fakeServices{installed: true, err: errQuery}, pid: detached, wantErr: errQuery},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := restartMode(tc.services, "a2a-coder", tc.pid)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("restartMode() error = %v, want %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("restartMode() = %v, want %v", got, tc.want)
			}
		})
	}
}
