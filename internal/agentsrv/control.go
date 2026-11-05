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
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/a2aproject/a2a-serve/internal/a2atext"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/control"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/inspector"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/channel"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
	"github.com/a2aproject/a2a-serve/internal/httpserver"
	"github.com/a2aproject/a2a-serve/internal/manage"
	"github.com/a2aproject/a2a-serve/internal/network"
	"github.com/a2aproject/a2a-serve/internal/schedule"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// serveControl serves the control socket, through which management commands
// change the running server without a restart.
func (s *Server) serveControl(ctx context.Context, closers *stack, errc chan<- error) error {
	path := s.dir.SocketPath()
	if err := fsutil.MkdirPrivate(filepath.Dir(path)); err != nil {
		return err
	}
	if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
		return errors.Join(fmt.Errorf("an agent server is already running in %s (a2a-serve status)", s.dir.Root), c.Close())
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
		Status:        s.status,
		Stop:          s.Stop,
		Config:        s.config,
		ReloadConfig:  s.reloadSavedConfig,
		Pairings:      s.pairings,
		Peers:         s.clients.discover,
		Contacts:      s.contacts,
		AddChannel:    s.addChannel,
		RemoveChannel: s.deleteChannel,
		Store:         s.store,
		AddSchedule: func(ctx context.Context, req schedule.Request, by auth.Principal) (store.Schedule, error) {
			return s.schedules.add(ctx, req, by, "")
		},
		RunSchedule:    s.schedules.runNow,
		RemoveSchedule: s.schedules.remove,
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
	cfg := s.config()
	local, private, public := s.URLs()
	st := &controlapi.Status{
		Name: cfg.Name, Harness: cfg.Harness, Dir: s.dir.Root, PID: pid(), Started: s.started,
		Local: local, Private: private, Public: public, Tasks: map[string]int{},
	}
	if u, err := url.Parse(private); err == nil && u.Host == network.AgentMeshHost {
		st.PrivateNote = "works only from other a2a-serve agents on the relay; give people the public URL"
	}
	st.Channels = s.supervisor.status()
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
	contacts, err := s.store.Contacts(ctx)
	if err != nil {
		return nil, err
	}
	nodes, err := s.store.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	st.Contacts, st.Nodes = len(contacts), len(nodes)
	approvals, err := s.inspector.PendingApprovals(ctx)
	if err != nil {
		return nil, err
	}
	st.Approvals = len(approvals)
	if st.MCPServers, st.Skills, err = ProjectStatus(s.dir, cfg, s.secrets()); err != nil {
		st.ProjectError = err.Error()
	}
	st.Backup = s.backups.get()
	if st.Access, err = s.access(ctx); err != nil {
		return nil, err
	}
	return st, nil
}

// contacts manages the contacts, refreshing the harness instructions
// which list them.
func (s *Server) contacts() manage.Contacts {
	m := s.clients.contacts()
	m.Changed = s.refreshInstructions
	return m
}

// addChannel checks the channel of req, saves it and starts it.
func (s *Server) addChannel(ctx context.Context, req controlapi.ChannelRequest) (*controlapi.ChannelStatus, error) {
	secrets, err := s.dir.LoadSecrets()
	if err != nil {
		return nil, err
	}
	maps.Copy(secrets, req.Secrets)
	next := s.config()
	if err := next.SetChannel(req.Kind, req.Token, req.Email); err != nil {
		return nil, agenterr.Invalid(err)
	}
	ch, err := s.buildChannel(next, secrets, req.Kind)
	if err != nil {
		return nil, agenterr.Invalid(err)
	}
	if err := channel.Validate(ctx, ch); err != nil {
		return nil, err
	}
	if err := s.dir.SaveSecrets(req.Secrets); err != nil {
		return nil, err
	}
	s.setSecrets(secrets)
	if err := s.updateConfig(func(c *agentdir.Config) error { return c.SetChannel(req.Kind, req.Token, req.Email) }); err != nil {
		return nil, err
	}
	s.supervisor.add(ch)
	return s.supervisor.channelStatus(ch, s.supervisor.ownerLink(ctx, ch)), nil
}

func (s *Server) deleteChannel(kind string) error {
	if err := (&agentdir.Config{}).SetChannel(kind, "", nil); err != nil {
		return agenterr.Invalid(err)
	}
	if !s.config().HasChannel(kind) {
		return &agenterr.NotFoundError{Kind: "channel", Name: kind}
	}
	if err := s.updateConfig(func(c *agentdir.Config) error { return c.SetChannel(kind, "", nil) }); err != nil {
		return err
	}
	s.supervisor.remove(kind)
	return nil
}

// reloadSavedConfig applies the live settings of config.yaml.
func (s *Server) reloadSavedConfig() error {
	saved, err := s.dir.LoadConfig()
	if err != nil {
		return err
	}
	s.reloadConfig(saved)
	return nil
}
