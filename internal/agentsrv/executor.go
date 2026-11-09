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
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aext/structuredinput"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/limiter"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/a2aproject/a2a-go/v2/integrations/executor/a2aacp"

	"github.com/a2aproject/a2a-serve/internal/a2acli"
	"github.com/a2aproject/a2a-serve/internal/a2aerr"
	"github.com/a2aproject/a2a-serve/internal/a2atext"
	"github.com/a2aproject/a2a-serve/internal/access"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agentkey"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/agentproxy"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/approvals"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/chat"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
	"github.com/a2aproject/a2a-serve/internal/harness"
	"github.com/a2aproject/a2a-serve/internal/store"
	"github.com/a2aproject/a2a-serve/internal/toolrules"
)

// harnessSocketName is the name of the Unix socket an agent process reaches
// the server through, in its directory of the harness socket directory.
const harnessSocketName = "a2a.sock"

// newExecutor starts the harness as an ACP agent in its sandbox. close stops
// the harness and cleans up its sandbox.
func (s *Server) newExecutor(ctx context.Context, harnessLog, imageProgress io.Writer) (exec a2asrv.AgentExecutor, close func(context.Context) error, err error) {
	if err := s.pruneContainers(ctx); err != nil {
		return nil, nil, err
	}
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
	for _, w := range h.prepared.Warnings {
		s.logf("! %s", w)
	}
	if p := h.prepared.EgressProblem; p != "" {
		slog.Warn(p)
	}
	s.noticeEgress(h.prepared)

	acp, err := a2aacp.NewExecutor(ctx, h.process(harnessLog), s.acpConfig())
	if err != nil {
		return nil, nil, errors.Join(fmt.Errorf("starting %s: %w", h.profile.Name(), err), h.stop(ctx))
	}
	s.logf("✓ %s is running (ACP)", h.profile.Name())
	return s.guardACP(acp), func(ctx context.Context) error { return errors.Join(acp.Close(), h.stop(ctx)) }, nil
}

// pruneContainers removes the containers of this agent directory that an
// earlier server left running, such as those of a crash or a stop that ran
// out of time. It fails only when docker doesn't answer, which would stall
// the start.
func (s *Server) pruneContainers(ctx context.Context) error {
	if sb := s.rt.Config().Sandbox; sb != "" && sb != harness.SandboxDocker {
		return nil
	}
	n, err := harness.PruneContainers(ctx, map[string]string{"a2a.dir": s.dir.Root})
	if errors.Is(err, harness.ErrDockerNoAnswer) {
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

// newGuestExecutor runs the conversations of guests in one
// agent process of their own. It starts on the first task, and stops when
// idle. It gets no way to call other agents: no socket, credential helper or
// CLI. With Docker it runs in its own container with its own home.
func (s *Server) newGuestExecutor(ctx context.Context, harnessLog io.Writer) (a2asrv.AgentExecutor, func(context.Context) error, error) {
	start := a2aacp.AgentFunc(func(ctx context.Context, key string) (io.ReadWriteCloser, error) {
		return s.startPoolAgent(ctx, &s.guests, key, harnessLog)
	})
	cfg := s.acpConfig()
	cfg.Pool = &a2aacp.PoolConfig{
		Key: func(context.Context, string) (string, error) { return agentkey.Guest, nil },
	}
	cfg.Turn = s.stalls.turn(s.countedTurn(&s.guests))
	// Approvals reject questions after GuestQuestionTimeout; this stops
	// the turn if that answer doesn't arrive.
	cfg.QuestionTimeout = s.timings.GuestQuestionTimeout + time.Minute
	acp, err := a2aacp.NewExecutor(ctx, start, cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("preparing the agent of guests: %w", err)
	}
	return s.guardACP(acp), func(context.Context) error { return acp.Close() }, nil
}

// maxUserAgents limits the agent processes of members that run
// at once; the least recently used idle one stops to start another.
const maxUserAgents = 10

// newUsersExecutor runs the conversations of each member in an
// agent process of their own, with its own home and, with Docker, its own
// container. A process starts on the user's first task, and stops when idle.
func (s *Server) newUsersExecutor(ctx context.Context, harnessLog io.Writer) (a2asrv.AgentExecutor, func(context.Context) error, error) {
	start := a2aacp.AgentFunc(func(ctx context.Context, key string) (io.ReadWriteCloser, error) {
		return s.startPoolAgent(ctx, &s.users, key, harnessLog)
	})
	cfg := s.acpConfig()
	cfg.Pool = &a2aacp.PoolConfig{Key: s.keys.of, MaxAgents: maxUserAgents}
	cfg.Turn = s.stalls.turn(s.countedTurn(&s.users))
	// Approvals reject questions after UserQuestionTimeout; this stops the
	// turn if that answer doesn't arrive.
	cfg.QuestionTimeout = s.timings.UserQuestionTimeout + time.Minute
	acp, err := a2aacp.NewExecutor(ctx, start, cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("preparing the agent processes of users: %w", err)
	}
	return s.guardACP(acp), func(context.Context) error { return acp.Close() }, nil
}

// countedTurn counts each turn in the agent process of its conversation
// among agents, so that new tasks are refused while all processes are busy.
// A process whose sandbox rules changed stops once its last turn ends.
func (s *Server) countedTurn(agents *userAgents) func(ctx context.Context, execCtx *a2asrv.ExecutorContext) (end func()) {
	return func(ctx context.Context, execCtx *a2asrv.ExecutorContext) (end func()) {
		key, err := s.keys.of(ctx, execCtx.ContextID)
		if err != nil {
			slog.Warn("looking up the session of a conversation", "context", execCtx.ContextID, "error", err)
		}
		endTurn := agents.turn(key)
		endClaims := s.claims.turn(ctx, execCtx)
		return func() {
			if endClaims != nil {
				endClaims()
			}
			endTurn()
		}
	}
}

// startPoolAgent starts the agent process of an agent key among agents:
// guests', or a user's unless the user's pairing was removed.
func (s *Server) startPoolAgent(ctx context.Context, agents *userAgents, key string, harnessLog io.Writer) (io.ReadWriteCloser, error) {
	id, isUser := agentkey.UserOf(key)
	if !isUser && key != agentkey.Guest {
		return nil, fmt.Errorf("unknown agent key %q", key)
	}
	if isUser && agentkey.CallerOf(key) != "" {
		// Removing the user holds the start of its own process, which
		// holds the starts of the processes of its callers too.
		base := agents.get(agentkey.User(id))
		base.startMu.Lock()
		defer base.startMu.Unlock()
	}
	ua := agents.get(key)
	ua.startMu.Lock()
	defer ua.startMu.Unlock()
	if isUser {
		if err := s.checkUser(ctx, id); err != nil {
			return nil, err
		}
	}
	release, err := agents.reserve(ua)
	if err != nil {
		return nil, err
	}
	defer release()
	h, err := s.prepareHarness(ctx, harnessLog, key)
	if err != nil {
		return nil, err
	}
	rwc, err := h.process(harnessLog).Start(ctx, key)
	if err != nil {
		return nil, errors.Join(err, h.stop(context.WithoutCancel(ctx)))
	}
	slog.Info("started an agent process", "key", key, "sandbox", h.prepared.Summary, "storage", h.sandbox.Storage)
	var proc io.ReadWriteCloser
	proc = a2aacp.WithCleanup(rwc, func() error {
		agents.exited(ua, proc)
		return h.stop(context.Background())
	})
	agents.started(ua, proc, h.sandbox)
	return proc, nil
}

// preparedHarness is a harness ready to start as an ACP agent.
type preparedHarness struct {
	profile  harness.Profile
	mounts   []agentdir.Mount
	prepared *harness.Prepared
	// stop cleans up after the harness exited.
	stop func(context.Context) error
	// maxMessage bounds a message from the agent.
	maxMessage int
	// sandbox is what the sandbox rules give its container.
	sandbox access.Sandbox
}

func (h *preparedHarness) process(stderr io.Writer) checkedProcess {
	return checkedProcess{
		Process:    a2aacp.Process{Argv: h.prepared.Args, Dir: h.mounts[0].Src, Env: h.prepared.Env, Stderr: stderr},
		check:      h.prepared.Check,
		onHost:     h.prepared.OnHost,
		maxMessage: h.maxMessage,
	}
}

// checkedProcess checks the sandbox before every start of the process, since
// a pool starts it again after it stops.
type checkedProcess struct {
	a2aacp.Process
	check func() error
	// onHost marks each process, so that the processes it starts are killed
	// once it stops.
	onHost bool
	// maxMessage bounds a message from the agent.
	maxMessage int
}

// Start implements [a2aacp.Agent]. A message from the agent over
// maxMessage fails the connection, and Close returns once the reader saw
// the output end.
func (p checkedProcess) Start(ctx context.Context, key string) (io.ReadWriteCloser, error) {
	if err := p.check(); err != nil {
		return nil, err
	}
	if !p.onHost {
		rwc, err := p.Process.Start(ctx, key)
		if err != nil {
			return nil, err
		}
		return newAwaitedClose(newBoundedMessages(rwc, p.maxMessage), agentReadEndGrace), nil
	}
	mark := harness.NewProcessMark()
	proc := p.Process
	proc.Env = append(slices.Clone(proc.Env), mark.Env())
	rwc, err := proc.Start(ctx, key)
	if err != nil {
		return nil, err
	}
	return newAwaitedClose(newBoundedMessages(mark.Watch(rwc), p.maxMessage), agentReadEndGrace), nil
}

// prepareHarness prepares the harness of the agent process of an agent
// key: the owner's for no key, guests' or a user's.
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
	spec := harness.Spec{
		Profile:    profile,
		Sandbox:    cfg.Sandbox,
		Mounts:     mounts,
		Home:       s.dir.HomeDir(),
		Hidden:     s.hidden(),
		Pins:       s.pins,
		MCPMask:    s.dir.MCPMaskPath(),
		ConfigMask: s.dir.ConfigMaskPath(),
		Env:        env,
		Model:      cfg.Model,
		Name:       "a2a-" + cfg.Name + "-" + s.dir.ID(),
		Image:      cfg.Image,
		Limits:     harness.Limits{Memory: cfg.Limits.Memory, CPUs: cfg.Limits.CPUs, PIDs: cfg.Limits.PIDs},
		Labels:     map[string]string{"a2a.agent": cfg.Name, "a2a.dir": s.dir.Root},
		Progress:   imageProgress,
	}
	_, isUser := agentkey.UserOf(key)
	switch {
	case key == agentkey.Guest:
		spec.Home = s.dir.GuestHomeDir()
	case isUser:
		spec.Home = s.userHomeDir(key)
	}
	if err := s.checkHome(cfg, mounts, spec.Home); err != nil {
		return nil, err
	}
	sb, err := s.sandboxes.of(ctx, key)
	if err != nil {
		return nil, err
	}
	inject, err := s.applySandbox(ctx, &spec, key, sb)
	if err != nil {
		return nil, err
	}
	s.setStorageNote(spec.Home, sb.Storage)
	if key == "" {
		s.mainSandbox.Store(&sb)
	}
	closeEndpoint := func() error { return nil }
	if key == agentkey.Guest {
		spec.Name += "-guest"
		spec.Labels["a2a.role"] = auth.RoleGuest
		if err := fsutil.CheckPrivate(spec.Home); err != nil {
			return nil, err
		}
		s.writeStorageNote(cfg, spec.Home)
	} else {
		ep := s.mainHarness
		if isUser {
			if err := s.prepareUserHome(ctx, &spec, key); err != nil {
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
		if !isUser {
			if err := s.writeInstructionsIn(ctx, spec.Home); err != nil {
				s.logf("! updating the agent instructions: %v", err)
			}
		}
		if cfg.Sandbox == harness.SandboxLocal {
			spec.CLI = s.localCLI()
		}
	}
	if err := s.egressSandbox(&spec, inject); err != nil {
		return nil, errors.Join(err, closeEndpoint())
	}
	prepared, err := harness.Prepare(ctx, spec)
	if err != nil {
		return nil, errors.Join(err, closeEndpoint())
	}
	s.egress.started(spec.EgressMode, prepared.EgressProblem)
	forget := s.egress.watch(prepared.FellBack)
	stop := func(ctx context.Context) error {
		forget()
		return errors.Join(prepared.Stop(ctx), closeEndpoint())
	}
	return &preparedHarness{profile: profile, mounts: mounts, prepared: prepared, stop: stop, maxMessage: s.maxAgentMessage, sandbox: sb}, nil
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
	spec.BridgeAddr = strings.TrimPrefix(s.harness.url, "http://")
	env, err := writeHarnessHelper(spec.Home, s.harness.dial, ep.key)
	if err != nil {
		return fmt.Errorf("saving the agent key: %w", err)
	}
	spec.Env = append(spec.Env, env)
	return nil
}

// prepareUserHome gives spec the container name of the agent process of
// a member's agent key, and its home the instructions of the owner's home.
func (s *Server) prepareUserHome(ctx context.Context, spec *harness.Spec, key string) error {
	spec.Name += "-" + userProcessName(key)
	spec.Labels["a2a.role"] = auth.RoleMember
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
		AutoAnswer:   s.decider,
		SessionStore: s.sessions,
		Session:      s.sessionParams,
		Settings:     s.sessionSettings,
		Turn:         s.stalls.turn(s.claims.turn),
		Converter:    s.stalls,
		Abandoned:    s.abandoned,
		Failure:      s.failure,
	}
}

// abandoned stores the event that ends a task whose agent stopped while it
// waited for an answer, since the executor can't emit it. A reply may have
// ended the task first; the store refuses tasks in a terminal state. The
// agent exits when the server stops, and the next start fails the open
// tasks as restarted.
func (s *Server) abandoned(ctx context.Context, ev *a2a.TaskStatusUpdateEvent) {
	if s.split.stopped() {
		return
	}
	_, err := s.tasks.Update(ctx, &taskstore.UpdateRequest{Task: &a2a.Task{ID: ev.TaskID}, Event: ev, PrevVersion: taskstore.TaskVersionMissing})
	if errors.Is(err, a2a.ErrUnsupportedOperation) {
		return
	}
	if err != nil {
		slog.Warn("failing a task the agent stopped working on", "task", ev.TaskID, "error", err)
		return
	}
	slog.Info("task "+a2atext.StateName(ev.Status.State), "task", ev.TaskID, "reason", "the agent stopped while the task waited for input")
	s.ends.finish(ctx, ev)
}

// guardACP fails the stalled turns of an executor made with acpConfig.
func (s *Server) guardACP(acp a2asrv.AgentExecutor) a2asrv.AgentExecutor {
	return s.stalls.guard(acp)
}

// failure returns the status message of a turn that fails with err, for
// callers. It drops the protocol details of the agent's error, and explains
// the failures that have a next step. It also covers the tasks whose agent
// stopped while they waited for an answer.
func (s *Server) failure(ctx context.Context, execCtx *a2asrv.ExecutorContext, err error) []*a2a.Part {
	reported := err.Error()
	cfg := s.rt.Config()
	granted := cfg.Secrets
	if key, err := s.keys.of(ctx, execCtx.ContextID); err == nil {
		if sb, err := s.sandboxes.of(ctx, key); err == nil {
			granted = nil
			for _, g := range sb.Secrets {
				granted = append(granted, g.Name)
			}
		}
	}
	text := explainFailure(cfg, granted, reported)
	if text == reported {
		return nil
	}
	slog.Info("the agent failed the turn", "task", execCtx.TaskID, "error", reported)
	return []*a2a.Part{a2a.NewTextPart(text)}
}

// agentExited ends the error of a turn whose agent process exited, such as
// "acp agent error: acp agent exited".
const agentExited = "acp agent exited"

// explainFailure returns the failure text of a turn for callers, in an
// agent container that gets the secrets granted. It tells which key is
// missing when the agent doesn't know the configured model: opencode lists
// no models of a provider without a key, so it reports only that the model
// isn't found.
func explainFailure(cfg *agentdir.Config, granted []string, reported string) string {
	if strings.HasSuffix(reported, agentExited) {
		return "the agent stopped unexpectedly; send the message again"
	}
	text := agentErrorText(reported)
	if !modelNotFound(text) && !authFailed(text) {
		return text
	}
	if provider, _ := harness.ModelKeyWithheld(cfg, granted); provider != "" {
		return "The owner hasn't given this agent a model key. The agent reported: " + text
	}
	if !modelNotFound(text) {
		return text
	}
	if hint := harness.MissingKey(cfg); hint != "" {
		return hint + "; the agent reported: " + text
	}
	// Other agents take their model in their command.
	if !slices.Contains(agentdir.Harnesses, cfg.Harness) {
		return text
	}
	model := "<model>"
	if cfg.Harness == "opencode" {
		model = "<provider/model>"
	}
	return text + "; choose another model with `a2a-serve config set --model " + model + " --restart`"
}

// authFailedText matches the failures of an agent or its provider that
// got no valid credentials, as in "Invalid API key", "Authentication
// required" or claude's "Please run /login".
var authFailedText = regexp.MustCompile(`(?i)\bapi[ _-]?key\b|\bauthenticat|\bunauthori[sz]ed\b|/login\b`)

func authFailed(text string) bool { return authFailedText.MatchString(text) }

// modelNotFoundText matches the failures of an agent or its provider that
// doesn't know the model, as in "model not found: google/x", "models/x is
// not found for API version v1beta", "The model `x` does not exist" or
// Anthropic's not_found_error for "model: x".
var modelNotFoundText = regexp.MustCompile("(?i)\\bmodels?[/: `][^\\n]*?\\b(?:not found|does not exist)|not_found_error[^\\n]*?\\bmodel\\b")

func modelNotFound(text string) bool { return modelNotFoundText.MatchString(text) }

// acpError matches a failure that a2aacp reports for an error response of
// the agent: its message, then its JSON-RPC code and data.
var acpError = regexp.MustCompile(`(?s)^(?:acp agent error: |configuring acp session: )?(.*?) \(code -?\d+\)(?:: .*)?$`)

// jsonrpcErrorNames start the messages of agents that report a standard
// JSON-RPC error with details.
var jsonrpcErrorNames = []string{"Parse error: ", "Invalid request: ", "Method not found: ", "Invalid params: ", "Internal error: "}

// agentErrorText returns the failure text of a turn without the protocol
// details of the agent's error.
func agentErrorText(text string) string {
	m := acpError.FindStringSubmatch(text)
	if m == nil {
		return text
	}
	msg := m[1]
	for _, name := range jsonrpcErrorNames {
		if i := strings.Index(msg, name); i >= 0 {
			msg = msg[:i] + msg[i+len(name):]
			break
		}
	}
	return "the agent failed: " + msg
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
	bus       *chat.Bus
	claims    *CallClaims
	approvals *approvals.Service
	// ends does what follows the end of an execution.
	ends *taskEnds
	// watches ask the A2A caller of a turn the questions of the agents it
	// calls, on the caller's task.
	watches *agentproxy.Watches
	// store has the records of those questions.
	store *store.Store
	// notes are put in the results of tasks.
	notes *toolNotes
	// moved continues conversations whose session is in another workspace.
	moved *movedConversations
	// decider suggests the rule that the always option of a permission
	// allows.
	decider *toolDecider
	// turns counts the executions until a2a-go stored their last event and
	// called Cleanup.
	turns *turnCount
	// stopping ends when the server stops, so that a task waiting for
	// another agent doesn't hold the stop. Nil never ends.
	stopping context.Context
	// answers saves what answers to permission questions allow.
	answers *ruleAnswers
}

var (
	_ a2asrv.AgentExecutor         = (*observedExecutor)(nil)
	_ a2asrv.AgentExecutionCleaner = (*observedExecutor)(nil)
)

// Cleanup ends the execution and relays the question the task stopped on.
// a2a-go calls it once the execution is done with its last event and
// stored it, so an answer to the question can start the next execution.
func (e *observedExecutor) Cleanup(ctx context.Context, execCtx *a2asrv.ExecutorContext, result a2a.SendMessageResult, err error) {
	if e.turns != nil {
		defer e.turns.release(execCtx)
	}
	task, ok := result.(*a2a.Task)
	var state a2a.TaskState
	switch {
	case err != nil:
	case ok:
		state = task.Status.State
	default:
		state = a2a.TaskStateCompleted
	}
	e.ends.ended(ctx, execCtx.TaskID, execCtx.ContextID, state)
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
	q := pendingApproval(execCtx.StoredTask)
	if q == nil {
		return e.observe(ctx, execCtx, e.withCallerQuestion(execCtx, e.turn(ctx, execCtx)))
	}
	opt, chosen := replyOption(execCtx.Message, q)
	_, ownOptions := q.AgentOption(a2aacp.OptionOnce)
	if !answersQuestion(execCtx.Message, q) || (ownOptions && !chosen) {
		return e.observe(ctx, execCtx, refuseReply(execCtx, q))
	}
	answer := execCtx
	if ownOptions {
		p, _ := auth.Caller(ctx)
		if err := e.answers.record(ctx, p, execCtx, q, opt); err != nil {
			slog.Error("saving what an approval allows", "task", execCtx.TaskID, "error", err)
			e.approvals.Release(context.WithoutCancel(ctx), execCtx.TaskID, q.ID, p.ID)
			return e.observe(ctx, execCtx, askAgain(reask(execCtx, "I couldn't save the answer, so the question still waits for an answer. Try again.\n")))
		}
	}
	// The agent takes only structured input for the question, and the
	// reply may be words or an untagged data part.
	if chosen {
		var err error
		if answer, err = agentAnswer(execCtx, q, opt); err != nil {
			slog.Warn("translating an approval", "task", execCtx.TaskID, "error", err)
			p, _ := auth.Caller(ctx)
			e.approvals.Release(context.WithoutCancel(ctx), execCtx.TaskID, q.ID, p.ID)
			return e.observe(ctx, execCtx, askAgain(reask(execCtx, "I couldn't pass the answer on, so the question still waits for an answer. Try again.\n")))
		}
	}
	return e.observe(ctx, execCtx, e.withAnswer(ctx, execCtx, q, e.withCallerQuestion(answer, e.turn(ctx, answer))))
}

// agentAnswer returns execCtx with its reply to q as structured input with
// the agent's option. For a question with the server's options, "always"
// becomes the option that allows once: [ruleAnswers] saved the grant of
// the conversation, and the server enforces it.
func agentAnswer(execCtx *a2asrv.ExecutorContext, q *approvals.Question, opt approvals.Option) (*a2asrv.ExecutorContext, error) {
	agentOption, ok := q.AgentOption(opt.ID)
	if !ok {
		agentOption = opt.ID
	}
	part, err := structuredinput.NewResponsePart(q.ID, a2aacp.Answer{Option: agentOption})
	if err != nil {
		return nil, err
	}
	msg := *execCtx.Message
	msg.Parts = []*a2a.Part{part}
	out := *execCtx
	out.Message = &msg
	return &out, nil
}

// withServerOptions gives the permission questions of the agent the
// server's options, with the narrowest useful rule for the always option.
func (e *observedExecutor) withServerOptions(execCtx *a2asrv.ExecutorContext, events iter.Seq2[a2a.Event, error]) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		for ev, err := range events {
			if err == nil {
				ev = e.serverOptions(execCtx, ev)
			}
			if !yield(ev, err) {
				return
			}
		}
	}
}

func (e *observedExecutor) serverOptions(execCtx *a2asrv.ExecutorContext, ev a2a.Event) a2a.Event {
	var status a2a.TaskStatus
	switch v := ev.(type) {
	case *a2a.TaskStatusUpdateEvent:
		status = v.Status
	case *a2a.Task:
		status = v.Status
	default:
		return ev
	}
	q := approvals.FromStatus(status)
	if q == nil || q.Type != approvals.TypePermission {
		return ev
	}
	rule := ""
	if e.decider != nil {
		rule = toolrules.Suggest(e.decider.call(q.Kind, q.ToolTitle, q.ToolInput))
	}
	msg, err := approvals.WithServerOptions(status.Message, rule)
	if err != nil {
		slog.Warn("writing the options of an approval", "task", execCtx.TaskID, "error", err)
		return ev
	}
	switch v := ev.(type) {
	case *a2a.TaskStatusUpdateEvent:
		c := *v
		c.Status.Message = msg
		return &c
	case *a2a.Task:
		c := *v
		c.Status.Message = msg
		return &c
	}
	return ev
}

// turn runs a turn of the agent, with the line of the agents it may call
// and the earlier messages of a moved conversation added to the prompt and
// kept out of the task store.
func (e *observedExecutor) turn(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	ctx, withLine := e.claims.withAgentsLine(ctx, execCtx)
	if execCtx.Message != nil {
		e.claims.addTokens(execCtx.ContextID, estimateTokens(execCtx.Message.Parts))
	}
	prompted, moved := e.moved.withEarlier(ctx, withLine)
	events := coalesceArtifacts(e.claims.countTokens(execCtx.ContextID, e.withServerOptions(execCtx, e.inner.Execute(ctx, prompted))), coalesceWindow)
	if prompted == execCtx && !moved {
		return events
	}
	return func(yield func(a2a.Event, error) bool) {
		if moved {
			e.notes.add(execCtx.TaskID, movedNote)
		}
		for ev, err := range events {
			if t, ok := ev.(*a2a.Task); ok {
				ev = withOriginalMessage(t, prompted.Message, execCtx.Message)
			}
			if !yield(ev, err) {
				return
			}
		}
	}
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
	return e.observe(ctx, execCtx, canceledFirst(execCtx, e.cancelingCalls(ctx, execCtx, e.inner.Cancel(context.WithoutCancel(ctx), execCtx))))
}

// cancelingCalls cancels the tasks that the task started at other agents,
// once the inner Cancel stopped its turn and with it new calls.
func (e *observedExecutor) cancelingCalls(ctx context.Context, execCtx *a2asrv.ExecutorContext, events iter.Seq2[a2a.Event, error]) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		defer e.cancelCalls(context.WithoutCancel(ctx), execCtx)
		for ev, err := range events {
			if !yield(ev, err) {
				return
			}
		}
	}
}

func (e *observedExecutor) cancelCalls(ctx context.Context, execCtx *a2asrv.ExecutorContext) {
	if e.watches == nil || e.claims == nil {
		return
	}
	if err := e.watches.CancelCalls(ctx, e.claims.claimsOf(ctx, execCtx)); err != nil {
		slog.Warn("canceling the calls of a canceled task", "task", execCtx.TaskID, "error", err)
	}
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
		if e.turns != nil {
			e.turns.hold(execCtx)
		}
		start := time.Now()
		from := ""
		if p, ok := auth.Caller(ctx); ok {
			from = p.ID
		}
		slog.Info("task received", "task", execCtx.TaskID, "context", execCtx.ContextID, "from", from)
		stopNotes := e.notes.start(execCtx.TaskID)
		defer stopNotes()
		closed := false
		for ev, err := range events {
			if err != nil {
				slog.Info("task failed", "task", execCtx.TaskID, "duration", time.Since(start).Round(time.Millisecond), "error", err)
			}
			if state, ok := eventState(ev); ok && err == nil && state.Terminal() {
				ev = withNotes(execCtx, ev, e.notes.take(execCtx.TaskID))
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
// agent chains go and the budget of guests.
type callerInterceptor struct {
	a2asrv.PassthroughCallInterceptor
	approvals *approvals.Service
	tasks     *store.TaskStore
	budget    *guestBudget
	keys      *agentKeys
	users     *userAgents
	// dir holds the agent homes of members.
	dir agentdir.Dir
	// removeHome forgets what the server keeps about an agent home that
	// goes.
	removeHome func(home string)
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
		_, err := i.tasks.Header(ctx, sub.ID)
		return ctx, nil, err
	}
	// Guests who proved nothing share one principal, so they
	// list only the conversation they name, whose ID proves it is theirs.
	if list, ok := req.Payload.(*a2a.ListTasksRequest); ok && p.ID == auth.GuestID && list.ContextID == "" {
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
		if err := i.checkWaiting(ctx, p, msg.Message.ContextID); err != nil {
			return ctx, nil, err
		}
		if err := i.checkUserAgents(ctx, msg.Message.ContextID); err != nil {
			return ctx, nil, err
		}
	}
	if p.Role == auth.RoleGuest && msg.Message.TaskID == "" {
		if err := i.budget.admit(budgetListener(p)); err != nil {
			return ctx, nil, fmt.Errorf("%w: %w", a2a.ErrUnauthorized, err)
		}
	}
	if msg.Message.TaskID != "" {
		// a2a-go reports a running execution before it loads the task in
		// the caller's scope, which tells another person's task from an
		// unknown one.
		header, headerErr := i.tasks.Header(ctx, msg.Message.TaskID)
		if errors.Is(headerErr, a2a.ErrTaskNotFound) {
			return ctx, nil, headerErr
		}
		if err := i.tasks.CheckNotInterrupted(ctx, msg.Message.TaskID); err != nil {
			return ctx, nil, err
		}
		if err := i.approvals.CheckAnswer(ctx, p, msg.Message.TaskID); err != nil {
			return ctx, nil, err
		}
		if headerErr == nil {
			ctx = i.withReply(ctx, msg.Message, header.Task)
		}
	}
	return ctx, nil, nil
}

// replyKey is the context key of the [reply] a request sends.
type replyKey struct{}

// reply is a message to an existing task, which the caller can see.
type reply struct {
	task      a2a.TaskID
	contextID string
	// approval is set when the reply answers the approval that the task
	// waits for, or that an answer just took.
	approval bool
}

// withReply returns ctx with msg, the reply to the task of header that it
// sends, for After. header is the task without its history, which is read
// only when the status holds no approval.
func (i *callerInterceptor) withReply(ctx context.Context, msg *a2a.Message, header *a2a.Task) context.Context {
	q := pendingApproval(header)
	if q == nil {
		stored, err := i.tasks.Get(ctx, msg.TaskID)
		if err != nil {
			return ctx
		}
		q = lastQuestion(stored.Task)
	}
	approval := q != nil && answersQuestion(msg, q)
	return context.WithValue(ctx, replyKey{}, reply{task: msg.TaskID, contextID: header.ContextID, approval: approval})
}

// lastQuestion returns the question of the last agent message in the
// history of task, which the messages after it answer.
func lastQuestion(task *a2a.Task) *approvals.Question {
	for _, m := range slices.Backward(task.History) {
		if m.Role == a2a.MessageRoleAgent {
			return approvals.FromMessage(m)
		}
	}
	return nil
}

// After names the task, and what to do next with the a2a CLI, in the error
// of a reply that the task refused since it already runs an execution.
// In-process senders, such as chats, render the outcome themselves.
func (i *callerInterceptor) After(ctx context.Context, _ *a2asrv.CallContext, resp *a2asrv.Response) error {
	r, ok := ctx.Value(replyKey{}).(reply)
	if !ok || auth.IsInProcess(ctx) || !a2aerr.ExecutionInProgress(resp.Err) {
		return nil
	}
	get := a2aCommand(ctx, "task get", string(r.task))
	if r.approval {
		resp.Err = a2aerr.InProgress(fmt.Sprintf("task %s already got an answer to this question; see where it stands with %s", r.task, get))
		return nil
	}
	resp.Err = a2aerr.InProgress(fmt.Sprintf("task %s is still working on an earlier message; check it with %s, and once it is done, continue with %s", r.task, get, a2aCommand(ctx, "send", "--context-id "+r.contextID)))
	return nil
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

// checkUserAgents refuses a new task in a conversation of a member while
// that member's agent process can't run, so that the caller gets
// an error to retry on and no failed task is stored.
func (i *callerInterceptor) checkUserAgents(ctx context.Context, contextID string) error {
	key, err := i.keys.of(ctx, contextID)
	if err != nil {
		return fmt.Errorf("looking up the session of conversation %s: %w", contextID, err)
	}
	if err := i.checkCallerHomes(key); err != nil {
		return err
	}
	if _, ok := agentkey.UserOf(key); !ok || i.users.hasRoom(key) {
		return nil
	}
	return fmt.Errorf("%w: %w", a2a.ErrServerError, errAgentsBusy)
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
func (i *callerInterceptor) checkWaiting(ctx context.Context, p auth.Principal, contextID string) error {
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
		err := i.approvals.CheckAnswer(ctx, p, t.ID)
		if errors.Is(err, a2a.ErrTaskNotFound) || errors.Is(err, a2a.ErrUnauthorized) {
			return fmt.Errorf("%w: conversation %s waits for the agent's owner to answer %q; send the message again after they answer, or leave out the context ID to start a new conversation",
				a2a.ErrInvalidRequest, contextID, q.Title)
		}
		if err != nil {
			return err
		}
		ids := make([]string, len(q.Options))
		for i, o := range q.Options {
			ids[i] = o.ID
		}
		answer := "with " + a2aCommand(ctx, "send", "--task-id "+string(t.ID)+" <answer>")
		if len(ids) > 0 {
			answer = fmt.Sprintf("with one of %s, as in %s", strings.Join(ids, ", "), a2aCommand(ctx, "send", "--task-id "+string(t.ID)+" "+ids[0]))
		}
		return fmt.Errorf("%w: task %s in conversation %s waits for an answer to %q; answer it first %s, or leave out the context ID to start a new conversation",
			a2a.ErrInvalidRequest, t.ID, contextID, q.Title, answer)
	}
	return nil
}
