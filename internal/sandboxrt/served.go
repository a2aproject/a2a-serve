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
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"path"
	"path/filepath"
	"strconv"
	"time"

	"github.com/a2aproject/a2a-serve/core/fsys"
	"github.com/a2aproject/a2a-serve/internal/boundary"
	"github.com/a2aproject/a2a-serve/internal/fsysmask"
)

// fsHeaderTimeout bounds the header of a request to a file server, and
// fsIdleTimeout an idle connection.
const (
	fsHeaderTimeout = 10 * time.Second
	fsIdleTimeout   = 5 * time.Minute
)

// serveTrees serves the trees of mounts on sockets in dir, the directory
// of the egress socket, gives r their mounts, and returns the function
// that stops the servers. A read-only mount refuses changes on its socket
// too, since the agent can reach the socket.
func serveTrees(r *DockerRunner, mounts []ServedMount, dir string) (func() error, error) {
	var closers []func() error
	closeAll := func() error {
		var errs []error
		for _, c := range closers {
			errs = append(errs, c())
		}
		return errors.Join(errs...)
	}
	for i, m := range mounts {
		if !path.IsAbs(m.Dst) || m.Tree == nil {
			return nil, errors.Join(&ServedMountError{Dst: m.Dst, Reason: "want an absolute path and a tree"}, closeAll())
		}
		tree := m.Tree
		if m.ReadOnly {
			tree = fsysmask.New(tree, fsysmask.Rules{ReadOnly: true})
		}
		name := boundary.FSSocketPrefix + strconv.Itoa(i) + ".sock"
		ln, err := net.Listen("unix", filepath.Join(dir, name))
		if err != nil {
			return nil, errors.Join(err, closeAll())
		}
		srv := &http.Server{Handler: fsys.Handler(tree), ReadHeaderTimeout: fsHeaderTimeout, IdleTimeout: fsIdleTimeout}
		go func() {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Warn("serving the live workspace of a container", "path", m.Dst, "error", err)
			}
		}()
		closers = append(closers, srv.Close)
		r.Served = append(r.Served, boundary.Mount{Dst: path.Clean(m.Dst), Socket: boundary.ContainerSocketDir + "/" + name, ReadOnly: m.ReadOnly})
	}
	return closeAll, nil
}

// checkServed returns the error of the mounts of spec that a Docker
// container can't show: without the egress boundary, whose launcher
// mounts them, or as [appArmorProblem] says.
func checkServed(ctx context.Context, spec Spec, appArmor string) error {
	dst := spec.Served[0].Dst
	if spec.Network.Boundary == nil {
		return &ServedMountError{Dst: dst, Reason: "a live workspace needs the egress boundary, whose launcher mounts it"}
	}
	if err := appArmorProblem(ctx, appArmor); err != nil {
		if _, ok := errors.AsType[*DockerError](err); ok {
			return err
		}
		return &ServedMountError{Dst: dst, Reason: err.Error()}
	}
	return nil
}

// errAppArmor is the problem of Docker's AppArmor profile, which denies
// the FUSE mount of a live workspace.
var errAppArmor = errors.New("the containers get the default AppArmor profile of Docker, which denies the FUSE mount; give live workspaces an AppArmor profile that allows it, or sync the workspace at turn boundaries")

// appArmorProblem returns errAppArmor when Docker applies AppArmor and
// profile, the profile of containers with live workspaces, is empty.
func appArmorProblem(ctx context.Context, profile string) error {
	if profile != "" {
		return nil
	}
	out, err := DockerQuery(ctx, "info", "--format", "{{.SecurityOptions}}")
	if err != nil {
		return err
	}
	if bytes.Contains(out, []byte("name=apparmor")) {
		return errAppArmor
	}
	return nil
}

// servedRefusals are what the output of docker run names when the runtime
// refuses the device or the capability that a live workspace takes.
var servedRefusals = []string{"/dev/fuse", "SYS_ADMIN"}
