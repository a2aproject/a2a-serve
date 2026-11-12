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
	"errors"
	"io"
	"os"
	"os/exec"
	"slices"
)

// Sandbox runs a command, which it can start again after it exits.
type Sandbox interface {
	// Start runs the check of the sandbox and starts the command, connected
	// to its stdio: reads come from its output, writes go to its input, and
	// Close stops it. The process outlives ctx. The caller may call Start
	// again after the process exits.
	Start(ctx context.Context) (io.ReadWriteCloser, error)
	// Close removes what the sandbox set up.
	Close(ctx context.Context) error
}

// ErrNoBoundary means that the egress boundary can't work on this host.
// Errors that wrap it say why.
var ErrNoBoundary = errors.New("the sandbox can't route its connections through the boundary")

// Options configure a [Sandbox].
type Options struct {
	// Stderr receives the stderr of the process. Discarded if nil.
	Stderr io.Writer
	// Check runs before every start, which it refuses with its error. Nil
	// checks nothing.
	Check func() error
}

func (o Options) check() error {
	if o.Check == nil {
		return nil
	}
	return o.Check()
}

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

// Limits are container resource limits. An empty field gets the value of
// [DefaultLimits].
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

// NewLocal returns the [Sandbox] that runs c directly on the host. Each
// process gets a [ProcessMark], so the processes it starts are killed once
// it exits.
func NewLocal(c Command, opts Options) Sandbox { return local{c: c, opts: opts} }

type local struct {
	c    Command
	opts Options
}

func (l local) Start(ctx context.Context) (io.ReadWriteCloser, error) {
	if err := l.opts.check(); err != nil {
		return nil, err
	}
	mark := NewProcessMark()
	c := l.c
	c.Env = append(slices.Clone(c.Env), mark.Env())
	cmd, err := LocalRunner{}.Command(context.WithoutCancel(ctx), c)
	if err != nil {
		return nil, err
	}
	rwc, err := startProcess(cmd, l.opts.Stderr)
	if err != nil {
		return nil, err
	}
	return mark.Watch(rwc), nil
}

func (local) Close(context.Context) error { return nil }
