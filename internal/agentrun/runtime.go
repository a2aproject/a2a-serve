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

package agentrun

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"github.com/a2aproject/a2a-serve/core/sandbox"
	"github.com/a2aproject/a2a-serve/internal/agentprofile"
	"github.com/a2aproject/a2a-serve/internal/sandboxrt"
)

// sandboxRuntime is how the runner runs its sandboxes: the backend of a runtime of
// core/sandbox, which it gives the features the public Spec leaves out, or
// any other runtime through the public interface.
type sandboxRuntime struct {
	// kind is agentprofile.SandboxDocker or agentprofile.SandboxLocal, or
	// "" for a custom runtime.
	kind    string
	info    sandboxrt.RuntimeInfo
	backend sandboxrt.Backend
	custom  sandbox.Runtime
}

func runtimeOf(rt sandbox.Runtime) (sandboxRuntime, error) {
	if rt == nil {
		return sandboxRuntime{}, errors.New("no sandbox runtime")
	}
	info, ok := sandboxrt.RuntimeOf(rt)
	switch {
	case !ok:
		return sandboxRuntime{custom: rt}, nil
	case info.Local:
		return sandboxRuntime{kind: agentprofile.SandboxLocal, info: info, backend: sandboxrt.Local{}}, nil
	}
	return sandboxRuntime{kind: agentprofile.SandboxDocker, info: info}, nil
}

// init sets up the Docker backend, whose env files are in the state
// directory of r.
func (rt *sandboxRuntime) init(r *Runner) {
	if rt.kind != agentprofile.SandboxDocker {
		return
	}
	launcher := r.boundaries.Launcher
	if rt.info.Launcher != "" {
		launcher = func() string { return rt.info.Launcher }
	}
	rt.backend = sandboxrt.NewDocker(filepath.Join(r.stateDir, "run", "env"), launcher)
}

// imageProgress returns where the main process reports fetching its image.
func (rt sandboxRuntime) imageProgress(stderr io.Writer) io.Writer {
	if rt.info.Progress != nil {
		return rt.info.Progress
	}
	return stderr
}

// customSandbox runs the agent in a sandbox of a custom runtime.
type customSandbox struct {
	sandbox.Sandbox
	name string
}

func (s customSandbox) Info() sandboxrt.Info {
	return sandboxrt.Info{Summary: "sandbox " + s.name}
}

// prepareCustom prepares the agent of spec in a sandbox of the custom
// runtime, through its public interface: the workspace and mounts, the
// home, the egress endpoint and the socket of the server.
func (rt sandboxRuntime) prepareCustom(ctx context.Context, spec agentprofile.Spec, mounts []sandbox.Mount, ep sandbox.Spec) (*agentprofile.Prepared, error) {
	if err := agentprofile.PrepareHome(spec); err != nil {
		return nil, err
	}
	ws := agentprofile.Workspace{Dir: spec.Mounts[0].Src, Home: spec.Home}
	if err := spec.Profile.Bootstrap(ws); err != nil {
		return nil, fmt.Errorf("bootstrapping %s: %w", spec.Profile.Name(), err)
	}
	cmd := spec.Profile.Command(ws)
	if spec.Model != "" {
		cmd.Env = append(cmd.Env, spec.Profile.ModelEnv(spec.Model)...)
	}
	image := spec.Base.Image
	if image == "" {
		image = spec.Profile.Image()
	}
	ep.Name, ep.Image, ep.Mounts, ep.Hide, ep.Home = spec.Base.Name, image, mounts, spec.Hidden, spec.Home
	ep.Command = sandbox.Command{Path: cmd.Path, Args: cmd.Args, Dir: ws.Dir, Env: append(cmd.Env, spec.Env...)}
	ep.Limits = sandbox.Limits(spec.Base.Limits)
	ep.Labels, ep.Stderr = spec.Base.Labels, spec.Base.Stderr
	sb, err := rt.custom.Create(ctx, ep)
	if err != nil {
		return nil, err
	}
	return &agentprofile.Prepared{Sandbox: customSandbox{Sandbox: sb, name: spec.Base.Name}, Summary: "sandbox " + spec.Base.Name}, nil
}
