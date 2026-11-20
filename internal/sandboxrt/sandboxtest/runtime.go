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

package sandboxtest

import (
	"fmt"
	"os"
	"testing"
)

// EnvRuntime names the OCI runtime that the tests run their Docker
// containers with, such as runsc for gVisor. A test that would skip for
// want of Docker or of what the runtime needs fails while it is set, so
// that a job that asks for the runtime runs the tests.
const EnvRuntime = "A2A_SERVE_TEST_DOCKER_RUNTIME"

// EnvRunsc is the path of a runsc binary that the tests of the launcher
// run it under, with runsc do.
const EnvRunsc = "A2A_SERVE_TEST_RUNSC"

// Runtime returns the runtime of [EnvRuntime], or "" for Docker's default.
func Runtime() string { return os.Getenv(EnvRuntime) }

// Skip skips t, or fails it while [EnvRuntime] is set.
func Skip(t testing.TB, format string, args ...any) {
	t.Helper()
	if r := Runtime(); r != "" {
		t.Fatalf("%s with %s=%s", fmt.Sprintf(format, args...), EnvRuntime, r)
	}
	t.Skipf(format, args...)
}
