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

package meshbackend

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/names"
	"github.com/a2aproject/a2a-serve/internal/relay/control"
	"github.com/a2aproject/a2a-serve/internal/relay/meshadmin"
)

// ServiceOptions are the dependencies of a [Service].
type ServiceOptions struct {
	Mesh *meshadmin.Client
	// Removed remembers the agents the relay removes, and InviteNames the
	// invites its agents joined with.
	Removed     *RemovedAgents
	InviteNames *InviteNames
	// Policy is applied to the mesh.
	Policy meshadmin.Policy
	// Status returns the static status of the relay.
	Status func() control.Status
	// Admits says which members that signed in may stay.
	Admits control.Admits
	Now    func() time.Time
}

// Service implements the operations of the control API with the admin API
// of the relay's agentmesh-one.
type Service struct {
	mesh    *meshadmin.Client
	removed *RemovedAgents
	names   *InviteNames
	policy  meshadmin.Policy
	status  func() control.Status
	admits  control.Admits
	now     func() time.Time
}

// NewService returns the operations of a relay over its agentmesh-one.
func NewService(o ServiceOptions) *Service {
	return &Service{mesh: o.Mesh, removed: o.Removed, names: o.InviteNames, policy: o.Policy, status: o.Status, admits: o.Admits, now: o.Now}
}

// ApplyPolicy applies the relay's policy to its mesh.
func (s *Service) ApplyPolicy(ctx context.Context) error {
	if err := s.mesh.SetPolicy(ctx, s.policy); err != nil {
		return fmt.Errorf("applying the relay's policy: %w", err)
	}
	return nil
}

// Status returns the relay's status.
func (s *Service) Status(ctx context.Context) (*control.Status, error) {
	members, err := s.nodes(ctx, false)
	if err != nil {
		return nil, err
	}
	st := s.status()
	st.Members = len(members)
	return &st, nil
}

// Invite mints an invite for the agent name.
func (s *Service) Invite(ctx context.Context, req control.InviteRequest) (*control.Invite, error) {
	name := req.Name
	if name == "" {
		return nil, agenterr.Invalidf("an invite needs the name of the agent it is for")
	}
	if !names.Valid(name) {
		return nil, agenterr.Invalidf("an invite name has %s, got %q", names.Rule, name)
	}
	hours := req.Hours
	if hours == 0 {
		hours = control.DefaultInviteHours
	}
	if hours < 1 || hours > control.MaxInviteHours {
		return nil, agenterr.Invalidf("an invite lasts from 1 hour to %d hours, got %d", control.MaxInviteHours, hours)
	}
	// One use is enough: meshnet spends the token only when the agent has
	// no credential it can refresh, and autonomous recovery lets it refresh
	// after the key that signed its credential retired.
	tok, err := s.mesh.CreateBootstrapToken(ctx, meshadmin.BootstrapTokenRequest{
		Role:               meshadmin.RoleNode,
		TTLHours:           hours,
		MaxUsages:          1,
		Description:        invitePrefix + name,
		AutonomousRecovery: true,
	})
	if err != nil {
		return nil, err
	}
	back, err := s.letBack(ctx, name)
	if err != nil {
		return nil, err
	}
	return &control.Invite{ID: tok.ID, Name: name, Token: tok.Token, Expires: tok.Expires, URL: s.status().URL, LetBack: back}, nil
}

// letBack lifts the ban of the removed invited agent named name, which
// keeps its identity, and returns it, or nil when there is none.
func (s *Service) letBack(ctx context.Context, name string) (*control.Member, error) {
	peers, err := s.removed.load()
	if err != nil {
		return nil, err
	}
	peer, ok := peers[name]
	if !ok {
		return nil, nil
	}
	all, err := s.nodes(ctx, true)
	if err != nil {
		return nil, err
	}
	var back *control.Member
	if i := slices.IndexFunc(all, func(m control.Member) bool { return m.Peer == peer }); i >= 0 && all[i].Banned && all[i].Invite {
		ms, err := s.unban(ctx, all[i:i+1])
		if err != nil {
			return nil, fmt.Errorf("letting back %s: %w", name, err)
		}
		back = &ms[0]
	}
	if err := s.removed.update(func(peers map[string]string) { forget(peers, peer) }); err != nil {
		return nil, err
	}
	return back, nil
}

// invitePrefix starts the description of the bootstrap tokens of invites,
// which ends in the invite's name.
const invitePrefix = "a2a-serve invite: "

// Members lists the agents on the relay, with banned the banned ones too,
// and then the unused invites.
func (s *Service) Members(ctx context.Context, banned bool) ([]control.Member, error) {
	ms, err := s.nodes(ctx, banned)
	if err != nil {
		return nil, err
	}
	unused, err := s.unusedInvites(ctx)
	if err != nil {
		return nil, err
	}
	return append(ms, unused...), nil
}

// nodes lists the agents on the relay, and with banned the banned ones too.
func (s *Service) nodes(ctx context.Context, banned bool) ([]control.Member, error) {
	st, err := s.mesh.Status(ctx)
	if err != nil {
		return nil, err
	}
	invites, err := s.inviteNames(ctx, st)
	if err != nil {
		return nil, err
	}
	ms := members(st, banned)
	for i, m := range ms {
		if m.Invite {
			ms[i].InviteName = invites[m.Peer]
		}
	}
	if !banned {
		return ms, nil
	}
	peers, err := s.removed.load()
	if err != nil {
		return nil, err
	}
	for i, m := range ms {
		if m.Banned && m.Agent == "" {
			ms[i].Agent = agentsOf(peers, m.Peer)
		}
	}
	return ms, nil
}

// inviteNames returns the names of the invites the nodes of st joined with,
// by peer. It saves the ones Agent Mesh still reports, and forgets the
// peers Agent Mesh forgot.
func (s *Service) inviteNames(ctx context.Context, st *meshadmin.Status) (map[string]string, error) {
	enrollments, err := s.mesh.Enrollments(ctx)
	if err != nil {
		return nil, err
	}
	tokens, err := s.mesh.BootstrapTokens(ctx)
	if err != nil {
		return nil, err
	}
	byID := map[string]string{}
	for _, t := range tokens {
		if name, ok := strings.CutPrefix(t.Description, invitePrefix); ok {
			byID[t.ID] = name
		}
	}
	known := map[string]bool{}
	for _, n := range st.Nodes {
		known[n.PeerID] = true
	}
	var out map[string]string
	err = s.names.update(func(saved map[string]string) {
		maps.DeleteFunc(saved, func(peer, _ string) bool { return !known[peer] })
		for _, e := range enrollments {
			if name, ok := byID[e.TokenID]; ok && known[e.PeerID] && e.Status == meshadmin.EnrollmentApproved {
				saved[e.PeerID] = name
			}
		}
		out = maps.Clone(saved)
	})
	return out, err
}

// unusedInvites lists the invites that can still join an agent, by expiry.
func (s *Service) unusedInvites(ctx context.Context) ([]control.Member, error) {
	tokens, err := s.mesh.BootstrapTokens(ctx)
	if err != nil {
		return nil, err
	}
	now := s.now()
	var out []control.Member
	for _, t := range tokens {
		name, ok := strings.CutPrefix(t.Description, invitePrefix)
		if !ok || !t.Usable(now) {
			continue
		}
		out = append(out, control.Member{Invite: true, InviteName: name, Unused: true, InviteID: t.ID, Expires: t.Expires})
	}
	slices.SortStableFunc(out, func(a, b control.Member) int { return a.Expires.Compare(b.Expires) })
	return out, nil
}

func members(st *meshadmin.Status, banned bool) []control.Member {
	var out []control.Member
	for _, n := range st.Nodes {
		if n.Role == meshadmin.RoleRouter || n.Banned && !banned {
			continue
		}
		m := control.Member{Peer: n.PeerID, Email: n.Email(), Domain: n.Domain(), Joined: n.Enrolled, Banned: n.Banned}
		m.Invite = m.Email == ""
		if svc, ok := st.Catalog[n.PeerID]; ok {
			var agents []string
			for _, s := range svc.Services {
				if s.Type == meshadmin.ServiceTypeA2A && s.Name != "" {
					agents = append(agents, s.Name)
				}
			}
			slices.Sort(agents)
			m.Agent, m.Seen = strings.Join(slices.Compact(agents), ","), svc.Reported
		}
		out = append(out, m)
	}
	slices.SortStableFunc(out, func(a, b control.Member) int { return a.Joined.Compare(b.Joined) })
	return out
}

// Remove bans the members that target names: the node with that peer ID,
// every node of that email, or the node that serves that agent. It also
// revokes the unused invites with that name or ID, before it bans anyone.
func (s *Service) Remove(ctx context.Context, req control.RemoveRequest) (*control.RemoveResponse, error) {
	target := strings.TrimSpace(req.Target)
	if target == "" {
		return nil, agenterr.Invalidf("name the member to remove: an agent, an invite, an email or a peer ID")
	}
	all, err := s.nodes(ctx, true)
	if err != nil {
		return nil, err
	}
	unused, err := s.unusedInvites(ctx)
	if err != nil {
		return nil, err
	}
	invites := slices.DeleteFunc(unused, func(m control.Member) bool {
		return strings.Contains(target, "@") || m.InviteName != target && m.InviteID != target
	})
	matched, err := match(slices.DeleteFunc(slices.Clone(all), func(m control.Member) bool { return m.Banned }), target)
	if err != nil {
		return nil, err
	}
	if len(matched) == 0 && len(invites) == 0 {
		return nil, &agenterr.NotFoundError{Kind: "member", Name: target}
	}
	if err := s.remember(all, matched); err != nil {
		return nil, err
	}
	revoked, err := s.revoke(ctx, invites)
	if err != nil {
		return nil, err
	}
	removed, err := s.ban(ctx, matched)
	if err != nil {
		if len(revoked) > 0 {
			return nil, fmt.Errorf("revoked the unused invites %s, then failed: %w", inviteIDs(revoked), err)
		}
		return nil, err
	}
	resp := &control.RemoveResponse{Removed: append(revoked, removed...)}
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
func (s *Service) Reconcile(ctx context.Context) (removed, readmitted []control.Member, err error) {
	all, err := s.nodes(ctx, true)
	if err != nil {
		return nil, nil, err
	}
	var refused, admitted []control.Member
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
	if readmitted, err = s.unban(ctx, admitted); err != nil {
		return removed, nil, fmt.Errorf("letting back the people the allowlist admits: %w", err)
	}
	return removed, readmitted, nil
}

// unban lifts the bans of ms and returns them unbanned. Agent Mesh
// announces no unban: routers and agents read the bans again on a
// control-plane sync, which a policy update starts, so it applies the
// policy after.
func (s *Service) unban(ctx context.Context, ms []control.Member) ([]control.Member, error) {
	if len(ms) == 0 {
		return nil, nil
	}
	back := make([]control.Member, 0, len(ms))
	for _, m := range ms {
		if err := s.mesh.Unban(ctx, m.Peer); err != nil {
			return nil, fmt.Errorf("unbanning %s (%s): %w", m.Who(), m.Peer, err)
		}
		m.Banned = false
		back = append(back, m)
	}
	if err := s.ApplyPolicy(ctx); err != nil {
		return nil, err
	}
	return back, nil
}

// remember records the agent names of the invited members among matched,
// which are about to be removed, and forgets the names of peers that all
// doesn't list as removed.
func (s *Service) remember(all, matched []control.Member) error {
	removed := map[string]bool{}
	for _, m := range all {
		removed[m.Peer] = m.Banned
	}
	return s.removed.update(func(peers map[string]string) {
		maps.DeleteFunc(peers, func(_, p string) bool { return !removed[p] })
		for _, m := range matched {
			if !m.Invite || m.Agent == "" {
				continue
			}
			for name := range strings.SplitSeq(m.Agent, ",") {
				peers[name] = m.Peer
			}
		}
	})
}

// forget drops the agent names of peer from peers.
func forget(peers map[string]string, peer string) {
	maps.DeleteFunc(peers, func(_, p string) bool { return p == peer })
}

// agentsOf returns the agent names peers record for peer.
func agentsOf(peers map[string]string, peer string) string {
	var agents []string
	for name, p := range peers {
		if p == peer {
			agents = append(agents, name)
		}
	}
	slices.Sort(agents)
	return strings.Join(agents, ",")
}

// revoke revokes the unused invites ms and returns them. It stops at the
// first failure.
func (s *Service) revoke(ctx context.Context, ms []control.Member) ([]control.Member, error) {
	var done []control.Member
	for _, m := range ms {
		if err := s.mesh.RevokeBootstrapToken(ctx, m.InviteID); err != nil {
			if len(done) > 0 {
				return nil, fmt.Errorf("revoked the unused invites %s, then failed: %w", inviteIDs(done), err)
			}
			return nil, err
		}
		done = append(done, m)
	}
	return done, nil
}

func inviteIDs(ms []control.Member) string {
	ids := make([]string, len(ms))
	for i, m := range ms {
		ids[i] = m.InviteID
	}
	return strings.Join(ids, ", ")
}

// ban bans ms and returns them banned. It stops at the first failure.
func (s *Service) ban(ctx context.Context, ms []control.Member) ([]control.Member, error) {
	var done []control.Member
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

func match(all []control.Member, target string) ([]control.Member, error) {
	var out []control.Member
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
				return []control.Member{m}, nil
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
	return out, nil
}

func peers(ms []control.Member) string {
	ids := make([]string, len(ms))
	for i, m := range ms {
		ids[i] = m.Peer
	}
	return strings.Join(ids, ", ")
}
