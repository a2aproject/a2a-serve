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
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/a2aproject/a2a-serve/internal/a2atext"
	"github.com/a2aproject/a2a-serve/internal/agentops"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/control"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/inspector"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/channel"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
	"github.com/a2aproject/a2a-serve/internal/httpserver"
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
		Peers:          s.clients.Discover,
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
	st.Channels = s.supervisor.Status()
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
	st.Backup = s.backups.get()
	a, err := s.ops.Access(ctx)
	if err != nil {
		return nil, err
	}
	st.Access = agentops.AccessInfo(a)
	return st, nil
}

// startChannel starts a channel that was added, and describes it with the
// link for the owner if nobody is linked on it yet.
func (s *Server) startChannel(ctx context.Context, ch channel.Channel) *controlapi.ChannelStatus {
	s.supervisor.Add(ch)
	return s.supervisor.ChannelStatus(ch, s.supervisor.OwnerLink(ctx, ch))
}

// reloadConfig applies the live settings of config.yaml. When they deny
// more tools, it restarts the agent processes, which forget the tool calls
// callers allowed always. Otherwise the new rules answer the approvals that
// wait.
func (s *Server) reloadConfig(ctx context.Context) error {
	before := s.rt.Config()
	if err := s.rt.Reload(); err != nil {
		return err
	}
	if before.Policy.PrivateAllowOutgoing != s.rt.Config().Policy.PrivateAllowOutgoing {
		s.refreshInstructions(ctx)
	}
	if !tightened(before.Approvals.Deny, s.rt.Config().Approvals.Deny) {
		return s.answerWaiting(ctx)
	}
	n, err := s.procs.restart()
	if err != nil {
		return fmt.Errorf("restarting the agent processes for the new deny-tools: %w", err)
	}
	slog.Info("restarted the agent processes, so that the new deny-tools apply to tools allowed always", "count", n)
	return nil
}
