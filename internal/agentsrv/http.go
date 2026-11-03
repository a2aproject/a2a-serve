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
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aext/structuredinput"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/a2aproject/a2a-serve/internal/a2acli"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/jsonhttp"
	"github.com/a2aproject/a2a-serve/internal/mailboxapi"
	"github.com/a2aproject/a2a-serve/internal/network"
	"github.com/a2aproject/a2a-serve/internal/output"
	"github.com/a2aproject/a2a-serve/internal/pairapi"
	"github.com/a2aproject/a2a-serve/internal/store"
	"github.com/a2aproject/a2a-serve/skills"
)

// routes returns the HTTP handler of one listener. baseURL is the URL the
// listener is reached at; empty derives it from the request.
func (s *Server) routes(kind, baseURL string) http.Handler {
	mux := http.NewServeMux()
	if kind == network.ListenerHarness {
		return s.peers.harnessRoutes(s.bridge.key)
	}
	card := func(w http.ResponseWriter, r *http.Request) {
		a2asrv.NewStaticAgentCardHandler(s.card(kind, base(baseURL, r))).ServeHTTP(w, r)
	}
	jsonrpc := a2asrv.NewJSONRPCHandler(s.handler, a2asrv.WithTransportKeepAlive(15*time.Second))
	rest := a2asrv.NewRESTHandler(s.handler, a2asrv.WithTransportKeepAlive(15*time.Second))

	mux.HandleFunc("GET /{$}", card)
	mux.HandleFunc("GET "+a2asrv.WellKnownAgentCardPath, card)
	mux.HandleFunc("GET /.well-known/agent.json", card)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		jsonhttp.Write(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /skill.md", handleCLISkill)
	nodeSkill := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		writeBody(w, strings.ReplaceAll(skills.A2AMailbox, "{{BASE}}", base(baseURL, r)))
	}
	mux.HandleFunc("GET "+mailboxapi.Prefix+"/skill.md", nodeSkill)
	mux.HandleFunc("GET "+mailboxapi.LegacyPrefix+"/skill.md", nodeSkill)
	mux.HandleFunc("GET /download/a2a", handleDownloadCLI)
	mux.HandleFunc("GET /download/a2a-serve", handleDownloadServe)
	mux.HandleFunc("POST "+pairapi.Path, func(w http.ResponseWriter, r *http.Request) { s.handlePair(w, r, base(baseURL, r)) })
	mux.HandleFunc("/hitl/{token}", s.approvals.HandlePage)
	s.oauth.register(mux, func(r *http.Request) string { return base(baseURL, r) })
	if kind == network.ListenerPrivate {
		s.registerGossip(mux)
	}
	// The inspector is for the owner, who reaches it on this machine or the
	// private network, not from the internet.
	if kind == network.ListenerLocal || kind == network.ListenerPrivate {
		s.inspector.register(mux)
	}
	s.nodes.Register(mux, kind, func(r *http.Request) string { return base(baseURL, r) })
	mux.Handle("POST /{$}", s.authenticate(kind, jsonrpc))
	mux.Handle("/", s.authenticate(kind, rest))
	return mux
}

// base returns the configured base URL or derives it from the request.
func base(baseURL string, r *http.Request) string {
	if baseURL != "" {
		return baseURL
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// listenerURL returns the URL of a listener kind, or "" if it has none.
func (s *Server) listenerURL(kind string) string {
	local, private, public := s.URLs()
	switch kind {
	case network.ListenerLocal:
		return local
	case network.ListenerPrivate:
		return private
	case network.ListenerPublic:
		return public
	}
	return ""
}

// authenticate resolves the principal of a request with the auth chain.
func (s *Server) authenticate(kind string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok, err := s.chain.Authenticate(r.Context(), auth.Request{Listener: kind, RemoteAddr: r.RemoteAddr, Header: r.Header})
		if err != nil {
			slog.Error("authentication failed", "error", err)
			jsonhttp.Error(w, http.StatusInternalServerError, "authentication failed")
			return
		}
		if !ok {
			w.Header().Set("WWW-Authenticate", s.oauth.challengeHeader(base(s.listenerURL(kind), r)))
			jsonhttp.Error(w, http.StatusUnauthorized, unauthorizedHint(kind))
			return
		}
		next.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), p)))
	})
}

// unauthorizedHint explains a 401. On the machine running the agent the
// caller is usually the owner whose a2a CLI doesn't attach the owner token.
func unauthorizedHint(kind string) string {
	local := "on the machine running the agent, enable the credential helper of a2a-serve (run `a2a-serve doctor --fix`)"
	remote := "elsewhere, ask the agent's owner for a pairing code and run `a2a serve tokens add <agent-url> <code>`"
	if kind == network.ListenerLocal {
		return "this agent needs a token: " + local + "; " + remote
	}
	return "this agent needs a token: " + remote + "; " + local
}

// card builds the agent card served at baseURL.
func (s *Server) card(kind, baseURL string) *a2a.AgentCard {
	cfg := s.config()
	desc := cfg.Description
	if desc == "" {
		desc = cfg.Name + " is " + output.WithArticle(cfg.Harness) + " coding agent reached through `a2a serve`."
	}
	card := &a2a.AgentCard{
		Name:        cfg.Name,
		Description: desc,
		Version:     "1.0.0",
		SupportedInterfaces: []*a2a.AgentInterface{
			a2a.NewAgentInterface(baseURL, a2a.TransportProtocolJSONRPC),
			a2a.NewAgentInterface(baseURL, a2a.TransportProtocolHTTPJSON),
		},
		Capabilities: a2a.AgentCapabilities{
			Streaming:  true,
			Extensions: []a2a.AgentExtension{structuredinput.NewExtension(structuredinput.Draft202012)},
		},
		DefaultInputModes:  []string{"text/plain", "application/octet-stream"},
		DefaultOutputModes: []string{"text/plain", "application/json"},
		Skills: []a2a.AgentSkill{{
			ID:          "coding",
			Name:        "Coding agent",
			Description: "Works in its project workspace with " + cfg.Harness + ". Tool calls may pause the task in input-required until an approver answers.",
			Tags:        []string{"coding", cfg.Harness},
		}},
	}
	if kind != network.ListenerPrivate {
		card.SecuritySchemes = a2a.NamedSecuritySchemes{
			"pairing": a2a.HTTPAuthSecurityScheme{Scheme: "Bearer", Description: "A token from pairing: `a2a serve tokens add <agent-url> <code>`, with a code from the owner."},
		}
		card.SecurityRequirements = a2a.SecurityRequirementsOptions{{"pairing": {}}}
	}
	return card
}

// handlePair exchanges a pair code for a caller token, or a node code for a
// node token.
func (s *Server) handlePair(w http.ResponseWriter, r *http.Request, baseURL string) {
	host := remoteHost(r.RemoteAddr)
	if !s.pairFailures.allow(host) {
		w.Header().Set("Retry-After", "60")
		jsonhttp.Error(w, http.StatusTooManyRequests, "too many invalid pairing codes; try again in a minute")
		return
	}
	var req pairapi.Request
	if !jsonhttp.Read(w, r, &req) {
		return
	}
	c, err := s.store.TakeCode(r.Context(), auth.NormalizeCode(req.Code), "")
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrCodeExpired) {
		s.pairFailures.fail(host)
		jsonhttp.Error(w, http.StatusForbidden, "invalid or expired pairing code")
		return
	}
	if err != nil {
		slog.Error("pairing failed", "error", err)
		jsonhttp.Error(w, http.StatusInternalServerError, "pairing failed")
		return
	}
	if c.Purpose == auth.PurposeNode {
		s.nodes.Pair(w, r, baseURL, c.Name)
		return
	}
	tok, p, err := auth.IssueToken(r.Context(), s.store, c, baseURL)
	if err != nil {
		slog.Error("pairing failed", "error", err)
		jsonhttp.Error(w, http.StatusInternalServerError, "pairing failed")
		return
	}
	slog.Info("paired", "principal", p.ID, "url", baseURL)
	jsonhttp.Write(w, http.StatusOK, pairapi.Response{Kind: pairapi.KindCaller, Token: tok, Principal: p.ID, Agent: s.config().Name, URL: baseURL})
}

// cliSkill is the agent skill of the a2a CLI on PATH, read once.
var cliSkill = sync.OnceValues(func() (string, error) {
	return a2acli.Skill(context.Background())
})

func handleCLISkill(w http.ResponseWriter, r *http.Request) {
	skill, err := cliSkill()
	if err != nil {
		slog.Warn("no a2a CLI skill to serve", "error", err)
		jsonhttp.Error(w, http.StatusNotFound, "skill not available: the a2a CLI is not installed next to this agent")
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	writeBody(w, skill)
}

// handleDownloadCLI serves the a2a CLI installed next to the server, which
// callers and nodes use to send it tasks.
func handleDownloadCLI(w http.ResponseWriter, r *http.Request) {
	exe, err := a2acli.Path()
	if err != nil {
		jsonhttp.Error(w, http.StatusNotFound, "binary not available")
		return
	}
	serveBinary(w, r, exe, a2acli.Binary)
}

// handleDownloadServe serves this binary, which nodes use to pair and to
// answer tasks.
func handleDownloadServe(w http.ResponseWriter, r *http.Request) {
	exe, err := os.Executable()
	if err != nil {
		jsonhttp.Error(w, http.StatusNotFound, "binary not available")
		return
	}
	serveBinary(w, r, exe, "a2a-serve")
}

func serveBinary(w http.ResponseWriter, r *http.Request, path, name string) {
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	http.ServeFile(w, r, path)
}

func writeBody(w io.Writer, body string) {
	if _, err := io.WriteString(w, body); err != nil {
		slog.Debug("writing response", "error", err)
	}
}

func trimSlash(u string) string { return strings.TrimSuffix(u, "/") }
