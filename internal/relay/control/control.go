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
	"time"
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
	// ID names the invite in `relay remove` without its token.
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Token   string    `json:"token"`
	Expires time.Time `json:"expires"`
	URL     string    `json:"url"`
	// LetBack is the removed agent of that name that the invite let back
	// with its identity.
	LetBack *Member `json:"letBack,omitempty"`
}

// Member is a node on the relay, or an unused invite that can join one.
type Member struct {
	// Agent names the agents the node serves.
	Agent string `json:"agent,omitempty"`
	// Email is set for a node that signed in, and Domain to the Google
	// Workspace domain of its account.
	Email  string `json:"email,omitempty"`
	Domain string `json:"domain,omitempty"`
	Invite bool   `json:"invite,omitempty"`
	// InviteName is the name of the invite the node joined with, when the
	// relay knows it.
	InviteName string `json:"inviteName,omitempty"`
	// Unused marks an invite no agent joined with yet. It has the ID and
	// expiry of the invite, and no peer.
	Unused   bool      `json:"unused,omitempty"`
	InviteID string    `json:"inviteId,omitempty"`
	Expires  time.Time `json:"expires,omitzero"`
	Joined   time.Time `json:"joined,omitzero"`
	Seen     time.Time `json:"seen,omitzero"`
	Peer     string    `json:"peer,omitempty"`
	Banned   bool      `json:"banned,omitempty"`
}

// Who says how the member joined: its email, or the invite it has.
func (m Member) Who() string {
	if m.Email != "" {
		return m.Email
	}
	if m.InviteName != "" {
		return "invite " + m.InviteName
	}
	return "invite"
}

// RemoveRequest names the members to ban: an agent, an email or a peer ID.
type RemoveRequest struct {
	Target string `json:"target"`
}

// RemoveResponse lists the banned members and the revoked unused invites,
// and the emails among the members that the relay still admits, which it
// lets back when it starts.
type RemoveResponse struct {
	Removed  []Member `json:"removed"`
	Admitted []string `json:"admitted,omitempty"`
}

// Admits reports whether the relay lets a member that signed in stay.
type Admits func(Member) bool

// Service implements the operations of the control API on the network
// behind the relay.
type Service interface {
	// Status returns the relay's status.
	Status(ctx context.Context) (*Status, error)
	// Invite mints an invite for one agent.
	Invite(ctx context.Context, req InviteRequest) (*Invite, error)
	// Members lists the agents on the relay, with banned the removed ones
	// too, and then the unused invites.
	Members(ctx context.Context, banned bool) ([]Member, error)
	// Remove removes the members a target names, and revokes the unused
	// invites it names.
	Remove(ctx context.Context, req RemoveRequest) (*RemoveResponse, error)
}
