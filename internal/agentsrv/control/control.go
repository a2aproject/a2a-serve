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

// Package control serves the control socket's API, through which
// management commands change the running server without a restart.
package control

import (
	"context"
	"net/http"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/gossip"
	"github.com/a2aproject/a2a-serve/internal/jsonhttp"
	"github.com/a2aproject/a2a-serve/internal/manage"
	"github.com/a2aproject/a2a-serve/internal/network"
	"github.com/a2aproject/a2a-serve/internal/pairings"
	"github.com/a2aproject/a2a-serve/internal/schedule"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// Deps are what the control API reads and changes. Only the owner reaches
// the control socket, so changes are made as the owner.
type Deps struct {
	// Status describes the running server.
	Status func(ctx context.Context) (*controlapi.Status, error)
	// Stop shuts the server down.
	Stop func()
	// Config returns the config the server runs with.
	Config func() *agentdir.Config
	// ReloadConfig applies the live settings of config.yaml, which the CLI
	// has validated and saved.
	ReloadConfig func() error
	Pairings     *pairings.Service
	// Peers lists the agents on the private network.
	Peers func(ctx context.Context) ([]network.PeerInfo, error)
	// Contacts manages the contacts.
	Contacts func() manage.Contacts
	// AddChannel checks a channel, saves it and starts it.
	AddChannel func(ctx context.Context, req controlapi.ChannelRequest) (*controlapi.ChannelStatus, error)
	// RemoveChannel stops a channel and removes it from the config.
	RemoveChannel func(kind string) error
	Store         *store.Store
	// AddSchedule creates a schedule made by by.
	AddSchedule func(ctx context.Context, req schedule.Request, by auth.Principal) (store.Schedule, error)
	// RunSchedule starts a run of a schedule at once.
	RunSchedule func(ctx context.Context, name string) (store.Schedule, error)
	// RemoveSchedule removes a schedule as by.
	RemoveSchedule func(ctx context.Context, name string, by auth.Principal) error
	// Ads are the advertised pairings of the private network.
	Ads *gossip.Table
	// InspectorLogin returns the path which logs in to the inspector once.
	InspectorLogin func() string
}

// applied are the settings which [Deps.ReloadConfig] applies.
var applied = []string{"description", "allow-tools", "deny-tools", "owner-chat", "private-access", "public-access", "allow-email"}

type api struct{ deps Deps }

// Handler returns the handler of the control API.
func Handler(deps Deps) http.Handler {
	a := &api{deps: deps}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", a.status)
	mux.HandleFunc("POST /stop", a.stop)
	mux.HandleFunc("POST /pair", a.pair)
	mux.HandleFunc("GET /pairings", a.pairings)
	mux.HandleFunc("GET /pairings/{name}", a.pairing)
	mux.HandleFunc("PATCH /pairings/{name}", a.updatePairing)
	mux.HandleFunc("DELETE /pair/{name}", a.revoke)
	mux.HandleFunc("GET /pair/{name}/removal", a.previewRevoke)
	mux.HandleFunc("DELETE /signins/{email}", a.signOut)
	mux.HandleFunc("GET /peers", a.peers)
	mux.HandleFunc("GET /contacts", a.contacts)
	mux.HandleFunc("POST /contacts", a.addContact)
	mux.HandleFunc("DELETE /contacts/{name}", a.deleteContact)
	mux.HandleFunc("POST /channels", a.addChannel)
	mux.HandleFunc("DELETE /channels/{kind}", a.deleteChannel)
	mux.HandleFunc("GET /schedules", a.schedules)
	mux.HandleFunc("POST /schedules", a.addSchedule)
	mux.HandleFunc("POST /schedules/{name}/run", a.runSchedule)
	mux.HandleFunc("DELETE /schedules/{name}", a.deleteSchedule)
	mux.HandleFunc("GET /advertisements", a.ads)
	mux.HandleFunc("POST /inspector/login", a.inspectorLogin)
	mux.HandleFunc("GET /config", a.config)
	mux.HandleFunc("POST /config/reload", a.reloadConfig)
	return mux
}

// reply writes v, or the error which produced it.
func reply(w http.ResponseWriter, v any, err error) {
	if err != nil {
		controlapi.WriteError(w, err)
		return
	}
	jsonhttp.Write(w, http.StatusOK, v)
}

func (a *api) status(w http.ResponseWriter, r *http.Request) {
	st, err := a.deps.Status(r.Context())
	reply(w, st, err)
}

func (a *api) stop(w http.ResponseWriter, _ *http.Request) {
	jsonhttp.Write(w, http.StatusOK, map[string]bool{"stopping": true})
	go a.deps.Stop()
}

func (a *api) pair(w http.ResponseWriter, r *http.Request) {
	var req controlapi.PairCodeRequest
	if !jsonhttp.Read(w, r, &req) {
		return
	}
	resp, err := a.deps.Pairings.Invite(r.Context(), req)
	reply(w, resp, err)
}

func (a *api) pairings(w http.ResponseWriter, r *http.Request) {
	out, err := a.deps.Pairings.List(r.Context())
	reply(w, out, err)
}

func (a *api) pairing(w http.ResponseWriter, r *http.Request) {
	p, err := a.deps.Pairings.Get(r.Context(), r.PathValue("name"))
	reply(w, p, err)
}

func (a *api) updatePairing(w http.ResponseWriter, r *http.Request) {
	var req controlapi.PairingUpdate
	if !jsonhttp.Read(w, r, &req) {
		return
	}
	resp, err := a.deps.Pairings.Update(r.Context(), r.PathValue("name"), req)
	reply(w, resp, err)
}

// revoke removes the pairing called name with its tokens, links and codes.
func (a *api) revoke(w http.ResponseWriter, r *http.Request) {
	resp, err := a.deps.Pairings.Remove(r.Context(), r.PathValue("name"))
	reply(w, resp, err)
}

// previewRevoke describes what revoke would remove.
func (a *api) previewRevoke(w http.ResponseWriter, r *http.Request) {
	resp, err := a.deps.Pairings.PreviewRemove(r.Context(), r.PathValue("name"))
	reply(w, resp, err)
}

// signOut revokes the Google sign-in tokens of an email.
func (a *api) signOut(w http.ResponseWriter, r *http.Request) {
	resp, err := a.deps.Pairings.SignOut(r.Context(), r.PathValue("email"))
	reply(w, resp, err)
}

func (a *api) peers(w http.ResponseWriter, r *http.Request) {
	peers, err := a.deps.Peers(r.Context())
	if err != nil {
		controlapi.WriteError(w, err)
		return
	}
	out := make([]controlapi.Peer, len(peers))
	for i, p := range peers {
		out[i] = controlapi.Peer{Name: p.Name, URL: p.URL, Description: p.Description, Online: p.Online}
	}
	jsonhttp.Write(w, http.StatusOK, out)
}

func (a *api) contacts(w http.ResponseWriter, r *http.Request) {
	out, err := a.deps.Contacts().List(r.Context())
	reply(w, out, err)
}

func (a *api) addContact(w http.ResponseWriter, r *http.Request) {
	var req controlapi.ContactRequest
	if !jsonhttp.Read(w, r, &req) {
		return
	}
	c, err := a.deps.Contacts().Add(r.Context(), req)
	if err != nil {
		controlapi.WriteError(w, err)
		return
	}
	jsonhttp.Write(w, http.StatusOK, manage.ContactInfo(c))
}

func (a *api) deleteContact(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	err := a.deps.Contacts().Remove(r.Context(), name)
	reply(w, map[string]string{"deleted": name}, err)
}

func (a *api) addChannel(w http.ResponseWriter, r *http.Request) {
	var req controlapi.ChannelRequest
	if !jsonhttp.Read(w, r, &req) {
		return
	}
	st, err := a.deps.AddChannel(r.Context(), req)
	reply(w, st, err)
}

func (a *api) deleteChannel(w http.ResponseWriter, r *http.Request) {
	kind := r.PathValue("kind")
	err := a.deps.RemoveChannel(kind)
	reply(w, map[string]string{"removed": kind}, err)
}

func (a *api) schedules(w http.ResponseWriter, r *http.Request) {
	all, err := a.deps.Store.Schedules(r.Context())
	if err != nil {
		controlapi.WriteError(w, err)
		return
	}
	out := []controlapi.ScheduleInfo{}
	for _, sc := range all {
		out = append(out, manage.ScheduleInfo(sc))
	}
	jsonhttp.Write(w, http.StatusOK, out)
}

func (a *api) addSchedule(w http.ResponseWriter, r *http.Request) {
	var req schedule.Request
	if !jsonhttp.Read(w, r, &req) {
		return
	}
	sc, err := a.deps.AddSchedule(r.Context(), req, auth.Owner())
	if err != nil {
		controlapi.WriteError(w, err)
		return
	}
	jsonhttp.Write(w, http.StatusOK, manage.ScheduleInfo(sc))
}

func (a *api) runSchedule(w http.ResponseWriter, r *http.Request) {
	sc, err := a.deps.RunSchedule(r.Context(), r.PathValue("name"))
	if err != nil {
		controlapi.WriteError(w, err)
		return
	}
	jsonhttp.Write(w, http.StatusOK, manage.ScheduleInfo(sc))
}

func (a *api) deleteSchedule(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	err := a.deps.RemoveSchedule(r.Context(), name, auth.Owner())
	reply(w, map[string]string{"deleted": name}, err)
}

func (a *api) ads(w http.ResponseWriter, _ *http.Request) {
	out := []controlapi.Advertisement{}
	for _, e := range a.deps.Ads.Live() {
		out = append(out, controlapi.Advertisement{Key: e.Key(), Name: e.Name, Host: e.Host, Description: e.Description, Expires: e.Expires})
	}
	jsonhttp.Write(w, http.StatusOK, out)
}

func (a *api) inspectorLogin(w http.ResponseWriter, _ *http.Request) {
	jsonhttp.Write(w, http.StatusOK, controlapi.InspectorLogin{Path: a.deps.InspectorLogin()})
}

func (a *api) config(w http.ResponseWriter, _ *http.Request) {
	jsonhttp.Write(w, http.StatusOK, a.deps.Config())
}

func (a *api) reloadConfig(w http.ResponseWriter, _ *http.Request) {
	err := a.deps.ReloadConfig()
	reply(w, controlapi.ReloadResponse{Applied: applied}, err)
}
