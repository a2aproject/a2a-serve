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

// Package sandboxnet gives the Docker sandbox its only network path, as the
// agents.net specification describes: a launcher in the container turns
// every connection into an HTTP CONNECT request on a Unix socket, and the
// boundary on the host decides each request by its destination.
package sandboxnet

// LaunchConfig configures the launcher, which runs as PID 1 of a container
// with no network and starts the agent as its child.
type LaunchConfig struct {
	// Boundary is the path of the boundary socket in the container.
	Boundary string
	// UID and GID are the IDs the agent runs as.
	UID, GID int
	// Argv is the agent's command, which gets the launcher's standard
	// streams.
	Argv []string
}

// Paths in the container.
const (
	// ContainerLauncher is where the launcher binary is mounted.
	ContainerLauncher = "/run/a2a-init"
	// ContainerSocketDir is where the directory of the boundary socket is
	// mounted.
	ContainerSocketDir = "/run/a2a-egress"
	// SocketName is the file name of the boundary socket.
	SocketName = "egress.sock"
)
