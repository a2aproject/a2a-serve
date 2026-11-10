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
	"testing"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/harness"
)

// TestLauncherCopyRunsAsAnotherUser checks the modes that let container
// root, without CAP_DAC_OVERRIDE and so as another user than the owner,
// run the launcher on rootful Docker, while the sockets stay private.
func TestLauncherCopyRunsAsAnotherUser(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" {
		t.Skip("the launcher is copied on Linux only")
	}
	dir := filepath.Join(t.TempDir(), "harness")
	e := newEgressSockets(dir, func() *agentdir.Config { return &agentdir.Config{} })
	launcher := e.launcher()
	if launcher == "" {
		t.Fatalf("launcher() = \"\", want a copy")
	}
	for _, p := range []string{launcher, filepath.Dir(launcher)} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm&0o005 != 0o005 {
			t.Fatalf("mode of %s = %v, want others to read and execute it", p, perm)
		}
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("mode of the socket directory %s = %v, want it private", dir, perm)
	}
	if got := e.launcher(); got != launcher {
		t.Fatalf("second launcher() = %q, want the same copy %q", got, launcher)
	}
}

func TestEgressProbeOnce(t *testing.T) {
	t.Parallel()
	setupErr := &harness.EgressSetupError{Reason: "no tun"}
	tests := []struct {
		name      string
		first     error
		wantCalls int
		wantErr   error
	}{
		{name: "check passes", wantCalls: 1},
		{name: "setup fails", first: setupErr, wantCalls: 1, wantErr: setupErr},
		{name: "docker fails", first: errors.New("docker: Error response from daemon: EOF"), wantCalls: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newEgressSockets(t.TempDir(), func() *agentdir.Config { return &agentdir.Config{} })
			calls := 0
			probe := func() error {
				calls++
				if calls == 1 {
					return tc.first
				}
				return nil
			}
			if err := e.probeOnce(probe); !errors.Is(err, tc.first) {
				t.Fatalf("egressSockets.probeOnce() error = %v, want %v", err, tc.first)
			}
			if err := e.probeOnce(probe); !errors.Is(err, tc.wantErr) {
				t.Fatalf("second egressSockets.probeOnce() error = %v, want %v", err, tc.wantErr)
			}
			if calls != tc.wantCalls {
				t.Fatalf("egressSockets.probeOnce() ran the check %d times, want %d", calls, tc.wantCalls)
			}
		})
	}
}
