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

import "golang.org/x/sys/unix"

// inTerminal reports whether the process runs in the foreground process
// group of a terminal, where Ctrl-C reaches it.
func inTerminal(pid int) (bool, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return false, err
	}
	// NODEV is -1.
	return kp.Eproc.Tdev != -1 && kp.Eproc.Tpgid == kp.Eproc.Pgid, nil
}
