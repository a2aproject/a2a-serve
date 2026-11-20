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

// Package agentrun is the engine of core/a2aserve: it runs an ACP agent as
// an A2A executor, with one agent process per key, from the objects
// that core/a2aserve receives. Keys are opaque to it: the caller decides
// which key a conversation runs under, what the process of each key gets,
// and what happens inside a turn, through [Callers] and [Hooks].
//
// The frontend of this module reaches the engine of a core/a2aserve agent
// through [Of], for what only it needs, and gives the agent what only it
// sets through [SetOptions] and [SetProcess].
package agentrun

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/integrations/executor/a2aacp"

	"github.com/a2aproject/a2a-serve/core/egress"
	"github.com/a2aproject/a2a-serve/core/fsys"
	"github.com/a2aproject/a2a-serve/core/sandbox"
	"github.com/a2aproject/a2a-serve/internal/agentprofile"
	"github.com/a2aproject/a2a-serve/internal/cryptoutil"
	"github.com/a2aproject/a2a-serve/internal/projectfiles"
	"github.com/a2aproject/a2a-serve/internal/sandboxrt"
	"github.com/a2aproject/a2a-serve/internal/turncount"
)

// Config configures a [Runner].
type Config struct {
	Profile agentprofile.Profile
	Model   string
	Runtime sandbox.Runtime
	// Egress is the boundary each process gets an endpoint of; nil gives
	// the processes the runtime's own network.
	Egress *egress.Boundary
	// Tools decides tool calls; nil asks about every call.
	Tools DecideTool
	// Main is what the process of key "" gets.
	Main Environment
	// Callers runs different callers in different processes; nil runs
	// every conversation in Main.
	Callers Callers
	// Pools are the pools of processes besides the main one, by name.
	Pools map[string]Pool
	// Sessions are the ACP sessions of the conversations; nil keeps them
	// in memory.
	Sessions a2aacp.SessionStore
	// AgentAPI serves the requests of the agent processes; [ProcessKey]
	// tells which process a request came from.
	AgentAPI http.Handler
	Hooks    Hooks
	// StateDir keeps the files of the processes; empty uses a temporary
	// directory, removed at Close.
	StateDir string
	// Log receives the records of the engine; nil uses the default logger.
	Log *slog.Logger
	// Stderr receives the stderr of the agent processes.
	Stderr io.Writer
	// Options are what the frontend of this module sets.
	Options Options
}

// Workspace returns the directory of the process of key, which it keeps
// until the Release of the attachment. stateDir is the state directory of
// the runner.
type Workspace func(ctx context.Context, key, stateDir string) (Attachment, error)

// Attachment is the directory of a process: Dir on the host, seen at Path,
// or at Dir if Path is empty. BeforeTurn, if set, runs before the agent
// gets the prompt of a turn, and its error fails the turn. AfterTurn, if
// set, runs once the agent stopped working on a message, on every exit
// path, and its error fails the turn; a [*TurnError] adds its metadata to
// the final status message. With either set, the turns of the process run
// one at a time, from BeforeTurn to AfterTurn. Served, if set, is the tree
// that the process sees at Path, live, over the empty directory Dir.
type Attachment struct {
	Dir, Path  string
	ReadOnly   bool
	Served     fsys.FS
	Release    func() error
	BeforeTurn func(ctx context.Context) error
	AfterTurn  func(ctx context.Context) error
}

// TurnError is an error of AfterTurn with the metadata of the final status
// message of the turn.
type TurnError struct {
	Err      error
	Metadata map[string]any
}

func (e *TurnError) Error() string { return e.Err.Error() }

func (e *TurnError) Unwrap() error { return e.Err }

// Environment is what one agent process gets.
type Environment struct {
	Workspace   Workspace
	Home        string
	Env         []string
	Credentials []egress.Credential
	Mounts      []sandbox.Mount
	Hide        []string
	Limits      sandbox.Limits
	Image       string
	Name        string
	Labels      map[string]string
	Pool        string
	// MCPServers are MCP servers of the caller that every new session of
	// the process gets, besides those of the workspace's .mcp.json. A
	// workspace server with the same name is left out.
	MCPServers []projectfiles.MCPServer
	// Process is what the frontend of this module sets; nil for others.
	Process *ProcessOptions
}

func (e Environment) process() ProcessOptions {
	if e.Process == nil {
		return ProcessOptions{}
	}
	return *e.Process
}

// ProcessOptions are what only the frontend of this module sets for a
// process.
type ProcessOptions struct {
	// Parent is the key whose Stop also stops this process.
	Parent string
	// LockHome makes the agent's own configuration in Home read-only.
	LockHome bool
	// NoAgentAPI keeps the process from reaching AgentAPI.
	NoAgentAPI bool
	// Storage names how the process sees the workspace, in the log.
	Storage string
	// State is kept with the process that starts with this Environment,
	// for [Runner.StartedWith].
	State any
	// StartError refuses to start the process, and SessionError to start
	// a session of a conversation in it.
	StartError, SessionError error
	// MCPDir is the directory whose .mcp.json sessions get, and NoMCP
	// gives them none. Empty takes the workspace of the running process.
	MCPDir string
	NoMCP  bool
	// MCPVars resolve the variables of MCP servers; nil resolves Env.
	MCPVars func(name string) (string, bool)
}

// Options are what only the frontend of this module sets for the engine.
type Options struct {
	// SocketDir holds the sockets of the processes, and of the egress
	// boundaries of containers; empty uses StateDir/harness.
	SocketDir string
	// Pins are the directories containers are checked against.
	Pins *agentprofile.Pins
	// Mounts are the directories of the agent's settings, the workspace
	// first, which a home is checked against before the process starts.
	Mounts []agentprofile.Mount
	// Egress returns the egress settings, which decide whether a container
	// may start without the boundary.
	Egress func() agentprofile.Egress
	// AskCommands tells whether the policy asks before every shell
	// command, and DisabledMCP the MCP servers sessions don't get.
	AskCommands func() bool
	DisabledMCP func() []string
	// CLI is the a2a binary of an agent without a sandbox, or "".
	CLI string
	// PruneLabels mark the containers of this agent, which New removes
	// when an earlier run left them.
	PruneLabels map[string]string
	// TurnStall fails a turn in which the agent sent nothing this long
	// while none of its tool calls ran, and ToolStall one in which one ran.
	// Zero takes 10 minutes and an hour.
	TurnStall, ToolStall time.Duration
	// MaxAgentMessage bounds a message from the agent; zero takes 64 MiB.
	MaxAgentMessage int
	// Out receives the progress of the main process.
	Out io.Writer
	// MainPrepared reports the sandbox of the main process before it
	// starts.
	MainPrepared func(Prepared)
	// AllSessions are the sessions of every workspace, which tell the key
	// of each conversation; nil takes Sessions.
	AllSessions a2aacp.SessionStore
	// Executor replaces the main process, and runs every conversation,
	// for tests.
	Executor a2asrv.AgentExecutor
	// KeepAbandonedOnStop leaves the tasks whose agent stops at the stop
	// while they wait for an answer as they are, for the frontend to fail
	// at its next start.
	KeepAbandonedOnStop bool
	// Hooks are the hooks only the frontend sets; core/a2aserve adds the
	// public ones to them.
	Hooks Hooks
	// PoolLabels name the processes of each pool in errors.
	PoolLabels map[string]string
}

// Callers tells the runner about keys, which it treats as opaque.
type Callers interface {
	// Key returns the key of a new conversation, such as its caller's.
	Key(ctx context.Context, contextID string) (string, error)
	// Environment returns what the process of key gets now. The runner
	// never asks for "", whose process gets Main.
	Environment(ctx context.Context, key string) (Environment, error)
	// Admit refuses a turn of the caller in ctx in the process of key, or
	// holds what the turn needs until release.
	Admit(ctx context.Context, key string, execCtx *a2asrv.ExecutorContext) (release func(), err error)
}

// Pool is a pool of agent processes that start on their first turn and
// stop when idle.
type Pool struct {
	// MaxProcesses bounds the processes that run at once; zero doesn't.
	MaxProcesses int
	// QuestionTimeout stops a turn whose question gets no answer.
	QuestionTimeout time.Duration
}

// Hooks are the caller's decisions inside a turn. Each is optional.
type Hooks struct {
	// Hold holds what an admitted turn needs while it runs, until release.
	// Its refusal fails the turn, and leaves a task that waits for an
	// answer waiting.
	Hold func(ctx context.Context, key string, execCtx *a2asrv.ExecutorContext) (release func(), err error)
	// CanStart refuses to start the process of key.
	CanStart func(ctx context.Context, key string) error
	// PrepareHome writes into the home of env before the process of key
	// starts with it.
	PrepareHome func(ctx context.Context, key string, env Environment) error
	// Turn runs while the agent works on a message.
	Turn func(ctx context.Context, execCtx *a2asrv.ExecutorContext) (end func())
	// Prompt returns the parts sent to the agent.
	Prompt func(ctx context.Context, execCtx *a2asrv.ExecutorContext) ([]*a2a.Part, error)
	// Denied notes a tool call that Tools denied.
	Denied func(execCtx *a2asrv.ExecutorContext, title string)
	// ToolDone receives each tool call of a turn that the agent reports
	// as completed or failed. It runs on the goroutine that reads the
	// agent's output, so it must return quickly.
	ToolDone func(ctx context.Context, execCtx *a2asrv.ExecutorContext, t ToolResult)
	// Refused words the error that refuses a turn, or returns "" for the
	// error's text.
	Refused func(err error) string
	// LeftOutMCP reports an MCP server left out of a session, since the
	// process doesn't get the variables missing.
	LeftOutMCP func(ctx context.Context, server string, missing []string)
	// Stalled returns the status message of a turn that the runner stopped
	// since the agent sent nothing for d, while a tool call ran if tool, or
	// nil for the runner's own.
	Stalled func(d time.Duration, tool bool) []*a2a.Part
	// Explain returns the status message of a failed turn, or nil for the
	// error's text.
	Explain func(ctx context.Context, execCtx *a2asrv.ExecutorContext, err error) []*a2a.Part
	// Abandoned stores the event that ends a task whose agent stopped
	// while it waited for an answer.
	Abandoned func(ctx context.Context, ev *a2a.TaskStatusUpdateEvent) error
	// AgentAPIError gets the errors of the listeners of AgentAPI.
	AgentAPIError func(err error)
}

// Prepared describes the sandbox of the main process.
type Prepared struct {
	// Summary describes the sandbox, and Warnings the problems that don't
	// stop the agent.
	Summary  string
	Warnings []error
	// Egress is set when the container's connections go through the
	// boundary, and NoBoundary is why they don't although the settings ask
	// for it.
	Egress     bool
	NoBoundary string
}

// EgressState is what the runner knows of the egress of the containers.
type EgressState struct {
	// Started is set once a container was prepared. Mode is the egress
	// mode the containers started with, and Problem why they have Docker's
	// own network although the mode asks for the boundary.
	Started bool
	Mode    string
	Problem string
}

// Bridge tells how an agents URL reaches the AgentAPI handler: through the
// Unix socket of the main process, or over TCP at URL.
type Bridge struct {
	Socket string
	URL    string
}

// ProcessStatus is a process that runs.
type ProcessStatus struct {
	Key     string
	Started time.Time
}

// ErrStopping fails turns that start, or would start agent processes,
// while the runner stops.
var ErrStopping = errors.New("the server is stopping")

// ErrStartTimeout is wrapped by the error of a main process that didn't
// start in time.
var ErrStartTimeout = errors.New("the agent didn't start in time")

// Runner runs an ACP agent as an A2A executor: the process of key "" starts
// with Start, and the processes of other keys start on their first turn.
type Runner struct {
	cfg Config

	rt         sandboxRuntime
	stateDir   string
	tempState  bool
	keys       *keys
	boundaries *sandboxrt.Boundaries
	egress     *egressState
	listeners  *listeners
	main       *endpoint
	policies   *policies
	stalls     *stallGuard
	toolsDone  *toolWatch
	decider    *toolDecider
	// callerMCP are the names of the MCP servers of Environments that
	// sessions got, which name the MCP tools of calls.
	callerMCP *nameSet
	split     *splitExecutor
	pools     map[string]*procs
	// workspaces are where the running processes see their workspace, and
	// turns the turn hooks of their attachments.
	workspaces sync.Map
	turns      *workspaceTurns
	// mainState is the state of the Environment the main process started
	// with, and mainSince when it started.
	mainState atomic.Pointer[any]
	mainSince atomic.Pointer[time.Time]
	closeMain func(context.Context) error
	stopServe func(context.Context) error
	// agentTurns counts the turns the agents work on, from the Turn hook
	// of a2aacp to its end.
	agentTurns turncount.Count
}

var _ a2asrv.AgentExecutor = (*Runner)(nil)

// New returns a runner that serves the agent of cfg. It opens the endpoints
// agent processes reach the caller through, and starts no process.
func New(cfg Config) (*Runner, error) {
	if cfg.Options.MaxAgentMessage == 0 {
		cfg.Options.MaxAgentMessage = maxAgentMessage
	}
	if cfg.Options.Out == nil {
		cfg.Options.Out = io.Discard
	}
	cfg.Options.TurnStall = cmp.Or(cfg.Options.TurnStall, 10*time.Minute)
	cfg.Options.ToolStall = cmp.Or(cfg.Options.ToolStall, time.Hour)
	if cfg.Profile == nil {
		return nil, errors.New("no agent profile")
	}
	rt, err := runtimeOf(cfg.Runtime)
	if err != nil {
		return nil, err
	}
	r := &Runner{cfg: cfg, rt: rt, stateDir: cfg.StateDir, pools: map[string]*procs{}}
	if r.stateDir == "" {
		if r.stateDir, err = os.MkdirTemp("", "a2aserve-"); err != nil {
			return nil, err
		}
		r.tempState = true
	}
	if r.cfg.Sessions == nil {
		r.cfg.Sessions = &memorySessions{m: map[string]a2aacp.Session{}}
	}
	all := cfg.Options.AllSessions
	if all == nil {
		all = r.cfg.Sessions
	}
	r.keys = newKeys(all, r.newKey)
	r.policies = &policies{dir: filepath.Join(r.stateDir, "policy")}
	r.stalls = newStallGuard(cfg.Options.TurnStall, cfg.Options.ToolStall, cfg.Hooks.Stalled)
	r.turns = newWorkspaceTurns(r.keys.of, cfg.Hooks.Explain, r.log)
	for name, p := range cfg.Pools {
		r.pools[name] = &procs{max: p.MaxProcesses}
	}
	r.egress = newEgressState()
	r.boundaries = sandboxrt.NewBoundaries(r.egressDir(), sandboxrt.SocketIDLen, idAlphabet, nil)
	r.rt.init(r)
	r.toolsDone = newToolWatch(cfg.Hooks.ToolDone)
	r.callerMCP = &nameSet{}
	r.decider = &toolDecider{tools: cfg.Tools, servers: r.toolServerNames, denied: cfg.Hooks.Denied, log: r.log}
	l, err := r.listen(cfg.Options.Executor == nil)
	if err != nil {
		return nil, errors.Join(err, r.boundaries.Close(context.Background()), r.removeState())
	}
	r.listeners = l
	if r.main, err = l.open(""); err != nil {
		return nil, errors.Join(err, l.close(), r.boundaries.Close(context.Background()), r.removeState())
	}
	return r, nil
}

// newKey returns the key of a new conversation.
func (r *Runner) newKey(ctx context.Context, contextID string) (string, error) {
	if r.cfg.Callers == nil {
		return "", nil
	}
	return r.cfg.Callers.Key(ctx, contextID)
}

// socketDir returns the directory of the sockets of the processes.
func (r *Runner) socketDir() string {
	if r.cfg.Options.SocketDir != "" {
		return r.cfg.Options.SocketDir
	}
	return filepath.Join(r.stateDir, "harness")
}

func (r *Runner) log() *slog.Logger {
	if r.cfg.Log != nil {
		return r.cfg.Log
	}
	return slog.Default()
}

// Start prunes what an earlier run left (containers, env files), prepares
// the main process and starts it. The processes run until Close.
func (r *Runner) Start(ctx context.Context) error {
	main, start := r.cfg.Options.Executor, r.startPool
	if main == nil {
		exec, closeMain, err := r.startMain(ctx)
		if err != nil {
			return err
		}
		main, r.closeMain = exec, closeMain
	} else {
		start = nil
	}
	r.split = newSplitExecutor(ctx, r, main, start)
	return nil
}

// Execute implements [a2asrv.AgentExecutor].
func (r *Runner) Execute(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return r.split.Execute(ctx, execCtx)
}

// Cancel implements [a2asrv.AgentExecutor].
func (r *Runner) Cancel(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return r.split.Cancel(ctx, execCtx)
}

// KeyOf returns the key of a conversation: the one stored with its session,
// or, for a new conversation, the one Callers.Key gives.
func (r *Runner) KeyOf(ctx context.Context, contextID string) (string, error) {
	return r.keys.of(ctx, contextID)
}

// Stop stops the processes of keys and of the keys under them, and holds
// their starts until release, which also forgets them.
func (r *Runner) Stop(ctx context.Context, keys ...string) (release func(), err error) {
	return r.stop(ctx, keys)
}

// StopIdle stops the process of key if it runs and no turn uses it, and
// reports whether it did.
func (r *Runner) StopIdle(key string) (bool, error) {
	return r.CloseIdleIf(key, func(any) bool { return true })
}

// CloseIdleIf stops the process of key if it runs, no turn uses it, it
// doesn't start or stop now, and stale reports the state it started with
// stale. It holds the process from starting until it stopped, and reports
// whether it stopped one. The main process doesn't stop.
func (r *Runner) CloseIdleIf(key string, stale func(state any) bool) (bool, error) {
	p := r.knownPool(key)
	if p == nil {
		return false, nil
	}
	return p.closeStale(key, stale)
}

// CloseIf stops the process of key if it runs and cond reports true for
// the state it started with, whatever its turns, holding it from starting
// meanwhile. It reports whether it stopped one.
func (r *Runner) CloseIf(ctx context.Context, key string, cond func(state any) bool) (bool, error) {
	p, err := r.poolOf(ctx, key)
	if err != nil || p == nil {
		return false, err
	}
	return p.closeIf(key, cond)
}

// HoldIdle holds the start of the process of key until release, unless it
// runs or starts: then ok is false.
func (r *Runner) HoldIdle(ctx context.Context, key string) (release func(), ok bool, err error) {
	p, err := r.poolOf(ctx, key)
	if err != nil || p == nil {
		return nil, false, err
	}
	release = p.holdIdle(key)
	return release, release != nil, nil
}

// Forget drops what the runner keeps of the process of key, if it doesn't
// run or start and no turn uses it.
func (r *Runner) Forget(key string) {
	if p := r.knownPool(key); p != nil {
		p.forget(key)
	}
}

// HasRoom reports whether a turn in the process of key can run now: the
// process runs or starts, or it can start, possibly after an idle process
// stops.
func (r *Runner) HasRoom(ctx context.Context, key string) (bool, error) {
	p, err := r.poolOf(ctx, key)
	if err != nil {
		return false, err
	}
	return p == nil || p.hasRoom(key), nil
}

// admission is what the runner knows of the admission of one turn.
type admission struct{ counted bool }

type admissionKey struct{}

// AdmitTurn counts the turn that Callers.Admit admits in ctx in the
// process of key, whose stop parent also stops, until release, so that
// the runner doesn't count it again. A process that stale reports stale
// stops first if no turn uses it. If one does, only an answer to a waiting
// task may use it; other turns get [ErrStale]. The main process isn't
// counted.
func (r *Runner) AdmitTurn(ctx context.Context, key, parent string, stale func(state any) bool, answer bool) (release func(), err error) {
	if a, ok := ctx.Value(admissionKey{}).(*admission); ok {
		a.counted = true
	}
	return r.admitTurn(ctx, key, func(p *procs) { p.setParent(p.get(key), parent) }, stale, answer)
}

// admitTurn counts a turn in the process of key, after prepare readies its
// entry, as AdmitTurn does.
func (r *Runner) admitTurn(ctx context.Context, key string, prepare func(*procs), stale func(state any) bool, answer bool) (release func(), err error) {
	p, err := r.poolOf(ctx, key)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return func() {}, nil
	}
	if prepare != nil {
		prepare(p)
	}
	return p.admit(key, stale, answer)
}

// WaitIdle waits up to d for the turns in the process of key to end, and
// reports whether they did.
func (r *Runner) WaitIdle(ctx context.Context, key string, d time.Duration) bool {
	p := r.knownPool(key)
	return p == nil || p.waitIdle(ctx, key, d)
}

// StartedWith returns the state of the Environment the process of key
// started with, if it runs. The main process keeps its state until Close.
func (r *Runner) StartedWith(key string) (any, bool) {
	if key == "" {
		if s := r.mainState.Load(); s != nil {
			return *s, true
		}
		return nil, false
	}
	p := r.knownPool(key)
	if p == nil {
		return nil, false
	}
	return p.stateOf(key)
}

// Processes returns the processes that run.
func (r *Runner) Processes() []ProcessStatus {
	var out []ProcessStatus
	if since := r.mainSince.Load(); since != nil {
		out = append(out, ProcessStatus{Key: "", Started: *since})
	}
	for _, p := range r.pools {
		for key, since := range p.running() {
			out = append(out, ProcessStatus{Key: key, Started: since})
		}
	}
	return out
}

// Egress reports the egress of the containers.
func (r *Runner) Egress() EgressState { return r.egress.state() }

// Bridge tells how an agents URL reaches the AgentAPI handler.
func (r *Runner) Bridge() Bridge { return Bridge{Socket: r.main.socket, URL: r.listeners.url} }

// WritePolicies writes the policies of the agent processes again, such as
// after the tool rules changed. Agents that reload their policy apply them
// in open sessions too.
func (r *Runner) WritePolicies() error {
	if !r.rt.isolated() {
		return nil
	}
	for _, lockHome := range []bool{false, true} {
		if _, err := r.policies.write(r.cfg.Profile, r.askCommands(), lockHome, nil); err != nil {
			return err
		}
	}
	return nil
}

// Serve serves AgentAPI on the endpoints of the agent processes until
// Close.
func (r *Runner) Serve(ctx context.Context) {
	if r.cfg.AgentAPI == nil {
		return
	}
	fatal := r.cfg.Hooks.AgentAPIError
	if fatal == nil {
		fatal = func(err error) { r.log().Error("serving the agent processes", "error", err) }
	}
	r.stopServe = r.listeners.serve(ctx, r.cfg.AgentAPI, fatal)
}

// StopServe stops serving AgentAPI, until Close.
func (r *Runner) StopServe(ctx context.Context) error {
	if r.stopServe == nil {
		return nil
	}
	err := r.stopServe(ctx)
	r.stopServe = nil
	return err
}

// Shutdown waits for the turns in progress, which end when the context of
// Start does, and stops the processes of the pools. Turns that start later
// fail.
func (r *Runner) Shutdown(ctx context.Context) error {
	if r.split == nil {
		return nil
	}
	return r.split.close(ctx, context.Background())
}

// StopTurns ends the turns in progress, as the end of the context of Start
// does, and waits within ctx for them to return.
func (r *Runner) StopTurns(ctx context.Context) {
	if r.split == nil {
		return
	}
	r.split.stopTurns()
	r.split.running.Wait(ctx)
}

// Close stops serving AgentAPI and every process: those of the pools, as
// Shutdown does but waiting for the turns only within ctx, then the main
// one. Once the turns that the agents worked on ended within ctx, which
// stores the end of the tasks that waited for an answer, it closes the
// endpoints.
func (r *Runner) Close(ctx context.Context) error {
	err := r.StopServe(ctx)
	if r.split != nil {
		err = errors.Join(err, r.split.close(ctx, ctx))
	}
	if r.closeMain != nil {
		err = errors.Join(err, r.closeMain(ctx))
		r.closeMain = nil
		r.mainSince.Store(nil)
	}
	r.agentTurns.Wait(ctx)
	return errors.Join(err, r.main.close(), r.listeners.close(), r.boundaries.Close(ctx), r.removeState())
}

// removeState removes a temporary state directory.
func (r *Runner) removeState() error {
	if !r.tempState {
		return nil
	}
	return os.RemoveAll(r.stateDir)
}

// environment returns what the process of key gets now: Main for "", and
// what Callers gives for other keys.
func (r *Runner) environment(ctx context.Context, key string) (Environment, error) {
	if key == "" {
		return r.cfg.Main, nil
	}
	if r.cfg.Callers != nil {
		return r.cfg.Callers.Environment(ctx, key)
	}
	return Environment{}, fmt.Errorf("unknown key %q", key)
}

// askCommands tells whether the policies ask before every shell command.
func (r *Runner) askCommands() bool {
	return r.cfg.Options.AskCommands != nil && r.cfg.Options.AskCommands()
}

// knownPool returns the processes of the pool that knows key, or nil.
func (r *Runner) knownPool(key string) *procs {
	if key == "" {
		return nil
	}
	for _, p := range r.pools {
		if p.knows(key) {
			return p
		}
	}
	return nil
}

// poolOf returns the processes of the pool that key runs in, or nil for the
// main process. It asks Callers only for keys no pool knows yet.
func (r *Runner) poolOf(ctx context.Context, key string) (*procs, error) {
	if key == "" {
		return nil, nil
	}
	if p := r.knownPool(key); p != nil {
		return p, nil
	}
	env, err := r.environment(ctx, key)
	if err != nil {
		return nil, err
	}
	return r.pools[env.Pool], nil
}

// memorySessions keeps the sessions of the conversations in memory.
type memorySessions struct {
	mu sync.Mutex
	m  map[string]a2aacp.Session
}

func (s *memorySessions) Get(_ context.Context, contextID string) (a2aacp.Session, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.m[contextID]
	return sess, ok, nil
}

func (s *memorySessions) Put(_ context.Context, contextID string, sess a2aacp.Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[contextID] = sess
	return nil
}

// idAlphabet names the directories of sockets.
const idAlphabet = cryptoutil.Unambiguous
