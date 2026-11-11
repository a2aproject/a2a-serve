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
	"os"
	"os/exec"
	"path/filepath"
)

// Executable returns the path under which a service should run this binary:
// the a2a-serve on PATH when it is this binary, and the running file
// otherwise.
func Executable() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	return StableExecutable(self, exec.LookPath), nil
}

// StableExecutable returns the path that lookPath finds for a2a-serve when it
// is the same file as self, and self otherwise. On Linux os.Executable
// resolves symlinks, so a binary that a package manager links onto PATH, such
// as Homebrew's, would otherwise be recorded at a versioned path that the
// next upgrade deletes.
func StableExecutable(self string, lookPath func(string) (string, error)) string {
	found, err := lookPath("a2a-serve")
	if err != nil {
		return self
	}
	if found, err = filepath.Abs(found); err != nil {
		return self
	}
	selfInfo, err := os.Stat(self)
	if err != nil {
		return self
	}
	foundInfo, err := os.Stat(found)
	if err != nil || !os.SameFile(selfInfo, foundInfo) {
		return self
	}
	return found
}
