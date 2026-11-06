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

// Package meshtest fakes the Agent Mesh admin API for tests.
package meshtest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-serve/internal/relay/meshadmin"
)

// Issued is when the fake mints bootstrap tokens.
var Issued = time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)

// Mesh is a fake agentmesh-one admin API.
type Mesh struct {
	URL string

	token    string
	mu       sync.Mutex
	nodes    []meshadmin.Node
	cat      map[string]meshadmin.NodeServices
	tokens   []token
	enrolled []meshadmin.Enrollment
	policies []meshadmin.Policy
}

type token struct {
	req  meshadmin.BootstrapTokenRequest
	info meshadmin.TokenInfo
}

// New starts a fake that accepts adminToken.
func New(t *testing.T, adminToken string) *Mesh {
	t.Helper()
	m := &Mesh{token: adminToken, cat: map[string]meshadmin.NodeServices{}}
	srv := httptest.NewServer(m.handler())
	t.Cleanup(srv.Close)
	m.URL = srv.URL
	return m
}

// AddNode enrolls a node that serves agents.
func (m *Mesh) AddNode(n meshadmin.Node, reported time.Time, agents ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nodes = append(m.nodes, n)
	var svc []meshadmin.Service
	for _, a := range agents {
		svc = append(svc, meshadmin.Service{Type: meshadmin.ServiceTypeA2A, Name: a})
	}
	if len(svc) > 0 {
		m.cat[n.PeerID] = meshadmin.NodeServices{Services: svc, Reported: reported}
	}
}

// Enroll enrolls a node with the bootstrap token id, which spends a use of
// it, like [Mesh.AddNode].
func (m *Mesh) Enroll(id string, n meshadmin.Node, reported time.Time, agents ...string) {
	m.mu.Lock()
	for i := range m.tokens {
		if m.tokens[i].info.ID == id {
			m.tokens[i].info.UsagesCount++
		}
	}
	m.enrolled = append(m.enrolled, meshadmin.Enrollment{PeerID: n.PeerID, TokenID: id, Status: meshadmin.EnrollmentApproved})
	m.mu.Unlock()
	m.AddNode(n, reported, agents...)
}

// ForgetEnrollments drops the enrollments, as Agent Mesh does some time
// after it resolves them.
func (m *Mesh) ForgetEnrollments() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.enrolled = nil
}

// Tokens returns the bootstrap tokens requested.
func (m *Mesh) Tokens() []meshadmin.BootstrapTokenRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]meshadmin.BootstrapTokenRequest, 0, len(m.tokens))
	for _, t := range m.tokens {
		out = append(out, t.req)
	}
	return out
}

// Revoked returns the IDs of the revoked bootstrap tokens.
func (m *Mesh) Revoked() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, t := range m.tokens {
		if !t.info.Revoked.IsZero() {
			out = append(out, t.info.ID)
		}
	}
	return out
}

// Policies returns the policies set.
func (m *Mesh) Policies() []meshadmin.Policy {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]meshadmin.Policy(nil), m.policies...)
}

// Banned returns the peer IDs of the banned nodes.
func (m *Mesh) Banned() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, n := range m.nodes {
		if n.Banned {
			out = append(out, n.PeerID)
		}
	}
	return out
}

func (m *Mesh) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("POST /policies", func(w http.ResponseWriter, r *http.Request) {
		var p meshadmin.Policy
		if !m.read(w, r, &p) {
			return
		}
		m.mu.Lock()
		m.policies = append(m.policies, p)
		m.mu.Unlock()
	})
	mux.HandleFunc("POST /admin/bootstrap-tokens", func(w http.ResponseWriter, r *http.Request) {
		var req meshadmin.BootstrapTokenRequest
		if !m.read(w, r, &req) {
			return
		}
		m.mu.Lock()
		n := len(m.tokens) + 1
		info := meshadmin.TokenInfo{
			ID: fmt.Sprint(n), MaxUsages: req.MaxUsages, Description: req.Description,
			Created: Issued, Expires: Issued.Add(time.Duration(req.TTLHours) * time.Hour),
		}
		m.tokens = append(m.tokens, token{req: req, info: info})
		m.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		write(w, map[string]any{
			"id":          info.ID,
			"token":       fmt.Sprintf("mesh-bt-%d", n),
			"expire_time": info.Expires,
		})
	})
	mux.HandleFunc("GET /admin/bootstrap-tokens", func(w http.ResponseWriter, r *http.Request) {
		if !m.auth(w, r) {
			return
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		out := []meshadmin.TokenInfo{}
		for _, t := range m.tokens {
			out = append(out, t.info)
		}
		write(w, map[string]any{"tokens": out})
	})
	mux.HandleFunc("DELETE /admin/bootstrap-tokens/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !m.auth(w, r) {
			return
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		for i := range m.tokens {
			if m.tokens[i].info.ID == r.PathValue("id") {
				m.tokens[i].info.Revoked = Issued
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		http.Error(w, "Bootstrap token not found", http.StatusNotFound)
	})
	mux.HandleFunc("GET /admin/enrollments", func(w http.ResponseWriter, r *http.Request) {
		if !m.auth(w, r) {
			return
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		write(w, map[string]any{"requests": append([]meshadmin.Enrollment{}, m.enrolled...)})
	})
	mux.HandleFunc("GET /admin/status", func(w http.ResponseWriter, r *http.Request) {
		if !m.auth(w, r) {
			return
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		// Agent Mesh forgets what a banned node serves.
		cat := map[string]meshadmin.NodeServices{}
		for _, n := range m.nodes {
			if svc, ok := m.cat[n.PeerID]; ok && !n.Banned {
				cat[n.PeerID] = svc
			}
		}
		write(w, meshadmin.Status{Nodes: m.nodes, Catalog: cat})
	})
	mux.HandleFunc("POST /admin/revoke", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			PeerID string `json:"peer_id"`
		}
		if !m.read(w, r, &req) {
			return
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		for i := range m.nodes {
			if m.nodes[i].PeerID == req.PeerID {
				m.nodes[i].Banned = true
				write(w, map[string]bool{"success": true})
				return
			}
		}
		http.Error(w, "Node not found", http.StatusNotFound)
	})
	mux.HandleFunc("POST /admin/nodes/{peer}/unban", func(w http.ResponseWriter, r *http.Request) {
		if !m.auth(w, r) {
			return
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		for i := range m.nodes {
			if m.nodes[i].PeerID == r.PathValue("peer") {
				m.nodes[i].Banned = false
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		http.Error(w, "Node not found", http.StatusNotFound)
	})
	return mux
}

func (m *Mesh) auth(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Authorization") != "Bearer "+m.token {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}

func (m *Mesh) read(w http.ResponseWriter, r *http.Request, v any) bool {
	if !m.auth(w, r) {
		return false
	}
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func write(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		return
	}
}
