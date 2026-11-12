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

package main

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// protocolStdout keeps stdout for the protocol. The SLIM library logs to
// descriptor 1, so os.Stdout moves to a copy of it and descriptor 1 points
// at stderr.
func protocolStdout() error {
	fd, err := unix.Dup(1)
	if err != nil {
		return err
	}
	unix.CloseOnExec(fd)
	if err := unix.Dup2(2, 1); err != nil {
		return errors.Join(err, unix.Close(fd))
	}
	os.Stdout = os.NewFile(uintptr(fd), "/dev/stdout")
	return nil
}
