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

package lifecycle

import (
	"errors"

	"golang.org/x/sys/unix"
)

// terminate asks the process to shut down, and continues it in case it
// was stopped. A process that is gone is fine.
func terminate(pid int) error {
	for _, sig := range []unix.Signal{unix.SIGTERM, unix.SIGCONT} {
		if err := unix.Kill(pid, sig); err != nil && !errors.Is(err, unix.ESRCH) {
			return err
		}
	}
	return nil
}

// killProcess kills the process. A process that is gone is fine.
func killProcess(pid int) error {
	if err := unix.Kill(pid, unix.SIGKILL); err != nil && !errors.Is(err, unix.ESRCH) {
		return err
	}
	return nil
}
