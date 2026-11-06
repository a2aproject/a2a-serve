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
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// maxKillRounds bounds the scans of Kill, which go on while they find
// processes, since a process can start another one during a scan.
const maxKillRounds = 5

// execTimeout bounds the wait of a scan for processes that execute a program
// to show their environment.
const execTimeout = time.Second

// Kill kills the processes with the mark that still run.
func (m ProcessMark) Kill() error {
	env := append([]byte(m.Env()), 0)
	for range maxKillRounds {
		pids, findErr := m.find(env)
		for _, pid := range pids {
			if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				return err
			}
		}
		if findErr != nil || len(pids) == 0 {
			return findErr
		}
	}
	return nil
}

// find returns the processes whose environment holds env, NUL-terminated,
// and an error for those that didn't show their environment in time.
// Processes of other users can't be read, and those that exit during the
// scan are gone, so both are skipped.
func (m ProcessMark) find(env []byte) ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(execTimeout)
	var pids []int
	var errs []error
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		data, err := environ(pid, deadline)
		if errors.Is(err, errStarting) {
			errs = append(errs, err)
			continue
		}
		if err != nil {
			continue
		}
		if bytes.HasPrefix(data, env) || bytes.Contains(data, append([]byte{0}, env...)) {
			pids = append(pids, pid)
		}
	}
	return pids, errors.Join(errs...)
}

var errStarting = errors.New("process is still starting a program")

// environ returns the environment of a process. A process that executes a
// program shows an empty environment until the kernel has set up the
// program's stack, which happens after its parent learns that the program
// started, so environ waits for that until deadline.
func environ(pid int, deadline time.Time) ([]byte, error) {
	dir := "/proc/" + strconv.Itoa(pid)
	for {
		data, err := os.ReadFile(dir + "/environ")
		if err != nil || len(data) > 0 {
			return data, err
		}
		starting, err := startingProgram(dir)
		if err != nil {
			return nil, err
		}
		if !starting {
			// The program may have finished starting after the first read.
			return os.ReadFile(dir + "/environ")
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("process %d: %w", pid, errStarting)
		}
		time.Sleep(time.Millisecond)
	}
}

// pfKthread is the flag of kernel threads in /proc/PID/stat.
const pfKthread = 0x00200000

// startingProgram reports whether the process of a /proc directory runs and
// has no environment address yet, which the kernel sets once a program has
// started.
func startingProgram(dir string) (bool, error) {
	data, err := os.ReadFile(dir + "/stat")
	if err != nil {
		return false, err
	}
	// The command name in parentheses can hold any character.
	i := bytes.LastIndex(data, []byte(") "))
	if i < 0 {
		return false, fmt.Errorf("parsing %s/stat: no command name", dir)
	}
	// Fields from the state (field 3) to env_end (field 51).
	fields := strings.Fields(string(data[i+2:]))
	if len(fields) < 49 {
		return false, fmt.Errorf("parsing %s/stat: %d fields", dir, len(fields)+2)
	}
	if state := fields[0]; state == "Z" || state == "X" {
		return false, nil
	}
	flags, err := strconv.ParseUint(fields[6], 10, 64)
	if err != nil {
		return false, fmt.Errorf("parsing the flags in %s/stat: %w", dir, err)
	}
	return flags&pfKthread == 0 && fields[48] == "0", nil
}
