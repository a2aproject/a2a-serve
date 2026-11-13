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

package sandbox

import (
	"context"
	"io"
	"slices"
)

// Backend sets up the sandboxes of one kind, such as Docker containers.
type Backend interface {
	// Probe runs the quick checks of this host, without starting a sandbox.
	Probe(ctx context.Context) (Capabilities, error)
	// Prepare runs the checks of Probe, then sets up the sandbox of spec,
	// such as its image and its egress boundary. When spec asks for a
	// boundary that can't work on this host, it fails with an error that
	// wraps [ErrNoBoundary]; the caller decides whether to prepare the
	// sandbox again without one.
	Prepare(ctx context.Context, spec Spec) (Sandbox, error)
}

// Spec describes a sandbox and the command it runs. The local backend runs
// the command on the host, and uses only Command, Expose, Check and Stderr.
type Spec struct {
	// Name names the sandbox, such as its container.
	Name string
	// Image is the image the sandbox runs, and Build the image to build
	// when Image can't be pulled, or nil.
	Image string
	Build *LocalBuild
	// Command is the process to start. Its Dir is in the sandbox.
	Command Command
	Mounts  []Mount
	Masks   []Mask
	// Scratch are paths in the sandbox which appear as empty directories,
	// whose changes stay in the sandbox.
	Scratch []string
	// Home is the host directory that the sandbox mounts as the home of the
	// process.
	Home    string
	Limits  Limits
	Labels  map[string]string
	Network Network
	// Expose are the endpoints on the host that the process reaches through
	// Unix sockets.
	Expose []Socket
	// Check runs before every start, which it refuses with its error. Nil
	// checks nothing.
	Check func() error
	// Stderr receives the stderr of the process, and Progress the output of
	// pulling or building the image. Discarded if nil.
	Stderr, Progress io.Writer
}

// Network is the network of a sandbox: the runtime's own, or none but the
// egress boundary.
type Network struct {
	// Boundary opens the egress boundary through which the sandbox makes
	// all its connections. Nil gives the sandbox the runtime's own network.
	Boundary OpenEgress
	// CA is the PEM certificate that the boundary signs the certificates of
	// some hosts with, which the sandbox trusts.
	CA []byte
	// Recheck makes every start check first that the boundary works, and
	// fail with an error that wraps [ErrNoBoundary] when it doesn't.
	Recheck bool
}

// Socket is an endpoint on the host that the process reaches through a
// Unix socket.
type Socket struct {
	// Path is the host path of a Unix socket, alone in its directory, which
	// a container mounts read-only so that the process can't replace it.
	// Addr is the host:port of a TCP listener in its place, for runtimes
	// that can't mount sockets; a container without the egress boundary
	// reaches it over its network.
	Path, Addr string
	// Env names the variable that gives the process the path of the socket
	// in the sandbox.
	Env string
}

// Info describes a sandbox that a backend prepared.
type Info struct {
	// Summary describes the sandbox for the user, such as its image.
	Summary string
	// Image is the image the sandbox runs, which a later Prepare can take
	// as the Image of its Spec, with no Build, to run the same one.
	Image string
	// Boundary is set when the sandbox makes its connections through the
	// egress boundary, and Relay when an egress container next to it
	// carries them.
	Boundary, Relay bool
}

// Local is the backend that runs commands directly on the host, for
// trusted setups or when Docker isn't available. It has no egress boundary.
type Local struct{}

var _ Backend = Local{}

// Probe implements [Backend].
func (Local) Probe(context.Context) (Capabilities, error) {
	return Capabilities{Boundary: &boundaryError{why: "the command runs on the host"}}, nil
}

// Prepare implements [Backend]. Each process gets a [ProcessMark], so the
// processes it starts are killed once it exits.
func (Local) Prepare(_ context.Context, spec Spec) (Sandbox, error) {
	if spec.Network.Boundary != nil {
		return nil, &boundaryError{why: "the command runs on the host"}
	}
	c := spec.Command
	c.Env = slices.Clone(c.Env)
	for _, s := range spec.Expose {
		if s.Path != "" && s.Env != "" {
			c.Env = append(c.Env, s.Env+"="+s.Path)
		}
	}
	return local{c: c, opts: Options{Stderr: spec.Stderr, Check: spec.Check}}, nil
}
