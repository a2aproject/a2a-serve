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
	"strconv"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
	"github.com/a2aproject/a2a-serve/internal/jsonhttp"
	"github.com/a2aproject/a2a-serve/internal/manage"
	"github.com/a2aproject/a2a-serve/internal/network"
	"github.com/a2aproject/a2a-serve/internal/pairings"
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
	if err := os.WriteFile(s.dir.PIDPath(), []byte(strconv.Itoa(pid())), 0o600); err != nil {
		return errors.Join(err, ln.Close())
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", s.ctlStatus)
	mux.HandleFunc("POST /stop", func(w http.ResponseWriter, r *http.Request) {
		jsonhttp.Write(w, http.StatusOK, map[string]bool{"stopping": true})
		go s.Stop()
	})
	mux.HandleFunc("POST /pair", s.ctlPair)
	mux.HandleFunc("GET /pairings", s.ctlPairings)
	mux.HandleFunc("GET /pairings/{name}", s.ctlPairing)
	mux.HandleFunc("PATCH /pairings/{name}", s.ctlUpdatePairing)
	mux.HandleFunc("DELETE /pair/{name}", s.ctlRevoke)
	mux.HandleFunc("GET /peers", s.ctlPeers)
	mux.HandleFunc("GET /contacts", s.ctlContacts)
	mux.HandleFunc("POST /contacts", s.ctlAddContact)
	mux.HandleFunc("DELETE /contacts/{name}", s.ctlDeleteContact)
	mux.HandleFunc("POST /channels", s.ctlAddChannel)
	mux.HandleFunc("DELETE /channels/{kind}", s.ctlDeleteChannel)
	mux.HandleFunc("GET /schedules", s.ctlSchedules)
	mux.HandleFunc("POST /schedules", s.ctlAddSchedule)
	mux.HandleFunc("POST /schedules/{name}/run", s.ctlRunSchedule)
	mux.HandleFunc("DELETE /schedules/{name}", s.ctlDeleteSchedule)
	mux.HandleFunc("GET /advertisements", s.ctlAds)
	mux.HandleFunc("POST /inspector/login", func(w http.ResponseWriter, r *http.Request) {
		jsonhttp.Write(w, http.StatusOK, controlapi.InspectorLogin{Path: InspectorPath + "/login?code=" + s.inspector.loginCode()})
	})
	mux.HandleFunc("GET /config", func(w http.ResponseWriter, r *http.Request) { jsonhttp.Write(w, http.StatusOK, s.config()) })
	mux.HandleFunc("POST /config/reload", s.ctlReloadConfig)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
	closers.push(func(ctx context.Context) error {
		err := srv.Shutdown(ctx)
		return errors.Join(err, removeIfExists(path), removeIfExists(s.dir.PIDPath()))
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

func (s *Server) ctlStatus(w http.ResponseWriter, r *http.Request) {
	st, err := s.status(r.Context())
	if err != nil {
		jsonhttp.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonhttp.Write(w, http.StatusOK, st)
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
	st.Channels = s.router.status()
	counts, err := s.tasks.TaskCounts(ctx)
	if err != nil {
		return nil, err
	}
	for state, n := range counts {
		st.Tasks[stateName(state)] = n
	}
	links, err := s.store.LinksOf(ctx, auth.OwnerID)
	if err != nil {
		return nil, err
	}
	for _, l := range links {
		st.Owners = append(st.Owners, l.Channel+":"+l.Identity)
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
	approvals, err := s.inspector.pendingApprovals(ctx)
	if err != nil {
		return nil, err
	}
	st.Approvals = len(approvals)
	if st.MCPServers, st.Skills, err = ProjectStatus(s.dir, cfg, s.secrets); err != nil {
		st.ProjectError = err.Error()
	}
	st.Backup = s.backups.get()
	return st, nil
}

func stateName(s a2a.TaskState) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(string(s), "TASK_STATE_")), "_", "-")
}

func (s *Server) ctlPair(w http.ResponseWriter, r *http.Request) {
	var req controlapi.PairCodeRequest
	if !jsonhttp.Read(w, r, &req) {
		return
	}
	resp, err := s.pairings.Invite(r.Context(), req)
	if err != nil {
		writePairingError(w, err)
		return
	}
	jsonhttp.Write(w, http.StatusOK, resp)
}

func (s *Server) ctlPairings(w http.ResponseWriter, r *http.Request) {
	out, err := s.pairings.List(r.Context())
	if err != nil {
		writePairingError(w, err)
		return
	}
	jsonhttp.Write(w, http.StatusOK, out)
}

func (s *Server) ctlPairing(w http.ResponseWriter, r *http.Request) {
	p, err := s.pairings.Get(r.Context(), r.PathValue("name"))
	if err != nil {
		writePairingError(w, err)
		return
	}
	jsonhttp.Write(w, http.StatusOK, p)
}

func (s *Server) ctlUpdatePairing(w http.ResponseWriter, r *http.Request) {
	var req controlapi.PairingUpdate
	if !jsonhttp.Read(w, r, &req) {
		return
	}
	resp, err := s.pairings.Update(r.Context(), r.PathValue("name"), req)
	if err != nil {
		writePairingError(w, err)
		return
	}
	jsonhttp.Write(w, http.StatusOK, resp)
}

// ctlRevoke removes the webhook node called name, or revokes the tokens,
// links and codes issued to it.
func (s *Server) ctlRevoke(w http.ResponseWriter, r *http.Request) {
	kind, err := s.pairings.Remove(r.Context(), r.PathValue("name"))
	if err != nil {
		writePairingError(w, err)
		return
	}
	jsonhttp.Write(w, http.StatusOK, controlapi.RevokeResponse{Kind: kind})
}

func writePairingError(w http.ResponseWriter, err error) {
	var pe *pairings.Error
	if !errors.As(err, &pe) {
		jsonhttp.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	status := http.StatusUnprocessableEntity
	switch pe.Kind {
	case pairings.Conflict:
		status = http.StatusConflict
	case pairings.NotFound:
		status = http.StatusNotFound
	}
	jsonhttp.Error(w, status, pe.Msg)
}

func (s *Server) ctlPeers(w http.ResponseWriter, r *http.Request) {
	peers, err := s.peers.discover(r.Context())
	if err != nil {
		jsonhttp.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	out := make([]controlapi.Peer, len(peers))
	for i, p := range peers {
		out[i] = controlapi.Peer{Name: p.Name, URL: p.URL, Description: p.Description, Online: p.Online}
	}
	jsonhttp.Write(w, http.StatusOK, out)
}

// contacts manages the contacts, refreshing the harness instructions
// which list them.
func (s *Server) contacts() manage.Contacts {
	m := s.peers.contacts()
	m.Changed = s.refreshInstructions
	return m
}

func (s *Server) ctlContacts(w http.ResponseWriter, r *http.Request) {
	out, err := s.contacts().List(r.Context())
	if err != nil {
		jsonhttp.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonhttp.Write(w, http.StatusOK, out)
}

func (s *Server) ctlAddContact(w http.ResponseWriter, r *http.Request) {
	var req controlapi.ContactRequest
	if !jsonhttp.Read(w, r, &req) {
		return
	}
	c, err := s.contacts().Add(r.Context(), req)
	if err != nil {
		jsonhttp.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	jsonhttp.Write(w, http.StatusOK, manage.ContactInfo(c))
}

func (s *Server) ctlDeleteContact(w http.ResponseWriter, r *http.Request) {
	err := s.contacts().Remove(r.Context(), r.PathValue("name"))
	if errors.Is(err, store.ErrNotFound) {
		jsonhttp.Error(w, http.StatusNotFound, "no contact "+r.PathValue("name"))
		return
	}
	if err != nil {
		jsonhttp.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonhttp.Write(w, http.StatusOK, map[string]string{"deleted": r.PathValue("name")})
}

func (s *Server) ctlAddChannel(w http.ResponseWriter, r *http.Request) {
	var req controlapi.ChannelRequest
	if !jsonhttp.Read(w, r, &req) {
		return
	}
	secrets, err := s.dir.LoadSecrets()
	if err != nil {
		jsonhttp.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	maps.Copy(secrets, req.Secrets)
	next := s.config()
	if err := next.SetChannel(req.Kind, req.Token, req.Email); err != nil {
		jsonhttp.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	ch, err := s.buildChannel(next, secrets, req.Kind)
	if err != nil {
		jsonhttp.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateChannel(r.Context(), ch); err != nil {
		jsonhttp.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.dir.SaveSecrets(req.Secrets); err != nil {
		jsonhttp.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.cfgMu.Lock()
	s.secrets = secrets
	s.cfgMu.Unlock()
	if err := s.updateConfig(func(c *agentdir.Config) error { return c.SetChannel(req.Kind, req.Token, req.Email) }); err != nil {
		jsonhttp.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.router.add(ch)
	jsonhttp.Write(w, http.StatusOK, s.router.channelStatus(ch, s.router.ownerHint(r.Context(), ch)))
}

func (s *Server) ctlDeleteChannel(w http.ResponseWriter, r *http.Request) {
	kind := r.PathValue("kind")
	if err := (&agentdir.Config{}).SetChannel(kind, "", nil); err != nil {
		jsonhttp.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if !s.config().HasChannel(kind) {
		jsonhttp.Error(w, http.StatusNotFound, kind+" is not configured")
		return
	}
	if err := s.updateConfig(func(c *agentdir.Config) error { return c.SetChannel(kind, "", nil) }); err != nil {
		jsonhttp.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.router.remove(kind)
	jsonhttp.Write(w, http.StatusOK, map[string]string{"removed": kind})
}

// ctlReloadConfig applies the live settings of config.yaml, which the CLI
// has validated and saved.
func (s *Server) ctlReloadConfig(w http.ResponseWriter, r *http.Request) {
	saved, err := s.dir.LoadConfig()
	if err != nil {
		jsonhttp.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	s.cfgMu.Lock()
	agentdir.CopyLive(s.cfg, saved)
	s.cfgMu.Unlock()
	jsonhttp.Write(w, http.StatusOK, controlapi.ReloadResponse{Applied: []string{"description", "allow-tools", "deny-tools", "owner-chat"}})
}

func (s *Server) ctlSchedules(w http.ResponseWriter, r *http.Request) {
	all, err := s.store.Schedules(r.Context())
	if err != nil {
		jsonhttp.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := []controlapi.ScheduleInfo{}
	for _, sc := range all {
		out = append(out, manage.ScheduleInfo(sc))
	}
	jsonhttp.Write(w, http.StatusOK, out)
}

func (s *Server) ctlAddSchedule(w http.ResponseWriter, r *http.Request) {
	var req schedule.Request
	if !jsonhttp.Read(w, r, &req) {
		return
	}
	sc, err := s.schedules.add(r.Context(), req, auth.OwnerID, "")
	if err != nil {
		jsonhttp.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	jsonhttp.Write(w, http.StatusOK, manage.ScheduleInfo(sc))
}

func (s *Server) ctlRunSchedule(w http.ResponseWriter, r *http.Request) {
	sc, err := s.schedules.runNow(r.Context(), r.PathValue("name"))
	switch {
	case errors.Is(err, store.ErrNotFound):
		jsonhttp.Error(w, http.StatusNotFound, "no schedule "+r.PathValue("name"))
	case err != nil:
		jsonhttp.Error(w, http.StatusBadRequest, err.Error())
	default:
		jsonhttp.Write(w, http.StatusOK, manage.ScheduleInfo(sc))
	}
}

func (s *Server) ctlDeleteSchedule(w http.ResponseWriter, r *http.Request) {
	err := s.schedules.remove(r.Context(), r.PathValue("name"), auth.Owner())
	switch {
	case errors.Is(err, store.ErrNotFound):
		jsonhttp.Error(w, http.StatusNotFound, "no schedule "+r.PathValue("name"))
	case err != nil:
		jsonhttp.Error(w, http.StatusInternalServerError, err.Error())
	default:
		jsonhttp.Write(w, http.StatusOK, map[string]string{"deleted": r.PathValue("name")})
	}
}
