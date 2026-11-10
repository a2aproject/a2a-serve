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

package service

import (
	"strconv"
	"strings"
)

// launchdPID returns the PID in the output of `launchctl print` for a
// service, or zero if it doesn't run. Nested blocks, which are indented
// further, can hold their own pid keys.
func launchdPID(out string) int {
	for line := range strings.Lines(out) {
		v, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n"), "\tpid = ")
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0
		}
		return pid
	}
	return 0
}
