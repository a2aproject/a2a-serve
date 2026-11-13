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
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/egress"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
	"github.com/a2aproject/a2a-serve/internal/project"
	"github.com/a2aproject/a2a-serve/internal/sandbox"
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
	// Sandbox is SandboxDocker (the default) or SandboxLocal, and Backend
	// the backend that runs it.
	Sandbox string
	Backend sandbox.Backend
	// Base is the sandbox that Prepare completes: its name, image, limits,
	// labels and the writers of its output. The image defaults to the
	// profile's.
	Base sandbox.Spec
	// Mounts are the directories the harness works on. The first is the
	// workspace and must keep its path in the sandbox.
	Mounts []agentdir.Mount
	// WorkspaceSrc is the host directory a container mounts at the path of
	// the workspace in its place, such as a private copy of it. Empty
	// mounts the workspace. A sandbox still hides the paths of Hidden and
	// masks .mcp.json at the workspace's path.
	WorkspaceSrc string
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
	// ConfigMask is the host path of an empty configuration file, which a
	// sandbox mounts over the agent's own configuration files in the
	// workspace and the mounts, so that the agent's permissions come only
	// from the server. Empty keeps them visible.
	ConfigMask string
	// PolicyDir is the host folder where [WritePolicy] wrote the settings
	// that the agent can't override, which a sandbox mounts read-only where
	// the agent reads them. Empty leaves them out.
	PolicyDir string
	// LockHome makes the agent's own configuration in Home read-only in a
	// sandbox, such as for the agents of members and guests, who could
	// otherwise change the permissions that the owner set for them.
	LockHome bool
	// Env holds NAME=value pairs for the harness, such as API keys.
	Env []string
	// Model is the model of the harness, in its own format. Empty keeps
	// the harness default.
	Model string

	// Server is the endpoint through which the harness reaches the server:
	// a Unix socket, or a TCP listener for runtimes that can't mount one.
	// Empty when the harness doesn't reach the server.
	Server sandbox.Socket
	// CLI is the host path of the a2a binary put first on the PATH of a
	// local harness. A harness in Docker uses the a2a of its image.
	CLI string
	// EgressMode is the egress setting. Unless it is unrestricted, a
	// container gets no network but the boundary OpenEgress opens, where
	// the host allows it. Nil OpenEgress gives containers Docker's network.
	EgressMode  string
	EgressAllow []string
	OpenEgress  sandbox.OpenEgress
	// Inject are the rules of the headers the boundary adds; with any, a
	// container doesn't start without the boundary. EgressCA is the PEM
	// certificate the boundary signs their hosts with, which the
	// container trusts.
	Inject   []agentdir.InjectRule
	EgressCA []byte
}

// Prepared is a harness ready to start in its sandbox. Start refuses to
// start the harness again when the agent could have redirected its sandbox
// since Prepare, or when another agent was created in a mount that the
// sandbox doesn't hide. Close removes the sandbox, such as its container.
type Prepared struct {
	sandbox.Sandbox
	// Summary describes the sandbox for the user.
	Summary string
	// Warnings are problems of the sandbox that don't stop the harness.
	Warnings []string
	// Egress is set when the container's connections go through the
	// server.
	Egress bool

	// problem is why the container got Docker's own network at Prepare
	// although the egress setting asks for the boundary, and fallback the
	// container that switches to it at a later start.
	problem  string
	fallback *boundaryFallback
}

// NoBoundary returns why the container has Docker's own network although
// the egress setting asks for the boundary, or "".
func (p *Prepared) NoBoundary() string {
	if p.problem != "" || p.fallback == nil {
		return p.problem
	}
	return p.fallback.problem()
}

// Prepare bootstraps the harness home and returns the sandbox that runs the
// harness.
func Prepare(ctx context.Context, spec Spec) (*Prepared, error) {
	if len(spec.Mounts) == 0 {
		return nil, fmt.Errorf("no workspace to mount")
	}
	// ACP sessions get the workspace as cwd, and the executor starts the
	// agent there, so the workspace must have the same path on both sides.
	if ws := spec.Mounts[0]; ws.Dst != ws.Src {
		return nil, fmt.Errorf("the first --mount is the workspace and must keep its path (%s), got %s", ws.Src, ws.Dst)
	}
	if spec.Sandbox != "" && spec.Sandbox != SandboxDocker && spec.Sandbox != SandboxLocal {
		return nil, fmt.Errorf("unknown sandbox %q: want docker or local", spec.Sandbox)
	}
	if err := PrepareHome(spec); err != nil {
		return nil, err
	}
	spec.Env = withProviderKeys(spec.Profile, spec.Env)
	nested, err := nestedAgents(spec)
	var warnings []string
	switch {
	case err != nil && spec.Sandbox == SandboxLocal:
		warnings = append(warnings, fmt.Sprintf("searching the mounts for other agents: %v", err))
	case err != nil:
		return nil, err
	case len(nested) > 0 && spec.Sandbox == SandboxLocal:
		warnings = append(warnings, fmt.Sprintf("found other agents in %s; --sandbox local doesn't hide their .a2a directories, so the agent can read their secrets", agentRoots(nested)))
	case len(nested) > 0:
		warnings = append(warnings, fmt.Sprintf("found other agents in %s; the sandbox hides their .a2a directories", agentRoots(nested)))
		spec.Hidden = append(slices.Clone(spec.Hidden), nested...)
	}
	ws := Workspace{Dir: spec.Mounts[0].Src, Home: spec.Home}
	if err := spec.Profile.Bootstrap(ws); err != nil {
		return nil, fmt.Errorf("bootstrapping %s: %w", spec.Profile.Name(), err)
	}
	if spec.Backend == nil {
		return nil, fmt.Errorf("no backend for the %s sandbox", cmp.Or(spec.Sandbox, SandboxDocker))
	}
	var p *Prepared
	if spec.Sandbox == SandboxLocal {
		p, err = prepareLocal(ctx, spec, ws)
	} else {
		p, err = prepareDocker(ctx, spec, ws)
	}
	if err != nil {
		return nil, err
	}
	p.Warnings = append(warnings, p.Warnings...)
	return p, nil
}

// maxNestedScan bounds the directories that a search of the mounts for
// other agents visits.
const maxNestedScan = 500_000

// nestedAgents returns the .a2a entries of other agents in the mounts of
// spec, those that spec doesn't hide yet. A mount that exposes the user's
// home directory, which the owner allowed with --allow-broad-mount,
// exposes the agents in it, and isn't searched.
func nestedAgents(spec Spec) ([]string, error) {
	hidden := make([]string, len(spec.Hidden))
	for i, h := range spec.Hidden {
		hidden[i] = fsutil.ResolvePath(h)
	}
	var out []string
	for _, m := range spec.Mounts {
		broad, err := agentdir.BroadMount(m.Src)
		if err != nil {
			return nil, err
		}
		if broad {
			continue
		}
		found, err := agentdir.FindBelow(fsutil.ResolvePath(m.Src), maxNestedScan)
		if err != nil {
			return nil, err
		}
		for _, p := range found {
			if !slices.Contains(hidden, p) && !slices.Contains(out, p) {
				out = append(out, p)
			}
		}
	}
	return out, nil
}

// checkNoNewAgents refuses a container start after another agent was
// created in a mount since Prepare: the container would expose its .a2a.
func checkNoNewAgents(spec Spec) error {
	nested, err := nestedAgents(spec)
	if err != nil {
		return err
	}
	if len(nested) > 0 {
		return fmt.Errorf("another agent was created in %s after this agent started, and its sandbox doesn't hide it yet; restart this agent with `a2a-serve restart`", agentRoots(nested))
	}
	return nil
}

// agentRoots lists the directories holding the .a2a entries.
func agentRoots(a2a []string) string {
	roots := make([]string, len(a2a))
	for i, p := range a2a {
		roots[i] = filepath.Dir(p)
	}
	return strings.Join(roots, ", ")
}

func prepareDocker(ctx context.Context, spec Spec, ws Workspace) (*Prepared, error) {
	// A local harness has the user's home, where a2a-serve doesn't write.
	if err := LinkSkills(spec.Profile, ws); err != nil {
		return nil, fmt.Errorf("linking project skills: %w", err)
	}
	sb := spec.Base
	var err error
	if sb.Image, sb.Build, err = imageOf(spec.Base.Image, spec.Profile); err != nil {
		return nil, err
	}
	sb.Command = profileCommand(spec, ws)
	sb.Command.Env = append(cliEnv(spec.Env), sb.Command.Env...)
	sb.Mounts = sandboxMounts(spec.Mounts)
	sb.Masks = hiddenMasks(spec)
	sb.Home = spec.Home
	if spec.WorkspaceSrc != "" {
		sb.Mounts[0].Src = spec.WorkspaceSrc
	}
	if spec.MCPMask != "" {
		masks, err := maskMCP(spec.MCPMask, spec.Mounts)
		if err != nil {
			return nil, err
		}
		sb.Masks = append(sb.Masks, masks...)
	}
	cm := configMasks{check: func() error { return nil }}
	if spec.ConfigMask != "" {
		if cm, err = maskAgentConfig(spec); err != nil {
			return nil, err
		}
		sb.Mounts = append(sb.Mounts, cm.mounts...)
		sb.Masks = append(sb.Masks, cm.masks...)
		sb.Scratch = cm.scratch
	}
	if spec.PolicyDir != "" {
		dst, err := policyMount(spec)
		if err != nil {
			return nil, err
		}
		if dst != "" {
			sb.Mounts = append(sb.Mounts, sandbox.Mount{Src: spec.PolicyDir, Dst: dst, ReadOnly: true})
		}
	}
	if spec.LockHome {
		locks, err := lockHomeConfig(spec)
		if err != nil {
			return nil, err
		}
		sb.Mounts = append(sb.Mounts, locks...)
	}
	sb.Expose = serverSocket(spec)
	sb.Check = func() error {
		if err := checkPaths(spec); err != nil {
			return err
		}
		if err := checkNoNewAgents(spec); err != nil {
			return err
		}
		return cm.check()
	}

	cfg := spec.egressConfig()
	strict := StrictEgress(cfg)
	boundary := spec.OpenEgress != nil && spec.EgressMode != egress.ModeUnrestricted
	warnings := slices.Clone(cm.warnings)
	// The agent's container gets Docker's own network when the boundary
	// can't work on this host, unless the config needs the boundary. Such a
	// container is prepared first, so that the one with the boundary runs
	// the image it resolved.
	var open sandbox.Sandbox
	var what string
	if !boundary || !strict {
		if open, err = spec.Backend.Prepare(ctx, sb); err != nil {
			return nil, err
		}
		info := open.Info()
		what = info.Summary
		sb.Image, sb.Build = info.Image, nil
	}
	if !boundary {
		if len(spec.Inject) > 0 {
			return nil, errors.Join(errors.New(noInject(cfg, "the agent's container has no egress boundary")), open.Close(ctx))
		}
		return &Prepared{Sandbox: open, Summary: what, Warnings: warnings}, nil
	}
	sb.Network = sandbox.Network{Boundary: spec.OpenEgress, CA: spec.EgressCA, Recheck: !strict}
	p, err := spec.Backend.Prepare(ctx, sb)
	if errors.Is(err, sandbox.ErrNoBoundary) {
		d := problemDecision(cfg, noBoundaryReason(err))
		if strict {
			return nil, errors.New(d.Warning)
		}
		warnings = append(warnings, d.Warning)
		return &Prepared{Sandbox: open, Summary: what, Warnings: warnings, problem: d.Warning}, nil
	}
	if err != nil {
		if open != nil {
			err = errors.Join(err, open.Close(ctx))
		}
		return nil, err
	}
	info := p.Info()
	if strict {
		what = info.Summary
	}
	if d := DecideEgress(cfg, func() sandbox.Capabilities { return sandbox.Capabilities{EgressRelay: info.Relay} }); d.Warning != "" {
		warnings = append(warnings, d.Warning)
	}
	what += "; egress " + cmp.Or(spec.EgressMode, egress.ModeInternet) + " through the server"
	if info.Relay {
		what += " (egress container)"
	}
	prepared := &Prepared{Sandbox: p, Summary: what, Warnings: warnings, Egress: true}
	if open != nil {
		prepared.fallback = &boundaryFallback{boundary: p, open: open, stderr: sb.Stderr}
		prepared.Sandbox = prepared.fallback
	}
	return prepared, nil
}

// noBoundaryReason says why the boundary of err, which wraps
// [sandbox.ErrNoBoundary], can't work.
func noBoundaryReason(err error) string {
	if _, ok := errors.AsType[*sandbox.EgressSetupError](err); ok {
		return "the agent's container can't set up its network: " + err.Error()
	}
	return err.Error()
}

// serverSocket returns the endpoint of the server that the harness of spec
// reaches, if any.
func serverSocket(spec Spec) []sandbox.Socket {
	if spec.Server.Path == "" && spec.Server.Addr == "" {
		return nil
	}
	s := spec.Server
	s.Env = EnvUnixSocket
	return []sandbox.Socket{s}
}

// egressConfig returns the settings of spec that decide its egress.
func (spec Spec) egressConfig() *agentdir.Config {
	return &agentdir.Config{Sandbox: spec.Sandbox, Egress: agentdir.EgressConfig{Mode: spec.EgressMode, Allow: spec.EgressAllow}, Inject: spec.Inject}
}

// ContainerHome is the home directory of the harness in the container.
const ContainerHome = sandbox.ContainerHome

// hiddenMasks returns the masks that lay an empty directory over the
// hidden paths of spec wherever its mounts expose them.
func hiddenMasks(spec Spec) []sandbox.Mask {
	var out []sandbox.Mask
	for _, dst := range sandbox.ContainerPaths(sandboxMounts(spec.Mounts), spec.Hidden) {
		out = append(out, sandbox.Mask{Dst: dst})
	}
	return out
}

// sandboxMounts converts the mounts of an agent to those of a sandbox.
func sandboxMounts(mounts []agentdir.Mount) []sandbox.Mount {
	out := make([]sandbox.Mount, len(mounts))
	for i, m := range mounts {
		out[i] = sandbox.Mount{Src: m.Src, Dst: m.Dst, ReadOnly: m.ReadOnly}
	}
	return out
}

// emptyMCP is a .mcp.json with no servers.
const emptyMCP = "{\"mcpServers\": {}}\n"

// maskMCP writes the file mask with no MCP servers and returns the mounts
// which put it over the workspace's .mcp.json wherever mounts expose it. A
// workspace without .mcp.json gets one with no servers, since docker
// would otherwise create an empty file to mount over.
func maskMCP(mask string, mounts []agentdir.Mount) ([]sandbox.Mask, error) {
	if err := fsutil.WriteAtomic(mask, []byte(emptyMCP), 0o644); err != nil {
		return nil, fmt.Errorf("writing %s: %w", mask, err)
	}
	file := filepath.Join(mounts[0].Src, project.MCPFile)
	if err := writeIfMissing(file, []byte(emptyMCP)); err != nil {
		return nil, fmt.Errorf("creating %s with no servers, which hides the project's MCP servers from the agent: %w", file, err)
	}
	var out []sandbox.Mask
	for _, dst := range sandbox.ContainerPaths(sandboxMounts(mounts), []string{file}) {
		out = append(out, sandbox.Mask{Src: mask, Dst: dst})
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

// profileCommand is the command of the profile, with the model of spec.
// The model's variables replace the command's own of the same name.
func profileCommand(spec Spec, ws Workspace) Command {
	c := spec.Profile.Command(ws)
	if spec.Model == "" {
		return c
	}
	model := spec.Profile.ModelEnv(spec.Model)
	env := slices.DeleteFunc(slices.Clone(c.Env), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return slices.ContainsFunc(model, func(m string) bool { return strings.HasPrefix(m, name+"=") })
	})
	c.Env = append(env, model...)
	return c
}

// cliEnv returns env with the a2a CLI timeout added unless env sets it.
func cliEnv(env []string) []string {
	if slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, EnvTimeout+"=") }) {
		return slices.Clone(env)
	}
	return append([]string{EnvTimeout + "=" + CLITimeout.String()}, env...)
}

func prepareLocal(ctx context.Context, spec Spec, ws Workspace) (*Prepared, error) {
	if len(spec.Inject) > 0 {
		return nil, errors.New(noInject(spec.egressConfig(), "the agent runs with --sandbox local"))
	}
	sb := spec.Base
	sb.Command = profileCommand(spec, ws)
	sb.Command.Env = append(sb.Command.Env, cliEnv(spec.Env)...)
	if spec.CLI != "" {
		sb.Command.Env = append(sb.Command.Env, "PATH="+filepath.Dir(spec.CLI)+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	sb.Expose = serverSocket(spec)
	path := sb.Command.Path
	sb.Check = func() error { return checkInstalled(path, ws.Dir) }
	p, err := spec.Backend.Prepare(ctx, sb)
	if err != nil {
		return nil, err
	}
	return &Prepared{Sandbox: p, Summary: p.Info().Summary}, nil
}

// checkInstalled refuses to start an agent without a sandbox whose command
// isn't on PATH, or whose path, relative to the workspace dir, is missing.
func checkInstalled(command, dir string) error {
	missing := fmt.Errorf("%s is not installed; install it, or run a2a-serve config set --sandbox docker, or run a2a-serve doctor", command)
	if !strings.ContainsRune(command, filepath.Separator) {
		if _, err := exec.LookPath(command); errors.Is(err, exec.ErrNotFound) {
			return missing
		}
		return nil
	}
	path := command
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return missing
	}
	return nil
}

// TCPBridge reports whether a harness in sandbox kind must reach the agent
// server over TCP rather than a Unix socket, and where the server listens
// (bind) and the harness connects (dial), as [sandbox.DockerTCPBridge] says.
func TCPBridge(kind string) (bind, dial string, ok bool) {
	if kind == SandboxLocal {
		return "", "", false
	}
	return sandbox.DockerTCPBridge()
}
