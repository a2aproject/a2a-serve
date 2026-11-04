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

// Package access turns the access settings of an agent into the policy in
// force, and describes who may call the agent under it.
package access

import (
	"cmp"
	"fmt"
	"strings"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/network"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// Network is what the policy and its description depend on in the
// networks of an agent.
type Network struct {
	// Private is the provider of the private network, or empty without one.
	Private string
	// Public reports whether the agent has a public URL.
	Public bool
	// SignIn reports whether the private network admits people by sign-in,
	// which makes an unset --private-access paired.
	SignIn bool
}

// NetworkOf returns the networks cfg configures. It can't tell whether a
// relay admits people by sign-in, so SignIn is left to the caller.
func NetworkOf(cfg *agentdir.Config) Network {
	n := Network{Public: cfg.Network.Public != ""}
	switch p := cfg.Network.Private; p {
	case "":
	case agentdir.NetworkTailscale:
		n.Private = network.ProviderTailscale
	default:
		n.Private = network.ProviderRelay
	}
	return n
}

// PrivateAccess is the --private-access in force: the setting, or by
// default everyone, unless the private network admits people by sign-in.
func PrivateAccess(p agentdir.PolicyConfig, signIn bool) string {
	switch {
	case p.PrivateAccess != "":
		return p.PrivateAccess
	case signIn:
		return agentdir.AccessPaired
	}
	return agentdir.AccessEveryone
}

// Policy is the policy of the settings p: the user running the agent and
// the --owner members are owners; the private network with
// --private-access everyone, --allow-email and the --allow members are
// authenticated; with --public-access anyone, anyone is anonymous.
func Policy(p agentdir.PolicyConfig, signIn bool) auth.Policy {
	out := auth.Policy{Owner: append([]string{auth.MemberSelf}, p.Owner...)}
	if PrivateAccess(p, signIn) == agentdir.AccessEveryone {
		out.Authenticated = append(out.Authenticated, auth.MemberPrivate)
	}
	for _, e := range p.AllowEmails {
		if e = strings.TrimSpace(e); e != "" {
			out.Authenticated = append(out.Authenticated, auth.MemberEmailPrefix+e)
		}
	}
	out.Authenticated = append(out.Authenticated, p.Allow...)
	if p.PublicAccess == agentdir.AccessAnyone {
		out.Anonymous = []string{auth.MemberAnyone}
	}
	return out
}

// Pairings counts the pairings of an agent besides the local one.
type Pairings struct {
	All, Owners int
}

// CountPairings counts the pairings of ps besides the local one.
func CountPairings(ps []store.Pairing) Pairings {
	var n Pairings
	for _, p := range ps {
		if p.Name == auth.LocalPairing {
			continue
		}
		n.All++
		if p.Role == auth.RoleOwner {
			n.Owners++
		}
	}
	return n
}

// Describe says who may call an agent with the settings p on the networks
// net, which has the pairings n.
func Describe(p agentdir.PolicyConfig, net Network, n Pairings) *controlapi.Access {
	pol := Policy(p, net.SignIn)
	a := &controlapi.Access{
		Owner: pol.Owner, Authenticated: nonNil(pol.Authenticated), Anonymous: nonNil(pol.Anonymous),
		Pairings: n.All, OwnerPairings: n.Owners, Others: pol.Others(net.Private),
	}
	if net.Private != "" {
		a.PrivateAccess = PrivateAccess(p, net.SignIn)
	}
	if net.Public {
		a.PublicAccess = cmp.Or(p.PublicAccess, agentdir.AccessPaired)
	}
	a.Summary = []string{"you: " + yourDevices(net.Private), others(a.Others, n)}
	if a.PrivateAccess == agentdir.AccessPaired {
		line := networkName(net.Private) + " needs a pairing"
		if p.PrivateAccess == "" {
			line += ", since it admits people by sign-in (--private-access everyone opens it)"
		}
		a.Summary = append(a.Summary, line)
	}
	switch {
	case !net.Public:
	case a.PublicAccess == agentdir.AccessAnyone:
		a.Summary = append(a.Summary, "the public URL is open to anyone")
	case len(p.Emails()) > 0:
		a.Summary = append(a.Summary, "the public URL needs a pairing or Google sign-in")
	default:
		a.Summary = append(a.Summary, "the public URL needs a pairing")
	}
	return a
}

// yourDevices names where the user running the agent is an owner.
func yourDevices(provider string) string {
	switch provider {
	case network.ProviderTailscale:
		return "this machine and your tailnet devices"
	case network.ProviderRelay:
		return "this machine and your devices on the relay"
	}
	return "this machine"
}

func others(who []string, n Pairings) string {
	if n.All > 0 {
		p := fmt.Sprintf("%d %s", n.All, plural(n.All, "pairing"))
		if n.Owners > 0 {
			p += fmt.Sprintf(" (%d %s)", n.Owners, plural(n.Owners, "owner"))
		}
		who = append(who, p)
	}
	if len(who) == 0 {
		return "nobody else"
	}
	return strings.Join(who, ", ")
}

func networkName(provider string) string {
	switch provider {
	case network.ProviderTailscale:
		return "the tailnet"
	case network.ProviderRelay:
		return "the relay"
	}
	return "the private network"
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
