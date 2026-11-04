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
	"io"
	"os"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-serve/internal/controlapi"
)

func TestRestart(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets are not used on this platform")
	}
	tests := []struct {
		name    string
		running bool
		wantErr string
	}{
		{name: "not running", wantErr: "coder is not running"},
		{name: "foreground", running: true, wantErr: "press Ctrl-C there and run `a2a-serve start` again"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := newAgent(t)
			f := &fakeControl{pid: os.Getpid()}
			if tc.running {
				serveControl(t, dir, f)
			}
			err := Restart(t.Context(), dir, controlapi.NewClient(dir), false, io.Discard)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Restart() error = %v, want %q", err, tc.wantErr)
			}
			if slices.Contains(f.Calls(), "POST /stop") {
				t.Fatalf("Restart() stopped the agent")
			}
		})
	}
}
