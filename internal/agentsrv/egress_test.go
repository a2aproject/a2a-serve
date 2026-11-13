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
	"testing"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/sandbox"
)

func TestEgressProbeOnce(t *testing.T) {
	t.Parallel()
	setupErr := &sandbox.EgressSetupError{Reason: "no tun"}
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
