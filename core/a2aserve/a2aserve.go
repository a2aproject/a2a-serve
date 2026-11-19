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

// Package a2aserve serves a coding agent as an A2A agent. It runs the agent
// in sandboxes, one per caller key if asked, and lets the caller decide
// tools, sessions and callers.
//
// An Agent runs a [Profile] with a [Workspace] per [Environment], and asks
// its [ToolDecider] about tool calls. It runs the processes in Docker unless
// Config.Runtime gives another [sandbox.Runtime], and an [egress.Boundary]
// decides their connections.
package a2aserve

import (
	"context"
	"errors"
	"io"
	"iter"
	"log/slog"
	"maps"
	"net/http"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aevent"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/limiter"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/a2aproject/a2a-go/v2/integrations/executor/a2aacp"

	"github.com/a2aproject/a2a-serve/core/egress"
	"github.com/a2aproject/a2a-serve/core/sandbox"
	"github.com/a2aproject/a2a-serve/internal/agentrun"
	"github.com/a2aproject/a2a-serve/internal/projectfiles"
	"github.com/a2aproject/a2a-serve/internal/turncount"
)

func init() {
	agentrun.Register(agentrun.Extensions{
		SetOptions: func(v any, o agentrun.Options) bool {
			cfg, ok := v.(*Config)
			if ok && cfg != nil {
				cfg.options = &o
			}
			return ok && cfg != nil
		},
		SetProcess: func(v any, p agentrun.ProcessOptions) bool {
			env, ok := v.(*Environment)
			if ok && env != nil {
				env.process = &p
			}
			return ok && env != nil
		},
		RunnerOf: func(v any) (*agentrun.Runner, bool) {
			a, ok := v.(*Agent)
			if !ok || a == nil {
				return nil, false
			}
			return a.r, true
		},
	})
}

// Config configures an [Agent]. It takes built objects, which the Agent
// calls, and never their settings.
type Config struct {
	Profile Profile
	// Model selects the model of every session, in the agent's format.
	// Empty keeps the agent's default.
	Model string
	// Runtime runs the processes; nil runs them in Docker containers, as
	// sandbox.Docker with no options does. The env files of Docker
	// containers and the sockets of their egress boundaries go to the
	// StateDir of sandbox.DockerOptions, and without one to StateDir.
	Runtime sandbox.Runtime
	// Egress is the boundary each process gets an endpoint of, with the
	// Credentials of its Environment, and whose router sees the process's
	// key as [egress.Flow.Key]. Nil gives the processes the runtime's own
	// network. A boundary with an egress.Router needs a runtime that
	// forces the connections through it: on the local runtime a process
	// fails to start with sandbox.ErrNoBoundary.
	Egress *egress.Boundary
	// Tools decide tool calls: the first that decides a call answers it.
	// A call that none decides is asked about.
	Tools []ToolDecider
	// Main is what the process of key "" gets: at its start, and for its
	// turns and sessions.
	Main Environment
	// Callers runs different callers in different processes; nil runs
	// every conversation in Main.
	Callers Callers
	// Pools are the pools of processes besides the main one, by the name
	// that Environment.Pool gives.
	Pools map[string]Pool
	// Sessions are the agent sessions of the conversations; nil keeps them
	// in memory.
	Sessions Sessions
	// AgentAPI serves the agent's own requests, on a socket each process
	// reaches; [ProcessKey] tells which process a request came from.
	AgentAPI http.Handler
	Hooks    Hooks
	// A2A configures the handler of [Agent.Handler].
	A2A A2A
	// StateDir keeps the files of the processes, which no process may
	// see, and the directories of [TurnSynced] workspaces, which only
	// their process sees; empty uses a temporary directory, removed at
	// Close.
	StateDir string
	// Log receives the records of the Agent; nil uses the default logger.
	Log *slog.Logger
	// Stderr receives the stderr of the agent processes.
	Stderr io.Writer

	options *agentrun.Options
}

// Environment is what one agent process gets.
type Environment struct {
	// Workspace gives the process its project directory: the Agent calls
	// Attach before the process starts and Release after it stops.
	Workspace Workspace
	// Home is the host directory of the process's home.
	Home string
	// Env holds NAME=value pairs of the process. With an Egress boundary,
	// the placeholders of the Credentials are added.
	Env         []string
	Credentials []egress.Credential
	// Mounts are directories the process sees besides its workspace, and
	// Hide host paths it doesn't see even inside a mount.
	Mounts []sandbox.Mount
	Hide   []string
	Limits sandbox.Limits
	// Image is the image of a container; Profile.Image() if empty.
	Image string
	// Name names the sandbox, such as its container.
	Name   string
	Labels map[string]string
	// Pool is the pool the process runs in; "" for Main.
	Pool string
	// MCPServers are MCP servers that every new session of the process
	// gets, besides those of the workspace's .mcp.json. A workspace server
	// with the same name is left out, so the agent can't replace one.
	MCPServers []MCPServer

	process *agentrun.ProcessOptions
}

// MCPServer is an MCP server that the agent reaches over streamable HTTP.
type MCPServer struct {
	// Name names the server in the agent's tool names and in the
	// "tool:<server>.<tool>" entries of [ToolRules]: 1 to 64 letters,
	// digits, _ or -.
	Name string
	// URL is the http or https URL where the agent reaches the server,
	// as seen from its sandbox.
	URL string
	// Headers are added to the agent's requests to the server.
	Headers map[string]string
}

// Callers runs different callers in different processes. Keys are opaque;
// the key "" is the process of Main, which gets Config.Main for its whole
// life: the Agent never calls Environment with "".
type Callers interface {
	// Key returns the key of a new conversation, such as its caller's.
	Key(ctx context.Context, contextID string) (string, error)
	// Environment returns what the process of key gets now. The Agent
	// calls it on every start, turn and session of the process, for every
	// key but "".
	Environment(ctx context.Context, key string) (Environment, error)
	// Admit refuses a turn of the caller in ctx in the process of key, or
	// holds what the turn needs until release.
	Admit(ctx context.Context, key string, task *a2asrv.ExecutorContext) (release func(), err error)
}

// Pool is a pool of processes that start on their first turn and stop
// when idle.
type Pool struct {
	// MaxProcesses bounds the processes that run at once; zero doesn't.
	MaxProcesses int
	// QuestionTimeout stops a turn whose question gets no answer.
	QuestionTimeout time.Duration
}

// Sessions keeps the agent session of each conversation, so that every
// task of a conversation continues the same agent session, in the process
// of the same key.
type Sessions interface {
	// Get returns the session of the conversation, or false if it has none.
	Get(ctx context.Context, contextID string) (Session, bool, error)
	// Put records the session of the conversation.
	Put(ctx context.Context, contextID string, s Session) error
}

// Session is the agent session of a conversation.
type Session struct {
	// ID is the agent's session ID.
	ID string
	// Key is the key of the process that has the session.
	Key string
}

// Hooks are the caller's decisions about turns. Each is optional.
type Hooks struct {
	// Turn runs while the agent works on a message.
	Turn func(ctx context.Context, task *a2asrv.ExecutorContext) (end func())
	// Explain returns the status message of a failed turn, or nil for the
	// error's text.
	Explain func(ctx context.Context, task *a2asrv.ExecutorContext, err error) []*a2a.Part
	// Prompt returns the parts sent to the agent for a message.
	Prompt func(ctx context.Context, task *a2asrv.ExecutorContext) ([]*a2a.Part, error)
	// Stalled returns the status message of a turn that the Agent stopped
	// since the agent sent nothing for d, while a tool call ran if tool, or
	// nil for the Agent's own.
	Stalled func(d time.Duration, tool bool) []*a2a.Part
	// ToolDone receives each tool call that the agent reports as completed
	// or failed during a turn. It runs on the goroutine that reads the
	// agent's output, so it must return quickly.
	ToolDone func(ctx context.Context, r ToolResult)
}

// ToolResult is a tool call that the agent reported as ended.
type ToolResult struct {
	// ID, Kind and Title are the agent's, as in [ToolCall].
	ID, Kind, Title string
	// Status is completed or failed.
	Status string
	// Output is the text the agent reported for the call, cut to 64 KiB,
	// and Truncated whether it was cut.
	Output    string
	Truncated bool
	// ContextID and TaskID identify the conversation and the task that
	// made the call.
	ContextID string
	TaskID    string
}

// A2A configures the A2A request handler of an [Agent].
type A2A struct {
	// TaskStore stores the tasks; nil keeps them in memory. The Agent also
	// stores there the end of a task whose agent stopped while the task
	// waited for an answer, which no execution can emit.
	TaskStore    taskstore.Store
	Materializer taskstore.UpdateMaterializer
	Interceptors []a2asrv.CallInterceptor
	// Middleware wraps the executor of the agent.
	Middleware func(a2asrv.AgentExecutor) a2asrv.AgentExecutor
	// MaxExecutions bounds the tasks that run at once, and
	// MaxScopeExecutions those of a limiter scope; zero doesn't.
	MaxExecutions      int
	MaxScopeExecutions func(scope string) int
	// Capabilities refuse the requests the agent card doesn't support.
	Capabilities *a2a.AgentCapabilities
}

// ProcessStatus is a process that runs.
type ProcessStatus struct {
	Key     string
	Started time.Time
}

// ProcessKey returns the key of the process that an AgentAPI request with
// ctx came from, or false when the request proves none.
func ProcessKey(ctx context.Context) (string, bool) { return agentrun.ProcessKey(ctx) }

// Agent serves a coding agent as an A2A agent.
type Agent struct {
	r   *agentrun.Runner
	a2a A2A
	log *slog.Logger

	once    sync.Once
	handler a2asrv.RequestHandler
	// executions counts the executions of the handler until a2a-go stored
	// their last event.
	executions turncount.Count
}

// New starts the process of Main and serves AgentAPI. The processes of
// other keys start on their first turn. Everything runs until Close, and
// the turns in progress end when ctx or Close does.
func New(ctx context.Context, cfg Config) (*Agent, error) {
	if cfg.Profile.profile == nil {
		return nil, errors.New("a2aserve: invalid agent profile: use one of Opencode, Claude, Command or LookupProfile")
	}
	if cfg.Runtime == nil {
		cfg.Runtime = sandbox.Docker(sandbox.DockerOptions{})
	}
	rc := agentrun.Config{
		Profile:  cfg.Profile.profile,
		Model:    cfg.Model,
		Runtime:  cfg.Runtime,
		Egress:   cfg.Egress,
		Tools:    decideTools(cfg.Tools),
		Main:     cfg.Main.engine(),
		Pools:    map[string]agentrun.Pool{},
		AgentAPI: cfg.AgentAPI,

		StateDir: cfg.StateDir,
		Log:      cfg.Log,
		Stderr:   cfg.Stderr,
	}
	if cfg.Callers != nil {
		rc.Callers = callers{cfg.Callers}
	}
	for name, p := range cfg.Pools {
		rc.Pools[name] = agentrun.Pool(p)
	}
	if cfg.Sessions != nil {
		rc.Sessions = sessions{cfg.Sessions}
	}
	if cfg.options != nil {
		rc.Options = *cfg.options
	}
	if cfg.A2A.TaskStore == nil {
		cfg.A2A.TaskStore = taskstore.NewInMemory(&taskstore.InMemoryStoreConfig{Authenticator: a2asrv.NewTaskStoreAuthenticator()})
	}
	rc.Hooks = cfg.Hooks.engine(rc.Options.Hooks, cfg.A2A)
	r, err := agentrun.New(rc)
	if err != nil {
		return nil, err
	}
	if err := r.Start(ctx); err != nil {
		return nil, errors.Join(err, r.Close(context.WithoutCancel(ctx)))
	}
	r.Serve(ctx)
	return &Agent{r: r, a2a: cfg.A2A, log: cfg.Log}, nil
}

// Handler returns the A2A request handler of the agent, which Config.A2A
// configures. It builds it on the first call.
func (a *Agent) Handler() a2asrv.RequestHandler {
	a.once.Do(func() {
		var exec a2asrv.AgentExecutor = a.r
		if a.a2a.Middleware != nil {
			exec = a.a2a.Middleware(exec)
		}
		exec = &countedExecutor{AgentExecutor: exec, executions: &a.executions}
		var opts []a2asrv.RequestHandlerOption
		if a.a2a.TaskStore != nil {
			opts = append(opts, a2asrv.WithTaskStore(a.a2a.TaskStore))
		}
		if a.a2a.Materializer != nil {
			opts = append(opts, a2asrv.WithUpdateMaterializer(a.a2a.Materializer))
		}
		if len(a.a2a.Interceptors) > 0 {
			opts = append(opts, a2asrv.WithCallInterceptors(a.a2a.Interceptors...))
		}
		if a.a2a.Capabilities != nil {
			opts = append(opts, a2asrv.WithCapabilityChecks(a.a2a.Capabilities))
		}
		if a.a2a.MaxExecutions > 0 || a.a2a.MaxScopeExecutions != nil {
			opts = append(opts, a2asrv.WithConcurrencyConfig(limiter.ConcurrencyConfig{MaxExecutions: a.a2a.MaxExecutions, GetMaxExecutions: a.a2a.MaxScopeExecutions}))
		}
		log := a.log
		if log == nil {
			log = slog.Default()
		}
		opts = append(opts, a2asrv.WithLogger(log))
		a.handler = a2asrv.NewHandler(exec, opts...)
	})
	return a.handler
}

// Stop stops the processes of keys now, and of the keys whose processes
// they started; their tasks fail. They don't start again until release.
func (a *Agent) Stop(ctx context.Context, keys ...string) (release func(), err error) {
	return a.r.Stop(ctx, keys...)
}

// StopIdle stops the process of key if no turn uses it, and reports
// whether it stopped one. The process of Main doesn't stop.
func (a *Agent) StopIdle(key string) (bool, error) { return a.r.StopIdle(key) }

// Processes returns the processes that run.
func (a *Agent) Processes() []ProcessStatus {
	ps := a.r.Processes()
	out := make([]ProcessStatus, len(ps))
	for i, p := range ps {
		out[i] = ProcessStatus(p)
	}
	return out
}

// Close stops serving AgentAPI and ends the turns in progress: their tasks
// fail, as when the context of New ends. It waits within ctx for the
// handler to store their last events, then stops every process. A task
// that waits for an answer fails when its process stops.
func (a *Agent) Close(ctx context.Context) error {
	err := a.r.StopServe(ctx)
	a.r.StopTurns(ctx)
	a.executions.Wait(ctx)
	return errors.Join(err, a.r.Close(ctx))
}

// countedExecutor counts the executions of the handler until a2a-go is
// done with their last event, which it stores before the cleanup.
type countedExecutor struct {
	a2asrv.AgentExecutor
	executions *turncount.Count
}

func (e *countedExecutor) Execute(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	e.executions.Hold(execCtx)
	return e.AgentExecutor.Execute(ctx, execCtx)
}

func (e *countedExecutor) Cancel(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	e.executions.Hold(execCtx)
	return e.AgentExecutor.Cancel(ctx, execCtx)
}

// Cleanup implements [a2asrv.AgentExecutionCleaner], after the cleanup of
// the executor it wraps.
func (e *countedExecutor) Cleanup(ctx context.Context, execCtx *a2asrv.ExecutorContext, result a2a.SendMessageResult, err error) {
	defer e.executions.Release(execCtx)
	if c, ok := e.AgentExecutor.(a2asrv.AgentExecutionCleaner); ok {
		c.Cleanup(ctx, execCtx, result, err)
	}
}

// engine returns e for the engine.
func (e Environment) engine() agentrun.Environment {
	return agentrun.Environment{
		Workspace:   attach(e.Workspace),
		Home:        e.Home,
		Env:         e.Env,
		Credentials: e.Credentials,
		Mounts:      e.Mounts,
		Hide:        e.Hide,
		Limits:      e.Limits,
		Image:       e.Image,
		Name:        e.Name,
		Labels:      e.Labels,
		Pool:        e.Pool,
		MCPServers:  mcpServers(e.MCPServers),
		Process:     e.process,
	}
}

// mcpServers returns servers for the engine.
func mcpServers(servers []MCPServer) []projectfiles.MCPServer {
	out := make([]projectfiles.MCPServer, len(servers))
	for i, s := range servers {
		out[i] = projectfiles.MCPServer{Name: s.Name, Type: projectfiles.MCPHTTP, URL: s.URL, Headers: maps.Clone(s.Headers)}
	}
	return out
}

// attach returns the engine's workspace of w, or nil without one.
func attach(w Workspace) agentrun.Workspace {
	if w == nil {
		return nil
	}
	return func(ctx context.Context, key, stateDir string) (agentrun.Attachment, error) {
		a, err := attachIn(ctx, w, key, stateDir)
		if err != nil {
			return agentrun.Attachment{}, err
		}
		out := agentrun.Attachment{Dir: a.Dir, Path: a.Path, ReadOnly: a.ReadOnly, Release: a.Release, BeforeTurn: a.BeforeTurn}
		if sw, ok := w.(servedWorkspace); ok {
			out.Served = sw.served()
		}
		if after := a.AfterTurn; after != nil {
			out.AfterTurn = func(ctx context.Context) error { return turnError(after(ctx)) }
		}
		return out, nil
	}
}

// turnError gives the engine the status message metadata of err.
func turnError(err error) error {
	conflict, ok := errors.AsType[*SyncConflictError](err)
	if !ok {
		return err
	}
	paths := make([]any, len(conflict.Paths))
	for i, p := range conflict.Paths {
		paths[i] = p
	}
	return &agentrun.TurnError{Err: err, Metadata: map[string]any{ConflictsKey: paths}}
}

// engine returns h for the engine, added to the hooks of the frontend of
// this module. Without its own, a task whose agent stopped while it waited
// for an answer ends in the task store of cfg.
func (h Hooks) engine(frontend agentrun.Hooks, cfg A2A) agentrun.Hooks {
	out := frontend
	out.Turn, out.Explain, out.Prompt, out.Stalled = h.Turn, h.Explain, h.Prompt, h.Stalled
	if done := h.ToolDone; done != nil {
		out.ToolDone = func(ctx context.Context, execCtx *a2asrv.ExecutorContext, t agentrun.ToolResult) {
			done(ctx, ToolResult{ID: t.ID, Kind: t.Kind, Title: t.Title, Status: t.Status, Output: t.Output, Truncated: t.Truncated, ContextID: execCtx.ContextID, TaskID: string(execCtx.TaskID)})
		}
	}
	if out.Abandoned == nil {
		materializer := cfg.Materializer
		if materializer == nil {
			materializer = taskstore.NewFullUpdateMaterializer(a2aevent.ApplyShallowUpdate)
		}
		out.Abandoned = abandon(cfg.TaskStore, materializer)
	}
	return out
}

// maxAbandonAttempts bounds the updates of an abandoned task that lose to
// concurrent writes, such as the handler still storing its question.
const maxAbandonAttempts = 10

// abandon stores the event that ends a task whose agent stopped while it
// waited for an answer, unless the task ended first, such as by a reply.
func abandon(store taskstore.Store, materializer taskstore.UpdateMaterializer) func(context.Context, *a2a.TaskStatusUpdateEvent) error {
	return func(ctx context.Context, ev *a2a.TaskStatusUpdateEvent) error {
		var err error
		for range maxAbandonAttempts {
			stored, gerr := store.Get(ctx, ev.TaskID)
			if gerr != nil {
				return gerr
			}
			if stored.Task.Status.State.Terminal() {
				return nil
			}
			task, aerr := materializer.ApplyUpdate(ctx, stored.Task, ev)
			if aerr != nil {
				return aerr
			}
			_, err = store.Update(ctx, &taskstore.UpdateRequest{Task: task, Event: ev, PrevTask: stored.Task, PrevVersion: stored.Version})
			if !errors.Is(err, taskstore.ErrConcurrentModification) {
				return err
			}
		}
		return err
	}
}

// callers adapts Callers to the engine.
type callers struct{ c Callers }

func (c callers) Key(ctx context.Context, contextID string) (string, error) {
	return c.c.Key(ctx, contextID)
}

func (c callers) Environment(ctx context.Context, key string) (agentrun.Environment, error) {
	env, err := c.c.Environment(ctx, key)
	if err != nil {
		return agentrun.Environment{}, err
	}
	return env.engine(), nil
}

func (c callers) Admit(ctx context.Context, key string, task *a2asrv.ExecutorContext) (func(), error) {
	return c.c.Admit(ctx, key, task)
}

// sessions adapts Sessions to the engine.
type sessions struct{ s Sessions }

func (s sessions) Get(ctx context.Context, contextID string) (a2aacp.Session, bool, error) {
	sess, ok, err := s.s.Get(ctx, contextID)
	return a2aacp.Session{ID: sess.ID, AgentKey: sess.Key}, ok, err
}

func (s sessions) Put(ctx context.Context, contextID string, sess a2aacp.Session) error {
	return s.s.Put(ctx, contextID, Session{ID: sess.ID, Key: sess.AgentKey})
}
