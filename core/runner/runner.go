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

// Package runner runs an ACP agent as an A2A executor, with one agent
// process per key. It knows agents, sandboxes, egress, tool rules, sessions
// and processes. Keys are opaque to it: the caller decides which key a
// conversation runs under, what the process of each key gets, and what
// happens inside a turn, through [Processes] and [Hooks].
package runner

import (
	"context"
	"errors"
	"io"
	"iter"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/integrations/executor/a2aacp"

	"github.com/a2aproject/a2a-serve/core/sandbox"
	"github.com/a2aproject/a2a-serve/core/sandbox/boundary"
	"github.com/a2aproject/a2a-serve/internal/agentprofile"
	"github.com/a2aproject/a2a-serve/internal/cryptoutil"
	"github.com/a2aproject/a2a-serve/internal/toolrules"
	"github.com/a2aproject/a2a-serve/internal/workspaces"
)

// Config configures a [Runner].
type Config struct {
	Dirs Dirs
	// Spec is the live configuration of the agent, read on every start,
	// turn and check. On error it still returns the settings it could read,
	// which the tool rules and egress use; starts and sessions fail.
	Spec func() (Spec, error)
	// Secrets returns the stored secret values.
	Secrets   func() map[string]string
	Processes Processes
	// Pools are the pools of processes besides the main one.
	Pools []Pool
	// Sessions are the ACP sessions of the conversations in the workspace,
	// and AllSessions those of every workspace, which tell the key of each
	// conversation.
	Sessions    a2aacp.SessionStore
	AllSessions a2aacp.SessionStore
	Tasks       Tasks
	Hooks       Hooks
	// PruneLabels mark the containers of this runner, which Start removes
	// when an earlier run left them.
	PruneLabels map[string]string
	Timings     Timings
	// Out receives the progress of the main process, AgentLog the stderr of
	// the agent processes, and ImageProgress the output of fetching the
	// agent's image, AgentLog if nil.
	Out, AgentLog, ImageProgress io.Writer
	// MaxAgentMessage bounds a message from the agent; zero takes 64 MiB.
	MaxAgentMessage int
	// Main replaces the main process, and runs every conversation, for
	// tests.
	Main a2asrv.AgentExecutor
}

// Dirs are where the runner keeps its files. The caller holds whatever
// lock guards them while the runner runs.
type Dirs struct {
	// Sockets holds the sockets of the agent processes and containers,
	// EnvFiles the env files of containers, Policies the policies the
	// agent can't override, and Workspaces the private copies of the
	// workspace.
	Sockets, EnvFiles, Policies, Workspaces string
	// MCPMask and ConfigMask are the files that sandboxes mount over the
	// workspace's .mcp.json and over the agent's own configuration.
	MCPMask, ConfigMask string
	// Hidden are host paths every sandbox hides.
	Hidden []string
	// Pins are the directories containers are checked against.
	Pins *agentprofile.Pins
}

// Spec is the live configuration of the agent.
type Spec struct {
	// Name and Labels name the containers.
	Name   string
	Labels map[string]string
	// Agent is the profile, such as opencode, and Command replaces its
	// command.
	Agent   string
	Command []string
	Image   string
	Model   string
	// Env holds NAME=value pairs for the agent.
	Env []string
	// Sandbox is agentprofile.SandboxDocker or agentprofile.SandboxLocal.
	Sandbox string
	Limits  sandbox.Limits
	// Mounts are the directories the agent works on; the first is the
	// workspace.
	Mounts      []agentprofile.Mount
	EgressMode  string
	EgressAllow []string
	Inject      []agentprofile.InjectRule
	Tools       toolrules.Rules
	DisabledMCP []string
	// CLI is the a2a binary of an agent without a sandbox, or "".
	CLI string
}

// egress returns the settings of s that decide its egress.
func (s Spec) egress() agentprofile.Egress {
	return agentprofile.Egress{Sandbox: s.Sandbox, Mode: s.EgressMode, Allow: s.EgressAllow, Inject: s.Inject}
}

// Processes tells the runner about keys, which it treats as opaque.
type Processes interface {
	// Key returns the key of a new conversation, such as its caller's.
	Key(ctx context.Context, contextID string) (string, error)
	// Process describes the process of key now. The runner calls it on
	// every start, turn and sandbox check, so that a changed grant applies.
	// It fails when it can't tell, so that a read error changes nothing.
	Process(ctx context.Context, key string) (Process, error)
}

// Process describes the agent process of a key.
type Process struct {
	// Pool is the pool it runs in, "" for the main process.
	Pool string
	// Parent is the key whose Stop also stops this process.
	Parent string
	// Name is added to Spec.Name for the container, and CopyName names its
	// private workspaces.
	Name, CopyName string
	// Home is the host directory of its agent home.
	Home   string
	Labels map[string]string
	// LockHome makes the agent's own configuration in Home read-only.
	LockHome bool
	// AgentAPI gives the process the socket to Hooks.AgentAPI, and the a2a
	// CLI without a sandbox.
	AgentAPI bool
	// Owner is who the private workspaces of the process belong to.
	Owner string
	Grant Grant
}

// Pool is a pool of agent processes that start on their first turn and
// stop when idle.
type Pool struct {
	Name string
	// Label names the processes of the pool in errors, such as "the agent
	// of guests".
	Label string
	// MaxAgents bounds the processes that run at once; zero doesn't.
	MaxAgents int
	// QuestionTimeout stops a turn whose question gets no answer.
	QuestionTimeout time.Duration
}

// Grant is what a process's container gets.
type Grant struct {
	// Storage is how the container sees the workspace and the mounts.
	Storage workspaces.Storage
	// Secrets are the variables the agent gets, in the order of the
	// agent's secrets.
	Secrets []SecretGrant
}

// SecretGrant passes the stored secret From to the agent as Name.
type SecretGrant struct {
	Name, From string
}

// Equal reports whether g and o give the same storage and secrets.
func (g Grant) Equal(o Grant) bool {
	return g.Storage == o.Storage && slices.Equal(g.Secrets, o.Secrets)
}

// Covers reports whether g gives at least what o gives: storage with as
// much access, and every secret of o.
func (g Grant) Covers(o Grant) bool {
	if StorageRank(g.Storage) > StorageRank(o.Storage) {
		return false
	}
	for _, s := range o.Secrets {
		if !slices.Contains(g.Secrets, s) {
			return false
		}
	}
	return true
}

// StorageRank orders the storage modes from the most access to the least.
func StorageRank(storage workspaces.Storage) int {
	if i := slices.Index(workspaces.StorageModes, storage); i >= 0 {
		return i
	}
	return len(workspaces.StorageModes)
}

// Hooks are the caller's decisions inside a turn. Each is optional.
type Hooks struct {
	// Admit refuses a turn of the caller in ctx in the process of key, or
	// holds what the turn needs until release.
	Admit func(ctx context.Context, key string, execCtx *a2asrv.ExecutorContext) (release func(), err error)
	// Hold holds what an admitted turn needs while it runs, such as a share
	// of a budget, until release. Its refusal fails the turn, and leaves a
	// task that waits for an answer waiting.
	Hold func(ctx context.Context, key string, execCtx *a2asrv.ExecutorContext) (release func(), err error)
	// CanStart refuses to start the process of key.
	CanStart func(ctx context.Context, key string) error
	// PrepareHome writes into home before the process of key starts with
	// the grant g.
	PrepareHome func(ctx context.Context, key, home string, g Grant) error
	// MainPrepared reports the sandbox of the main process before it
	// starts.
	MainPrepared func(p Prepared)
	// Turn runs while the agent works on a message.
	Turn func(ctx context.Context, execCtx *a2asrv.ExecutorContext) (end func())
	// Prompt returns the parts sent to the agent.
	Prompt func(ctx context.Context, execCtx *a2asrv.ExecutorContext) ([]*a2a.Part, error)
	// Decide answers a permission that the tool rules didn't.
	Decide func(ctx context.Context, execCtx *a2asrv.ExecutorContext, call toolrules.Call) (Decision, bool, error)
	// Denied notes a tool call that the tool rules denied.
	Denied func(execCtx *a2asrv.ExecutorContext, title string)
	// Refused words the error that refuses a turn, or returns "" for the
	// error's text.
	Refused func(err error) string
	// LeftOutMCP reports an MCP server left out of a session, since the
	// container doesn't get the variables missing.
	LeftOutMCP func(ctx context.Context, server string, missing []string)
	// Stalled returns the status message of a turn that the runner stopped
	// since the agent sent nothing for d, while a tool call ran if tool, or
	// nil for the runner's own.
	Stalled func(d time.Duration, tool bool) []*a2a.Part
	// Failure returns the status message of a failed turn, or nil for the
	// error's text.
	Failure func(ctx context.Context, execCtx *a2asrv.ExecutorContext, err error) []*a2a.Part
	// AgentAPI is what agent processes reach through their socket, given
	// how to tell which process a request came from.
	AgentAPI func(processOf func(*http.Request) (key string, ok bool)) http.Handler
}

// Decision is how a tool call is answered without asking.
type Decision struct {
	toolrules.Decision
	// By is what decided, such as a preference, and User whose.
	By, User string
}

// Prepared describes the sandbox of the main process.
type Prepared struct {
	// Summary describes the sandbox, Mounts are the directories it mounts,
	// and Warnings the problems that don't stop the agent.
	Summary  string
	Mounts   []agentprofile.Mount
	Warnings []error
	// Egress is set when the container's connections go through the
	// runner, and NoBoundary is why they don't although the settings ask
	// for it.
	Egress     bool
	NoBoundary string
}

// Tasks is what the runner needs of the task store and the A2A handler.
type Tasks interface {
	// UnfinishedIn returns the unfinished tasks of the conversations that
	// run in the process of key.
	UnfinishedIn(ctx context.Context, key string) ([]a2a.TaskID, error)
	// Cancel cancels a task.
	Cancel(ctx context.Context, id a2a.TaskID) error
	// Abandon stores the event that ends a task whose agent stopped while
	// it waited for an answer.
	Abandon(ctx context.Context, ev *a2a.TaskStatusUpdateEvent) error
}

// Timings are the delays and timeouts of the runner.
type Timings struct {
	// TurnStall fails a turn in which the agent sent nothing this long
	// while none of its tool calls ran, and ToolStall one in which one ran.
	TurnStall, ToolStall time.Duration
	// SandboxCheck is how often the grants of running processes are
	// checked.
	SandboxCheck time.Duration
	// NarrowedCancelGrace is how long the tasks of a process whose grant
	// now gives less get to cancel before it stops.
	NarrowedCancelGrace time.Duration
}

// EgressState is what the runner knows of the egress of the containers.
type EgressState struct {
	// Started is set once a container was prepared. Mode is the egress
	// mode the containers started with, and Problem why they have Docker's
	// own network although the mode asks for the boundary.
	Started bool
	Mode    string
	Problem string
	// Blocked counts the refused connections, and Recent are the latest.
	Blocked int
	Recent  []boundary.Block
}

// Bridge tells how an agents URL reaches the AgentAPI handler: through the
// Unix socket of the main process, or over TCP at URL.
type Bridge struct {
	Socket string
	URL    string
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

	keys      *keys
	docker    *sandbox.Docker
	egress    *egressSockets
	listeners *listeners
	main      *endpoint
	policies  *policies
	copies    *workspaces.Copies
	stalls    *stallGuard
	decider   *toolDecider
	split     *splitExecutor
	pools     map[string]*procs
	// mainGrant is the grant the main process started with.
	mainGrant atomic.Pointer[Grant]
	// closeMain stops the main process.
	closeMain  func(context.Context) error
	background sync.WaitGroup
}

var _ a2asrv.AgentExecutor = (*Runner)(nil)

// New returns a runner that serves the agent of cfg. It opens the endpoints
// agent processes reach the caller through, and starts no process.
func New(cfg Config) (*Runner, error) {
	if cfg.MaxAgentMessage == 0 {
		cfg.MaxAgentMessage = maxAgentMessage
	}
	if cfg.ImageProgress == nil {
		cfg.ImageProgress = cfg.AgentLog
	}
	if cfg.Out == nil {
		cfg.Out = io.Discard
	}
	r := &Runner{
		cfg:      cfg,
		keys:     newKeys(cfg.AllSessions, cfg.Processes.Key),
		policies: &policies{dir: cfg.Dirs.Policies},
		copies:   workspaces.NewCopies(cfg.Dirs.Workspaces),
		stalls:   newStallGuard(cfg.Timings.TurnStall, cfg.Timings.ToolStall, cfg.Hooks.Stalled),
		pools:    map[string]*procs{},
	}
	for _, p := range cfg.Pools {
		r.pools[p.Name] = &procs{max: p.MaxAgents}
	}
	r.egress = newEgressSockets(cfg.Dirs.Sockets, func() agentprofile.Egress { return r.spec().egress() })
	r.docker = sandbox.NewDocker(cfg.Dirs.EnvFiles, r.egress.boundaries.Launcher)
	r.decider = &toolDecider{rules: func() toolrules.Rules { return r.spec().Tools }, servers: r.mcpServerNames, decide: cfg.Hooks.Decide, denied: cfg.Hooks.Denied}
	l, err := r.listen(cfg.Main == nil)
	if err != nil {
		return nil, errors.Join(err, r.egress.boundaries.Close(context.Background()))
	}
	r.listeners = l
	if r.main, err = l.open(""); err != nil {
		return nil, errors.Join(err, l.close(), r.egress.boundaries.Close(context.Background()))
	}
	return r, nil
}

// Start prunes what an earlier run left (containers, env files), prepares
// the main process and starts it. The processes run until Close.
func (r *Runner) Start(ctx context.Context) error {
	main, start := r.cfg.Main, r.startPool
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
	r.background.Go(func() { r.sandboxLoop(ctx) })
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
// or, for a new conversation, the one Processes.Key gives.
func (r *Runner) KeyOf(ctx context.Context, contextID string) (string, error) {
	return r.keys.of(ctx, contextID)
}

// Stop stops the processes of keys and of the keys under them, and holds
// their starts until release, which also forgets them.
func (r *Runner) Stop(ctx context.Context, keys ...string) (release func(), err error) {
	return r.stop(ctx, keys)
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

// Running returns the keys of the processes that run.
func (r *Runner) Running() []string {
	var out []string
	for _, p := range r.pools {
		out = append(out, p.runningKeys()...)
	}
	return out
}

// Started returns the grant the process of key started with.
func (r *Runner) Started(key string) (Grant, bool) {
	if key == "" {
		if g := r.mainGrant.Load(); g != nil {
			return *g, true
		}
		return Grant{}, false
	}
	p := r.knownPool(key)
	if p == nil {
		return Grant{}, false
	}
	return p.grantOf(key)
}

// Egress reports the egress of the containers.
func (r *Runner) Egress() EgressState { return r.egress.state() }

// Bridge tells how an agents URL reaches the AgentAPI handler.
func (r *Runner) Bridge() Bridge { return Bridge{Socket: r.main.socket, URL: r.listeners.url} }

// Call returns the call of a permission, with the MCP servers of the
// workspace.
func (r *Runner) Call(kind, title string, input any) toolrules.Call {
	return r.decider.call(kind, title, input)
}

// Copies are the private workspaces of the processes.
func (r *Runner) Copies() *workspaces.Copies { return r.copies }

// WritePolicies writes the policies of the agent processes again, such as
// after the tool rules changed. Agents that reload their policy apply them
// in open sessions too.
func (r *Runner) WritePolicies() error {
	spec := r.spec()
	if spec.Sandbox == agentprofile.SandboxLocal {
		return nil
	}
	for _, lockHome := range []bool{false, true} {
		if _, err := r.policies.write(spec, lockHome, nil); err != nil {
			return err
		}
	}
	return nil
}

// Serve serves Hooks.AgentAPI on the endpoints of the agent processes, and
// returns the function that shuts them down. fatal gets the errors of the
// listeners.
func (r *Runner) Serve(ctx context.Context, fatal func(error)) func(context.Context) error {
	handler := http.NotFoundHandler()
	if r.cfg.Hooks.AgentAPI != nil {
		handler = r.cfg.Hooks.AgentAPI(r.listeners.processOf)
	}
	return r.listeners.serve(ctx, handler, fatal)
}

// Shutdown waits for the turns in progress, which end when the context of
// Start does, and stops the processes of the pools. Turns that start later
// fail.
func (r *Runner) Shutdown(ctx context.Context) error {
	r.background.Wait()
	if r.split == nil {
		return nil
	}
	return r.split.close(ctx)
}

// Close stops every process: those of the pools, as Shutdown does, then
// the main one.
func (r *Runner) Close(ctx context.Context) error {
	err := r.Shutdown(ctx)
	if r.closeMain != nil {
		err = errors.Join(err, r.closeMain(ctx))
		r.closeMain = nil
	}
	return err
}

// Release closes the endpoints and the egress boundaries, after Close.
func (r *Runner) Release(ctx context.Context) error {
	return errors.Join(r.main.close(), r.listeners.close(), r.egress.boundaries.Close(ctx))
}

// spec returns the settings that the spec could read. The start or session
// that needs all of them reads the error of Config.Spec.
func (r *Runner) spec() Spec {
	spec, err := r.cfg.Spec()
	if err != nil {
		slog.Debug("reading the agent's settings", "error", err)
	}
	return spec
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
// main process. It asks Processes only for keys no pool knows yet.
func (r *Runner) poolOf(ctx context.Context, key string) (*procs, error) {
	if key == "" {
		return nil, nil
	}
	if p := r.knownPool(key); p != nil {
		return p, nil
	}
	proc, err := r.cfg.Processes.Process(ctx, key)
	if err != nil {
		return nil, err
	}
	return r.pools[proc.Pool], nil
}

// idAlphabet names the directories of sockets.
const idAlphabet = cryptoutil.Unambiguous
