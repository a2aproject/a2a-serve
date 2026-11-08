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
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
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
