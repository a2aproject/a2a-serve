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
	"context"
	"errors"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/controlapi"
)

func TestStop(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets are not used on this platform")
	}
	tests := []struct {
		name      string
		running   bool
		wait      Wait
		want      StopResult
		wantCalls []string
	}{
		{name: "not running", wait: WaitStopped},
		{name: "running", running: true, want: StopResult{Name: "coder", Running: true}, wantCalls: []string{"GET /status", "POST /stop"}},
		{name: "running and waits", running: true, wait: WaitStopped, want: StopResult{Name: "coder", Running: true}, wantCalls: []string{"GET /status", "POST /stop"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := newAgent(t)
			f := &fakeControl{}
			if tc.running {
				serveControl(t, dir, f)
			}

			got, err := Stop(t.Context(), dir, controlapi.NewClient(dir), StopOptions{Wait: tc.wait})
			if err != nil {
				t.Fatalf("Stop() error = %v", err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("Stop() wrong result (-want +got) diff = %s", diff)
			}
			if diff := cmp.Diff(tc.wantCalls, f.Calls()); diff != "" {
				t.Fatalf("Stop() wrong calls (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestStopSignalsServerThatIsStarting(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("the test server is a unix process")
	}
	tests := []struct {
		name        string
		locked      bool
		wantRunning bool
		wantExited  bool
	}{
		{name: "lock held", locked: true, wantRunning: true, wantExited: true},
		{name: "stale PID file", locked: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := newAgent(t)
			var proc *fakeProcess
			if tc.locked {
				proc = startingServer(t, dir)
			} else {
				proc = pidFileProcess(t, dir)
			}

			got, err := Stop(t.Context(), dir, controlapi.NewClient(dir), StopOptions{})
			if err != nil {
				t.Fatalf("Stop() error = %v", err)
			}
			if got.Running != tc.wantRunning {
				t.Fatalf("Stop().Running = %v, want %v", got.Running, tc.wantRunning)
			}
			if tc.wantExited {
				if !proc.exited() {
					t.Fatalf("Stop() did not signal the starting server")
				}
				return
			}
			select {
			case <-proc.done:
				t.Fatalf("Stop() signaled the process of a stale PID file")
			case <-time.After(200 * time.Millisecond):
			}
		})
	}
}

func TestStopWithoutStateDir(t *testing.T) {
	t.Parallel()
	dir := newAgent(t)
	if err := os.RemoveAll(dir.StateDir()); err != nil {
		t.Fatalf("os.RemoveAll() error = %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	got, err := Stop(ctx, dir, controlapi.NewClient(dir), StopOptions{Wait: WaitStopped})
	if err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if diff := cmp.Diff(StopResult{}, got); diff != "" {
		t.Fatalf("Stop() wrong result (-want +got) diff = %s", diff)
	}
}

func TestWaitStoppedWithoutLockFile(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		state   func(t *testing.T, path string)
		wantErr bool
	}{
		{name: "state directory removed", state: func(*testing.T, string) {}},
		{name: "state is a file", state: func(t *testing.T, path string) {
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatalf("os.WriteFile() error = %v", err)
			}
		}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := newAgent(t)
			if err := os.RemoveAll(dir.StateDir()); err != nil {
				t.Fatalf("os.RemoveAll() error = %v", err)
			}
			tc.state(t, dir.StateDir())
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()

			err := waitStopped(ctx, dir, time.Minute, func() error { return nil })
			if gotErr := err != nil; gotErr != tc.wantErr || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("waitStopped() error = %v, want error %v", err, tc.wantErr)
			}
		})
	}
}
