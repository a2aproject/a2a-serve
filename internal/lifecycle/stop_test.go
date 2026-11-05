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
	"runtime"
	"testing"

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
		{name: "running", running: true, want: StopResult{Running: true}, wantCalls: []string{"GET /status", "POST /stop"}},
		{name: "running and waits", running: true, wait: WaitStopped, want: StopResult{Running: true}, wantCalls: []string{"GET /status", "POST /stop"}},
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
