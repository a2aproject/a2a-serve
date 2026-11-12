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
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"slices"
	"strings"
	"time"

	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/names"
	"github.com/a2aproject/a2a-serve/internal/relay/control"
)

// invitePrefix starts the token of an invite to a SLIM relay.
const invitePrefix = "slim-invite-"

// service implements the control API over the relay's store, and closes
// the connections of the members it removes.
type service struct {
	store  *store
	conns  *conns
	status func() control.Status
	now    func() time.Time
}

var _ control.Service = (*service)(nil)

func (s *service) Status(context.Context) (*control.Status, error) {
	st := s.status()
	snap := s.store.snapshot()
	st.Members = countFunc(snap.Members, func(m member) bool { return !m.removed() })
	return &st, nil
}

func (s *service) Invite(_ context.Context, req control.InviteRequest) (*control.Invite, error) {
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
	if _, ok := s.activeAgent(name); ok {
		return nil, agenterr.Conflictf("an agent named %s is on the relay; remove it first with `a2a-serve relay remove %s`, or pick another name", name, name)
	}
	secret := make([]byte, 32)
	id := make([]byte, 4)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	if _, err := rand.Read(id); err != nil {
		return nil, err
	}
	token := invitePrefix + base64.RawURLEncoding.EncodeToString(secret)
	inv := invite{ID: hex.EncodeToString(id), Name: name, Hash: hashToken(token), Expires: s.now().Add(time.Duration(hours) * time.Hour)}
	if err := s.store.addInvite(inv); err != nil {
		return nil, err
	}
	return &control.Invite{ID: inv.ID, Name: name, Token: token, Expires: inv.Expires, URL: s.status().URL}, nil
}

func (s *service) Members(_ context.Context, banned bool) ([]control.Member, error) {
	snap := s.store.snapshot()
	var out []control.Member
	for _, m := range snap.Members {
		if m.removed() && !banned {
			continue
		}
		out = append(out, controlMember(m, s.conns.seen(m.Agent)))
	}
	slices.SortStableFunc(out, func(a, b control.Member) int { return a.Joined.Compare(b.Joined) })
	invites := slices.Clone(snap.Invites)
	slices.SortStableFunc(invites, func(a, b invite) int { return a.Expires.Compare(b.Expires) })
	now := s.now()
	for _, inv := range invites {
		if now.Before(inv.Expires) {
			out = append(out, control.Member{Invite: true, InviteName: inv.Name, Unused: true, InviteID: inv.ID, Expires: inv.Expires})
		}
	}
	return out, nil
}

func (s *service) Remove(_ context.Context, req control.RemoveRequest) (*control.RemoveResponse, error) {
	target := strings.TrimSpace(req.Target)
	if target == "" {
		return nil, agenterr.Invalidf("name the member to remove: an agent or an invite")
	}
	ms, invs, err := s.store.remove(target)
	if err != nil {
		return nil, err
	}
	resp := &control.RemoveResponse{}
	for _, inv := range invs {
		resp.Removed = append(resp.Removed, control.Member{Invite: true, InviteName: inv.Name, Unused: true, InviteID: inv.ID, Expires: inv.Expires})
	}
	for _, m := range ms {
		s.conns.closeAgent(m.Agent)
		resp.Removed = append(resp.Removed, controlMember(m, time.Time{}))
	}
	return resp, nil
}

// activeAgent returns the key of the active member named agent.
func (s *service) activeAgent(agent string) (member, bool) {
	snap := s.store.snapshot()
	i, ok := snap.active(agent)
	if !ok {
		return member{}, false
	}
	return snap.Members[i], true
}

// memberKey returns the agent and key of the active member with slimName.
func (s *service) memberKey(slimName string) (string, []byte, bool) {
	agent, ok := strings.CutPrefix(slimName, org+"/"+namespace+"/")
	if !ok {
		return "", nil, false
	}
	m, ok := s.activeAgent(agent)
	if !ok {
		return "", nil, false
	}
	return m.Agent, m.Key, true
}

func controlMember(m member, seen time.Time) control.Member {
	return control.Member{
		Agent: m.Agent, Invite: true, InviteName: m.InviteName, InviteID: m.InviteID,
		Joined: m.Joined, Seen: seen, Peer: SLIMName(m.Agent), Banned: m.removed(),
	}
}
