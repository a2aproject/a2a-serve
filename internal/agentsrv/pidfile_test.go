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
	"strconv"
	"testing"
)

func TestServerKeepsPIDFileWhileRunning(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})

	data, err := os.ReadFile(ts.dir.PIDPath())
	if err != nil {
		t.Fatalf("os.ReadFile(PIDPath) error = %v", err)
	}
	if got, want := string(data), strconv.Itoa(os.Getpid()); got != want {
		t.Fatalf("PID file = %q, want %q", got, want)
	}

	ts.stop()
	if _, err := os.Stat(ts.dir.PIDPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("os.Stat(PIDPath) after stop error = %v, want %v", err, os.ErrNotExist)
	}
}
