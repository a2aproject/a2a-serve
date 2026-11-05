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

package harness

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
	"github.com/a2aproject/a2a-serve/internal/project"
)

// Sandboxes a harness can run in.
const (
	SandboxDocker = "docker"
	SandboxLocal  = "local"
)

// EnvUnixSocket tells the a2a CLI to send every HTTP request through the
// Unix socket at its path, whatever the URL's host.
const EnvUnixSocket = "A2ACLI_UNIX_SOCKET"

// EnvCredentialHelper sets the credential helper of the a2a CLI:
// [scope=]path of a program which prints the headers for a URL.
const EnvCredentialHelper = "A2ACLI_CREDENTIAL_HELPER"

// EnvTimeout sets the request timeout of the a2a CLI. Harnesses get
// CLITimeout by default, since the agents they call can take minutes to reply.
const EnvTimeout = "A2ACLI_TIMEOUT"

// CLITimeout is the default request timeout of the a2a CLI in harnesses.
const CLITimeout = 5 * time.Minute

// Spec describes a harness to run for an agent.
type Spec struct {
	Profile Profile
	// Sandbox is SandboxDocker (the default) or SandboxLocal.
	Sandbox string
	// Mounts are the directories the harness works on. The first is the
	// workspace and must keep its path in the sandbox.
	Mounts []agentdir.Mount
	// Home is the host directory holding the harness home.
	Home string
	// Hidden are host paths a sandbox hides even where a mount exposes them.
	Hidden []string
	// Pins are the directories the server pinned. A container doesn't
	// start when one of them was replaced. Nil skips the check.
	Pins *Pins
	// MCPMask is the host path of a file with no MCP servers, which a
	// sandbox mounts over the workspace's .mcp.json, so that the harness
	// gets MCP servers only from the server. Empty keeps .mcp.json visible.
	MCPMask string
	// Env holds NAME=value pairs for the harness, such as API keys.
	Env []string

	// Name, Image, Limits and Labels configure the container. Image
	// defaults to the profile's.
	Name   string
	Image  string
	Limits Limits
	Labels map[string]string

	// Socket is the host path of the Unix socket through which the harness
	// reaches the server, alone in its directory, which a container
	// mounts read-only so the harness can't replace it. Empty when the
	// harness reaches the server over TCP.
	Socket string
	// CLI is the host path of the a2a binary put first on the PATH of a
	// local harness. A harness in Docker uses the a2a of its image.
	CLI string

	// Progress receives the output of pulling or building the image.
	Progress io.Writer
}

// Prepared is a harness ready to start.
type Prepared struct {
	// Args, Env and Dir start the harness as an ACP agent.
	Args []string
	Env  []string
	Dir  string
	// Summary describes the sandbox for the user.
	Summary string
	// Stop cleans up after the harness exited, such as removing its container.
	Stop func(context.Context) error
	// Check refuses to start the harness again when the agent could have
	// redirected its sandbox since Prepare.
	Check func() error
}

// Prepare bootstraps the harness home and returns the command which runs
// the harness in its sandbox.
func Prepare(ctx context.Context, spec Spec) (*Prepared, error) {
	if len(spec.Mounts) == 0 {
		return nil, fmt.Errorf("no workspace to mount")
	}
	// ACP sessions get the workspace as cwd, and the executor starts the
	// agent there, so the workspace must have the same path on both sides.
	if ws := spec.Mounts[0]; ws.Dst != ws.Src {
		return nil, fmt.Errorf("the first --mount is the workspace and must keep its path (%s), got %s", ws.Src, ws.Dst)
	}
	if err := PrepareHome(spec); err != nil {
		return nil, err
	}
	ws := Workspace{Dir: spec.Mounts[0].Src, Home: spec.Home}
	if err := spec.Profile.Bootstrap(ws); err != nil {
		return nil, fmt.Errorf("bootstrapping %s: %w", spec.Profile.Name(), err)
	}
	switch spec.Sandbox {
	case "", SandboxDocker:
		return prepareDocker(ctx, spec, ws)
	case SandboxLocal:
		return prepareLocal(ctx, spec, ws)
	}
	return nil, fmt.Errorf("unknown sandbox %q: want docker or local", spec.Sandbox)
}

func prepareDocker(ctx context.Context, spec Spec, ws Workspace) (*Prepared, error) {
	rootless, err := CheckDocker(ctx)
	if err != nil {
		return nil, err
	}
	// A local harness has the user's home, where a2a-serve doesn't write.
	if err := LinkSkills(spec.Profile, ws); err != nil {
		return nil, fmt.Errorf("linking project skills: %w", err)
	}
	image, what, err := EnsureImage(ctx, spec.Image, spec.Profile, spec.Progress)
	if err != nil {
		return nil, err
	}
	dr := &DockerRunner{
		Name:     spec.Name,
		Image:    image,
		Mounts:   slices.Clone(spec.Mounts),
		Hidden:   spec.Hidden,
		Home:     spec.Home,
		Env:      cliEnv(spec.Env),
		Limits:   spec.Limits,
		Labels:   spec.Labels,
		Rootless: rootless,
	}
	if spec.MCPMask != "" {
		masks, err := maskMCP(spec.MCPMask, spec.Mounts)
		if err != nil {
			return nil, err
		}
		dr.Mounts = append(dr.Mounts, masks...)
	}
	if spec.Socket != "" {
		dr.Mounts = append(dr.Mounts, agentdir.Mount{Src: filepath.Dir(spec.Socket), Dst: SandboxDir, ReadOnly: true})
		dr.Env = append(dr.Env, EnvUnixSocket+"="+filepath.Join(SandboxDir, filepath.Base(spec.Socket)))
	}
	cmd, err := dr.Command(ctx, spec.Profile.Command(ws))
	if err != nil {
		return nil, err
	}
	return &Prepared{Args: cmd.Args, Env: cmd.Env, Dir: cmd.Dir, Summary: "docker: " + what, Stop: dr.Stop, Check: func() error { return checkPaths(spec) }}, nil
}

// emptyMCP is a .mcp.json with no servers.
const emptyMCP = "{\"mcpServers\": {}}\n"

// maskMCP writes the file mask with no MCP servers and returns the mounts
// which put it over the workspace's .mcp.json wherever mounts expose it. A
// workspace without .mcp.json gets one with no servers, since docker
// would otherwise create an empty file to mount over.
func maskMCP(mask string, mounts []agentdir.Mount) ([]agentdir.Mount, error) {
	if err := fsutil.WriteAtomic(mask, []byte(emptyMCP), 0o644); err != nil {
		return nil, fmt.Errorf("writing %s: %w", mask, err)
	}
	file := filepath.Join(mounts[0].Src, project.MCPFile)
	if err := writeIfMissing(file, []byte(emptyMCP)); err != nil {
		return nil, fmt.Errorf("creating %s with no servers, which hides the project's MCP servers from the agent: %w", file, err)
	}
	var out []agentdir.Mount
	for _, dst := range hiddenDsts(mounts, []string{file}) {
		out = append(out, agentdir.Mount{Src: mask, Dst: dst, ReadOnly: true})
	}
	return out, nil
}

func writeIfMissing(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return errors.Join(err, f.Close())
	}
	return f.Close()
}

// cliEnv returns env with the a2a CLI timeout added unless env sets it.
func cliEnv(env []string) []string {
	if slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, EnvTimeout+"=") }) {
		return slices.Clone(env)
	}
	return append([]string{EnvTimeout + "=" + CLITimeout.String()}, env...)
}

func prepareLocal(ctx context.Context, spec Spec, ws Workspace) (*Prepared, error) {
	c := spec.Profile.Command(ws)
	c.Env = append(c.Env, cliEnv(spec.Env)...)
	if spec.CLI != "" {
		c.Env = append(c.Env, "PATH="+filepath.Dir(spec.CLI)+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	if spec.Socket != "" {
		c.Env = append(c.Env, EnvUnixSocket+"="+spec.Socket)
	}
	cmd, err := LocalRunner{}.Command(ctx, c)
	if err != nil {
		return nil, err
	}
	return &Prepared{Args: cmd.Args, Env: cmd.Env, Dir: cmd.Dir, Summary: "no sandbox", Stop: func(context.Context) error { return nil }, Check: func() error { return nil }}, nil
}

// TCPBridge reports whether a harness in sandbox must reach the agent
// server over TCP rather than a Unix socket, and where the server listens
// (bind) and the harness connects (dial). Docker Desktop can't mount a
// socket but forwards host.docker.internal to the host's loopback.
func TCPBridge(sandbox string) (bind, dial string, ok bool) {
	if sandbox == SandboxLocal || runtime.GOOS == "linux" {
		return "", "", false
	}
	return "127.0.0.1", "host.docker.internal", true
}
