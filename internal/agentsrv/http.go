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
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aext/structuredinput"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/a2aproject/a2a-serve/internal/a2acli"
	"github.com/a2aproject/a2a-serve/internal/access"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/httpserver"
	"github.com/a2aproject/a2a-serve/internal/jsonhttp"
	"github.com/a2aproject/a2a-serve/internal/mailboxapi"
	"github.com/a2aproject/a2a-serve/internal/network"
	"github.com/a2aproject/a2a-serve/internal/output"
	"github.com/a2aproject/a2a-serve/internal/pairapi"
	"github.com/a2aproject/a2a-serve/internal/pairings"
	"github.com/a2aproject/a2a-serve/skills"
)

// listener is one listener the server serves its routes on.
type listener struct {
	// kind is one of the network.Listener* kinds.
	kind string
	// baseURL is the URL the listener is reached at; empty derives it from
	// the request.
	baseURL string
	// inspector serves the web inspector, on listeners where its login link
	// and session cookie stay private.
	inspector bool
	// tunneled is set when every caller arrives from the address of a
	// tunnel, such as Funnel or the relay.
	tunneled bool
}

// privateListener describes the private listener reached at u: the
// inspector only over Tailscale's HTTPS, and tunneled on a relay.
func (s *Server) privateListener(u *url.URL) listener {
	l := listener{kind: network.ListenerPrivate, baseURL: u.String()}
	switch s.private.Name() {
	case network.ProviderTailscale:
		l.inspector = true
	case network.ProviderRelay:
		l.tunneled = true
	}
	return l
}

const (
	// maxPairBody bounds the body of a pairing request.
	maxPairBody = 4 << 10
	// maxHITLBody bounds the form that answers a question at /hitl.
	maxHITLBody = 4 << 10
)

// routes returns the HTTP handler of one listener.
func (s *Server) routes(l listener) http.Handler {
	baseURL := l.baseURL
	mux := http.NewServeMux()
	card := func(w http.ResponseWriter, r *http.Request) {
		a2asrv.NewStaticAgentCardHandler(s.card(l.kind, base(baseURL, r))).ServeHTTP(w, r)
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
	mux.HandleFunc("GET "+mailboxapi.Prefix+"/skill.md", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		writeBody(w, strings.ReplaceAll(skills.A2AMailbox, "{{BASE}}", base(baseURL, r)))
	})
	mux.HandleFunc("GET /download/a2a", handleDownloadCLI)
	mux.HandleFunc("GET /download/a2a-serve", handleDownloadServe)
	// Routes that serve callers before they authenticate share a pool of
	// their own, limited for each client.
	unauthenticated := httpserver.NewKeyedLimiter(httpserver.MaxUnauthenticated, httpserver.MaxUnauthenticatedPerClient, func(r *http.Request) string { return s.pairClient(r, l) })
	mux.Handle("POST "+pairapi.Path, unauthenticated.Handler(jsonhttp.RequireJSON(jsonhttp.LimitUnauthenticatedBody(maxPairBody, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.handlePair(w, r, base(baseURL, r), l) })))))
	mux.Handle("/hitl/{token}", unauthenticated.Handler(jsonhttp.LimitUnauthenticatedBody(maxHITLBody, http.HandlerFunc(s.approvals.HandlePage))))
	if l.kind == network.ListenerPublic {
		s.oauth.Register(mux, func(r *http.Request) string { return base(baseURL, r) }, unauthenticated.Handler)
	}
	if l.kind == network.ListenerPrivate {
		s.registerGossip(mux)
	}
	if l.inspector {
		s.inspector.Register(mux)
	}
	s.nodes.Register(mux, l.kind, func(r *http.Request) string { return base(baseURL, r) })
	mux.Handle("POST /{$}", jsonhttp.RequireJSON(jsonhttp.LimitBody(s.authenticate(l.kind, jsonrpc))))
	mux.Handle("/", jsonhttp.RequireJSON(jsonhttp.LimitBody(s.authenticate(l.kind, rest))))
	return http.NewCrossOriginProtection().Handler(mux)
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

// authenticate resolves the principal of a request with the auth chain: 401
// for a caller without a credential, and 403 naming a known caller whom
// the policy refuses.
func (s *Server) authenticate(kind string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := s.chain.Authenticate(r.Context(), auth.Request{Listener: kind, RemoteAddr: r.RemoteAddr, Header: r.Header})
		var refused *auth.RefusedError
		switch {
		case errors.As(err, &refused):
			slog.Info("refused a caller", "caller", refused.Identity.Describe())
			jsonhttp.Error(w, http.StatusForbidden, refused.Error()+"; "+pairHint)
			return
		case errors.Is(err, auth.ErrNoCredential):
			w.Header().Set("WWW-Authenticate", s.oauth.ChallengeHeader(kind, base(s.listenerURL(kind), r)))
			jsonhttp.Error(w, http.StatusUnauthorized, unauthorizedHint(kind))
			return
		case err != nil:
			slog.Error("authentication failed", "error", err)
			jsonhttp.Error(w, http.StatusInternalServerError, "authentication failed")
			return
		}
		next.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), p)))
	})
}

const pairHint = "ask the agent's owner for a pairing code and run `a2a-serve pair join <agent-url> <code>`"

// unauthorizedHint explains a 401. On the machine running the agent the
// caller is usually the owner whose a2a CLI doesn't attach the owner token.
func unauthorizedHint(kind string) string {
	local := "on the machine running the agent, enable the credential helper of a2a-serve (run `a2a-serve doctor --fix`)"
	remote := "elsewhere, " + pairHint
	if kind == network.ListenerLocal {
		return "this agent needs a token: " + local + "; " + remote
	}
	return "this agent needs a token: " + remote + "; " + local
}

// card builds the agent card served at baseURL.
func (s *Server) card(kind, baseURL string) *a2a.AgentCard {
	cfg := s.rt.Config()
	desc := cfg.Description
	if desc == "" {
		desc = cfg.Name + " is " + output.WithArticle(cfg.Harness) + " coding agent reached through `a2a-serve`."
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
			Description: "Works in its project workspace with " + cfg.Harness + ". Tool calls may pause the task in input-required until whoever started it answers.",
			Tags:        []string{"coding", cfg.Harness},
		}},
	}
	if !s.needsPairing(kind) {
		return card
	}
	card.SecuritySchemes = a2a.NamedSecuritySchemes{
		"pairing": a2a.HTTPAuthSecurityScheme{Scheme: "Bearer", Description: "A token from pairing: `a2a-serve pair join <agent-url> <code>`, with a code from the owner."},
	}
	card.SecurityRequirements = a2a.SecurityRequirementsOptions{{"pairing": {}}}
	if kind != network.ListenerPublic {
		return card
	}
	// The sign-in schemes are alternatives to pairing.
	if scheme := s.oauth.CardScheme(baseURL); scheme != nil {
		card.SecuritySchemes["google"] = scheme
		card.SecurityRequirements = append(card.SecurityRequirements, a2a.SecurityRequirements{"google": {}})
	}
	return card
}

// needsPairing reports whether callers at the listener kind need a pairing
// token, which the agent card then advertises: the local listener always,
// the private one with --private-access paired, and the public one unless
// --public-access anyone.
func (s *Server) needsPairing(kind string) bool {
	p := s.rt.Config().Policy
	switch kind {
	case network.ListenerPrivate:
		return access.PrivateAccess(p) == agentdir.AccessPaired
	case network.ListenerPublic:
		return p.PublicAccess != agentdir.AccessAnyone
	}
	return true
}

// handlePair exchanges a pair code for a caller token, or a node code for a
// node token.
func (s *Server) handlePair(w http.ResponseWriter, r *http.Request, baseURL string, l listener) {
	var req pairapi.Request
	if !jsonhttp.Read(w, r, &req) {
		return
	}
	red, err := s.pairings.Redeem(r.Context(), req.Code, pairings.Via{Client: s.pairClient(r, l), URL: baseURL, Kind: req.Kind})
	var (
		limited *pairings.RateLimitError
		invalid *agenterr.InvalidError
	)
	switch {
	case errors.As(err, &limited):
		w.Header().Set("Retry-After", strconv.Itoa(limited.Seconds()))
		jsonhttp.Error(w, http.StatusTooManyRequests, limited.Error())
		return
	case errors.As(err, &invalid):
		jsonhttp.Error(w, http.StatusBadRequest, invalid.Error())
		return
	case errors.Is(err, pairings.ErrLocalCode):
		jsonhttp.Error(w, http.StatusForbidden, err.Error())
		return
	case errors.Is(err, pairings.ErrInvalidCode):
		jsonhttp.Error(w, http.StatusForbidden, "invalid or expired pairing code")
		return
	case err != nil:
		slog.Error("pairing failed", "error", err)
		jsonhttp.Error(w, http.StatusInternalServerError, "pairing failed")
		return
	}
	agent := s.rt.Config().Name
	if red.Kind == pairapi.KindNode {
		agent = red.Node
	}
	jsonhttp.Write(w, http.StatusOK, pairapi.Response{Kind: red.Kind, Token: red.Token, Principal: red.Principal, Agent: agent, URL: red.URL})
}

// pairClient identifies the client of a pairing request for the limit on
// failed attempts, and of unauthenticated requests for their limit on
// concurrent ones: the source of a Funnel connection, the verified peer of
// a relay connection, or the remote address of a direct one. Other
// tunneled clients can't be told apart, so they get "".
func (s *Server) pairClient(r *http.Request, l listener) string {
	if src, ok := r.Context().Value(funnelSourceKey{}).(string); ok {
		return src
	}
	if !l.tunneled {
		return remoteHost(r.RemoteAddr)
	}
	p := s.private
	if l.kind == network.ListenerPublic {
		p = s.public
	}
	if p == nil || p.Name() != network.ProviderRelay {
		return ""
	}
	peer, err := p.WhoIs(r.Context(), r.RemoteAddr)
	if err != nil || peer.Node == "" {
		return ""
	}
	return "peer:" + peer.Node
}

// funnelSourceKey keys the client address of a Funnel connection in the
// contexts of its requests.
type funnelSourceKey struct{}

// connContext adds the client address of a Funnel connection to ctx.
func connContext(ctx context.Context, c net.Conn) context.Context {
	if src, ok := network.FunnelSource(c); ok {
		return context.WithValue(ctx, funnelSourceKey{}, src)
	}
	return ctx
}

// remoteHost is the host of a request's remote address.
func remoteHost(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
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
