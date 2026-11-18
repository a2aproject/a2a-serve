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
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/a2aproject/a2a-go/v2/integrations/executor/a2aacp"

	"github.com/a2aproject/a2a-serve/core/a2aserve"
	"github.com/a2aproject/a2a-serve/core/agent"
	"github.com/a2aproject/a2a-serve/core/egress"
	"github.com/a2aproject/a2a-serve/core/sandbox"
	"github.com/a2aproject/a2a-serve/core/workspace"
	"github.com/a2aproject/a2a-serve/internal/a2acli"
	"github.com/a2aproject/a2a-serve/internal/a2atext"
	"github.com/a2aproject/a2a-serve/internal/access"
	"github.com/a2aproject/a2a-serve/internal/agentcfg"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/agentkey"
	"github.com/a2aproject/a2a-serve/internal/agentprofile"
	"github.com/a2aproject/a2a-serve/internal/agentrun"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/chat"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/boundary"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/egressrules"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
	"github.com/a2aproject/a2a-serve/internal/projectfiles"
	"github.com/a2aproject/a2a-serve/internal/store"
	"github.com/a2aproject/a2a-serve/internal/toolrules"
	"github.com/a2aproject/a2a-serve/internal/workspaces"
)

// newAgent builds the agent of the server with core/a2aserve, which starts
// the owner's agent process. The hooks it gets read the components that
// build creates later, when they run.
func (s *Server) newAgent(ctx context.Context, opts Options) error {
	cfg := s.rt.Config()
	profile, err := agent.Lookup(cfg.Harness, cfg.Command)
	if err != nil {
		return err
	}
	mounts, err := s.dir.Mounts(cfg)
	if err != nil {
		return err
	}
	main, err := callers{s}.Environment(ctx, "")
	if err != nil {
		return err
	}
	b, err := egress.New(egress.Config{Policy: s.egressPolicy})
	if err != nil {
		return err
	}
	ac := a2aserve.Config{
		Profile: profile,
		Model:   cfg.Model,
		Runtime: sandboxRuntime(cfg, opts),
		Egress:  b,
		Tools:   s.toolDecider(),
		Main:    main,
		Callers: callers{s},
		Pools: map[string]a2aserve.Pool{
			// Approvals reject questions after their timeout; this stops the
			// turn if that answer doesn't arrive.
			guestPool: {QuestionTimeout: s.timings.GuestQuestionTimeout + time.Minute, Label: "the agent of guests"},
			usersPool: {MaxProcesses: maxUserAgents, QuestionTimeout: s.timings.UserQuestionTimeout + time.Minute, Label: "the agent processes of users"},
		},
		Sessions: sessionStore{s.sessions},
		AgentAPI: http.HandlerFunc(s.serveAgentAPI),
		Hooks: a2aserve.Hooks{
			Turn:    func(ctx context.Context, execCtx *a2asrv.ExecutorContext) func() { return s.claims.turn(ctx, execCtx) },
			Explain: s.failure,
			Refused: func(err error) string { return agentcfg.Explain(err).Error() },
			Prompt: func(ctx context.Context, execCtx *a2asrv.ExecutorContext) ([]*a2a.Part, error) {
				return s.queued.prompt(ctx, execCtx)
			},
			Stalled:   stalledTurn,
			Abandoned: runnerTasks{s}.Abandon,
			Denied:    func(execCtx *a2asrv.ExecutorContext, title string) { s.notes.Add(execCtx.TaskID, deniedNote(title)) },
			LeftOutMCP: func(ctx context.Context, server string, missing []string) {
				slog.WarnContext(ctx, "leaving out an MCP server with variables that the conversation's container doesn't get; pass them with --secret, or give them in the sandbox rule of its user", "server", server, "missing", missing)
			},
			Hold:          s.holdBudget,
			CanStart:      s.canStart,
			PrepareHome:   s.prepareHome,
			AgentAPIError: func(err error) { reportFatal(s.fatal, err) },
		},
		A2A: a2aserve.A2A{
			TaskStore:          s.tasks.CutLists(),
			Materializer:       s.tasks.UpdateMaterializer(),
			Interceptors:       s.interceptors(),
			Middleware:         func(inner a2asrv.AgentExecutor) a2asrv.AgentExecutor { return s.observe(ctx, inner) },
			MaxExecutions:      maxExecutions,
			MaxScopeExecutions: maxExecutionsOf,
			Capabilities:       &a2a.AgentCapabilities{Streaming: true},
		},
		StateDir: s.dir.StateDir(),
		Stderr:   opts.HarnessLog,
	}
	cli := ""
	if cfg.Sandbox == agentprofile.SandboxLocal {
		cli = localCLI()
	}
	err = agentrun.SetOptions(&ac, agentrun.Options{
		SocketDir:       s.dir.HarnessSocketDir(),
		Pins:            s.pins,
		Mounts:          mounts,
		Egress:          func() agentprofile.Egress { return agentcfg.Egress(s.rt.Config()) },
		AskCommands:     func() bool { return toolrules.DeniesCommands(s.rt.Config().Approvals.Deny) },
		DisabledMCP:     func() []string { return s.rt.Config().MCP.Disabled },
		CLI:             cli,
		PruneLabels:     map[string]string{"a2a.dir": s.dir.Root},
		TurnStall:       s.timings.TurnStallTimeout,
		ToolStall:       s.timings.ToolStallTimeout,
		MaxAgentMessage: opts.MaxAgentMessage,
		Out:             s.out,
		MainPrepared:    s.mainPrepared,
		AllSessions:     s.store.Sessions(),
		Executor:        opts.Executor,
	})
	if err != nil {
		return errors.Join(err, b.Close())
	}
	a, err := a2aserve.New(ctx, ac)
	if err != nil {
		return errors.Join(err, b.Close())
	}
	engine, err := agentrun.Of(a)
	if err != nil {
		return errors.Join(err, a.Close(ctx), b.Close())
	}
	s.agent, s.engine, s.boundary = a, engine, b
	return nil
}

// sandboxRuntime returns the runtime of the sandbox setting of cfg.
func sandboxRuntime(cfg *agentdir.Config, opts Options) sandbox.Runtime {
	if cfg.Sandbox == agentprofile.SandboxLocal {
		return sandbox.Local()
	}
	progress := opts.ImageProgress
	if progress == nil {
		progress = opts.HarnessLog
	}
	return sandbox.Docker(sandbox.DockerOptions{Progress: progress})
}

// egressPolicy is the policy of the egress boundary under the current
// settings.
func (s *Server) egressPolicy() egress.Policy {
	cfg := s.rt.Config()
	return egress.Policy{Mode: egress.Mode(cfg.Egress.Mode), Allow: cfg.Egress.Allow}
}

// serveAgentAPI serves the agents URLs to the agent processes. A request
// that arrives while the server starts waits for the routes.
func (s *Server) serveAgentAPI(w http.ResponseWriter, r *http.Request) {
	select {
	case <-s.agentRoutesReady:
	case <-r.Context().Done():
		return
	}
	s.agentRoutes.ServeHTTP(w, r)
}

// processOf returns the agent key of the process that a request on the
// agents URLs came from.
func processOf(r *http.Request) (string, bool) { return a2aserve.ProcessKey(r.Context()) }

// stalledTurn is the status message of a turn that the runner stopped
// since the agent sent nothing for d: its model may refuse requests. A
// turn whose tool call ran long gets the runner's own.
func stalledTurn(d time.Duration, tool bool) []*a2a.Part {
	if tool {
		return nil
	}
	return []*a2a.Part{a2a.NewTextPart(fmt.Sprintf("the agent sent nothing for %s, so the server stopped the turn. Its model may refuse requests, for example a free model over its usage limit: set a model and its key with `a2a-serve config set --model <provider/model> --secret <KEY>`", d))}
}

// deniedNote notes in the result of a task a tool call that the tool rules
// denied.
func deniedNote(title string) string { return "Tool call denied by --deny-tools: " + title + "." }

// localCLI returns the a2a CLI on PATH, with which an agent without a
// sandbox calls other agents, or "" when there is none. The setup problems
// that the server prints on start report a missing or outdated CLI.
func localCLI() string {
	cli, err := a2acli.Path()
	if err != nil {
		slog.Debug("no a2a CLI for the agent", "error", err)
		return ""
	}
	return cli
}

// callers tells the agent about agent keys: the owner's process for "",
// the one of guests, and one per member and per person behind a member's
// calls.
type callers struct{ s *Server }

// Key returns the agent key of the caller who starts a conversation.
func (c callers) Key(ctx context.Context, _ string) (string, error) {
	return agentkey.Of(auth.Caller(ctx)), nil
}

// Admit lets the caller in ctx write into the container of an agent key,
// and then lets the container take the turn under the sandbox rules in
// force.
func (c callers) Admit(ctx context.Context, key string, execCtx *a2asrv.ExecutorContext) (func(), error) {
	release, err := c.s.admitCaller(ctx, key, execCtx)
	if err != nil {
		return nil, err
	}
	counted, err := c.s.admitTurn(ctx, key, execCtx)
	if err != nil {
		release()
		return nil, err
	}
	return func() {
		counted()
		release()
	}, nil
}

// Environment describes the agent process of key under the sandbox rules
// and the settings in force.
func (c callers) Environment(ctx context.Context, key string) (a2aserve.Environment, error) {
	s := c.s
	owner, sb, err := s.sandboxes.grant(ctx, key)
	if err != nil {
		return a2aserve.Environment{}, err
	}
	cfg := s.rt.Config()
	env := a2aserve.Environment{
		Home:   s.dir.HomeDir(),
		Image:  cfg.Image,
		Name:   "a2a-" + cfg.Name + "-" + s.dir.ID(),
		Labels: map[string]string{"a2a.agent": cfg.Name, "a2a.dir": s.dir.Root},
		Limits: sandbox.Limits{Memory: cfg.Limits.Memory, CPUs: cfg.Limits.CPUs, PIDs: cfg.Limits.PIDs},
		Hide:   []string{s.dir.Path()},
	}
	proc := agentrun.ProcessOptions{Storage: sb.Storage, State: sb}
	_, isUser := agentkey.UserOf(key)
	switch {
	case key == "":
	case key == agentkey.Guest:
		// Guests get no way to call other agents: no socket, credential
		// helper or CLI.
		env.Pool, env.Name, env.Home, proc.NoAgentAPI = guestPool, env.Name+"-guest", s.dir.GuestHomeDir(), true
		env.Labels["a2a.role"] = auth.RoleGuest
		proc.LockHome = true
	case isUser:
		env.Pool, env.Name, env.Home = usersPool, env.Name+"-"+userProcessName(key), s.userHomeDir(key)
		env.Labels["a2a.role"] = auth.RoleMember
		proc.LockHome = true
		proc.Parent = parentOf(key)
	default:
		return a2aserve.Environment{}, fmt.Errorf("unknown agent key %q", key)
	}
	values, inject, missing, err := secretEnv(cfg.Inject, sb, s.rt.Secrets())
	switch {
	case err != nil:
		proc.StartError, proc.SessionError = err, err
	case len(missing) > 0:
		proc.StartError = &agentcfg.MissingSecretError{Name: missing[0], Main: key == ""}
	}
	env.Env = append(slices.Clone(cfg.Env), values...)
	for _, inj := range inject {
		cred, ok := boundary.CredentialOf(inj).(egress.Credential)
		if !ok {
			return a2aserve.Environment{}, errors.New("no egress credentials without core/egress")
		}
		env.Credentials = append(env.Credentials, cred)
	}
	proc.MCPVars = projectfiles.EnvVars(cfg.Env, values)
	mounts, err := s.dir.Mounts(cfg)
	if err != nil {
		proc.StartError, proc.SessionError = err, err
		mounts = []agentdir.Mount{{}}
	}
	s.workspaceOf(&env, &proc, cfg, key, owner.ID, sb.Storage, mounts)
	if err := agentrun.SetProcess(&env, proc); err != nil {
		return a2aserve.Environment{}, err
	}
	return env, nil
}

// parentOf returns the agent key whose stop also stops the process of key:
// the user's own process for a person behind the user's calls.
func parentOf(key string) string {
	id, ok := agentkey.UserOf(key)
	if ok && agentkey.CallerOf(key) != "" {
		return agentkey.User(id)
	}
	return ""
}

// workspaceOf gives env the workspace and the mounts of a process of key
// with storage, and proc the MCP servers its sessions get. Without the
// Docker sandbox, storage doesn't apply. Own and empty storage mount a
// private workspace, which the process sees at the workspace's path.
func (s *Server) workspaceOf(env *a2aserve.Environment, proc *agentrun.ProcessOptions, cfg *agentdir.Config, key, owner, storage string, mounts []agentdir.Mount) {
	ws := mounts[0]
	local := cfg.Sandbox == agentprofile.SandboxLocal
	if local {
		storage = workspaces.StorageShared
	}
	var w workspace.Workspace = workspace.Dir(ws.Src)
	switch {
	case ws.Dst != ws.Src:
		w = pathError{err: &agentprofile.WorkspacePathError{Src: ws.Src, Dst: ws.Dst}}
	case storage == workspaces.StorageOwn || storage == workspaces.StorageEmpty:
		w = &privateWorkspace{copies: s.copies, name: workspaceName(key), owner: owner, storage: storage, src: ws.Src}
	case ws.ReadOnly && !local || storage == workspaces.StorageReadOnly:
		w = workspace.ReadOnly(w)
	}
	env.Workspace = w
	for _, m := range grantMounts(storage, mounts)[1:] {
		env.Mounts = append(env.Mounts, sandbox.Mount{Host: m.Src, Path: m.Dst, ReadOnly: m.ReadOnly})
	}
	switch storage {
	case workspaces.StorageEmpty:
		proc.NoMCP = true
	case workspaces.StorageOwn:
		proc.MCPDir = s.copies.Dir(workspaceName(key), workspaces.StorageOwn)
	default:
		proc.MCPDir = ws.Src
	}
}

// grantMounts returns the mounts of a container with storage: as they are
// for shared, read-only for read-only and own, and only the workspace for
// empty. Own and empty mount a private workspace in place of the workspace.
func grantMounts(storage workspaces.Storage, mounts []agentprofile.Mount) []agentprofile.Mount {
	switch storage {
	case workspaces.StorageShared:
		return mounts
	case workspaces.StorageEmpty:
		ws := mounts[0]
		ws.ReadOnly = false
		return []agentprofile.Mount{ws}
	}
	out := slices.Clone(mounts)
	for i := range out {
		out[i].ReadOnly = storage != workspaces.StorageOwn || i > 0
	}
	return out
}

// privateWorkspace is the private workspace of a process, own or empty,
// named after its agent key and recorded as the owner's, which a container
// mounts at the path of the workspace.
type privateWorkspace struct {
	copies               *workspaces.Copies
	name, owner, storage string
	src                  string
}

func (p *privateWorkspace) Attach(ctx context.Context, _ string) (workspace.Attachment, error) {
	dir, err := p.copies.Prepare(ctx, p.name, p.owner, p.storage, p.src)
	if errors.Is(err, workspaces.ErrTooLarge) || errors.Is(err, workspaces.ErrTooMany) {
		return workspace.Attachment{}, &agentcfg.CopyError{Err: err}
	}
	if err != nil {
		return workspace.Attachment{}, err
	}
	return workspace.Attachment{Dir: dir, Path: p.src, Release: func() error { return nil }}, nil
}

// pathError is a workspace that a process can't see at its own path.
type pathError struct{ err error }

func (p pathError) Attach(context.Context, string) (workspace.Attachment, error) {
	return workspace.Attachment{}, p.err
}

// sessionStore gives the agent the sessions of the conversations in the
// workspace.
type sessionStore struct{ s store.SessionStore }

func (ss sessionStore) Get(ctx context.Context, contextID string) (a2aserve.Session, bool, error) {
	sess, ok, err := ss.s.Get(ctx, contextID)
	return a2aserve.Session{ID: sess.ID, Key: sess.AgentKey}, ok, err
}

func (ss sessionStore) Put(ctx context.Context, contextID string, sess a2aserve.Session) error {
	return ss.s.Put(ctx, contextID, a2aacp.Session{ID: sess.ID, AgentKey: sess.Key})
}

// canStart refuses to start the agent process of a member whose pairing
// was removed.
func (s *Server) canStart(ctx context.Context, key string) error {
	if id, ok := agentkey.UserOf(key); ok {
		return s.checkUser(ctx, id)
	}
	return nil
}

// stateOf returns the sandbox that an agent process started with, from
// its state.
func stateOf(state any) access.Sandbox {
	sb, _ := state.(access.Sandbox)
	return sb
}

// startedWith returns the sandbox that the container of key started with, if
// it runs. The owner's container keeps it until the restart.
func (s *Server) startedWith(key string) (access.Sandbox, bool) {
	if s.engine == nil {
		return access.Sandbox{}, false
	}
	state, ok := s.engine.StartedWith(key)
	return stateOf(state), ok
}

// prepareHome writes the storage note and the instructions into the agent
// home of key, after it checks that the home is private.
func (s *Server) prepareHome(ctx context.Context, key string, env a2aserve.Environment) error {
	home := env.Home
	s.setStorageNote(home, stateOf(agentrun.ProcessOf(env).State).Storage)
	_, isUser := agentkey.UserOf(key)
	switch {
	case key == agentkey.Guest:
		if err := fsutil.CheckPrivate(home); err != nil {
			return err
		}
		s.writeStorageNote(s.rt.Config(), home)
	case isUser:
		for _, dir := range []string{s.dir.UserHomesDir(), home} {
			if err := fsutil.CheckPrivate(dir); err != nil {
				return err
			}
		}
		return s.writeInstructionsIn(ctx, home)
	default:
		if err := s.writeInstructionsIn(ctx, home); err != nil {
			s.logf("! updating the agent instructions: %v", err)
		}
	}
	return nil
}

// mainPrepared prints the sandbox of the owner's agent process.
func (s *Server) mainPrepared(p agentrun.Prepared) {
	s.logf("✓ %s", p.Summary)
	mounts, err := s.dir.Mounts(s.rt.Config())
	if err != nil {
		slog.Debug("reading the mounts of the agent", "error", err)
	}
	mountDesc := make([]string, len(mounts))
	for i, m := range mounts {
		mountDesc[i] = m.String()
	}
	s.logf("✓ workspace %s; config %s", strings.Join(mountDesc, ", "), s.dir.ConfigPath())
	for _, w := range p.Warnings {
		s.logf("! %s", agentcfg.Warning(w))
	}
	if p.NoBoundary != "" {
		slog.Warn(agentcfg.NoBoundary(p.NoBoundary))
	}
	s.noticeEgress(p.Egress)
}

// startError words the error of an agent that didn't start.
func startError(err error) error {
	if errors.Is(err, agentrun.ErrStartTimeout) {
		err = agenterr.WithHint(err, agenterr.HintAgentStartTimeout, "")
	}
	return agentcfg.Explain(err)
}

// runnerTasks is what the agent needs of the task store and the handler.
type runnerTasks struct{ s *Server }

func (t runnerTasks) Cancel(ctx context.Context, id a2a.TaskID) error {
	as := auth.InProcess(ctx, auth.Owner())
	if _, err := t.s.sender.CancelTask(as, &a2a.CancelTaskRequest{ID: id}); err != nil && !errors.Is(err, a2a.ErrTaskNotCancelable) {
		return err
	}
	return nil
}

// Abandon stores the event that ends a task whose agent stopped while it
// waited for an answer. A reply may have ended the task first; the store
// refuses tasks in a terminal state. The next start fails the open tasks
// as restarted.
func (t runnerTasks) Abandon(ctx context.Context, ev *a2a.TaskStatusUpdateEvent) error {
	_, err := t.s.tasks.Update(ctx, &taskstore.UpdateRequest{Task: &a2a.Task{ID: ev.TaskID}, Event: ev, PrevVersion: taskstore.TaskVersionMissing})
	if errors.Is(err, a2a.ErrUnsupportedOperation) {
		return nil
	}
	if err != nil {
		return err
	}
	slog.Info("task "+a2atext.StateName(ev.Status.State), "task", ev.TaskID, "reason", "the agent stopped while the task waited for input")
	t.s.ends.finish(ctx, ev)
	return nil
}

// stopUsers cancels the unfinished tasks that removing principals deletes
// and stops their agent processes, which don't start again until release
// is called. It implements [pairings.Users].
type stopUsers struct {
	tasks     *store.TaskStore
	sender    *handlerSender
	agent     *a2aserve.Agent
	posted    *chat.PostedTurns
	sandboxes *sandboxes
}

func (u stopUsers) Stop(ctx context.Context, principals ...string) (func(), error) {
	// Tasks are canceled before the starts are held: a task that waits for
	// its agent process to start would keep its cancellation waiting.
	u.posted.DropUsers(principals)
	as := auth.InProcess(ctx, auth.Owner())
	keys := make([]string, len(principals))
	for i, p := range principals {
		keys[i] = agentkey.User(p)
		ids, err := u.tasks.UnfinishedRemoved(ctx, store.PairingUser{Principal: p, AgentKey: keys[i], CallerKeys: agentkey.CallerPrefix(p)})
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			if _, err := u.sender.CancelTask(as, &a2a.CancelTaskRequest{ID: id}); err != nil && !errors.Is(err, a2a.ErrTaskNotCancelable) {
				return nil, fmt.Errorf("canceling task %s: %w", id, err)
			}
		}
	}
	release, err := u.agent.Stop(ctx, keys...)
	if err != nil {
		return nil, err
	}
	u.sandboxes.forget(keys...)
	return release, nil
}

// userHomes deletes the homes of users' agent processes and forgets their
// pins. It implements [pairings.Homes].
type userHomes struct{ s *Server }

func (h userHomes) RemoveUserHome(id string) error {
	homes, err := h.s.dir.CallerHomeDirs(id)
	for _, home := range append(homes, h.s.dir.UserHomeDir(id)) {
		h.s.forgetHome(home)
	}
	return errors.Join(err, h.s.dir.RemoveUserHome(id))
}

// forgetHome forgets the pins and the storage note of an agent home that
// goes.
func (s *Server) forgetHome(home string) {
	s.pins.Unpin(home)
	s.forgetStorageNote(home)
}

func (h userHomes) UserHomeDir(id string) string { return h.s.dir.UserHomeDir(id) }

func (h userHomes) UserWorkspacesDir(id string) string { return h.s.dir.UserWorkspacesDir(id) }

func (h userHomes) CallerHomeDirs(id string) ([]string, error) { return h.s.dir.CallerHomeDirs(id) }

func (h userHomes) CallerWorkspacesDirs(id string) ([]string, error) {
	return h.s.dir.CallerWorkspacesDirs(id)
}

// errRemovedUser is returned when the agent process of a user whose
// pairing or webhook node was removed would start.
var errRemovedUser = errors.New("this user's pairing was removed")

// checkUser returns errRemovedUser if the principal id names a pairing or
// webhook node that no longer exists.
func (s *Server) checkUser(ctx context.Context, id string) error {
	identity := auth.IdentityOf(id)
	var err error
	switch {
	case identity.Pairing != "":
		_, err = s.store.Pairing(ctx, identity.Pairing)
	case identity.WebhookNode != "":
		_, err = s.store.Node(ctx, identity.WebhookNode)
	}
	if errors.Is(err, store.ErrNotFound) {
		return errRemovedUser
	}
	return err
}

// egressBoundary reports whether the containers of cfg get the egress
// boundary: as the last container started, or as [agentcfg.DecideEgress]
// decides before the first.
func (s *Server) egressBoundary(cfg *agentdir.Config) bool {
	if s.engine == nil {
		return agentcfg.HostEgress(cfg).Boundary
	}
	if st := s.engine.Egress(); st.Started {
		return st.Problem == "" && cfg.Sandbox != agentprofile.SandboxLocal && cfg.Egress.Mode != egressrules.ModeUnrestricted
	}
	return agentcfg.HostEgress(cfg).Boundary
}

// egressStatus returns what status shows about the sandbox's connections:
// the mode the containers run with, the current allow rules, and the
// latest refused destinations.
func (s *Server) egressStatus(cfg *agentdir.Config) *controlapi.EgressStatus {
	if cfg.Sandbox == agentprofile.SandboxLocal && cfg.Egress.Mode == "" && len(cfg.Egress.Allow) == 0 {
		return nil
	}
	es := s.engine.Egress()
	bs := s.boundary.Status()
	st := &controlapi.EgressStatus{Mode: cmp.Or(es.Mode, cfg.Egress.Mode, egressrules.ModeInternet), Allow: cfg.Egress.Allow, Blocked: bs.Refused}
	if es.Problem != "" {
		st.Problem = agentcfg.NoBoundary(es.Problem)
	}
	for _, r := range cfg.Inject {
		st.Inject = append(st.Inject, controlapi.EgressInjection{Host: r.Target(), Secrets: r.Secrets()})
	}
	if st.Problem == "" {
		st.Problem = agentcfg.HostEgress(cfg).Warning
	}
	p, err := egressrules.NewPolicy(st.Mode, cfg.Egress.Allow)
	if err != nil {
		p = egressrules.Policy{Mode: egressrules.ModeAllowlist}
	}
	for _, b := range bs.Recent {
		st.Recent = append(st.Recent, controlapi.EgressBlock{Destination: b.Destination, Allow: b.Rule, Unresolved: b.Unresolved, Allowed: !b.Unresolved && allowedByRule(p, b.Rule)})
	}
	return st
}

// allowedByRule reports whether a rule of p names the destination host:port.
func allowedByRule(p egressrules.Policy, dest string) bool {
	host, port, err := net.SplitHostPort(dest)
	if err != nil {
		return false
	}
	n, err := egressrules.ParsePort(port)
	if err != nil {
		return false
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		_, ok := p.AddrRule(addr, n)
		return ok
	}
	_, ok := p.NameRule(host, n)
	return ok
}

// egressNoticeFile records in the state directory that start told the
// owner that the sandbox blocks private networks.
const egressNoticeFile = "egress-notice"

// noticeEgress tells the owner of an agent that an earlier version created
// once, at the first start with the egress boundary, that the sandbox now
// blocks the LAN and the tailnet. A new agent only records it.
func (s *Server) noticeEgress(boundary bool) {
	cfg := s.rt.Config()
	if !boundary || cfg.Egress.Mode == egressrules.ModeAllowlist {
		return
	}
	marker := filepath.Join(s.dir.StateDir(), egressNoticeFile)
	if _, err := os.Stat(marker); err == nil {
		return
	}
	if err := fsutil.WriteAtomic(marker, nil, 0o600); err != nil {
		slog.Warn("recording the egress notice", "error", err)
		return
	}
	if !s.existed {
		return
	}
	s.logf("! the sandbox now blocks the LAN, the tailnet and this machine; allow a host with a2a-serve config set --egress-allow <host>")
}
