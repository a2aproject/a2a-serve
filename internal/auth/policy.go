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
	"strings"

	"github.com/a2aproject/a2a-serve/internal/network"
)

// Roles, from the most rights to the fewest.
const (
	// RoleOwner sees every task, answers any approval and uses the inspector.
	RoleOwner = "owner"
	// RoleAuthenticated starts tasks, and sees and approves its own.
	RoleAuthenticated = "authenticated"
	// RoleAnonymous starts tasks in its own conversation, and never answers
	// approvals, calls agents or uses the schedules agent.
	RoleAnonymous = "anonymous"
)

// Members of a role.
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

// Policy gives each role a list of members, as an Agent Mesh binding does.
type Policy struct {
	Owner         []string
	Authenticated []string
	Anonymous     []string
}

// DefaultPolicy is the policy of an agent: the user running it is owner;
// everyone on the private network, if privateEveryone, and the emails of
// allowEmails are authenticated.
func DefaultPolicy(privateEveryone bool, allowEmails []string) Policy {
	p := Policy{Owner: []string{MemberSelf}}
	if privateEveryone {
		p.Authenticated = append(p.Authenticated, MemberPrivate)
	}
	for _, e := range allowEmails {
		if e = strings.TrimSpace(e); e != "" {
			p.Authenticated = append(p.Authenticated, MemberEmailPrefix+e)
		}
	}
	return p
}

// Resolve returns the role of an identity, or "" if the policy refuses it:
//
//  1. the highest role it matches, across the bindings and its pairing;
//  2. at most authenticated through another agent;
//  3. anonymous for an identified caller that matches nothing, if anonymous
//     has members.
func (p Policy) Resolve(id Identity) string {
	role := ""
	switch {
	case matchesAny(p.Owner, id) || id.PairingRole == RoleOwner:
		role = RoleOwner
	case matchesAny(p.Authenticated, id) || id.PairingRole == RoleAuthenticated || id.WebhookNode != "":
		role = RoleAuthenticated
	case matchesAny(p.Anonymous, id):
		role = RoleAnonymous
	}
	if role == RoleOwner && id.Hops > 0 {
		role = RoleAuthenticated
	}
	if role == "" && id.identified() && len(p.Anonymous) > 0 {
		role = RoleAnonymous
	}
	return role
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
// an address, or *@<domain> for an email whose source proves domain, such
// as a Google Workspace or mail which passed DMARC.
func EmailMatches(pattern, email, domain string) bool {
	pattern, email = strings.ToLower(strings.TrimSpace(pattern)), strings.ToLower(email)
	if d, ok := strings.CutPrefix(pattern, "*@"); ok {
		return d != "" && strings.EqualFold(domain, d) && strings.HasSuffix(email, "@"+d)
	}
	return pattern == email
}
