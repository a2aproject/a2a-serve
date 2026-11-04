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
	"net"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/limiter"
	"github.com/a2aproject/a2a-go/v2/executor/a2aacp"

	"github.com/a2aproject/a2a-serve/internal/a2acli"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/approvals"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/cryptoutil"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
	"github.com/a2aproject/a2a-serve/internal/harness"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// harnessSocketName is the name of the Unix socket the harness reaches the
// server through, in the harness socket directory.
const harnessSocketName = "a2a.sock"

// newExecutor starts the harness as an ACP agent in its sandbox. close stops
// the harness and cleans up its sandbox.
func (s *Server) newExecutor(ctx context.Context, harnessLog, imageProgress io.Writer) (exec a2asrv.AgentExecutor, close func(context.Context) error, err error) {
	cfg := s.config()
	profile, err := harness.Lookup(cfg.Harness, cfg.Command)
	if err != nil {
		return nil, nil, err
	}
	mounts, err := s.dir.Mounts(cfg)
	if err != nil {
		return nil, nil, err
	}
	env := append([]string{}, cfg.Env...)
	for _, name := range cfg.Secrets {
		v, ok := s.secrets()[name]
		if !ok {
			return nil, nil, fmt.Errorf("secret %s is not set; pass --secret %s again with the variable set", name, name)
		}
		env = append(env, name+"="+v)
	}
	if s.bridge.helperEnv != "" {
		env = append(env, s.bridge.helperEnv)
	}
	spec := harness.Spec{
		Profile:  profile,
		Sandbox:  cfg.Sandbox,
		Mounts:   mounts,
		Home:     s.dir.HomeDir(),
		Hidden:   []string{s.dir.Path()},
		Env:      env,
		Name:     "a2a-" + cfg.Name + "-" + s.dir.ID(),
		Image:    cfg.Image,
		Limits:   harness.Limits{Memory: cfg.Limits.Memory, CPUs: cfg.Limits.CPUs, PIDs: cfg.Limits.PIDs},
		Labels:   map[string]string{"a2a.agent": cfg.Name, "a2a.dir": s.dir.Root},
		Socket:   s.bridge.socket,
		Progress: imageProgress,
	}
	if cfg.Sandbox == harness.SandboxLocal {
		spec.CLI = s.localCLI(ctx)
	}
	prepared, err := harness.Prepare(ctx, spec)
	if err != nil {
		return nil, nil, err
	}
	s.logf("✓ %s", prepared.Summary)
	mountDesc := make([]string, len(mounts))
	for i, m := range mounts {
		mountDesc[i] = m.String()
	}
	s.logf("✓ workspace %s; config %s", strings.Join(mountDesc, ", "), s.dir.ConfigPath())

	agent := a2aacp.Process{Argv: prepared.Args, Dir: mounts[0].Src, Env: prepared.Env, Stderr: harnessLog}
	acp, err := a2aacp.NewExecutor(ctx, agent, &a2aacp.Config{
		AutoAnswer:   liveToolRules(s.config),
		SessionStore: s.store.Sessions(),
		Session:      s.sessionParams,
	})
	if err != nil {
		return nil, nil, errors.Join(fmt.Errorf("starting %s: %w", profile.Name(), err), prepared.Stop(ctx))
	}
	s.logf("✓ %s is running (ACP)", profile.Name())
	return acp, func(ctx context.Context) error { return errors.Join(acp.Close(), prepared.Stop(ctx)) }, nil
}

// localCLI returns the a2a CLI on PATH, with which a harness without a
// sandbox calls other agents, and warns when that can't work.
func (s *Server) localCLI(ctx context.Context) string {
	cli, err := a2acli.Path()
	if err != nil {
		s.logf("! the harness can't call other agents: %v", err)
		return ""
	}
	if v := a2acli.Version(ctx); a2acli.Outdated(v) {
		s.logf("! the harness calls other agents with a2a %s at %s, which is older than %s; run a2a-serve doctor", v, cli, a2acli.PinnedVersion)
	}
	return cli
}

// liveToolRules answers tool permissions with the allow and deny rules of
// the current config, so that changing them doesn't need a restart.
type liveToolRules func() *agentdir.Config

func (f liveToolRules) AutoAnswer(ctx context.Context, execCtx *a2asrv.ExecutorContext, q a2aacp.Question) (a2aacp.Answer, bool) {
	cfg := f()
	return a2aacp.ToolRules{Allow: cfg.Approvals.Allow, Deny: cfg.Approvals.Deny}.AutoAnswer(ctx, execCtx, q)
}

// listenHarness opens the listener through which the harness calls other
// agents: a Unix socket, which is mounted into a container, or TCP with a key
// for Docker Desktop, which can't mount it.
func (s *Server) listenHarness(sandboxed bool) (net.Listener, harnessBridge, error) {
	bind, dial, tcp := harness.TCPBridge(s.config().Sandbox)
	if !sandboxed || !tcp {
		return s.listenHarnessSocket()
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(bind, "0"))
	if err != nil {
		return nil, harnessBridge{}, fmt.Errorf("listening for the harness on %s: %w", bind, err)
	}
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		return nil, harnessBridge{}, errors.Join(err, ln.Close())
	}
	b := harnessBridge{url: "http://" + net.JoinHostPort(dial, port), key: cryptoutil.RandomString(40, cryptoutil.Alphanumeric)}
	b.helperEnv, err = writeHarnessHelper(s.dir.HomeDir(), dial, b.key)
	if err != nil {
		return nil, harnessBridge{}, errors.Join(fmt.Errorf("saving the harness key: %w", err), ln.Close())
	}
	return ln, b, nil
}

func (s *Server) listenHarnessSocket() (net.Listener, harnessBridge, error) {
	dir := s.dir.HarnessSocketDir()
	if err := fsutil.MkdirPrivate(filepath.Dir(dir)); err != nil {
		return nil, harnessBridge{}, err
	}
	if err := fsutil.MkdirPrivate(dir); err != nil {
		return nil, harnessBridge{}, err
	}
	sock := filepath.Join(dir, harnessSocketName)
	if err := removeIfExists(sock); err != nil {
		return nil, harnessBridge{}, err
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, harnessBridge{}, fmt.Errorf("listening for the harness on %s: %w", sock, err)
	}
	return ln, harnessBridge{socket: sock}, nil
}

// observedExecutor publishes every event of the agent to the server's event
// bus and relays questions which the task's caller can't answer.
type observedExecutor struct {
	inner     a2asrv.AgentExecutor
	bus       *eventBus
	peers     *Peers
	approvals *approvals.Service
}

var _ a2asrv.AgentExecutor = (*observedExecutor)(nil)

func (e *observedExecutor) Execute(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	withLine := e.peers.withAgentsLine(ctx, execCtx)
	events := withAnswerOrder(execCtx, coalesceArtifacts(e.peers.duringTurn(ctx, execCtx, e.inner.Execute(ctx, withLine))))
	if withLine == execCtx {
		return e.observe(ctx, execCtx, events)
	}
	return e.observe(ctx, execCtx, func(yield func(a2a.Event, error) bool) {
		for ev, err := range events {
			if t, ok := ev.(*a2a.Task); ok {
				ev = withOriginalMessage(t, withLine.Message, execCtx.Message)
			}
			if !yield(ev, err) {
				return
			}
		}
	})
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
	return e.observe(ctx, execCtx, canceledFirst(execCtx, e.inner.Cancel(ctx, execCtx)))
}

// canceledFirst yields the canceled event before the inner Cancel runs.
// a2aacp stops the turn before yielding it, which can end a running
// execution and close its queue first, failing the cancel with "queue is
// closed": https://github.com/a2aproject/a2a-go/issues/518. The inner
// events still run for their side effects; its canceled event is dropped.
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
		for ev, err := range events {
			if err != nil {
				slog.Info("task failed", "task", execCtx.TaskID, "duration", time.Since(start).Round(time.Millisecond), "error", err)
			}
			if err == nil {
				logTransition(execCtx.TaskID, ev, start)
				e.bus.publish(execCtx.TaskID, ev)
				if q := approvals.FromEvent(ev); q != nil {
					e.approvals.OnQuestion(ctx, execCtx, q)
				}
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
	var state a2a.TaskState
	switch e := ev.(type) {
	case *a2a.Task:
		state = e.Status.State
	case *a2a.TaskStatusUpdateEvent:
		state = e.Status.State
	default:
		return
	}
	if !state.Terminal() && state != a2a.TaskStateInputRequired && state != a2a.TaskStateAuthRequired {
		return
	}
	slog.Info("task "+stateName(state), "task", id, "duration", time.Since(start).Round(time.Millisecond))
}

// eventBus fans out task events to in-process followers, such as a channel
// conversation waiting for a task someone else answered. Followers never
// miss events: a slow follower's events queue up.
type eventBus struct {
	mu   sync.Mutex
	subs map[a2a.TaskID]map[*subscription]struct{}
}

// subscription is the queue of events of one follower. ready has a value
// when events are queued.
type subscription struct {
	mu     sync.Mutex
	events []a2a.Event
	ready  chan struct{}
}

// next returns the next event, or false when ctx ends or done is closed.
func (s *subscription) next(ctx context.Context, done <-chan time.Time) (a2a.Event, bool) {
	for {
		s.mu.Lock()
		if len(s.events) > 0 {
			ev := s.events[0]
			s.events = s.events[1:]
			s.mu.Unlock()
			return ev, true
		}
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, false
		case <-done:
			return nil, false
		case <-s.ready:
		}
	}
}

func (s *subscription) push(ev a2a.Event) {
	s.mu.Lock()
	s.events = append(s.events, ev)
	s.mu.Unlock()
	select {
	case s.ready <- struct{}{}:
	default:
	}
}

func newEventBus() *eventBus { return &eventBus{subs: map[a2a.TaskID]map[*subscription]struct{}{}} }

func (b *eventBus) subscribe(id a2a.TaskID) (*subscription, func()) {
	sub := &subscription{ready: make(chan struct{}, 1)}
	b.mu.Lock()
	if b.subs[id] == nil {
		b.subs[id] = map[*subscription]struct{}{}
	}
	b.subs[id][sub] = struct{}{}
	b.mu.Unlock()
	return sub, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		delete(b.subs[id], sub)
		if len(b.subs[id]) == 0 {
			delete(b.subs, id)
		}
	}
}

func (b *eventBus) publish(id a2a.TaskID, ev a2a.Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for sub := range b.subs[id] {
		sub.push(ev)
	}
}

// callerInterceptor makes the authenticated principal the A2A user and
// enforces who may join a conversation, who may answer approvals and how
// deep agent chains go.
type callerInterceptor struct {
	a2asrv.PassthroughCallInterceptor
	approvals *approvals.Service
	tasks     *store.TaskStore
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
	msg, ok := req.Payload.(*a2a.SendMessageRequest)
	if !ok || msg.Message == nil {
		return ctx, nil, nil
	}
	if err := i.checkConversation(ctx, p, msg.Message.ContextID); err != nil {
		return ctx, nil, err
	}
	if msg.Message.TaskID != "" {
		if err := i.approvals.CheckAnswer(ctx, p, msg.Message.TaskID); err != nil {
			return ctx, nil, err
		}
	}
	return ctx, nil, nil
}

// checkConversation refuses a message into a conversation that holds
// another user's tasks, unless an owner sends it or the server picked the
// conversation, as for a group chat.
func (i *callerInterceptor) checkConversation(ctx context.Context, p auth.Principal, contextID string) error {
	if contextID == "" || p.IsOwner() || auth.IsInProcess(ctx) {
		return nil
	}
	others, err := i.tasks.HasOthersTasks(ctx, contextID, p.ID)
	if err != nil {
		return fmt.Errorf("checking conversation %s: %w", contextID, err)
	}
	if others {
		return fmt.Errorf("%w: conversation %s belongs to someone else; leave out the context ID to start a new one", a2a.ErrUnauthorized, contextID)
	}
	return nil
}
