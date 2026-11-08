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

package auth

import (
	"slices"
	"strings"

	"github.com/a2aproject/a2a-serve/internal/network"
)

// Roles, from the most rights to the fewest.
const (
	// RoleOwner is the user running the agent: the local token, their
	// devices and their linked chats. It sees every task, answers any
	// approval and uses the inspector.
	RoleOwner = "owner"
	// RoleMember is a caller the owner let in: pairings, private-network
	// members and --allow-email sign-ins. It starts tasks, and sees and
	// approves its own.
	RoleMember = "member"
	// RoleGuest is a caller nobody vouched for, such as an anonymous caller
	// on the public URL or a stranger in a direct chat. It starts tasks in
	// its own conversation, and never answers approvals, calls agents or
	// uses the schedules agent.
	RoleGuest = "guest"
)

// Callers a role can hold.
const (
	// MemberSelf is the user running the agent.
	MemberSelf = "self"
	// MemberPrivate is everyone on the private network.
	MemberPrivate = "private"
	// MemberAnyone is anyone on the public URL and in direct chats.
	MemberAnyone = "anyone"
	// MemberEmailPrefix starts email:<address> and email:*@<domain>.
	MemberEmailPrefix = "email:"
)

// Policy gives the member and guest roles a list of the callers they hold,
// as an Agent Mesh binding does. The owner role holds only the user running the
// agent and owner pairings.
type Policy struct {
	Members []string
	Guests  []string
}

// Resolve returns the role of an identity, or "" if the policy refuses it:
//
//  1. the highest role it matches, across the bindings and its pairing;
//  2. at most member through another agent;
//  3. guest for an identified caller that matches nothing, if guest holds
//     anyone.
func (p Policy) Resolve(id Identity) string {
	role := ""
	switch {
	case matches(MemberSelf, id) || id.PairingRole == RoleOwner:
		role = RoleOwner
	case matchesAny(p.Members, id) || id.PairingRole == RoleMember || id.WebhookNode != "":
		role = RoleMember
	case matchesAny(p.Guests, id):
		role = RoleGuest
	}
	if role == RoleOwner && id.Hops > 0 {
		role = RoleMember
	}
	if role == "" && id.identified() && len(p.Guests) > 0 {
		role = RoleGuest
	}
	return role
}

// AllowsSignIn reports whether one of the policy's email members matches
// a Google sign-in of email, with domain its Workspace domain or empty.
func (p Policy) AllowsSignIn(email, domain string) bool {
	for _, m := range slices.Concat(p.Members, p.Guests) {
		if pattern, ok := strings.CutPrefix(m, MemberEmailPrefix); ok && EmailMatches(pattern, email, domain) {
			return true
		}
	}
	return false
}

func matchesAny(members []string, id Identity) bool {
	for _, m := range members {
		if matches(m, id) {
			return true
		}
	}
	return false
}

func matches(member string, id Identity) bool {
	switch member {
	case MemberSelf:
		return id.Self && id.Network != network.ListenerPublic
	case MemberPrivate:
		return id.Network == network.ListenerPrivate
	case MemberAnyone:
		return id.Network == network.ListenerPublic || (id.Network == NetworkChat && id.Direct)
	}
	pattern, ok := strings.CutPrefix(member, MemberEmailPrefix)
	if !ok || id.Email == "" {
		return false
	}
	return EmailMatches(pattern, id.Email, id.Domain)
}

// EmailMatches reports whether an email matches an --allow-email pattern:
// an address, or *@<domain> for an email whose source proves the domain:
// Google Workspace sign-in, or mail which passed DMARC. Tailnet and relay
// emails come without such a proof, so they match full addresses only.
func EmailMatches(pattern, email, domain string) bool {
	pattern, email = strings.ToLower(strings.TrimSpace(pattern)), strings.ToLower(email)
	if d, ok := strings.CutPrefix(pattern, "*@"); ok {
		return d != "" && strings.EqualFold(domain, d) && strings.HasSuffix(email, "@"+d)
	}
	return pattern == email
}

func anyoneOn(public, chat bool) string {
	switch {
	case public && chat:
		return "anyone on the public URL or in a direct chat"
	case public:
		return "anyone on the public URL"
	case chat:
		return "anyone in a direct chat"
	}
	return "anyone (no public URL or messenger is set up yet)"
}

// Others describes who may call under the policy without a pairing, such
// as "everyone on the tailnet" or "emails matching *@example.com". provider
// is the private network's provider, or empty without one. public and chat
// say whether the agent has a public URL and a messenger, the routes that
// anyone reaches it by.
func (p Policy) Others(provider string, public, chat bool) []string {
	var out []string
	add := func(members []string) {
		for _, m := range members {
			var who string
			switch pattern, email := strings.CutPrefix(m, MemberEmailPrefix); {
			case m == MemberPrivate && provider != "":
				who = "everyone" + where(Identity{Network: network.ListenerPrivate, Provider: provider})
			case m == MemberAnyone:
				who = anyoneOn(public, chat)
			case email:
				who = "emails matching " + pattern
			}
			if who != "" && !slices.Contains(out, who) {
				out = append(out, who)
			}
		}
	}
	add(p.Members)
	add(p.Guests)
	return out
}
