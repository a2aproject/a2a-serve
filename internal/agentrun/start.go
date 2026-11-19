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
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/integrations/executor/a2aacp"

	"github.com/a2aproject/a2a-serve/core/sandbox"
	"github.com/a2aproject/a2a-serve/internal/agentprofile"
	"github.com/a2aproject/a2a-serve/internal/boundary"
	"github.com/a2aproject/a2a-serve/internal/sandboxrt"
)

// agentSocketName is the name of the Unix socket an agent process reaches
// the caller through, in its directory of the socket directory.
const agentSocketName = "a2a.sock"

// startTimeoutError is the error of a main process that didn't start in
// time.
type startTimeoutError struct{ err error }

func (e *startTimeoutError) Error() string { return e.err.Error() }

func (e *startTimeoutError) Unwrap() error { return e.err }

func (e *startTimeoutError) Is(target error) bool { return target == ErrStartTimeout }

// startMain starts the main process as an ACP agent in its sandbox. close
// stops it and cleans up its sandbox.
func (r *Runner) startMain(ctx context.Context) (exec a2asrv.AgentExecutor, close func(context.Context) error, err error) {
	if err := r.pruneContainers(ctx); err != nil {
		return nil, nil, err
	}
	r.pruneEnvFiles()
	h, err := r.prepare(ctx, r.rt.imageProgress(r.cfg.Stderr), r.cfg.Stderr, "", r.cfg.Main)
	if err != nil {
		return nil, nil, err
	}
	if hook := r.cfg.Options.MainPrepared; hook != nil {
		hook(Prepared{Summary: h.prepared.Summary, Warnings: h.prepared.Warnings, Egress: h.prepared.Egress, NoBoundary: h.prepared.NoBoundary()})
	}
	acp, err := a2aacp.NewExecutor(ctx, h.process(), r.acpConfig(nil))
	if err != nil {
		err = fmt.Errorf("starting %s: %w", r.cfg.Profile.Name(), err)
		if errors.Is(err, context.DeadlineExceeded) {
			err = &startTimeoutError{err: err}
		}
		return nil, nil, errors.Join(err, h.stop(ctx))
	}
	r.logf("✓ %s is running (ACP)", r.cfg.Profile.Name())
	since := time.Now()
	r.mainSince.Store(&since)
	return r.turns.guard(r.stalls.guard(r.toolsDone.guard(acp))), func(ctx context.Context) error { return errors.Join(acp.Close(), h.stop(ctx)) }, nil
}

func (r *Runner) logf(format string, args ...any) {
	if _, err := fmt.Fprintf(r.cfg.Options.Out, format+"\n", args...); err != nil {
		r.log().Debug("writing progress", "error", err)
	}
}

// pruneContainers removes the containers of this runner that an earlier
// run left running, such as those of a crash or a stop that ran out of
// time. It fails only when docker doesn't answer, which would stall the
// start.
func (r *Runner) pruneContainers(ctx context.Context) error {
	if r.rt.kind != agentprofile.SandboxDocker || len(r.cfg.Options.PruneLabels) == 0 {
		return nil
	}
	n, err := sandboxrt.PruneContainers(ctx, r.cfg.Options.PruneLabels)
	if errors.Is(err, sandboxrt.ErrDockerNoAnswer) {
		return err
	}
	if err != nil {
		r.log().Warn("removing containers left by an earlier run", "error", err)
		return nil
	}
	if n > 0 {
		r.log().Info("removed containers left by an earlier run", "count", n)
	}
	return nil
}

// envFilesDir holds the env files of containers: in the state directory of
// the Docker runtime, if it has one.
func (r *Runner) envFilesDir() string {
	if r.rt.info.StateDir != "" {
		return filepath.Join(r.rt.info.StateDir, "env")
	}
	return filepath.Join(r.stateDir, "run", "env")
}

// egressDir holds the sockets of the egress boundaries of containers: in
// the state directory of the Docker runtime, if it has one.
func (r *Runner) egressDir() string {
	if r.rt.info.StateDir != "" {
		return filepath.Join(r.rt.info.StateDir, "egress")
	}
	return r.socketDir()
}

// pruneEnvFiles removes the env files of containers that an earlier run
// left behind, before any container starts.
func (r *Runner) pruneEnvFiles() {
	n, err := sandboxrt.PruneEnvFiles(r.envFilesDir())
	if err != nil {
		r.log().Warn("removing env files left by an earlier run", "error", err)
	}
	if n > 0 {
		r.log().Info("removed env files left by an earlier run", "count", n)
	}
}

// startPool starts the executor of the pool name, whose agent processes
// start on their first turn and stop when idle.
func (r *Runner) startPool(ctx context.Context, name string) (a2asrv.AgentExecutor, func(context.Context) error, error) {
	pool, ok := r.cfg.Pools[name]
	if !ok {
		return nil, nil, fmt.Errorf("no pool %q", name)
	}
	procs := r.pools[name]
	start := a2aacp.AgentFunc(func(ctx context.Context, key string) (io.ReadWriteCloser, error) {
		return r.startPoolAgent(ctx, procs, key)
	})
	cfg := r.acpConfig(procs)
	cfg.Pool = &a2aacp.PoolConfig{Key: r.keys.of, MaxAgents: pool.MaxProcesses}
	cfg.QuestionTimeout = pool.QuestionTimeout
	acp, err := a2aacp.NewExecutor(ctx, start, cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("preparing %s: %w", cmp.Or(r.cfg.Options.PoolLabels[name], "the agent processes of the pool "+name), err)
	}
	return r.turns.guard(r.stalls.guard(r.toolsDone.guard(acp))), func(context.Context) error { return acp.Close() }, nil
}

// countedTurn counts each turn in the agent process of its conversation
// among procs, so that new tasks are refused while all processes are busy.
func (r *Runner) countedTurn(procs *procs) func(ctx context.Context, execCtx *a2asrv.ExecutorContext) (end func()) {
	return func(ctx context.Context, execCtx *a2asrv.ExecutorContext) (end func()) {
		key, err := r.keys.of(ctx, execCtx.ContextID)
		if err != nil {
			r.log().Warn("looking up the session of a conversation", "context", execCtx.ContextID, "error", err)
		}
		endTurn := procs.turn(key)
		endHook := r.hookTurn(ctx, execCtx)
		return func() {
			if endHook != nil {
				endHook()
			}
			endTurn()
		}
	}
}

// hookTurn runs Hooks.Turn, if any.
func (r *Runner) hookTurn(ctx context.Context, execCtx *a2asrv.ExecutorContext) func() {
	if r.cfg.Hooks.Turn == nil {
		return func() {}
	}
	return r.cfg.Hooks.Turn(ctx, execCtx)
}

// startPoolAgent starts the agent process of key among procs, unless
// Hooks.CanStart refuses it.
func (r *Runner) startPoolAgent(ctx context.Context, procs *procs, key string) (io.ReadWriteCloser, error) {
	env, err := r.environment(ctx, key)
	if err != nil {
		return nil, err
	}
	parent := env.process().Parent
	if parent != "" && parent != key {
		// Stopping the parent waits for the processes under it to start,
		// and holds new ones until it ends.
		base := procs.get(parent)
		base.childrenMu.RLock()
		defer base.childrenMu.RUnlock()
	}
	ua := procs.get(key)
	ua.startMu.Lock()
	defer ua.startMu.Unlock()
	procs.setParent(ua, parent)
	if canStart := r.cfg.Hooks.CanStart; canStart != nil {
		if err := canStart(ctx, key); err != nil {
			return nil, err
		}
	}
	release, err := procs.reserve(ua)
	if err != nil {
		return nil, err
	}
	defer release()
	if env, err = r.environment(ctx, key); err != nil {
		return nil, err
	}
	h, err := r.prepare(ctx, r.cfg.Stderr, r.cfg.Stderr, key, env)
	if err != nil {
		return nil, err
	}
	rwc, err := h.process().Start(ctx, key)
	if err != nil {
		return nil, errors.Join(err, h.stop(context.WithoutCancel(ctx)))
	}
	r.log().Info("started an agent process", "key", key, "sandbox", h.prepared.Summary, "storage", env.process().Storage)
	var started io.ReadWriteCloser
	started = a2aacp.WithCleanup(rwc, func() error {
		procs.exited(ua, started)
		return h.stop(context.Background())
	})
	procs.started(ua, started, env.process().State)
	return started, nil
}

// prepared is an agent ready to start as an ACP agent.
type prepared struct {
	prepared *agentprofile.Prepared
	// stop cleans up after the agent exited.
	stop func(context.Context) error
	// maxMessage bounds a message from the agent.
	maxMessage int
}

func (h *prepared) process() agentProcess {
	return agentProcess{sandbox: h.prepared, maxMessage: h.maxMessage}
}

// agentProcess starts the agent process in its sandbox, which checks the
// sandbox before every start, since a pool starts it again after it stops.
type agentProcess struct {
	sandbox sandboxrt.Sandbox
	// maxMessage bounds a message from the agent.
	maxMessage int
}

// Start implements [a2aacp.Agent]. A message from the agent over
// maxMessage fails the connection, and Close returns once the reader saw
// the output end.
func (p agentProcess) Start(ctx context.Context, _ string) (io.ReadWriteCloser, error) {
	rwc, err := p.sandbox.Start(ctx)
	if err != nil {
		return nil, err
	}
	return newAwaitedClose(newBoundedMessages(rwc, p.maxMessage), agentReadEndGrace), nil
}

// prepare prepares the agent process of key with env. The process writes
// its stderr to stderr.
func (r *Runner) prepare(ctx context.Context, imageProgress, stderr io.Writer, key string, env Environment) (h *prepared, err error) {
	proc := env.process()
	labels := maps.Clone(env.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	spec := agentprofile.Spec{
		Profile: r.cfg.Profile,
		Sandbox: r.rt.kind,
		Backend: r.rt.backend,
		Base: sandboxrt.Spec{
			Name:     env.Name,
			Image:    env.Image,
			Limits:   sandboxrt.Limits(env.Limits),
			Labels:   labels,
			Progress: imageProgress,
			Stderr:   stderr,
		},
		Home:       env.Home,
		Hidden:     env.Hide,
		Pins:       r.cfg.Options.Pins,
		MCPMask:    filepath.Join(r.stateDir, "mcp-mask.json"),
		ConfigMask: filepath.Join(r.stateDir, "config-mask.json"),
		Env:        slices.Clone(env.Env),
		Model:      r.cfg.Model,
	}
	if err := agentprofile.PrepareHome(agentprofile.Spec{Sandbox: r.rt.kind, Mounts: r.homeMounts(env), Hidden: env.Hide, Home: env.Home, Pins: spec.Pins}); err != nil {
		return nil, err
	}
	if r.rt.kind == agentprofile.SandboxDocker {
		spec.LockHome = proc.LockHome
		if spec.PolicyDir, err = r.policies.write(r.cfg.Profile, r.askCommands(), spec.LockHome, r.mcpServerNames()); err != nil {
			return nil, err
		}
	}
	if proc.StartError != nil {
		return nil, proc.StartError
	}
	if env.Workspace == nil {
		return nil, errors.New("no workspace to mount")
	}
	att, err := env.Workspace(ctx, key, r.stateDir)
	if err != nil {
		return nil, err
	}
	var cleanups []func() error
	if att.Release != nil {
		cleanups = append(cleanups, att.Release)
	}
	cleanup := func() error {
		var errs []error
		for _, c := range slices.Backward(cleanups) {
			errs = append(errs, c())
		}
		return errors.Join(errs...)
	}
	defer func() {
		if err == nil {
			return
		}
		if cerr := cleanup(); cerr != nil {
			err = errors.Join(err, cerr)
		}
	}()
	pm := workspaceMounts(att, env.Mounts)
	spec.Mounts, spec.WorkspaceSrc = pm.dirs, pm.workspaceSrc
	if r.rt.custom == nil {
		if err := r.tmpfsMounts(&spec, pm.tmpfs); err != nil {
			return nil, err
		}
	}
	if key == "" {
		state := proc.State
		r.mainState.Store(&state)
	}
	if hook := r.cfg.Hooks.PrepareHome; hook != nil {
		if err := hook(ctx, key, env); err != nil {
			return nil, err
		}
	}
	var custom sandbox.Spec
	if !proc.NoAgentAPI && r.cfg.AgentAPI != nil {
		ep := r.main
		if key != "" {
			if ep, err = r.listeners.open(key); err != nil {
				return nil, err
			}
			cleanups = append(cleanups, ep.close)
		}
		if err := r.connect(&spec, ep); err != nil {
			return nil, err
		}
		if ep.ln != nil {
			custom.Sockets = []sandbox.Socket{{Listener: ep.ln, Env: agentprofile.EnvUnixSocket}}
		}
		if r.rt.kind == agentprofile.SandboxLocal {
			spec.CLI = r.cfg.Options.CLI
		}
	}
	closeEgress, err := r.egressSandbox(&spec, &custom, key, env)
	if err != nil {
		return nil, err
	}
	cleanups = append(cleanups, closeEgress)
	var p *agentprofile.Prepared
	if r.rt.custom != nil {
		p, err = r.rt.prepareCustom(ctx, spec, pm.custom, custom)
	} else {
		p, err = agentprofile.Prepare(ctx, spec)
	}
	if err != nil {
		return nil, err
	}
	problem := p.NoBoundary()
	r.egress.started(spec.EgressMode, problem)
	forget := func() {}
	if problem == "" {
		// A container can still fall back to Docker's network at a later start.
		forget = r.egress.watch(p.NoBoundary)
	}
	seen := spec.Mounts[0].Src
	r.workspaces.Store(key, seen)
	detach := r.turns.attach(key, att)
	stop := func(ctx context.Context) error {
		forget()
		detach()
		r.workspaces.CompareAndDelete(key, seen)
		cerr := cleanup()
		if cerr != nil {
			r.log().Warn("releasing the workspace of a stopped agent process", "key", key, "error", cerr)
		}
		return errors.Join(p.Close(ctx), cerr)
	}
	return &prepared{prepared: p, stop: stop, maxMessage: r.cfg.Options.MaxAgentMessage}, nil
}

// homeMounts returns the mounts that the home of env is checked against
// before anything is written into it: those of the agent's settings, or
// those of env.
func (r *Runner) homeMounts(env Environment) []agentprofile.Mount {
	if r.cfg.Options.Mounts != nil {
		return r.cfg.Options.Mounts
	}
	var out []agentprofile.Mount
	for _, m := range env.Mounts {
		if m.Src.Kind() == sandbox.MountHostDir {
			out = append(out, agentprofile.Mount{Src: m.Src.HostPath(), Dst: m.Dst, ReadOnly: m.ReadOnly})
		}
	}
	return out
}

// processMounts are the mounts of a process.
type processMounts struct {
	// dirs are the host directories, the workspace first at the path the
	// process sees it, and workspaceSrc the directory a container mounts
	// there in its place, if another.
	dirs         []agentprofile.Mount
	workspaceSrc string
	// tmpfs are the mounts in memory.
	tmpfs []sandbox.Mount
	// custom are the mounts for a custom runtime.
	custom []sandbox.Mount
}

// workspaceMounts returns the mounts of a process whose workspace is att.
func workspaceMounts(att Attachment, extra []sandbox.Mount) processMounts {
	seen := cmp.Or(att.Path, att.Dir)
	pm := processMounts{
		dirs:   []agentprofile.Mount{{Src: seen, Dst: seen, ReadOnly: att.ReadOnly}},
		custom: []sandbox.Mount{{Dst: seen, Src: sandbox.HostDir(att.Dir), ReadOnly: att.ReadOnly}},
	}
	for _, m := range extra {
		pm.custom = append(pm.custom, m)
		switch m.Src.Kind() {
		case sandbox.MountHostDir:
			pm.dirs = append(pm.dirs, agentprofile.Mount{Src: m.Src.HostPath(), Dst: m.Dst, ReadOnly: m.ReadOnly})
		case sandbox.MountTmpfs:
			pm.tmpfs = append(pm.tmpfs, m)
		}
	}
	if att.Dir != seen {
		pm.workspaceSrc = att.Dir
	}
	return pm
}

// tmpfsMounts gives the sandbox of spec the mounts in memory: a writable
// one as scratch, and a read-only one as an empty mask. The local sandbox
// has none, and leaves the host directories where they are, as the
// command sees them.
func (r *Runner) tmpfsMounts(spec *agentprofile.Spec, mounts []sandbox.Mount) error {
	for _, m := range mounts {
		if r.rt.kind == agentprofile.SandboxLocal {
			return &sandbox.UnsupportedMountError{Backend: "local", Dst: m.Dst, Kind: m.Src.Kind(), Reason: "the command runs on the host, with no mount namespace"}
		}
		if m.ReadOnly {
			spec.Base.Masks = append(spec.Base.Masks, sandboxrt.Mask{Dst: m.Dst})
		} else {
			spec.Base.Scratch = append(spec.Base.Scratch, m.Dst)
		}
	}
	return nil
}

// egressSandbox gives spec the egress settings and an endpoint of the
// boundary for the process of key, with the credentials of env, and
// returns the function that closes the endpoint.
func (r *Runner) egressSandbox(spec *agentprofile.Spec, custom *sandbox.Spec, key string, env Environment) (func() error, error) {
	eg := r.egressSettings()
	spec.EgressMode, spec.EgressAllow, spec.Inject = eg.Mode, eg.Allow, eg.Inject
	if r.cfg.Egress == nil {
		return func() error { return nil }, nil
	}
	ep, err := r.cfg.Egress.Open(env.Name, key, env.Credentials...)
	if err != nil {
		return nil, err
	}
	be, err := boundary.EndpointOf(ep)
	if err != nil {
		return nil, errors.Join(err, ep.Close())
	}
	if r.cfg.Options.Egress == nil {
		// Credentials need the boundary, as the inject rules of the
		// frontend do.
		spec.EgressMode = be.Mode()
		for range env.Credentials {
			spec.Inject = append(spec.Inject, agentprofile.InjectRule{})
		}
	}
	if be.Router {
		spec.EgressMode, spec.EgressRouter = "", true
	}
	spec.EgressCA = be.CA
	spec.OpenEgress = r.boundaries.OpenerOf(be.Open)
	spec.Env = append(spec.Env, ep.Env()...)
	custom.Egress = ep
	return ep.Close, nil
}

// egressSettings returns the egress settings of the frontend, or none.
func (r *Runner) egressSettings() agentprofile.Egress {
	if r.cfg.Options.Egress == nil {
		return agentprofile.Egress{Sandbox: r.rt.kind}
	}
	return r.cfg.Options.Egress()
}

// connect gives spec the endpoint through which its agent process reaches
// the caller: the socket, or the key for the TCP listener in a credential
// helper in its home.
func (r *Runner) connect(spec *agentprofile.Spec, ep *endpoint) error {
	if ep.key == "" {
		spec.Server.Path = ep.socket
		return nil
	}
	spec.Server.Addr = strings.TrimPrefix(r.listeners.url, "http://")
	env, err := writeCredentialHelper(spec.Home, r.listeners.dial, ep.key)
	if err != nil {
		return fmt.Errorf("saving the agent key: %w", err)
	}
	spec.Env = append(spec.Env, env)
	return nil
}

// acpConfig is the config of the ACP executors of the runner. Turns of a
// pool's procs are counted there.
func (r *Runner) acpConfig(procs *procs) *a2aacp.Config {
	turn := r.hookTurn
	if procs != nil {
		turn = r.countedTurn(procs)
	}
	cfg := &a2aacp.Config{
		AutoAnswer:   r.decider,
		SessionStore: r.cfg.Sessions,
		Session:      r.sessionParams,
		Settings:     r.sessionSettings,
		Turn:         r.countAgentTurn(r.stalls.turn(r.turns.turn(turn))),
		Converter:    r.toolsDone.converter(r.stalls),
		Abandoned:    r.abandoned,
		Failure:      r.cfg.Hooks.Explain,
	}
	cfg.Prompt = r.turns.prompt(r.cfg.Hooks.Prompt)
	return cfg
}

// countAgentTurn counts each turn of next until its end, for Close.
func (r *Runner) countAgentTurn(next func(context.Context, *a2asrv.ExecutorContext) func()) func(context.Context, *a2asrv.ExecutorContext) func() {
	return func(ctx context.Context, execCtx *a2asrv.ExecutorContext) func() {
		r.agentTurns.Add()
		end := next(ctx, execCtx)
		return func() {
			defer r.agentTurns.Done()
			if end != nil {
				end()
			}
		}
	}
}

// abandoned stores the event that ends a task whose agent stopped while it
// waited for an answer, since the executor can't emit it. With
// KeepAbandonedOnStop, the agent exits when the runner stops, and the
// caller fails the open tasks then.
func (r *Runner) abandoned(ctx context.Context, ev *a2a.TaskStatusUpdateEvent) {
	if r.split == nil || r.cfg.Hooks.Abandoned == nil || r.split.stopped() && r.cfg.Options.KeepAbandonedOnStop {
		return
	}
	if err := r.cfg.Hooks.Abandoned(ctx, ev); err != nil {
		r.log().Warn("failing a task the agent stopped working on", "task", ev.TaskID, "error", err)
	}
}

// sessionSettings selects the model in every session, so that a model the
// agent doesn't know fails the turn.
func (r *Runner) sessionSettings(context.Context, *a2asrv.ExecutorContext) (map[string]any, error) {
	return r.cfg.Profile.SessionSettings(r.cfg.Model), nil
}

// listen opens the listeners through which agent processes reach the
// caller: Unix sockets, which are mounted into containers, or TCP with a
// key for Docker Desktop, which can't mount them.
func (r *Runner) listen(sandboxed bool) (*listeners, error) {
	if r.rt.kind != agentprofile.SandboxDocker {
		return listenSockets(r.socketDir())
	}
	bind, dial, tcp := agentprofile.TCPBridge(r.rt.kind)
	if !sandboxed || !tcp {
		return listenSockets(r.socketDir())
	}
	return listenTCP(bind, dial)
}

// workspaceOf returns where the process of key sees its workspace.
func (r *Runner) workspaceOf(key string) (string, error) {
	if ws, ok := r.workspaces.Load(key); ok {
		return ws.(string), nil
	}
	if len(r.cfg.Options.Mounts) > 0 {
		return r.cfg.Options.Mounts[0].Src, nil
	}
	return "", errors.New("no workspace to mount")
}
