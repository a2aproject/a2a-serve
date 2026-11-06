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

package agentsrv

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/limiter"
	"github.com/a2aproject/a2a-go/v2/integrations/executor/a2aacp"

	"github.com/a2aproject/a2a-serve/internal/a2acli"
	"github.com/a2aproject/a2a-serve/internal/a2atext"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/agentkey"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/agentproxy"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/approvals"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/messenger"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
	"github.com/a2aproject/a2a-serve/internal/harness"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// harnessSocketName is the name of the Unix socket an agent process reaches
// the server through, in its directory of the harness socket directory.
const harnessSocketName = "a2a.sock"

// newExecutor starts the harness as an ACP agent in its sandbox. close stops
// the harness and cleans up its sandbox.
func (s *Server) newExecutor(ctx context.Context, harnessLog, imageProgress io.Writer) (exec a2asrv.AgentExecutor, close func(context.Context) error, err error) {
	s.pruneContainers(ctx)
	h, err := s.prepareHarness(ctx, imageProgress, "")
	if err != nil {
		return nil, nil, err
	}
	s.logf("✓ %s", h.prepared.Summary)
	mountDesc := make([]string, len(h.mounts))
	for i, m := range h.mounts {
		mountDesc[i] = m.String()
	}
	s.logf("✓ workspace %s; config %s", strings.Join(mountDesc, ", "), s.dir.ConfigPath())

	acp, err := a2aacp.NewExecutor(ctx, h.process(harnessLog), s.acpConfig())
	if err != nil {
		return nil, nil, errors.Join(fmt.Errorf("starting %s: %w", h.profile.Name(), err), h.stop(ctx))
	}
	s.logf("✓ %s is running (ACP)", h.profile.Name())
	return s.guardACP(acp), func(ctx context.Context) error { return errors.Join(acp.Close(), h.stop(ctx)) }, nil
}

// pruneContainers removes the containers of this agent directory that an
// earlier server left running, such as those of a crash or a stop that ran
// out of time.
func (s *Server) pruneContainers(ctx context.Context) {
	if sb := s.rt.Config().Sandbox; sb != "" && sb != harness.SandboxDocker {
		return
	}
	n, err := harness.PruneContainers(ctx, map[string]string{"a2a.dir": s.dir.Root})
	if err != nil {
		slog.Warn("removing containers left by an earlier run", "error", err)
		return
	}
	if n > 0 {
		slog.Info("removed containers left by an earlier run", "count", n)
	}
}

// newAnonymousExecutor prepares the separate harness of anonymous
// conversations. Its agent starts on the first task, and stops when idle.
// It gets no way to call other agents: no socket, credential helper or CLI.
// With Docker it runs in its own container with its own home.
func (s *Server) newAnonymousExecutor(ctx context.Context, harnessLog io.Writer) (a2asrv.AgentExecutor, func(context.Context) error, error) {
	h, err := s.prepareHarness(ctx, harnessLog, agentkey.Anonymous)
	if err != nil {
		return nil, nil, err
	}
	cfg := s.acpConfig()
	cfg.Pool = &a2aacp.PoolConfig{
		Key: func(context.Context, string) (string, error) { return agentkey.Anonymous, nil },
	}
	// Approvals reject questions after AnonymousQuestionTimeout; this stops
	// the turn if that answer doesn't arrive.
	cfg.QuestionTimeout = s.timings.AnonymousQuestionTimeout + time.Minute
	acp, err := a2aacp.NewExecutor(ctx, h.process(harnessLog), cfg)
	if err != nil {
		return nil, nil, errors.Join(fmt.Errorf("preparing %s for anonymous callers: %w", h.profile.Name(), err), h.stop(ctx))
	}
	slog.Info("prepared the agent of anonymous callers", "sandbox", h.prepared.Summary)
	return s.guardACP(acp), func(ctx context.Context) error { return errors.Join(acp.Close(), h.stop(ctx)) }, nil
}

// maxUserAgents limits the agent processes of authenticated users that run
// at once; the least recently used idle one stops to start another.
const maxUserAgents = 10

// newUsersExecutor runs the conversations of each authenticated user in an
// agent process of their own, with its own home and, with Docker, its own
// container. A process starts on the user's first task, and stops when idle.
func (s *Server) newUsersExecutor(ctx context.Context, harnessLog io.Writer) (a2asrv.AgentExecutor, func(context.Context) error, error) {
	start := a2aacp.AgentFunc(func(ctx context.Context, key string) (io.ReadWriteCloser, error) {
		return s.startUserAgent(ctx, key, harnessLog)
	})
	cfg := s.acpConfig()
	cfg.Pool = &a2aacp.PoolConfig{Key: s.keys.of, MaxAgents: maxUserAgents}
	// Approvals reject questions after UserQuestionTimeout; this stops the
	// turn if that answer doesn't arrive.
	cfg.QuestionTimeout = s.timings.UserQuestionTimeout + time.Minute
	acp, err := a2aacp.NewExecutor(ctx, start, cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("preparing the agent processes of users: %w", err)
	}
	return s.guardACP(acp), func(context.Context) error { return acp.Close() }, nil
}

// startUserAgent starts the agent process of a user's agent key, unless
// the user's pairing was removed.
func (s *Server) startUserAgent(ctx context.Context, key string, harnessLog io.Writer) (io.ReadWriteCloser, error) {
	id, ok := agentkey.UserOf(key)
	if !ok {
		return nil, fmt.Errorf("unknown agent key %q", key)
	}
	ua := s.users.get(key)
	ua.startMu.Lock()
	defer ua.startMu.Unlock()
	if err := s.checkUser(ctx, id); err != nil {
		return nil, err
	}
	h, err := s.prepareHarness(ctx, harnessLog, key)
	if err != nil {
		return nil, err
	}
	rwc, err := h.process(harnessLog).Start(ctx, key)
	if err != nil {
		return nil, errors.Join(err, h.stop(context.WithoutCancel(ctx)))
	}
	slog.Info("started the agent process of a user", "user", id, "sandbox", h.prepared.Summary)
	var proc io.ReadWriteCloser
	proc = a2aacp.WithCleanup(rwc, func() error {
		s.users.exited(ua, proc)
		return h.stop(context.Background())
	})
	s.users.started(ua, proc)
	return proc, nil
}

// preparedHarness is a harness ready to start as an ACP agent.
type preparedHarness struct {
	profile  harness.Profile
	mounts   []agentdir.Mount
	prepared *harness.Prepared
	// stop cleans up after the harness exited.
	stop func(context.Context) error
}

func (h *preparedHarness) process(stderr io.Writer) checkedProcess {
	return checkedProcess{
		Process: a2aacp.Process{Argv: h.prepared.Args, Dir: h.mounts[0].Src, Env: h.prepared.Env, Stderr: stderr},
		check:   h.prepared.Check,
	}
}

// checkedProcess checks the sandbox before every start of the process, since
// a pool starts it again after it stops.
type checkedProcess struct {
	a2aacp.Process
	check func() error
}

// Start implements [a2aacp.Agent].
func (p checkedProcess) Start(ctx context.Context, key string) (io.ReadWriteCloser, error) {
	if err := p.check(); err != nil {
		return nil, err
	}
	return p.Process.Start(ctx, key)
}

// prepareHarness prepares the harness of the agent process of an agent
// key: the owner's for no key, anonymous callers' or a user's.
func (s *Server) prepareHarness(ctx context.Context, imageProgress io.Writer, key string) (*preparedHarness, error) {
	cfg := s.rt.Config()
	profile, err := harness.Lookup(cfg.Harness, cfg.Command)
	if err != nil {
		return nil, err
	}
	mounts, err := s.dir.Mounts(cfg)
	if err != nil {
		return nil, err
	}
	env := append([]string{}, cfg.Env...)
	for _, name := range cfg.Secrets {
		v, ok := s.rt.Secrets()[name]
		if !ok {
			return nil, fmt.Errorf("secret %s is not set; pass --secret %s again with the variable set", name, name)
		}
		env = append(env, name+"="+v)
	}
	spec := harness.Spec{
		Profile:  profile,
		Sandbox:  cfg.Sandbox,
		Mounts:   mounts,
		Home:     s.dir.HomeDir(),
		Hidden:   s.hidden(),
		Pins:     s.pins,
		MCPMask:  s.dir.MCPMaskPath(),
		Env:      env,
		Model:    cfg.Model,
		Name:     "a2a-" + cfg.Name + "-" + s.dir.ID(),
		Image:    cfg.Image,
		Limits:   harness.Limits{Memory: cfg.Limits.Memory, CPUs: cfg.Limits.CPUs, PIDs: cfg.Limits.PIDs},
		Labels:   map[string]string{"a2a.agent": cfg.Name, "a2a.dir": s.dir.Root},
		Progress: imageProgress,
	}
	id, isUser := agentkey.UserOf(key)
	switch {
	case key == agentkey.Anonymous:
		spec.Home = s.dir.AnonymousHomeDir()
	case isUser:
		spec.Home = s.dir.UserHomeDir(id)
	}
	if err := s.checkHome(cfg, mounts, spec.Home); err != nil {
		return nil, err
	}
	closeEndpoint := func() error { return nil }
	if key == agentkey.Anonymous {
		spec.Name += "-anonymous"
		spec.Labels["a2a.role"] = auth.RoleAnonymous
		if err := fsutil.CheckPrivate(spec.Home); err != nil {
			return nil, err
		}
	} else {
		ep := s.mainHarness
		if isUser {
			if err := s.prepareUserHome(ctx, &spec, id); err != nil {
				return nil, err
			}
			if ep, err = s.harness.open(key); err != nil {
				return nil, err
			}
			closeEndpoint = ep.close
		}
		if err := s.connectHarness(&spec, ep); err != nil {
			return nil, errors.Join(err, closeEndpoint())
		}
		if cfg.Sandbox == harness.SandboxLocal {
			spec.CLI = s.localCLI()
		}
	}
	prepared, err := harness.Prepare(ctx, spec)
	if err != nil {
		return nil, errors.Join(err, closeEndpoint())
	}
	stop := func(ctx context.Context) error { return errors.Join(prepared.Stop(ctx), closeEndpoint()) }
	return &preparedHarness{profile: profile, mounts: mounts, prepared: prepared, stop: stop}, nil
}

// checkHome makes home a real directory and pins it, after the sandbox
// checks of cfg pass. Every write of the server into an agent home comes
// after it.
func (s *Server) checkHome(cfg *agentdir.Config, mounts []agentdir.Mount, home string) error {
	return harness.PrepareHome(harness.Spec{Sandbox: cfg.Sandbox, Mounts: mounts, Hidden: s.hidden(), Home: home, Pins: s.pins})
}

// hidden are the host paths sandboxes hide even where a mount exposes them.
func (s *Server) hidden() []string { return []string{s.dir.Path()} }

// connectHarness gives spec the endpoint through which its agent process
// reaches the server: the socket, or the key for the TCP listener in a
// credential helper in its home.
func (s *Server) connectHarness(spec *harness.Spec, ep *harnessEndpoint) error {
	if ep.key == "" {
		spec.Socket = ep.socket
		return nil
	}
	env, err := writeHarnessHelper(spec.Home, s.harness.dial, ep.key)
	if err != nil {
		return fmt.Errorf("saving the agent key: %w", err)
	}
	spec.Env = append(spec.Env, env)
	return nil
}

// prepareUserHome gives spec the container name of the agent process of
// the user id, and its home the instructions of the owner's home.
func (s *Server) prepareUserHome(ctx context.Context, spec *harness.Spec, id string) error {
	spec.Name += "-" + agentdir.UserKey(id)
	spec.Labels["a2a.role"] = auth.RoleAuthenticated
	for _, dir := range []string{s.dir.UserHomesDir(), spec.Home} {
		if err := fsutil.CheckPrivate(dir); err != nil {
			return err
		}
	}
	return s.writeInstructionsIn(ctx, spec.Home)
}

// localCLI returns the a2a CLI on PATH, with which an agent without a
// sandbox calls other agents, or "" when there is none. The setup problems
// that the server prints on start report a missing or outdated CLI.
func (s *Server) localCLI() string {
	cli, err := a2acli.Path()
	if err != nil {
		slog.Debug("no a2a CLI for the agent", "error", err)
		return ""
	}
	return cli
}

// acpConfig is the config of every ACP executor of the server.
func (s *Server) acpConfig() *a2aacp.Config {
	return &a2aacp.Config{
		AutoAnswer:   liveToolRules(s.rt.Config),
		SessionStore: s.store.Sessions(),
		Session:      s.sessionParams,
		Settings:     s.sessionSettings,
		Turn:         s.stalls.turn(s.claims.turn),
		Converter:    s.stalls,
	}
}

// guardACP fails the stalled turns of an executor made with acpConfig, and
// explains its model errors.
func (s *Server) guardACP(acp a2asrv.AgentExecutor) a2asrv.AgentExecutor {
	return &modelKeyHint{inner: s.stalls.guard(acp), config: s.rt.Config}
}

// modelKeyHint tells which key is missing when a turn fails because the
// agent doesn't know the configured model. opencode lists no models of a
// provider without a key, so it reports only that the model isn't found.
type modelKeyHint struct {
	inner  a2asrv.AgentExecutor
	config func() *agentdir.Config
}

var _ a2asrv.AgentExecutor = (*modelKeyHint)(nil)

func (e *modelKeyHint) Execute(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		for ev, err := range e.inner.Execute(ctx, execCtx) {
			if !yield(e.explain(execCtx, ev), err) {
				return
			}
		}
	}
}

func (e *modelKeyHint) explain(execCtx *a2asrv.ExecutorContext, ev a2a.Event) a2a.Event {
	up, ok := ev.(*a2a.TaskStatusUpdateEvent)
	if !ok || up.Status.State != a2a.TaskStateFailed {
		return ev
	}
	text := a2atext.Message(up.Status.Message)
	if !strings.Contains(text, "model not found") {
		return ev
	}
	hint := harness.MissingKey(e.config())
	if hint == "" {
		return ev
	}
	out := *up
	out.Status.Message = a2a.NewMessageForTask(a2a.MessageRoleAgent, execCtx, a2a.NewTextPart(hint+"; the agent reported: "+text))
	return &out
}

func (e *modelKeyHint) Cancel(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return e.inner.Cancel(ctx, execCtx)
}

// sessionSettings selects the model of the config in every session, so that
// a model the harness doesn't know fails the turn.
func (s *Server) sessionSettings(context.Context, *a2asrv.ExecutorContext) (map[string]any, error) {
	cfg := s.rt.Config()
	profile, err := harness.Lookup(cfg.Harness, cfg.Command)
	if err != nil {
		return nil, err
	}
	return profile.SessionSettings(cfg.Model), nil
}

// liveToolRules answers tool permissions with the allow and deny rules of
// the current config, so that changing them doesn't need a restart.
type liveToolRules func() *agentdir.Config

func (f liveToolRules) AutoAnswer(ctx context.Context, execCtx *a2asrv.ExecutorContext, q a2aacp.Question) (a2aacp.Answer, bool) {
	cfg := f()
	return a2aacp.ToolRules{Allow: cfg.Approvals.Allow, Deny: cfg.Approvals.Deny}.AutoAnswer(ctx, execCtx, q)
}

// listenHarness opens the listeners through which agent processes call
// other agents: Unix sockets, which are mounted into containers, or TCP with
// a key for Docker Desktop, which can't mount them.
func (s *Server) listenHarness(sandboxed bool) (*harnessListeners, error) {
	bind, dial, tcp := harness.TCPBridge(s.rt.Config().Sandbox)
	if !sandboxed || !tcp {
		return listenHarnessSockets(s.dir.HarnessSocketDir())
	}
	return listenHarnessTCP(bind, dial)
}

// observedExecutor publishes every event of the agent to the server's event
// bus and relays questions which the task's caller can't answer.
type observedExecutor struct {
	inner     a2asrv.AgentExecutor
	bus       *messenger.Bus
	claims    *CallClaims
	approvals *approvals.Service
	// unattended is told when the task of a context ends.
	unattended *messenger.ContextSet
	// watches ask the A2A caller of a turn the questions of the agents it
	// calls, on the caller's task.
	watches *agentproxy.Watches
	// store has the records of those questions.
	store *store.Store
}

var (
	_ a2asrv.AgentExecutor         = (*observedExecutor)(nil)
	_ a2asrv.AgentExecutionCleaner = (*observedExecutor)(nil)
)

// Cleanup tells the followers of the task's context that an execution
// ended, relays the question the task stopped on, and forgets that nobody
// watches the context of a task that ended. a2a-go calls it once the
// execution is done with its last event and stored it, so an answer to the
// question can start the next execution.
func (e *observedExecutor) Cleanup(ctx context.Context, execCtx *a2asrv.ExecutorContext, result a2a.SendMessageResult, err error) {
	e.bus.ExecutionEnded(execCtx.ContextID)
	task, ok := result.(*a2a.Task)
	if err != nil || (ok && task.Status.State.Terminal()) {
		e.unattended.Remove(execCtx.ContextID)
	}
	if err != nil || !ok {
		return
	}
	if q := approvals.FromStatus(task.Status); q != nil && !askedBefore(execCtx.StoredTask, q) {
		e.approvals.OnQuestion(ctx, execCtx, q)
	}
}

func (e *observedExecutor) Execute(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	if id, ok := callerQuestionOf(execCtx.StoredTask); ok {
		return e.observe(ctx, execCtx, e.answerCallerQuestion(ctx, execCtx, id))
	}
	// a2aacp takes a reply that isn't an answer as a new instruction, and
	// rejects the tool call without saying so.
	if q := pendingApproval(execCtx.StoredTask); q != nil && !answersQuestion(execCtx.Message, q) {
		return e.observe(ctx, execCtx, refuseReply(execCtx, q))
	}
	return e.observe(ctx, execCtx, e.withCallerQuestion(execCtx, e.turn(ctx, execCtx)))
}

// turn runs a turn of the agent, with the line of the agents it may call
// added to the prompt and kept out of the task store.
func (e *observedExecutor) turn(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	ctx, withLine := e.claims.withAgentsLine(ctx, execCtx)
	if execCtx.Message != nil {
		e.claims.addTokens(execCtx.ContextID, estimateTokens(execCtx.Message.Parts))
	}
	events := withAnswerOrder(execCtx, coalesceArtifacts(e.claims.countTokens(execCtx.ContextID, e.inner.Execute(ctx, withLine))))
	if withLine == execCtx {
		return events
	}
	return func(yield func(a2a.Event, error) bool) {
		for ev, err := range events {
			if t, ok := ev.(*a2a.Task); ok {
				ev = withOriginalMessage(t, withLine.Message, execCtx.Message)
			}
			if !yield(ev, err) {
				return
			}
		}
	}
}

// withAnswerOrder starts the events of a turn which answers the task's
// status message, such as a question, with a snapshot of the task which has
// the question before the answer in its history. a2a-go appends the answer
// to the history right away, but moves the question there only with the
// next status update, after the answer.
// https://github.com/a2aproject/a2a-go/issues/498
func withAnswerOrder(execCtx *a2asrv.ExecutorContext, events iter.Seq2[a2a.Event, error]) iter.Seq2[a2a.Event, error] {
	t := answerOrdered(execCtx.StoredTask, execCtx.Message)
	if t == nil {
		return events
	}
	return func(yield func(a2a.Event, error) bool) {
		if !yield(t, nil) {
			return
		}
		for ev, err := range events {
			if !yield(ev, err) {
				return
			}
		}
	}
}

// answerOrdered returns task with its status message moved into the history
// before msg, the message which continues the task, or nil if msg doesn't
// follow a status message which isn't in the history yet.
func answerOrdered(task *a2a.Task, msg *a2a.Message) *a2a.Task {
	if task == nil || msg == nil || task.Status.Message == nil || len(task.History) == 0 {
		return nil
	}
	q := task.Status.Message
	last := len(task.History) - 1
	if task.History[last].ID != msg.ID || slices.ContainsFunc(task.History, func(m *a2a.Message) bool { return m.ID == q.ID }) {
		return nil
	}
	c := *task
	c.History = slices.Concat(task.History[:last], []*a2a.Message{q, task.History[last]})
	now := time.Now()
	c.Status = a2a.TaskStatus{State: a2a.TaskStateWorking, Timestamp: &now}
	return &c
}

// withOriginalMessage replaces the message with the agents line in a task's
// history by the caller's message, so the line stays out of the task store.
func withOriginalMessage(t *a2a.Task, withLine, original *a2a.Message) *a2a.Task {
	if !slices.Contains(t.History, withLine) {
		return t
	}
	c := *t
	c.History = slices.Clone(t.History)
	for i, m := range c.History {
		if m == withLine {
			c.History[i] = original
		}
	}
	return &c
}

func (e *observedExecutor) Cancel(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	// a2asrv cancels ctx once it has the canceled event, which comes before
	// the inner Cancel runs, but the turn must still stop.
	return e.observe(ctx, execCtx, canceledFirst(execCtx, e.inner.Cancel(context.WithoutCancel(ctx), execCtx)))
}

// canceledFirst yields the canceled event before the inner Cancel runs.
// a2aacp stops the turn before yielding it, which can end a running
// execution first, so that the cancel resolves to the state the execution
// ended in, such as input-required. The inner events still run for their
// side effects; its canceled event is dropped.
func canceledFirst(execCtx *a2asrv.ExecutorContext, events iter.Seq2[a2a.Event, error]) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		open := yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCanceled, nil), nil)
		for ev, err := range events {
			if !open {
				continue
			}
			if u, ok := ev.(*a2a.TaskStatusUpdateEvent); ok && err == nil && u.Status.State == a2a.TaskStateCanceled {
				continue
			}
			open = yield(ev, err)
		}
	}
}

func (e *observedExecutor) observe(ctx context.Context, execCtx *a2asrv.ExecutorContext, events iter.Seq2[a2a.Event, error]) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		start := time.Now()
		from := ""
		if p, ok := auth.Caller(ctx); ok {
			from = p.ID
		}
		slog.Info("task received", "task", execCtx.TaskID, "context", execCtx.ContextID, "from", from)
		closed := false
		for ev, err := range events {
			if err != nil {
				slog.Info("task failed", "task", execCtx.TaskID, "duration", time.Since(start).Round(time.Millisecond), "error", err)
			}
			if err == nil {
				logTransition(execCtx.TaskID, ev, start)
				if state, ok := eventState(ev); ok && state != a2a.TaskStateInputRequired && !closed {
					closed = true
					e.approvals.CloseQuestions(ctx, execCtx.TaskID)
				}
				e.bus.Publish(execCtx.TaskID, ev)
			}
			if !yield(ev, err) {
				return
			}
		}
	}
}

// logTransition logs a task reaching a state where it stops: it finished or
// waits for input.
func logTransition(id a2a.TaskID, ev a2a.Event, start time.Time) {
	state, ok := eventState(ev)
	if !ok {
		return
	}
	if !state.Terminal() && state != a2a.TaskStateInputRequired && state != a2a.TaskStateAuthRequired {
		return
	}
	slog.Info("task "+a2atext.StateName(state), "task", id, "duration", time.Since(start).Round(time.Millisecond))
}

// eventState returns the task state an event reports, if it reports one.
func eventState(ev a2a.Event) (a2a.TaskState, bool) {
	switch e := ev.(type) {
	case *a2a.Task:
		return e.Status.State, true
	case *a2a.TaskStatusUpdateEvent:
		return e.Status.State, true
	}
	return "", false
}

// callerInterceptor makes the authenticated principal the A2A user and
// enforces who may join a conversation, who may answer approvals, how deep
// agent chains go and the budget of anonymous callers.
type callerInterceptor struct {
	a2asrv.PassthroughCallInterceptor
	approvals *approvals.Service
	tasks     *store.TaskStore
	budget    *anonymousBudget
}

func (i *callerInterceptor) Before(ctx context.Context, callCtx *a2asrv.CallContext, req *a2asrv.Request) (context.Context, any, error) {
	p, ok := auth.Caller(ctx)
	if !ok {
		return ctx, nil, a2a.ErrUnauthenticated
	}
	if p.Hops > auth.MaxHops {
		return ctx, nil, fmt.Errorf("%w: too many agents in this chain (%d)", a2a.ErrUnauthorized, p.Hops)
	}
	auth.SetCallUser(callCtx, p)
	if !p.IsOwner() {
		ctx = limiter.AttachScope(ctx, p.ID)
	}
	// a2a-go streams a running task to any subscriber, so subscribers get
	// the same scoping as GetTask.
	if sub, ok := req.Payload.(*a2a.SubscribeToTaskRequest); ok {
		_, err := i.tasks.Get(ctx, sub.ID)
		return ctx, nil, err
	}
	// Anonymous callers who proved nothing share one principal, so they
	// list only the conversation they name, whose ID proves it is theirs.
	if list, ok := req.Payload.(*a2a.ListTasksRequest); ok && p.ID == auth.AnonymousID && list.ContextID == "" {
		return ctx, &a2a.ListTasksResponse{Tasks: []*a2a.Task{}, PageSize: list.PageSize}, nil
	}
	msg, ok := req.Payload.(*a2a.SendMessageRequest)
	if !ok || msg.Message == nil {
		return ctx, nil, nil
	}
	if err := i.checkConversation(ctx, p, msg.Message.ContextID); err != nil {
		return ctx, nil, err
	}
	if !p.IsOwner() && msg.Message.TaskID == "" {
		if err := i.checkUnfinished(ctx, p); err != nil {
			return ctx, nil, err
		}
	}
	if msg.Message.TaskID == "" {
		if err := i.checkWaiting(ctx, msg.Message.ContextID); err != nil {
			return ctx, nil, err
		}
	}
	if p.Role == auth.RoleAnonymous && msg.Message.TaskID == "" {
		if err := i.budget.admit(budgetListener(p)); err != nil {
			return ctx, nil, fmt.Errorf("%w: %w", a2a.ErrUnauthorized, err)
		}
	}
	if msg.Message.TaskID != "" {
		if err := i.approvals.CheckAnswer(ctx, p, msg.Message.TaskID); err != nil {
			return ctx, nil, err
		}
	}
	return ctx, nil, nil
}

// checkUnfinished refuses a new task of a caller who has
// maxCallerExecutions unfinished tasks. The execution limit counts only
// running tasks, and tasks waiting for an answer hold resources too.
func (i *callerInterceptor) checkUnfinished(ctx context.Context, p auth.Principal) error {
	ids, err := i.tasks.UnfinishedOf(ctx, p.ID)
	if err != nil {
		return fmt.Errorf("counting unfinished tasks: %w", err)
	}
	if len(ids) >= maxCallerExecutions {
		return fmt.Errorf("%w: %w", a2a.ErrUnauthorized, errCallerBusy)
	}
	return nil
}

// checkConversation refuses a message into a conversation that another
// user started, unless an owner sends it or the server picked the
// conversation, as for a group chat.
func (i *callerInterceptor) checkConversation(ctx context.Context, p auth.Principal, contextID string) error {
	if contextID == "" || p.IsOwner() || auth.IsInProcess(ctx) {
		return nil
	}
	starter, err := i.tasks.ContextStarter(ctx, contextID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("checking conversation %s: %w", contextID, err)
	}
	if starter != p.ID {
		return fmt.Errorf("%w: conversation %s belongs to someone else; leave out the context ID to start a new one", a2a.ErrUnauthorized, contextID)
	}
	return nil
}

// checkWaiting refuses a new task in a conversation with a task that waits
// for an answer to a question of the agent, since the new task would wait
// behind it without a word. Messages from chats and schedules still wait.
// Questions of other agents don't hold the conversation.
func (i *callerInterceptor) checkWaiting(ctx context.Context, contextID string) error {
	if contextID == "" || auth.IsInProcess(ctx) {
		return nil
	}
	history := 0
	waiting, err := i.tasks.List(auth.WithPrincipal(ctx, auth.Owner()), &a2a.ListTasksRequest{
		ContextID: contextID, Status: a2a.TaskStateInputRequired, HistoryLength: &history,
	})
	if err != nil {
		return fmt.Errorf("checking conversation %s: %w", contextID, err)
	}
	for _, t := range waiting.Tasks {
		q := approvals.FromStatus(t.Status)
		if q == nil || asksCallerQuestion(t) {
			continue
		}
		ids := make([]string, len(q.Options))
		for i, o := range q.Options {
			ids[i] = o.ID
		}
		return fmt.Errorf("%w: task %s in conversation %s waits for an answer to %q; answer it first with a2a send --task-id %s and one of: %s, or leave out the context ID to start a new conversation",
			a2a.ErrInvalidRequest, t.ID, contextID, q.Title, t.ID, strings.Join(ids, ", "))
	}
	return nil
}
