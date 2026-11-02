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
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
	"github.com/a2aproject/a2a-serve/internal/jsonhttp"
	"github.com/a2aproject/a2a-serve/internal/network"
	"github.com/a2aproject/a2a-serve/internal/pairapi"
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
		return errors.Join(fmt.Errorf("an agent server is already running in %s (a2a serve status)", s.dir.Root), c.Close())
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
	mux.HandleFunc("GET /tokens", s.ctlTokens)
	mux.HandleFunc("DELETE /pair/{name}", s.ctlRevoke)
	mux.HandleFunc("GET /peers", s.ctlPeers)
	mux.HandleFunc("GET /contacts", s.ctlContacts)
	mux.HandleFunc("POST /contacts", s.ctlAddContact)
	mux.HandleFunc("DELETE /contacts/{name}", s.ctlDeleteContact)
	mux.HandleFunc("POST /channels", s.ctlAddChannel)
	mux.HandleFunc("DELETE /channels/{kind}", s.ctlDeleteChannel)
	mux.HandleFunc("GET /nodes", s.ctlNodes)
	mux.HandleFunc("GET /schedules", s.ctlSchedules)
	mux.HandleFunc("POST /schedules", s.ctlAddSchedule)
	mux.HandleFunc("POST /schedules/{name}/run", s.ctlRunSchedule)
	mux.HandleFunc("DELETE /schedules/{name}", s.ctlDeleteSchedule)
	mux.HandleFunc("GET /advertisements", s.ctlAds)
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
	cfg := s.config()
	local, private, public := s.URLs()
	st := controlapi.Status{
		Name: cfg.Name, Harness: cfg.Harness, Dir: s.dir.Root, PID: pid(), Started: s.started,
		Local: local, Private: private, Public: public, Tasks: map[string]int{},
	}
	if u, err := url.Parse(private); err == nil && u.Host == network.AgentMeshHost {
		st.PrivateNote = "works only from other a2a-serve agents on the relay; give people the public URL"
	}
	st.Channels = s.router.status()
	counts, err := s.tasks.TaskCounts(r.Context())
	if err != nil {
		jsonhttp.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	for state, n := range counts {
		st.Tasks[stateName(state)] = n
	}
	links, err := s.store.LinksOf(r.Context(), auth.OwnerID)
	if err != nil {
		jsonhttp.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, l := range links {
		st.Owners = append(st.Owners, l.Channel+":"+l.Identity)
	}
	contacts, err := s.store.Contacts(r.Context())
	if err != nil {
		jsonhttp.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	nodes, err := s.store.Nodes(r.Context())
	if err != nil {
		jsonhttp.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	st.Contacts, st.Nodes = len(contacts), len(nodes)
	if st.MCPServers, st.Skills, err = ProjectStatus(s.dir, cfg, s.secrets); err != nil {
		st.ProjectError = err.Error()
	}
	st.Backup = s.backups.get()
	jsonhttp.Write(w, http.StatusOK, st)
}

func stateName(s a2a.TaskState) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(string(s), "TASK_STATE_")), "_", "-")
}

func (s *Server) ctlPair(w http.ResponseWriter, r *http.Request) {
	var req controlapi.PairCodeRequest
	if !jsonhttp.Read(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || strings.ContainsAny(req.Name, " /:") {
		jsonhttp.Error(w, http.StatusBadRequest, "the name must be non-empty, without spaces, slashes or colons")
		return
	}
	if req.Name == localOwnerName {
		jsonhttp.Error(w, http.StatusBadRequest, fmt.Sprintf("%q is reserved for the owner token of this machine; pick another name", localOwnerName))
		return
	}
	_, err := s.store.Node(r.Context(), req.Name)
	isNode := err == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		jsonhttp.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	if req.Webhook != "" && req.Name == schedulesAgent {
		jsonhttp.Error(w, http.StatusBadRequest, fmt.Sprintf("%q is the agent the harness manages schedules with; pick another name", schedulesAgent))
		return
	}
	if req.Advertised != nil && req.Webhook == "" && !isNode {
		jsonhttp.Error(w, http.StatusBadRequest, "--advertised applies to webhook nodes; add --webhook")
		return
	}
	if (req.Webhook != "" || isNode) && req.Role != "" {
		jsonhttp.Error(w, http.StatusBadRequest, "--role doesn't apply to webhook nodes")
		return
	}
	if req.Webhook != "" || isNode {
		s.pairNode(w, r, req, isNode)
		return
	}
	if req.Role == "" {
		req.Role = auth.RoleMember
	}
	if req.Role != auth.RoleMember && req.Role != auth.RoleOwner {
		jsonhttp.Error(w, http.StatusBadRequest, "--role must be member or owner")
		return
	}
	code, expires, err := auth.MintCode(r.Context(), s.store, auth.PurposePair, req.Name, req.Role)
	if err != nil {
		jsonhttp.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp := controlapi.PairCodeResponse{Code: code, Expires: expires, URL: s.bestURL(), PairCommand: controlapi.PairCommand(s.bestURL(), code), Hints: s.router.linkHints(code)}
	jsonhttp.Write(w, http.StatusOK, resp)
}

// pairNode creates or updates a webhook node and mints a node code for it.
func (s *Server) pairNode(w http.ResponseWriter, r *http.Request, req controlapi.PairCodeRequest, exists bool) {
	if req.Webhook != "" && !exists {
		toks, err := s.store.Tokens(r.Context())
		if err != nil {
			jsonhttp.Error(w, http.StatusInternalServerError, err.Error())
			return
		}
		if slices.ContainsFunc(toks, func(t store.Token) bool { return t.Name == req.Name }) {
			jsonhttp.Error(w, http.StatusConflict, req.Name+" is already paired as a caller; pick another name, or run `a2a serve pair rm "+req.Name+"` first")
			return
		}
	}
	var (
		resp *controlapi.PairCodeResponse
		err  error
	)
	if req.Webhook != "" {
		resp, err = s.nodes.Put(r.Context(), req)
	} else {
		resp, err = s.nodes.NewCode(r.Context(), req.Name)
	}
	if err != nil {
		jsonhttp.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Advertised != nil {
		node, err := s.store.Node(r.Context(), req.Name)
		if err == nil {
			err = s.setAdvertised(node.ID, node.Description, *req.Advertised)
		}
		if err != nil {
			jsonhttp.Error(w, http.StatusInternalServerError, err.Error())
			return
		}
		resp.Advertised = *req.Advertised
	}
	if !exists {
		s.refreshInstructions(r.Context())
	}
	jsonhttp.Write(w, http.StatusOK, resp)
}

func (s *Server) ctlTokens(w http.ResponseWriter, r *http.Request) {
	toks, err := s.store.Tokens(r.Context())
	if err != nil {
		jsonhttp.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := []controlapi.TokenInfo{}
	for _, t := range toks {
		out = append(out, controlapi.TokenInfo{Name: t.Name, Principal: t.Principal, Roles: t.Roles, URL: t.URL, Created: t.Created, LastUsed: t.LastUsed})
	}
	jsonhttp.Write(w, http.StatusOK, out)
}

// ctlRevoke removes the webhook node called name, or revokes the tokens and
// links issued to it.
func (s *Server) ctlRevoke(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	err := s.store.DeleteNode(r.Context(), name)
	if err == nil {
		if err := s.ads.Withdraw(name); err != nil {
			slog.Warn("withdrawing the advertisement of a removed node", "node", name, "error", err)
		}
		s.refreshInstructions(r.Context())
		jsonhttp.Write(w, http.StatusOK, controlapi.RevokeResponse{Kind: pairapi.KindNode})
		return
	}
	if !errors.Is(err, store.ErrNotFound) {
		jsonhttp.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	n, err := s.store.RevokeTokens(r.Context(), name, auth.PrincipalForCode(name, auth.RoleMember))
	if err != nil {
		jsonhttp.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	if n == 0 {
		jsonhttp.Error(w, http.StatusNotFound, "no token or link named "+name)
		return
	}
	jsonhttp.Write(w, http.StatusOK, controlapi.RevokeResponse{Kind: pairapi.KindCaller})
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

func (s *Server) ctlContacts(w http.ResponseWriter, r *http.Request) {
	cs, err := s.store.Contacts(r.Context())
	if err != nil {
		jsonhttp.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := []controlapi.ContactInfo{}
	for _, c := range cs {
		out = append(out, controlapi.ContactInfo{Name: c.Name, URL: c.URL, Via: c.Via, HasToken: c.Token != ""})
	}
	jsonhttp.Write(w, http.StatusOK, out)
}

func (s *Server) ctlAddContact(w http.ResponseWriter, r *http.Request) {
	var req controlapi.ContactRequest
	if !jsonhttp.Read(w, r, &req) {
		return
	}
	c, err := s.peers.addContact(r.Context(), req)
	if err != nil {
		jsonhttp.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	s.refreshInstructions(r.Context())
	jsonhttp.Write(w, http.StatusOK, controlapi.ContactInfo{Name: c.Name, URL: c.URL, Via: c.Via, HasToken: c.Token != ""})
}

func (s *Server) ctlDeleteContact(w http.ResponseWriter, r *http.Request) {
	err := s.store.DeleteContact(r.Context(), r.PathValue("name"))
	if errors.Is(err, store.ErrNotFound) {
		jsonhttp.Error(w, http.StatusNotFound, "no contact "+r.PathValue("name"))
		return
	}
	if err != nil {
		jsonhttp.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.refreshInstructions(r.Context())
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
	jsonhttp.Write(w, http.StatusOK, controlapi.ReloadResponse{Applied: []string{"description", "approver", "allow-tools", "deny-tools"}})
}

func (s *Server) ctlNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.store.Nodes(r.Context())
	if err != nil {
		jsonhttp.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := []controlapi.NodeInfo{}
	for _, n := range nodes {
		out = append(out, s.nodes.Info(n))
	}
	jsonhttp.Write(w, http.StatusOK, out)
}

func (s *Server) ctlSchedules(w http.ResponseWriter, r *http.Request) {
	all, err := s.store.Schedules(r.Context())
	if err != nil {
		jsonhttp.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := []controlapi.ScheduleInfo{}
	for _, sc := range all {
		out = append(out, scheduleInfo(sc))
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
	jsonhttp.Write(w, http.StatusOK, scheduleInfo(sc))
}

func (s *Server) ctlRunSchedule(w http.ResponseWriter, r *http.Request) {
	sc, err := s.schedules.runNow(r.Context(), r.PathValue("name"))
	switch {
	case errors.Is(err, store.ErrNotFound):
		jsonhttp.Error(w, http.StatusNotFound, "no schedule "+r.PathValue("name"))
	case err != nil:
		jsonhttp.Error(w, http.StatusBadRequest, err.Error())
	default:
		jsonhttp.Write(w, http.StatusOK, scheduleInfo(sc))
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
