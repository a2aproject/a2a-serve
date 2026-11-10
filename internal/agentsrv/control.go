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
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/a2aproject/a2a-serve/internal/a2atext"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agentops"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/control"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/inspector"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
	"github.com/a2aproject/a2a-serve/internal/httpserver"
	"github.com/a2aproject/a2a-serve/internal/messenger"
	"github.com/a2aproject/a2a-serve/internal/network"
)

// serveControl serves the control socket, through which management commands
// change the running server without a restart.
func (s *Server) serveControl(ctx context.Context, closers *stack, errc chan<- error) error {
	path := s.dir.SocketPath()
	if err := fsutil.MkdirPrivate(filepath.Dir(path)); err != nil {
		return err
	}
	if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
		return errors.Join(fmt.Errorf("the server is already running in %s (a2a-serve status)", s.dir.Root), c.Close())
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("control socket: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return errors.Join(err, ln.Close())
	}
	mux := control.Handler(control.Deps{
		Ops:            s.ops,
		Status:         s.status,
		Stop:           s.Stop,
		Config:         s.rt.Config,
		ReloadConfig:   s.reloadConfig,
		Card:           s.listenerCard,
		Peers:          s.listPeers,
		Ads:            s.ads,
		InspectorLogin: func() string { return inspector.Path + "/login?code=" + s.inspector.LoginCode() },
	})
	srv := httpserver.New(ctx, mux)
	closers.push(func(ctx context.Context) error {
		err := srv.Shutdown(ctx)
		return errors.Join(err, removeIfExists(path))
	})
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			reportFatal(errc, fmt.Errorf("control socket: %w", err))
		}
	}()
	return nil
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// status describes the running server, as `a2a-serve status` shows it.
func (s *Server) status(ctx context.Context) (*controlapi.Status, error) {
	cfg := s.rt.Config()
	local, private, public := s.URLs()
	st := &controlapi.Status{
		Name: cfg.Name, Harness: cfg.Harness, Dir: s.dir.Root, PID: pid(), Started: s.started,
		Local: local, Private: private, Public: public, Tasks: map[string]int{},
	}
	if u, err := url.Parse(private); err == nil && u.Host == network.AgentMeshHost {
		st.PrivateNote = "works only from other a2a-serve agents on the relay; give people the public URL"
	}
	if s.private != nil {
		st.PrivateProblem = networkProblem(network.HealthOf(s.private))
	}
	if s.public != nil {
		st.PublicProblem = networkProblem(network.HealthOf(s.public))
	}
	if p := st.PrivateProblem; p != nil && p.State == string(network.HealthRemoved) {
		st.Private, st.PrivateNote = "", ""
	}
	if p := st.PublicProblem; p != nil && p.State == string(network.HealthRemoved) {
		st.Public = ""
	}
	st.Messengers = s.supervisor.Status()
	st.Egress = s.egress.status(cfg)
	counts, err := s.tasks.TaskCounts(ctx)
	if err != nil {
		return nil, err
	}
	for state, n := range counts {
		st.Tasks[a2atext.StateName(state)] = n
	}
	links, err := s.store.LinksOf(ctx, auth.LocalPairing)
	if err != nil {
		return nil, err
	}
	for _, l := range links {
		st.Owners = append(st.Owners, l.Identity)
	}
	cs, err := s.store.Contacts(ctx)
	if err != nil {
		return nil, err
	}
	nodes, err := s.store.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	st.Contacts, st.Nodes = len(cs), len(nodes)
	if st.Approvals, err = inspector.ApprovalCount(ctx, s.store, s.tasks); err != nil {
		return nil, err
	}
	if st.MCPServers, st.Skills, err = ProjectStatus(s.dir, cfg, s.rt.Secrets()); err != nil {
		st.ProjectError = err.Error()
	}
	st.AllowTools, st.DenyTools = cfg.Approvals.Allow, cfg.Approvals.Deny
	st.Backup = s.backups.get()
	a, err := s.ops.Access(ctx)
	if err != nil {
		return nil, err
	}
	st.Access = agentops.AccessInfo(a)
	st.Access.SandboxNotes = s.sandboxNotes(st.Access.SandboxNotes)
	return st, nil
}

// startMessenger starts a messenger that was added, and describes it with the
// link for the owner if nobody is linked on it yet.
func (s *Server) startMessenger(ctx context.Context, ch messenger.Messenger) *controlapi.MessengerStatus {
	s.supervisor.Add(ch)
	return s.supervisor.MessengerStatus(ch, s.supervisor.OwnerLink(ctx, ch))
}

// reloadConfig applies the live settings of config.yaml, and the new rules
// answer the approvals that wait. Rules decide before preferences and the
// grants of conversations, so a new deny stops the calls they allowed.
func (s *Server) reloadConfig(ctx context.Context) error {
	before := s.rt.Config()
	if err := s.rt.Reload(); err != nil {
		return err
	}
	if before.Policy.PrivateAllowOutgoing != s.rt.Config().Policy.PrivateAllowOutgoing {
		s.refreshInstructions(ctx)
	}
	if before.ContactRefresh != s.rt.Config().ContactRefresh {
		select {
		case s.cardRefreshChanged <- struct{}{}:
		default:
		}
	}
	// Claude Code reloads its policy in open sessions too, so the agent asks
	// about shell commands as soon as the rules deny some.
	if cfg := s.rt.Config(); cfg.Sandbox != agentdir.SandboxLocal {
		for _, lockHome := range []bool{false, true} {
			if _, err := s.policies.write(cfg, lockHome, nil); err != nil {
				return fmt.Errorf("writing the agent's policy: %w", err)
			}
		}
	}
	return s.answerWaiting(ctx, nil)
}

// peersRediscoverEvery bounds how often `a2a-serve peers` discovers the
// agents on the private network, so it stops listing an agent soon after
// it leaves.
const peersRediscoverEvery = time.Second

func (s *Server) listPeers(ctx context.Context) ([]network.PeerInfo, error) {
	return s.clients.Rediscover(ctx, peersRediscoverEvery)
}

// networkProblem describes h for the status, or returns nil when the
// network reaches the agent.
func networkProblem(h network.Health) *controlapi.NetworkProblem {
	if h.OK() {
		return nil
	}
	return &controlapi.NetworkProblem{State: string(h.State), Problem: h.Problem, Fix: h.Fix}
}
