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

// Package sandbox runs a command in isolation: with its own filesystem
// view and home, and with all of its network through an egress endpoint.
package sandbox

import (
	"context"
	"io"
	"net"

	"github.com/a2aproject/a2a-serve/core/egress"
	"github.com/a2aproject/a2a-serve/internal/sandboxrt"
)

func init() {
	sandboxrt.RegisterRuntime(func(v any) (sandboxrt.RuntimeInfo, bool) {
		switch rt := v.(type) {
		case local:
			return sandboxrt.RuntimeInfo{Local: true}, true
		case *docker:
			return sandboxrt.RuntimeInfo{Launcher: rt.opts.Launcher, Progress: rt.opts.Progress}, true
		}
		return sandboxrt.RuntimeInfo{}, false
	})
}

// Runtime creates sandboxes of one kind.
type Runtime interface {
	// Probe checks this machine without creating a sandbox.
	Probe(ctx context.Context) (Capabilities, error)
	// Create sets up a sandbox, such as its image and network. A Spec with
	// an Egress endpoint fails with an error that wraps ErrNoBoundary where
	// the runtime can't route through it.
	Create(ctx context.Context, spec Spec) (Sandbox, error)
}

// Sandbox runs a command, which it can start again after it exits.
type Sandbox interface {
	// Start starts the command and returns its stdio: reads come from its
	// output, writes go to its input, and Close stops it. The command
	// outlives ctx. Start may be called again after the command exits.
	Start(ctx context.Context) (io.ReadWriteCloser, error)
	// Close removes what Create set up.
	Close(ctx context.Context) error
}

// ErrNoBoundary means that the sandbox can't route its connections
// through an egress endpoint on this machine. Errors that wrap it say why.
var ErrNoBoundary = sandboxrt.ErrNoBoundary

// Spec describes a sandbox and the command it runs.
type Spec struct {
	// Name names the sandbox, such as its container.
	Name string
	// Image is the image the sandbox runs; Local ignores it.
	Image   string
	Command Command
	Mounts  []Mount
	// Hide are host paths that appear as empty directories wherever a
	// mount exposes them.
	Hide []string
	// Home is the host directory mounted as the command's $HOME.
	Home   string
	Limits Limits
	Labels map[string]string
	// Egress is the endpoint through which the sandbox makes all its
	// connections, trusting its CA. Nil gives it the runtime's own
	// network. Add Egress.Env() to Command.Env for its placeholders.
	Egress *egress.Endpoint
	// Sockets are host listeners that the command reaches.
	Sockets []Socket
	// Stderr receives the stderr of the command. Discarded if nil.
	Stderr io.Writer
}

// Command is a process to start in a sandbox.
type Command struct {
	Path string
	Args []string
	// Dir is the working directory in the sandbox.
	Dir string
	// Env holds NAME=value pairs added to the environment of the process.
	Env []string
}

// Mount is a host directory or file visible in the sandbox at Path.
type Mount struct {
	Host     string
	Path     string
	ReadOnly bool
}

// Socket is a host listener that the command reaches.
type Socket struct {
	// Listener is a Unix listener, whose socket a Docker sandbox mounts
	// with the rest of its directory, or, on Docker Desktop, a TCP
	// listener.
	Listener net.Listener
	// Env names the variable that gives the command the address of the
	// listener in the sandbox.
	Env string
}

// Limits are resource limits. An empty field gets the runtime's default.
type Limits struct {
	// Memory and CPUs take the values of docker run --memory and --cpus.
	Memory string
	CPUs   string
	PIDs   int
}

// Capabilities are what a runtime can do on this machine.
type Capabilities struct {
	// Boundary is set when sandboxes can route their connections through
	// an egress endpoint.
	Boundary bool
	// Problem says why Boundary isn't set, or what may still fail.
	Problem string
}

func mounts(ms []Mount) []sandboxrt.Mount {
	out := make([]sandboxrt.Mount, len(ms))
	for i, m := range ms {
		out[i] = sandboxrt.Mount{Src: m.Host, Dst: m.Path, ReadOnly: m.ReadOnly}
	}
	return out
}

func command(c Command) sandboxrt.Command {
	return sandboxrt.Command{Path: c.Path, Args: c.Args, Dir: c.Dir, Env: c.Env}
}
