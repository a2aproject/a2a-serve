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

// Package sandbox runs a command in a sandbox: on the host, or in a Docker
// container with its mounts, limits and egress boundary. It knows nothing
// about agents or agent directories.
package sandbox

import (
	"context"
	"os"
	"os/exec"
)

// Command describes a process to start in a sandbox.
type Command struct {
	Path string
	Args []string
	// Dir is the working directory.
	Dir string
	// Env holds NAME=value pairs added to the environment of the process.
	Env []string
}

// Mount is a host directory or file visible in the sandbox at Dst.
type Mount struct {
	Src      string
	Dst      string
	ReadOnly bool
}

// Limits are container resource limits. An empty field sets no limit.
type Limits struct {
	Memory string
	CPUs   string
	PIDs   int
}

// Runner decides where a process runs.
type Runner interface {
	// Command returns the process which runs c.
	Command(ctx context.Context, c Command) (*exec.Cmd, error)
}

// LocalRunner runs the process directly on the host, for trusted setups or
// when Docker isn't available.
type LocalRunner struct{}

var _ Runner = LocalRunner{}

// Command implements [Runner].
func (LocalRunner) Command(ctx context.Context, c Command) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, c.Path, c.Args...)
	cmd.Dir = c.Dir
	cmd.Env = append(os.Environ(), c.Env...)
	return cmd, nil
}
