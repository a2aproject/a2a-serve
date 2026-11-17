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
	"fmt"
	"os"
	"strconv"
	"strings"
)

// inTerminal reports whether the process runs in the foreground process
// group of a terminal, where Ctrl-C reaches it.
func inTerminal(pid int) (bool, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false, err
	}
	// The command name may hold spaces, so the fields start after its ")".
	i := strings.LastIndexByte(string(data), ')')
	if i < 0 {
		return false, fmt.Errorf("parsing /proc/%d/stat", pid)
	}
	// state ppid pgrp session tty_nr tpgid
	fields := strings.Fields(string(data[i+1:]))
	if len(fields) < 6 {
		return false, fmt.Errorf("parsing /proc/%d/stat", pid)
	}
	pgrp, err := strconv.Atoi(fields[2])
	if err != nil {
		return false, err
	}
	tpgid, err := strconv.Atoi(fields[5])
	if err != nil {
		return false, err
	}
	return fields[4] != "0" && tpgid == pgrp, nil
}
