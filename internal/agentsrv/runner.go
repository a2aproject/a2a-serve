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
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"

	"github.com/a2aproject/a2a-serve/internal/a2acli"
	"github.com/a2aproject/a2a-serve/internal/a2atext"
	"github.com/a2aproject/a2a-serve/internal/agentcfg"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/agentkey"
	"github.com/a2aproject/a2a-serve/internal/agentprofile"
	"github.com/a2aproject/a2a-serve/internal/agentrun"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/chat"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/egressrules"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
	"github.com/a2aproject/a2a-serve/internal/store"
	"github.com/a2aproject/a2a-serve/internal/toolrules"
)

// newRunner builds the runner of the agent's processes. The hooks it gets
// read the components that build creates later, when they run.
func (s *Server) newRunner(opts Options) (*agentrun.Runner, error) {
	progress := opts.ImageProgress
	if progress == nil {
		progress = opts.HarnessLog
	}
	return agentrun.New(agentrun.Config{
		Dirs: agentrun.Dirs{
			Sockets:    s.dir.HarnessSocketDir(),
			EnvFiles:   s.dir.EnvFilesDir(),
			Policies:   s.dir.PoliciesDir(),
			Workspaces: s.dir.WorkspacesDir(),
			MCPMask:    s.dir.MCPMaskPath(),
			ConfigMask: s.dir.ConfigMaskPath(),
			Hidden:     []string{s.dir.Path()},
			Pins:       s.pins,
		},
		Spec:      s.runnerSpec,
		Secrets:   func() map[string]string { return s.rt.Secrets() },
		Processes: processes{s},
		Pools: []agentrun.Pool{
			// Approvals reject questions after their timeout; this stops the
			// turn if that answer doesn't arrive.
			{Name: guestPool, Label: "the agent of guests", QuestionTimeout: s.timings.GuestQuestionTimeout + time.Minute},
			{Name: usersPool, Label: "the agent processes of users", MaxAgents: maxUserAgents, QuestionTimeout: s.timings.UserQuestionTimeout + time.Minute},
		},
		Sessions:    s.sessions,
		AllSessions: s.store.Sessions(),
		Tasks:       runnerTasks{s},
		Hooks: agentrun.Hooks{
			Admit:        s.admitCaller,
			Hold:         s.holdBudget,
			CanStart:     s.canStart,
			PrepareHome:  s.prepareHome,
			MainPrepared: s.mainPrepared,
			Turn:         func(ctx context.Context, execCtx *a2asrv.ExecutorContext) func() { return s.claims.turn(ctx, execCtx) },
			Prompt: func(ctx context.Context, execCtx *a2asrv.ExecutorContext) ([]*a2a.Part, error) {
				return s.queued.prompt(ctx, execCtx)
			},
			Decide: func(ctx context.Context, execCtx *a2asrv.ExecutorContext, call toolrules.Call) (agentrun.Decision, bool, error) {
				return s.decider.hook(ctx, execCtx, call)
			},
			Denied:  func(execCtx *a2asrv.ExecutorContext, title string) { s.notes.Add(execCtx.TaskID, deniedNote(title)) },
			Refused: func(err error) string { return agentcfg.Explain(err).Error() },
			LeftOutMCP: func(ctx context.Context, server string, missing []string) {
				slog.WarnContext(ctx, "leaving out an MCP server with variables that the conversation's container doesn't get; pass them with --secret, or give them in the sandbox rule of its user", "server", server, "missing", missing)
			},
			Stalled:  stalledTurn,
			Failure:  s.failure,
			AgentAPI: func(processOf func(*http.Request) (string, bool)) http.Handler { return s.proxy.Routes(processOf) },
		},
		PruneLabels: map[string]string{"a2a.dir": s.dir.Root},
		Timings: agentrun.Timings{
			TurnStall:           s.timings.TurnStallTimeout,
			ToolStall:           s.timings.ToolStallTimeout,
			SandboxCheck:        s.timings.SandboxCheckInterval,
			NarrowedCancelGrace: s.timings.NarrowedCancelGrace,
		},
		Out:             s.out,
		AgentLog:        opts.HarnessLog,
		ImageProgress:   progress,
		MaxAgentMessage: opts.MaxAgentMessage,
		Main:            opts.Executor,
	})
}

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

// runnerSpec is what the runner runs for the agent's config now.
func (s *Server) runnerSpec() (agentrun.Spec, error) {
	cfg := s.rt.Config()
	cli := ""
	if cfg.Sandbox == agentprofile.SandboxLocal {
		cli = localCLI()
	}
	return agentcfg.Spec(cfg, s.dir, cli)
}

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

// processes tells the runner about agent keys: the owner's process for "",
// the one of guests, and one per member and per person behind a member's
// calls.
type processes struct{ s *Server }

// Key returns the agent key of the caller who starts a conversation.
func (p processes) Key(ctx context.Context, _ string) (string, error) {
	return agentkey.Of(auth.Caller(ctx)), nil
}

// Process describes the agent process of key under the sandbox rules in
// force.
func (p processes) Process(ctx context.Context, key string) (agentrun.Process, error) {
	s := p.s
	owner, sb, err := s.sandboxes.grant(ctx, key)
	if err != nil {
		return agentrun.Process{}, err
	}
	proc := agentrun.Process{
		Home:     s.dir.HomeDir(),
		CopyName: workspaceName(key),
		AgentAPI: true,
		Owner:    owner.ID,
		Grant:    agentrun.Grant(sb),
	}
	id, isUser := agentkey.UserOf(key)
	switch {
	case key == "":
	case key == agentkey.Guest:
		// Guests get no way to call other agents: no socket, credential
		// helper or CLI.
		proc.Pool, proc.Name, proc.Home, proc.AgentAPI = guestPool, "guest", s.dir.GuestHomeDir(), false
		proc.Labels = map[string]string{"a2a.role": auth.RoleGuest}
		proc.LockHome = true
	case isUser:
		proc.Pool, proc.Name, proc.Home = usersPool, userProcessName(key), s.userHomeDir(key)
		proc.Labels = map[string]string{"a2a.role": auth.RoleMember}
		proc.LockHome = true
		if agentkey.CallerOf(key) != "" {
			proc.Parent = agentkey.User(id)
		}
	default:
		return agentrun.Process{}, fmt.Errorf("unknown agent key %q", key)
	}
	return proc, nil
}

// canStart refuses to start the agent process of a member whose pairing
// was removed.
func (s *Server) canStart(ctx context.Context, key string) error {
	if id, ok := agentkey.UserOf(key); ok {
		return s.checkUser(ctx, id)
	}
	return nil
}

// prepareHome writes the storage note and the instructions into the agent
// home of key, after it checks that the home is private.
func (s *Server) prepareHome(ctx context.Context, key, home string, g agentrun.Grant) error {
	s.setStorageNote(home, g.Storage)
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
	mountDesc := make([]string, len(p.Mounts))
	for i, m := range p.Mounts {
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

// startError words the error of a runner that didn't start.
func startError(err error) error {
	if errors.Is(err, agentrun.ErrStartTimeout) {
		err = agenterr.WithHint(err, agenterr.HintAgentStartTimeout, "")
	}
	return agentcfg.Explain(err)
}

// runnerTasks is what the runner needs of the task store and the handler.
type runnerTasks struct{ s *Server }

func (t runnerTasks) UnfinishedIn(ctx context.Context, key string) ([]a2a.TaskID, error) {
	return t.s.tasks.UnfinishedIn(ctx, key)
}

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
	runner    *agentrun.Runner
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
	release, err := u.runner.Stop(ctx, keys...)
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
	if s.runner == nil {
		return agentcfg.HostEgress(cfg).Boundary
	}
	if st := s.runner.Egress(); st.Started {
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
	es := s.runner.Egress()
	st := &controlapi.EgressStatus{Mode: cmp.Or(es.Mode, cfg.Egress.Mode, egressrules.ModeInternet), Allow: cfg.Egress.Allow, Blocked: es.Blocked}
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
	for _, b := range es.Recent {
		st.Recent = append(st.Recent, controlapi.EgressBlock{Destination: b.Destination, Allow: b.Allow, Unresolved: b.Unresolved, Allowed: !b.Unresolved && allowedByRule(p, b.Allow)})
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
