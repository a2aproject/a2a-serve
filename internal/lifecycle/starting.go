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
	"errors"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
)

// findStarting returns the PID of a server that holds the lock of dir but
// doesn't serve its control socket yet, as while it pulls an image or logs
// in to a network. The lock tells a live server from a PID file that a
// crash left behind, and other holders of the lock remove such a file when
// they take it. Anything unreadable means that no server is starting.
func findStarting(dir agentdir.Dir) (int, bool) {
	release, err := dir.Lock()
	if err == nil {
		if err := release(); err != nil {
			slog.Debug("releasing the agent directory lock", "error", err)
		}
		return 0, false
	}
	if !errors.Is(err, fsutil.ErrLocked) {
		slog.Debug("checking for a starting server", "error", err)
		return 0, false
	}
	data, err := os.ReadFile(dir.PIDPath())
	if err != nil {
		slog.Debug("reading the PID file", "error", err)
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		slog.Debug("the PID file does not hold a PID", "path", dir.PIDPath())
		return 0, false
	}
	return pid, true
}
