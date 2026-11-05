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

// Package control is the control API of a running relay: invites, members,
// removal and status. A local relay serves it on its control socket, and
// every relay serves it at its URL to callers with the admin token.
package control

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/relay/meshadmin"
)

// Versions are the versions a relay runs.
type Versions struct {
	A2AServe  string `json:"a2aServe"`
	AgentMesh string `json:"agentMesh"`
}

// Status describes a running relay.
type Status struct {
	URL      string    `json:"url"`
	PID      int       `json:"pid"`
	Started  time.Time `json:"started"`
	Versions Versions  `json:"versions"`
	// Join says who can join the relay.
	Join    string `json:"join"`
	Members int    `json:"members"`
}

// InviteRequest asks for an invite for one agent.
type InviteRequest struct {
	Name string `json:"name"`
	// Hours is how long the invite stays valid; zero means DefaultInviteHours.
	Hours int `json:"hours,omitempty"`
}

// Invite limits.
const (
	DefaultInviteHours = 24
	MaxInviteHours     = 7 * 24
)

// Invite is a single-use token that joins one agent to the relay.
type Invite struct {
	Name    string    `json:"name"`
	Token   string    `json:"token"`
	Expires time.Time `json:"expires"`
	URL     string    `json:"url"`
}

// Member is a node on the relay.
type Member struct {
	// Agent names the agents the node serves.
	Agent string `json:"agent,omitempty"`
	// Email is set for a node that signed in, and Domain to the Google
	// Workspace domain of its account.
	Email  string    `json:"email,omitempty"`
	Domain string    `json:"domain,omitempty"`
	Invite bool      `json:"invite,omitempty"`
	Joined time.Time `json:"joined,omitzero"`
	Seen   time.Time `json:"seen,omitzero"`
	Peer   string    `json:"peer"`
	Banned bool      `json:"banned,omitempty"`
}

// Who says how the member joined: its email, or that it has an invite.
func (m Member) Who() string {
	if m.Email != "" {
		return m.Email
	}
	return "invite"
}

// RemoveRequest names the members to ban: an agent, an email or a peer ID.
type RemoveRequest struct {
	Target string `json:"target"`
}

// RemoveResponse lists the banned members, and the emails among them that
// the relay still admits, which it lets back when it starts.
type RemoveResponse struct {
	Removed  []Member `json:"removed"`
	Admitted []string `json:"admitted,omitempty"`
}

// Admits reports whether the relay lets a member that signed in stay.
type Admits func(Member) bool

// Service implements the operations of the control API with the admin API
// of the relay's agentmesh-one.
type Service struct {
	mesh   *meshadmin.Client
	status func() Status
	admits Admits
}

// NewService returns the operations of a relay, whose static status comes
// from status, and which lets the members that signed in stay that admits
// allows.
func NewService(mesh *meshadmin.Client, status func() Status, admits Admits) *Service {
	return &Service{mesh: mesh, status: status, admits: admits}
}

// Status returns the relay's status.
func (s *Service) Status(ctx context.Context) (*Status, error) {
	members, err := s.Members(ctx, false)
	if err != nil {
		return nil, err
	}
	st := s.status()
	st.Members = len(members)
	return &st, nil
}

// Invite mints an invite for the agent name.
func (s *Service) Invite(ctx context.Context, req InviteRequest) (*Invite, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return nil, agenterr.Invalidf("an invite needs the name of the agent it is for")
	}
	hours := req.Hours
	if hours == 0 {
		hours = DefaultInviteHours
	}
	if hours < 1 || hours > MaxInviteHours {
		return nil, agenterr.Invalidf("an invite lasts from 1 hour to %d hours, got %d", MaxInviteHours, hours)
	}
	// One use is enough: meshnet spends the token only when the agent has
	// no credential it can refresh, and autonomous recovery lets it refresh
	// after the key that signed its credential retired.
	tok, err := s.mesh.CreateBootstrapToken(ctx, meshadmin.BootstrapTokenRequest{
		Role:               meshadmin.RoleNode,
		TTLHours:           hours,
		MaxUsages:          1,
		Description:        "a2a-serve invite: " + name,
		AutonomousRecovery: true,
	})
	if err != nil {
		return nil, err
	}
	return &Invite{Name: name, Token: tok.Token, Expires: tok.Expires, URL: s.status().URL}, nil
}

// Members lists the agents on the relay, and with banned the banned ones too.
func (s *Service) Members(ctx context.Context, banned bool) ([]Member, error) {
	st, err := s.mesh.Status(ctx)
	if err != nil {
		return nil, err
	}
	return members(st, banned), nil
}

func members(st *meshadmin.Status, banned bool) []Member {
	var out []Member
	for _, n := range st.Nodes {
		if n.Role == meshadmin.RoleRouter || n.Banned && !banned {
			continue
		}
		m := Member{Peer: n.PeerID, Email: n.Email(), Domain: n.Domain(), Joined: n.Enrolled, Banned: n.Banned}
		m.Invite = m.Email == ""
		if svc, ok := st.Catalog[n.PeerID]; ok {
			var names []string
			for _, s := range svc.Services {
				if s.Type == meshadmin.ServiceTypeA2A && s.Name != "" {
					names = append(names, s.Name)
				}
			}
			slices.Sort(names)
			m.Agent, m.Seen = strings.Join(slices.Compact(names), ","), svc.Reported
		}
		out = append(out, m)
	}
	slices.SortStableFunc(out, func(a, b Member) int { return a.Joined.Compare(b.Joined) })
	return out
}

// Remove bans the members that target names: the node with that peer ID,
// every node of that email, or the node that serves that agent.
func (s *Service) Remove(ctx context.Context, req RemoveRequest) (*RemoveResponse, error) {
	target := strings.TrimSpace(req.Target)
	if target == "" {
		return nil, agenterr.Invalidf("name the member to remove: an agent, an email or a peer ID")
	}
	all, err := s.Members(ctx, false)
	if err != nil {
		return nil, err
	}
	matched, err := match(all, target)
	if err != nil {
		return nil, err
	}
	removed, err := s.ban(ctx, matched)
	if err != nil {
		return nil, err
	}
	resp := &RemoveResponse{Removed: removed}
	for _, m := range removed {
		if m.Email != "" && s.admits(m) && !slices.Contains(resp.Admitted, m.Email) {
			resp.Admitted = append(resp.Admitted, m.Email)
		}
	}
	return resp, nil
}

// Reconcile makes the members that signed in match the allowlist: it bans
// the ones the relay no longer admits and lets back the banned ones it
// admits. For people who sign in, the allowlist decides; Agent Mesh keeps
// renewing an agent until its session ends, so the relay applies a changed
// list when it starts.
func (s *Service) Reconcile(ctx context.Context) (removed, readmitted []Member, err error) {
	all, err := s.Members(ctx, true)
	if err != nil {
		return nil, nil, err
	}
	var refused, admitted []Member
	for _, m := range all {
		switch {
		case m.Email == "":
		case !m.Banned && !s.admits(m):
			refused = append(refused, m)
		case m.Banned && s.admits(m):
			admitted = append(admitted, m)
		}
	}
	if removed, err = s.ban(ctx, refused); err != nil {
		return nil, nil, err
	}
	for _, m := range admitted {
		if err := s.mesh.Unban(ctx, m.Peer); err != nil {
			return removed, readmitted, fmt.Errorf("letting back %s: %w", m.Email, err)
		}
		m.Banned = false
		readmitted = append(readmitted, m)
	}
	return removed, readmitted, nil
}

// ban bans ms and returns them banned. It stops at the first failure.
func (s *Service) ban(ctx context.Context, ms []Member) ([]Member, error) {
	var done []Member
	for _, m := range ms {
		if err := s.mesh.Ban(ctx, m.Peer); err != nil {
			if len(done) > 0 {
				return nil, fmt.Errorf("removed %s, then failed: %w", peers(done), err)
			}
			return nil, err
		}
		m.Banned = true
		done = append(done, m)
	}
	return done, nil
}

func match(all []Member, target string) ([]Member, error) {
	var out []Member
	switch {
	case strings.Contains(target, "@"):
		for _, m := range all {
			if strings.EqualFold(m.Email, target) {
				out = append(out, m)
			}
		}
	default:
		for _, m := range all {
			if m.Peer == target {
				return []Member{m}, nil
			}
		}
		for _, m := range all {
			if slices.Contains(strings.Split(m.Agent, ","), target) {
				out = append(out, m)
			}
		}
		if len(out) > 1 {
			return nil, agenterr.Conflictf("%d members serve an agent named %s (%s); remove one by its peer ID", len(out), target, peers(out))
		}
	}
	if len(out) == 0 {
		return nil, &agenterr.NotFoundError{Kind: "member", Name: target}
	}
	return out, nil
}

func peers(ms []Member) string {
	ids := make([]string, len(ms))
	for i, m := range ms {
		ids[i] = m.Peer
	}
	return strings.Join(ids, ", ")
}
