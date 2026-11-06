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

package harness

import (
	"bytes"
	"errors"
	"os"
	"strconv"
	"syscall"
)

// maxKillRounds bounds the scans of Kill, which go on while they find
// processes, since a process can start another one during a scan.
const maxKillRounds = 5

// Kill kills the processes with the mark that still run.
func (m ProcessMark) Kill() error {
	env := append([]byte(m.Env()), 0)
	for range maxKillRounds {
		pids, err := m.find(env)
		if err != nil || len(pids) == 0 {
			return err
		}
		for _, pid := range pids {
			if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				return err
			}
		}
	}
	return nil
}

// find returns the processes whose environment holds env, NUL-terminated.
// Processes of other users can't be read, and those that exit during the
// scan are gone, so both are skipped.
func (m ProcessMark) find(env []byte) ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		data, err := os.ReadFile("/proc/" + e.Name() + "/environ")
		if err != nil {
			continue
		}
		if bytes.HasPrefix(data, env) || bytes.Contains(data, append([]byte{0}, env...)) {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}
