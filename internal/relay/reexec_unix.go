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

//go:build unix

package relay

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// Reexec replaces this process with the a2a-serve binary on disk, which
// runs the relay in dir with its saved settings, so that an upgraded binary
// runs the relay. It keeps the process ID, so a service manager or terminal
// keeps it.
func Reexec(dir string) error {
	exe := os.Args[0]
	if !filepath.IsAbs(exe) {
		// os.Executable names the running file, which an upgrade may have
		// replaced; the path the binary was started by names the new one.
		p, err := exec.LookPath(exe)
		if err != nil {
			return err
		}
		if exe, err = filepath.Abs(p); err != nil {
			return err
		}
	}
	return syscall.Exec(exe, []string{os.Args[0], "relay", "run", "--dir", dir}, os.Environ())
}
