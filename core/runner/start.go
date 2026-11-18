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

package runner

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/integrations/executor/a2aacp"

	"github.com/a2aproject/a2a-serve/core/agent"
	"github.com/a2aproject/a2a-serve/core/sandbox"
	"github.com/a2aproject/a2a-serve/core/workspace"
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
	h, err := r.prepare(ctx, r.cfg.ImageProgress, r.cfg.AgentLog, "")
	if err != nil {
		return nil, nil, err
	}
	if hook := r.cfg.Hooks.MainPrepared; hook != nil {
		hook(Prepared{Summary: h.prepared.Summary, Mounts: h.mounts, Warnings: h.prepared.Warnings, Egress: h.prepared.Egress, NoBoundary: h.prepared.NoBoundary()})
	}
	acp, err := a2aacp.NewExecutor(ctx, h.process(), r.acpConfig(nil))
	if err != nil {
		err = fmt.Errorf("starting %s: %w", h.profile.Name(), err)
		if errors.Is(err, context.DeadlineExceeded) {
			err = &startTimeoutError{err: err}
		}
		return nil, nil, errors.Join(err, h.stop(ctx))
	}
	r.logf("✓ %s is running (ACP)", h.profile.Name())
	return r.stalls.guard(acp), func(ctx context.Context) error { return errors.Join(acp.Close(), h.stop(ctx)) }, nil
}

func (r *Runner) logf(format string, args ...any) {
	if _, err := fmt.Fprintf(r.cfg.Out, format+"\n", args...); err != nil {
		slog.Debug("writing progress", "error", err)
	}
}

// pruneContainers removes the containers of this runner that an earlier
// run left running, such as those of a crash or a stop that ran out of
// time. It fails only when docker doesn't answer, which would stall the
// start.
func (r *Runner) pruneContainers(ctx context.Context) error {
	if sb := r.spec().Sandbox; sb != "" && sb != agent.SandboxDocker || len(r.cfg.PruneLabels) == 0 {
		return nil
	}
	n, err := sandbox.PruneContainers(ctx, r.cfg.PruneLabels)
	if errors.Is(err, sandbox.ErrDockerNoAnswer) {
		return err
	}
	if err != nil {
		slog.Warn("removing containers left by an earlier run", "error", err)
		return nil
	}
	if n > 0 {
		slog.Info("removed containers left by an earlier run", "count", n)
	}
	return nil
}

// pruneEnvFiles removes the env files of containers that an earlier run
// left behind, before any container starts.
func (r *Runner) pruneEnvFiles() {
	n, err := sandbox.PruneEnvFiles(r.cfg.Dirs.EnvFiles)
	if err != nil {
		slog.Warn("removing env files left by an earlier run", "error", err)
	}
	if n > 0 {
		slog.Info("removed env files left by an earlier run", "count", n)
	}
}

// startPool starts the executor of the pool name, whose agent processes
// start on their first turn and stop when idle.
func (r *Runner) startPool(ctx context.Context, name string) (a2asrv.AgentExecutor, func(context.Context) error, error) {
	procs, pool, err := r.pool(name)
	if err != nil {
		return nil, nil, err
	}
	start := a2aacp.AgentFunc(func(ctx context.Context, key string) (io.ReadWriteCloser, error) {
		return r.startPoolAgent(ctx, procs, key)
	})
	cfg := r.acpConfig(procs)
	cfg.Pool = &a2aacp.PoolConfig{Key: r.keys.of, MaxAgents: pool.MaxAgents}
	cfg.QuestionTimeout = pool.QuestionTimeout
	acp, err := a2aacp.NewExecutor(ctx, start, cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("preparing %s: %w", cmp.Or(pool.Label, "the agent processes of the pool "+name), err)
	}
	return r.stalls.guard(acp), func(context.Context) error { return acp.Close() }, nil
}

// pool returns the processes and the configuration of the pool name.
func (r *Runner) pool(name string) (*procs, Pool, error) {
	for _, p := range r.cfg.Pools {
		if p.Name == name {
			return r.pools[name], p, nil
		}
	}
	return nil, Pool{}, fmt.Errorf("no pool %q", name)
}

// countedTurn counts each turn in the agent process of its conversation
// among procs, so that new tasks are refused while all processes are busy.
// A process whose grant changed stops once its last turn ends.
func (r *Runner) countedTurn(procs *procs) func(ctx context.Context, execCtx *a2asrv.ExecutorContext) (end func()) {
	return func(ctx context.Context, execCtx *a2asrv.ExecutorContext) (end func()) {
		key, err := r.keys.of(ctx, execCtx.ContextID)
		if err != nil {
			slog.Warn("looking up the session of a conversation", "context", execCtx.ContextID, "error", err)
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
		return nil
	}
	return r.cfg.Hooks.Turn(ctx, execCtx)
}

// startPoolAgent starts the agent process of key among procs, unless
// Hooks.CanStart refuses it.
func (r *Runner) startPoolAgent(ctx context.Context, procs *procs, key string) (io.ReadWriteCloser, error) {
	proc, err := r.cfg.Processes.Process(ctx, key)
	if err != nil {
		return nil, err
	}
	if proc.Parent != "" && proc.Parent != key {
		// Stopping the parent waits for the processes under it to start,
		// and holds new ones until it ends.
		base := procs.get(proc.Parent)
		base.childrenMu.RLock()
		defer base.childrenMu.RUnlock()
	}
	ua := procs.get(key)
	ua.startMu.Lock()
	defer ua.startMu.Unlock()
	procs.setParent(ua, proc.Parent)
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
	h, err := r.prepare(ctx, r.cfg.AgentLog, r.cfg.AgentLog, key)
	if err != nil {
		return nil, err
	}
	rwc, err := h.process().Start(ctx, key)
	if err != nil {
		return nil, errors.Join(err, h.stop(context.WithoutCancel(ctx)))
	}
	slog.Info("started an agent process", "key", key, "sandbox", h.prepared.Summary, "storage", h.grant.Storage)
	var started io.ReadWriteCloser
	started = a2aacp.WithCleanup(rwc, func() error {
		procs.exited(ua, started)
		return h.stop(context.Background())
	})
	procs.started(ua, started, h.grant)
	return started, nil
}

// prepared is an agent ready to start as an ACP agent.
type prepared struct {
	profile  agent.Profile
	mounts   []agent.Mount
	prepared *agent.Prepared
	// stop cleans up after the agent exited.
	stop func(context.Context) error
	// maxMessage bounds a message from the agent.
	maxMessage int
	// grant is what its container gets.
	grant Grant
}

func (h *prepared) process() agentProcess {
	return agentProcess{sandbox: h.prepared, maxMessage: h.maxMessage}
}

// agentProcess starts the agent process in its sandbox, which checks the
// sandbox before every start, since a pool starts it again after it stops.
type agentProcess struct {
	sandbox sandbox.Sandbox
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

// prepare prepares the agent process of key. The process writes its
// stderr to stderr.
func (r *Runner) prepare(ctx context.Context, imageProgress, stderr io.Writer, key string) (*prepared, error) {
	cfg, err := r.cfg.Spec()
	if err != nil {
		return nil, err
	}
	profile, err := agent.Lookup(cfg.Agent, cfg.Command)
	if err != nil {
		return nil, err
	}
	proc, err := r.cfg.Processes.Process(ctx, key)
	if err != nil {
		return nil, err
	}
	labels := maps.Clone(cfg.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	maps.Copy(labels, proc.Labels)
	name := cfg.Name
	if proc.Name != "" {
		name += "-" + proc.Name
	}
	spec := agent.Spec{
		Profile: profile,
		Sandbox: cfg.Sandbox,
		Backend: r.backend(cfg.Sandbox),
		Base: sandbox.Spec{
			Name:     name,
			Image:    cfg.Image,
			Limits:   cfg.Limits,
			Labels:   labels,
			Progress: imageProgress,
			Stderr:   stderr,
		},
		Mounts:     cfg.Mounts,
		Home:       proc.Home,
		Hidden:     r.cfg.Dirs.Hidden,
		Pins:       r.cfg.Dirs.Pins,
		MCPMask:    r.cfg.Dirs.MCPMask,
		ConfigMask: r.cfg.Dirs.ConfigMask,
		Env:        append([]string{}, cfg.Env...),
		Model:      cfg.Model,
	}
	if err := agent.PrepareHome(agent.Spec{Sandbox: cfg.Sandbox, Mounts: cfg.Mounts, Hidden: r.cfg.Dirs.Hidden, Home: proc.Home, Pins: r.cfg.Dirs.Pins}); err != nil {
		return nil, err
	}
	if cfg.Sandbox != agent.SandboxLocal {
		spec.LockHome = proc.LockHome
		if spec.PolicyDir, err = r.policies.write(cfg, spec.LockHome, r.mcpServerNames()); err != nil {
			return nil, err
		}
	}
	inject, err := r.grantSandbox(ctx, &spec, cfg, key, proc)
	if err != nil {
		return nil, err
	}
	if key == "" {
		g := proc.Grant
		r.mainGrant.Store(&g)
	}
	if hook := r.cfg.Hooks.PrepareHome; hook != nil {
		if err := hook(ctx, key, proc.Home, proc.Grant); err != nil {
			return nil, err
		}
	}
	closeEndpoint := func() error { return nil }
	if proc.AgentAPI {
		ep := r.main
		if key != "" {
			if ep, err = r.listeners.open(key); err != nil {
				return nil, err
			}
			closeEndpoint = ep.close
		}
		if err := r.connect(&spec, ep); err != nil {
			return nil, errors.Join(err, closeEndpoint())
		}
		if cfg.Sandbox == agent.SandboxLocal {
			spec.CLI = cfg.CLI
		}
	}
	if err := r.egressSandbox(&spec, cfg, inject); err != nil {
		return nil, errors.Join(err, closeEndpoint())
	}
	p, err := agent.Prepare(ctx, spec)
	if err != nil {
		return nil, errors.Join(err, closeEndpoint())
	}
	problem := p.NoBoundary()
	r.egress.started(spec.EgressMode, problem)
	forget := func() {}
	if problem == "" {
		// A container can still fall back to Docker's network at a later start.
		forget = r.egress.watch(p.NoBoundary)
	}
	stop := func(ctx context.Context) error {
		forget()
		return errors.Join(p.Close(ctx), closeEndpoint())
	}
	return &prepared{profile: profile, mounts: cfg.Mounts, prepared: p, stop: stop, maxMessage: r.cfg.MaxAgentMessage, grant: proc.Grant}, nil
}

// backend returns the backend of the sandbox kind.
func (r *Runner) backend(kind string) sandbox.Backend {
	if kind == agent.SandboxLocal {
		return sandbox.Local{}
	}
	return r.docker
}

// connect gives spec the endpoint through which its agent process reaches
// the caller: the socket, or the key for the TCP listener in a credential
// helper in its home.
func (r *Runner) connect(spec *agent.Spec, ep *endpoint) error {
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
		Turn:         r.stalls.turn(turn),
		Converter:    r.stalls,
		Abandoned:    r.abandoned,
		Failure:      r.cfg.Hooks.Failure,
	}
	if r.cfg.Hooks.Prompt != nil {
		cfg.Prompt = r.cfg.Hooks.Prompt
	}
	return cfg
}

// abandoned stores the event that ends a task whose agent stopped while it
// waited for an answer, since the executor can't emit it. The agent exits
// when the runner stops, and the caller fails the open tasks then.
func (r *Runner) abandoned(ctx context.Context, ev *a2a.TaskStatusUpdateEvent) {
	if r.split == nil || r.split.stopped() || r.cfg.Tasks == nil {
		return
	}
	if err := r.cfg.Tasks.Abandon(ctx, ev); err != nil {
		slog.Warn("failing a task the agent stopped working on", "task", ev.TaskID, "error", err)
	}
}

// sessionSettings selects the model of the spec in every session, so that
// a model the agent doesn't know fails the turn.
func (r *Runner) sessionSettings(context.Context, *a2asrv.ExecutorContext) (map[string]any, error) {
	cfg, err := r.cfg.Spec()
	if err != nil {
		return nil, err
	}
	profile, err := agent.Lookup(cfg.Agent, cfg.Command)
	if err != nil {
		return nil, err
	}
	return profile.SessionSettings(cfg.Model), nil
}

// listen opens the listeners through which agent processes reach the
// caller: Unix sockets, which are mounted into containers, or TCP with a
// key for Docker Desktop, which can't mount them.
func (r *Runner) listen(sandboxed bool) (*listeners, error) {
	bind, dial, tcp := agent.TCPBridge(r.spec().Sandbox)
	if !sandboxed || !tcp {
		return listenSockets(r.cfg.Dirs.Sockets)
	}
	return listenTCP(bind, dial)
}

// workspaceOf returns the working directory of the agent's sessions.
func workspaceOf(spec Spec) (string, error) {
	if len(spec.Mounts) == 0 {
		return "", errors.New("no workspace to mount")
	}
	return spec.Mounts[0].Src, nil
}

// copyDir returns the private workspace of storage of the process p.
func (r *Runner) copyDir(p Process, storage workspace.Storage) string {
	return r.copies.Dir(p.CopyName, storage)
}
