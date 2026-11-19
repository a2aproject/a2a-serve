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

package sandboxrt

import "errors"

// BubblewrapError tells why bubblewrap can't run sandboxes on this machine,
// and how to fix it.
type BubblewrapError struct {
	// Problem follows "bwrap", as in "is not installed".
	Problem string
	Fix     string
	Err     error
}

func (e *BubblewrapError) Error() string {
	s := "bwrap " + e.Problem + "; " + e.Fix
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}

func (e *BubblewrapError) Unwrap() error { return e.Err }

// BubblewrapConfig configures the bubblewrap backend.
type BubblewrapConfig struct {
	// Dir keeps the files of the sandboxes, where no sandbox must see it.
	Dir string
	// Launcher returns the host path of the binary that the sandboxes with
	// the egress boundary run as their launcher, or "" to use this binary.
	Launcher func() string
	// Programs name the programs of this machine that the commands run,
	// besides their own, such as the a2a CLI.
	Programs []string
	// HostNetwork shares this machine's network with the sandboxes without
	// the egress boundary, which otherwise have only loopback.
	HostNetwork bool
}

// errBroadProgramDir is the error of a program whose directory holds the
// home directory, which the sandbox would show.
var errBroadProgramDir = errors.New("its directory holds your home directory, so the agent could read your credentials there")

// ProgramError is the error of a program that a bubblewrap sandbox runs
// from this machine, which this machine doesn't have.
type ProgramError struct {
	Name string
	Err  error
}

func (e *ProgramError) Error() string {
	if errors.Is(e.Err, errBroadProgramDir) {
		return "the bubblewrap sandbox can't run " + e.Name + " from this machine: " + e.Err.Error() + "; move it to a directory of its own"
	}
	return "the bubblewrap sandbox runs " + e.Name + " from this machine, which doesn't have it: install it, or add its directory to PATH"
}

func (e *ProgramError) Unwrap() error { return e.Err }
