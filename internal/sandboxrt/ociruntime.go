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

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
)

// GVisorInstallDoc is where gVisor says how to install runsc and register
// it with Docker.
const GVisorInstallDoc = "https://gvisor.dev/docs/user_guide/install/"

// errGVisorServed is why a gVisor container can't show a live workspace.
var errGVisorServed = errors.New("gVisor's FUSE can't map the workspace's files into memory, so git and the programs in the workspace fail there")

// relayRuntimeProblem is why a container of the runtime name doesn't get
// an egress container.
func relayRuntimeProblem(name string) error {
	return &boundaryError{why: "the egress container, which Docker Desktop needs, doesn't work with the runtime " + name + " yet"}
}

// ociRuntime is an OCI runtime that the docker daemon has, as docker info
// lists it.
type ociRuntime struct {
	Name string
	// Path is the runtime binary, and Args the flags the daemon gives it,
	// for a runtime that the daemon runs itself; both are empty for one
	// that a containerd shim runs, such as io.containerd.runsc.v1.
	Path string   `json:"path"`
	Args []string `json:"runtimeArgs"`
	Type string   `json:"runtimeType"`
}

// gVisor reports whether r is runsc, gVisor's runtime.
func (r ociRuntime) gVisor() bool {
	return r.Name == "runsc" || filepath.Base(r.Path) == "runsc" || strings.Contains(r.Type, ".runsc.")
}

// hostSocketsProblem returns why the containers of r, a gVisor runtime,
// can't connect to the egress socket, or nil. gVisor refuses connections
// to the host's sockets unless runsc runs with --host-uds=open or all. A
// runtime that a containerd shim runs reads its flags elsewhere, and the
// launcher's check finds out.
func (r ociRuntime) hostSocketsProblem() error {
	if r.Path == "" {
		return nil
	}
	for i, a := range r.Args {
		v, ok := strings.CutPrefix(a, "--host-uds=")
		if !ok && a == "--host-uds" && i+1 < len(r.Args) {
			v, ok = r.Args[i+1], true
		}
		if ok && (v == "open" || v == "all") || a == "--fsgofer-host-uds" || a == "--fsgofer-host-uds=true" {
			return nil
		}
	}
	return &boundaryError{why: fmt.Sprintf("Docker runs %s without --host-uds=open, so gVisor refuses the container's connections to the egress socket; add --host-uds=open to the runtimeArgs of %s in the Docker daemon's configuration", r.Name, r.Name)}
}

// lookupRuntime returns the runtime name of the docker daemon, or an
// error that names the runtimes it has.
func lookupRuntime(ctx context.Context, name string) (ociRuntime, error) {
	out, err := DockerQuery(ctx, "info", "--format", "{{json .Runtimes}}")
	if err != nil {
		return ociRuntime{}, err
	}
	var runtimes map[string]ociRuntime
	if err := json.Unmarshal(out, &runtimes); err != nil {
		return ociRuntime{}, fmt.Errorf("reading the runtimes of docker info: %w", err)
	}
	r, ok := runtimes[name]
	if !ok {
		fix := "pick one of " + strings.Join(slices.Sorted(maps.Keys(runtimes)), ", ")
		if name == "runsc" {
			fix = "install gVisor and register runsc with Docker, as " + GVisorInstallDoc + " describes"
		}
		return ociRuntime{}, &DockerError{Problem: "has no runtime " + name, Fix: fix}
	}
	r.Name = name
	return r, nil
}
