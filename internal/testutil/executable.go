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

package testutil

import (
	"os"
	"syscall"
	"testing"
)

// WriteExecutable writes an executable file for a test to run. A child
// forked by a parallel test while the file is open for writing keeps the
// descriptor until it execs, and running the file then fails with ETXTBSY
// (golang.org/issue/22315). Holding syscall.ForkLock keeps forks out of
// that window.
func WriteExecutable(t testing.TB, path string, data []byte) {
	t.Helper()
	syscall.ForkLock.RLock()
	err := os.WriteFile(path, data, 0o755)
	syscall.ForkLock.RUnlock()
	if err != nil {
		t.Fatalf("os.WriteFile(%q) error = %v", path, err)
	}
}
