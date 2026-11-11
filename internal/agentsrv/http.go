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
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/a2aproject/a2a-serve/internal/a2acli"
	"github.com/a2aproject/a2a-serve/internal/agentcard"
	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/httpserver"
	"github.com/a2aproject/a2a-serve/internal/jsonhttp"
	"github.com/a2aproject/a2a-serve/internal/mailboxapi"
	"github.com/a2aproject/a2a-serve/internal/network"
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
	// host, if set, is the only Host the listener serves. A tailnet over
	// plain HTTP has no TLS name check, so a web page could rebind its own
	// name to the agent and reach it from the user's device.
	host string
}

// privateListener describes the private listener reached at u: the
// inspector on a tailnet, and tunneled on a relay.
func (s *Server) privateListener(u *url.URL) listener {
	l := listener{kind: network.ListenerPrivate, baseURL: u.String()}
	if s.private.Name() == network.ProviderTailscale {
		l.inspector = true
		if u.Scheme == "http" {
			l.host = u.Host
		}
		return l
	}
	_, l.tunneled = s.private.(network.ClientKeyer)
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
	card := httpserver.PublicCORS(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a2asrv.NewStaticAgentCardHandler(s.card(l.kind, base(baseURL, r))).ServeHTTP(w, r)
	}))
	jsonrpc := a2asrv.NewJSONRPCHandler(s.handler, a2asrv.WithTransportKeepAlive(15*time.Second))
	rest := a2asrv.NewRESTHandler(s.handler, a2asrv.WithTransportKeepAlive(15*time.Second))

	for _, path := range []string{"/{$}", a2asrv.WellKnownAgentCardPath, "/.well-known/agent.json"} {
		mux.Handle("GET "+path, card)
		mux.Handle("OPTIONS "+path, card)
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		jsonhttp.Write(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /skill.md", handleCLISkill)
	mux.HandleFunc("GET "+mailboxapi.Prefix+"/skill.md", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		writeBody(w, strings.ReplaceAll(skills.A2AMailbox, "{{BASE}}", base(baseURL, r)))
	})
	// Routes that serve callers before they authenticate share a pool of
	// their own, limited for each client that the server can tell apart.
	unauthenticated := httpserver.NewKeyedLimiter(httpserver.MaxUnauthenticated, httpserver.MaxUnauthenticatedPerClient, func(r *http.Request) string { return s.pairClient(r, l) })
	download := func(h http.HandlerFunc) http.Handler {
		return unauthenticated.Handler(httpserver.WriteTimeout(s.timings.DownloadTimeout, h))
	}
	mux.Handle("GET /download/a2a", download(handleDownloadCLI))
	mux.Handle("GET /download/a2a-serve", download(handleDownloadServe))
	mux.Handle("POST "+pairapi.Path, unauthenticated.Handler(jsonhttp.RequireJSON(jsonhttp.Limit{Max: maxPairBody, Total: s.timings.PairReadTimeout}.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.handlePair(w, r, base(baseURL, r), l) })))))
	mux.Handle("/hitl/{token}", unauthenticated.Handler(jsonhttp.Limit{Max: maxHITLBody, Total: s.timings.PairReadTimeout}.Handler(http.HandlerFunc(s.approvals.HandlePage))))
	if l.kind == network.ListenerPublic {
		s.oauth.Register(mux, func(r *http.Request) string { return base(baseURL, r) }, unauthenticated.Handler)
	}
	if l.kind == network.ListenerPrivate {
		s.registerGossip(mux, unauthenticated.Handler)
	}
	if l.inspector {
		s.inspector.Register(mux)
	}
	members := func(h http.Handler) http.Handler { return s.roleRequests[l.kind].Handler(auth.RoleMember, h) }
	s.nodes.Register(mux, l.kind, func(r *http.Request) string { return base(baseURL, r) }, members)
	bodies := newGuestBodies(maxGuestBodies, jsonhttp.ReadIdleTimeout)
	mux.Handle("POST /{$}", s.a2aRoute(l.kind, protocolJSONRPC, bodies, jsonrpc))
	mux.Handle("/", s.a2aRoute(l.kind, protocolREST, bodies, rest))
	h := http.NewCrossOriginProtection().Handler(mux)
	if l.host != "" {
		return requireHost(l.host, h)
	}
	return h
}

// requireHost serves next only the requests for host, a host of a URL,
// with port 80 when it names none.
func requireHost(host string, next http.Handler) http.Handler {
	want := hostPort(host)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(hostPort(r.Host), want) {
			jsonhttp.Error(w, http.StatusMisdirectedRequest, "this server doesn't serve "+r.Host)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// hostPort returns host with port 80 when it names none, and its name
// without a trailing dot.
func hostPort(host string) string {
	name, port, err := net.SplitHostPort(host)
	if err != nil {
		name, port = host, "80"
	}
	return net.JoinHostPort(strings.TrimSuffix(name, "."), port)
}

// a2aRoute serves an A2A transport to authenticated callers, and answers
// a body over the limit as an error of p. The bodies of guests are charged
// to bodies, the budget of the listener.
func (s *Server) a2aRoute(kind string, p a2aProtocol, bodies *guestBodies, h http.Handler) http.Handler {
	limit := jsonhttp.Limit{Max: jsonhttp.MaxBody, Idle: jsonhttp.ReadIdleTimeout, TooLarge: p.tooLarge}
	return jsonhttp.RequireJSON(limit.Handler(s.authenticate(kind, bodies.handler(p, p.readBody(h)))))
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

// newRoleShares returns the shares of one listener's requests for members
// and guests. The owner has no share, so the others leave it room.
func newRoleShares() *httpserver.Shares {
	return httpserver.NewShares(map[string]int{auth.RoleMember: httpserver.MaxMemberRequests, auth.RoleGuest: httpserver.MaxGuestRequests})
}

// authenticate resolves the principal of a request with the auth chain: 401
// for a caller without a credential, and 403 naming a known caller whom
// the policy refuses. The request then holds one of its role's share of
// the listener's requests until it ends, and gets 503 when the share is in
// use.
func (s *Server) authenticate(kind string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := s.chain.Authenticate(r.Context(), auth.Request{Listener: kind, RemoteAddr: r.RemoteAddr, Header: r.Header})
		proto := protocolOf(r)
		var refused *auth.RefusedError
		switch {
		case errors.As(err, &refused):
			slog.Info("refused a caller", "caller", refused.Identity.Describe())
			proto.writeError(w, http.StatusForbidden, a2a.ErrUnauthorized, s.refusal(r.Context(), refused, base(s.listenerURL(kind), r)))
			return
		case errors.Is(err, auth.ErrUnknownToken):
			agentURL := base(s.listenerURL(kind), r)
			w.Header().Set("WWW-Authenticate", s.oauth.ChallengeHeader(kind, agentURL))
			hint := pairHint
			if auth.Hops(r.Header) > 0 {
				hint = s.contactSteps("<name>", agentURL)
			}
			proto.writeError(w, http.StatusUnauthorized, a2a.ErrUnauthenticated, err.Error()+", which may be mistyped, revoked or replaced; "+hint)
			return
		case errors.Is(err, auth.ErrNoCredential):
			w.Header().Set("WWW-Authenticate", s.oauth.ChallengeHeader(kind, base(s.listenerURL(kind), r)))
			proto.writeError(w, http.StatusUnauthorized, a2a.ErrUnauthenticated, unauthorizedHint(kind))
			return
		case err != nil:
			slog.Error("authentication failed", "error", err)
			proto.writeError(w, http.StatusInternalServerError, a2a.ErrInternalError, "authentication failed")
			return
		}
		agentURL := base(s.listenerURL(kind), r)
		if auth.Hops(r.Header) > 0 {
			// An agent calls through its server, at a URL only that server knows.
			agentURL = "<agent URL>"
		}
		release, ok := s.roleRequests[kind].Acquire(p.Role)
		if !ok {
			httpserver.SetBusy(w.Header())
			proto.writeError(w, http.StatusServiceUnavailable, a2a.ErrInternalError, httpserver.BusyMessage)
			return
		}
		defer release()
		ctx := context.WithValue(auth.WithPrincipal(r.Context(), p), agentURLKey{}, agentURL)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// agentURLKey is the context key of the URL a caller reached the agent at,
// as the caller can pass it to the a2a CLI.
type agentURLKey struct{}

// a2aCommand words the a2a CLI command `a2a <args>` for the caller of ctx,
// with the agent URL the caller used inserted after the subcommand words.
func a2aCommand(ctx context.Context, sub, args string) string {
	cmd := "a2a " + sub
	if u, _ := ctx.Value(agentURLKey{}).(string); u != "" {
		cmd += " -a " + u
	}
	return "`" + cmd + " " + args + "`"
}

const pairHint = "ask the agent's owner for a pairing code and run `a2a-serve pair join <agent-url> <code>`"

// refusal explains a 403 to a known caller. A person pairs with a code; an
// agent becomes a contact with one, which its owner adds.
func (s *Server) refusal(ctx context.Context, refused *auth.RefusedError, agentURL string) string {
	id := refused.Identity
	if id.Hops == 0 {
		return refused.Error() + "; " + pairHint
	}
	who, invitee := id.Describe(), "<name>"
	if name := s.peerName(ctx, id.Node); name != "" {
		who, invitee = name+", "+who, name
	}
	return fmt.Sprintf("this agent doesn't accept %s; %s", who, s.contactSteps(invitee, agentURL))
}

// contactSteps words how the calling agent becomes a contact of this agent
// at agentURL, under the pairing name invitee.
func (s *Server) contactSteps(invitee, agentURL string) string {
	return fmt.Sprintf("to pair, this agent's owner runs `a2a-serve pair invite %s`, and the calling agent's owner adds the code with `a2a-serve contact add %s --url %s --code <code>`",
		invitee, s.rt.Config().Name, agentURL)
}

// peerNameTimeout bounds looking up the name of a refused agent.
const peerNameTimeout = 5 * time.Second

// peerName returns the name of the agent on the private network's machine
// node, or "" when it isn't found.
func (s *Server) peerName(ctx context.Context, node string) string {
	if node == "" || s.private == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, peerNameTimeout)
	defer cancel()
	peers, err := s.clients.Discover(ctx)
	if err != nil {
		slog.Debug("looking up a refused agent", "error", err)
		return ""
	}
	for _, p := range peers {
		if p.Node == node {
			return p.Name
		}
	}
	return ""
}

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

// listenerCard returns the agent card the listener kind serves.
func (s *Server) listenerCard(kind string) (*a2a.AgentCard, error) {
	switch kind {
	case network.ListenerLocal, network.ListenerPrivate, network.ListenerPublic:
	default:
		return nil, agenterr.Invalidf("unknown listener %q: want local, private or public", kind)
	}
	u := s.listenerURL(kind)
	if u == "" {
		return nil, agenterr.Invalidf("the agent has no %s URL", kind)
	}
	return s.card(kind, u), nil
}

// card builds the agent card served at baseURL on the listener kind.
func (s *Server) card(kind, baseURL string) *a2a.AgentCard {
	l := agentcard.Listener{Kind: kind, URL: baseURL}
	if kind == network.ListenerPublic {
		l.SignIn = s.oauth.CardScheme(baseURL)
	}
	return agentcard.Build(s.rt.Config(), l)
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
// concurrent ones: the source of a Funnel connection, the client key a
// network provider gives a connection, or the remote address of a direct
// one. Other tunneled clients can't be told apart, so they get "". Their
// concurrent requests count only toward the total, as one client could
// otherwise take the share of all of them.
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
	ck, ok := p.(network.ClientKeyer)
	if !ok {
		return ""
	}
	return ck.ClientKey(r.RemoteAddr, l.kind == network.ListenerPublic)
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
