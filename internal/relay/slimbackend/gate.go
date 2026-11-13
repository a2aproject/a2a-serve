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

package slimbackend

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/jsonhttp"
)

// Routes of the gate, under the relay's URL.
const (
	EnrollPath  = "/slim/enroll"
	MembersPath = "/slim/members"
	NodePath    = "/slim/node"
)

// Bounds of the gate.
const (
	maxEnrollBody = 4096
	// maxConnsPerMember bounds the node connections of one member. An agent
	// holds one, and another while it reconnects.
	maxConnsPerMember = 4
)

// EnrollRequest joins an agent to the relay with an invite and the public
// key it signs its tokens with.
type EnrollRequest struct {
	Token string `json:"token"`
	Key   []byte `json:"key"`
}

// EnrollResponse tells an agent its name on the relay.
type EnrollResponse struct {
	Agent string `json:"agent"`
	// SLIMName is the agent's SLIM name, the subject of its tokens.
	SLIMName string `json:"slimName"`
}

// MemberInfo is a member of the relay, as members see it.
type MemberInfo struct {
	Agent    string `json:"agent"`
	SLIMName string `json:"slimName"`
	Key      []byte `json:"key"`
}

// MembersResponse lists the relay's members.
type MembersResponse struct {
	Members []MemberInfo `json:"members"`
}

// gate serves the relay's SLIM routes: enrollment, the member list, and
// the node, which it lets only members reach.
type gate struct {
	svc *service
	// aud is the audience of the tokens members present, from
	// [GateAudience].
	aud   string
	node  *httputil.ReverseProxy
	conns *conns
	now   func() time.Time
}

type agentKey struct{}

func newGate(svc *service, aud string, node *url.URL, now func() time.Time) *gate {
	g := &gate{svc: svc, aud: aud, conns: svc.conns, now: now}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	g.node = &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(node)
			r.Out.URL.Path, r.Out.URL.RawPath = "/", ""
		},
		Transport: &http.Transport{
			DisableKeepAlives: true,
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				c, err := dialer.DialContext(ctx, network, addr)
				if err != nil {
					return nil, err
				}
				agent, _ := ctx.Value(agentKey{}).(string)
				return g.track(agent, c), nil
			},
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if !errors.Is(err, r.Context().Err()) {
				slog.Warn("proxying to the SLIM node", "error", err)
			}
			jsonhttp.Error(w, http.StatusBadGateway, "the relay's SLIM node didn't answer")
		},
	}
	return g
}

func (g *gate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == EnrollPath && r.Method == http.MethodPost:
		g.enroll(w, r)
	case r.URL.Path == MembersPath && r.Method == http.MethodGet:
		g.members(w, r)
	case r.URL.Path == NodePath && r.Method == http.MethodGet:
		g.proxyNode(w, r)
	case strings.HasPrefix(r.URL.Path, "/ingress/"):
		jsonhttp.Error(w, http.StatusNotFound, "SLIM relays don't give agents public URLs yet")
	default:
		jsonhttp.Error(w, http.StatusNotFound, "not found")
	}
}

func (g *gate) enroll(w http.ResponseWriter, r *http.Request) {
	var req EnrollRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxEnrollBody)).Decode(&req); err != nil {
		jsonhttp.Error(w, http.StatusBadRequest, "the body isn't an enrollment request")
		return
	}
	if len(req.Key) != ed25519.PublicKeySize {
		jsonhttp.Error(w, http.StatusBadRequest, "the key isn't an Ed25519 public key")
		return
	}
	m, err := g.svc.store.enroll(req.Token, req.Key)
	var conflict *agenterr.ConflictError
	switch {
	case errors.As(err, &conflict):
		jsonhttp.Error(w, http.StatusConflict, err.Error())
		return
	case errors.Is(err, errInvite):
		jsonhttp.Error(w, http.StatusUnauthorized, err.Error())
		return
	case err != nil:
		slog.Error("enrolling an agent", "error", err)
		jsonhttp.Error(w, http.StatusInternalServerError, "the relay couldn't save the agent")
		return
	}
	jsonhttp.Write(w, http.StatusOK, EnrollResponse{Agent: m.Agent, SLIMName: SLIMName(m.Agent)})
}

func (g *gate) members(w http.ResponseWriter, r *http.Request) {
	if _, ok := g.authenticate(w, r); !ok {
		return
	}
	snap := g.svc.store.snapshot()
	resp := MembersResponse{Members: []MemberInfo{}}
	for _, m := range snap.Members {
		if !m.removed() {
			resp.Members = append(resp.Members, MemberInfo{Agent: m.Agent, SLIMName: SLIMName(m.Agent), Key: m.Key})
		}
	}
	jsonhttp.Write(w, http.StatusOK, resp)
}

func (g *gate) proxyNode(w http.ResponseWriter, r *http.Request) {
	agent, ok := g.authenticate(w, r)
	if !ok {
		return
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		jsonhttp.Error(w, http.StatusBadRequest, "the SLIM node takes WebSocket connections")
		return
	}
	if g.conns.count(agent) >= maxConnsPerMember {
		jsonhttp.Error(w, http.StatusTooManyRequests, "this agent has too many connections to the SLIM node")
		return
	}
	g.node.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), agentKey{}, agent)))
}

// authenticate returns the member whose token r carries, or answers 401.
func (g *gate) authenticate(w http.ResponseWriter, r *http.Request) (string, bool) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		jsonhttp.Error(w, http.StatusUnauthorized, "a member's token is required")
		return "", false
	}
	agent, err := verifyToken(token, g.aud, g.now(), func(slimName string) (string, ed25519.PublicKey, bool) {
		agent, key, ok := g.svc.memberKey(slimName)
		return agent, key, ok
	})
	if err != nil {
		jsonhttp.Error(w, http.StatusUnauthorized, err.Error())
		return "", false
	}
	return agent, true
}

// track registers c as a connection of agent, and closes it right away
// when agent was removed while it connected.
func (g *gate) track(agent string, c net.Conn) net.Conn {
	tc := g.conns.add(agent, c)
	if _, ok := g.svc.activeAgent(agent); !ok {
		if err := tc.Close(); err != nil {
			slog.Debug("closing the connection of a removed agent", "error", err)
		}
	}
	return tc
}

// conns are the node connections of the relay's members.
type conns struct {
	mu     sync.Mutex
	byName map[string]map[*trackedConn]struct{}
	last   map[string]time.Time
	now    func() time.Time
}

func newConns(now func() time.Time) *conns {
	return &conns{byName: map[string]map[*trackedConn]struct{}{}, last: map[string]time.Time{}, now: now}
}

type trackedConn struct {
	net.Conn
	once  sync.Once
	agent string
	conns *conns
}

func (c *trackedConn) Close() error {
	c.once.Do(func() { c.conns.remove(c) })
	return c.Conn.Close()
}

func (cs *conns) add(agent string, c net.Conn) *trackedConn {
	tc := &trackedConn{Conn: c, agent: agent, conns: cs}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.byName[agent] == nil {
		cs.byName[agent] = map[*trackedConn]struct{}{}
	}
	cs.byName[agent][tc] = struct{}{}
	cs.last[agent] = cs.now()
	return tc
}

func (cs *conns) remove(c *trackedConn) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	delete(cs.byName[c.agent], c)
	if len(cs.byName[c.agent]) == 0 {
		delete(cs.byName, c.agent)
	}
}

func (cs *conns) count(agent string) int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return len(cs.byName[agent])
}

// seen returns when agent last connected to the node since the relay
// started, or the zero time.
func (cs *conns) seen(agent string) time.Time {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if len(cs.byName[agent]) > 0 {
		return cs.now()
	}
	return cs.last[agent]
}

// closeAgent closes the connections of agent and forgets when it was last
// seen.
func (cs *conns) closeAgent(agent string) {
	cs.mu.Lock()
	var open []*trackedConn
	for c := range cs.byName[agent] {
		open = append(open, c)
	}
	delete(cs.last, agent)
	cs.mu.Unlock()
	for _, c := range open {
		if err := c.Close(); err != nil {
			slog.Debug("closing the connection of a removed agent", "error", err)
		}
	}
}
