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

import "io"

// RuntimeInfo is what a composing package needs of a runtime of the
// public core/sandbox package to run its backend directly, with the
// features that the public Spec leaves out.
type RuntimeInfo struct {
	// Local is set for the runtime of the host, and Bubblewrap for the
	// one of bubblewrap. Both are unset for Docker.
	Local, Bubblewrap bool
	// Launcher is the launcher that sandboxes with an egress boundary run,
	// or "" for a copy of this binary.
	Launcher string
	// Programs and HostNetwork are the BubblewrapConfig of bubblewrap.
	Programs    []string
	HostNetwork bool
	// Progress receives the output of pulling an image, or nil.
	Progress io.Writer
	// StateDir keeps the env files of the containers in env, the files of
	// bubblewrap sandboxes in bwrap, and the sockets of their egress
	// boundaries in egress, or is "".
	StateDir string
	// ServedAppArmor is the AppArmor profile of Docker containers with
	// live workspaces, or "" for Docker's default.
	ServedAppArmor string
	// Runtime is the OCI runtime of Docker containers, or "" for Docker's
	// default.
	Runtime string
}

var runtimeOf func(any) (RuntimeInfo, bool)

// RegisterRuntime sets how RuntimeOf reads a runtime of core/sandbox. It
// is called once, from the init of core/sandbox.
func RegisterRuntime(f func(any) (RuntimeInfo, bool)) { runtimeOf = f }

// RuntimeOf returns what rt, a runtime of core/sandbox, runs, or false for
// any other runtime.
func RuntimeOf(rt any) (RuntimeInfo, bool) {
	if runtimeOf == nil {
		return RuntimeInfo{}, false
	}
	return runtimeOf(rt)
}
