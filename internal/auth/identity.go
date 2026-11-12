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

// NetworkChat is the network of messenger messages.
const NetworkChat = "chat"

// Sources of a verified email.
const (
	// EmailFromNetwork is the login of a tailnet user or a relay principal.
	EmailFromNetwork = "network"
	EmailFromGoogle  = "google"
	// EmailFromMail is the sender of mail which passed DMARC at the
	// trusted mail server.
	EmailFromMail = "mail"
)

// Identity is what a request proves about its caller: verified claims, and
// where each came from. The policy gives it a role.
type Identity struct {
	// Network is one of the network.Listener* kinds, or NetworkChat.
	Network string `json:"network,omitempty"`
	// Provider is the network provider, such as tailscale, or the messenger.
	Provider string `json:"provider,omitempty"`
	Email    string `json:"email,omitempty"`
	// EmailSource is one of the EmailFrom* sources.
	EmailSource string `json:"emailSource,omitempty"`
	// Domain is the domain of Email when its source proves it: the Google
	// Workspace domain, or the domain which passed DMARC.
	Domain string `json:"domain,omitempty"`
	// Node is the calling machine: a tailnet StableID or a relay peer ID.
	Node string `json:"node,omitempty"`
	// Pairing is the pairing whose token or chat link was used, and
	// PairingRole its role.
	Pairing     string `json:"pairing,omitempty"`
	PairingRole string `json:"pairingRole,omitempty"`
	// WebhookNode is the webhook node whose token was used.
	WebhookNode string `json:"webhookNode,omitempty"`
	// ChatAccount is the sender of a messenger message: telegram:<id>,
	// discord:<id> or an email address.
	ChatAccount string `json:"chatAccount,omitempty"`
	// Direct is true for a message in a direct chat.
	Direct bool `json:"direct,omitempty"`
	// Self is true for the user running the agent: on this machine, on
	// their own devices on the private network, or in their linked chats.
	Self bool `json:"self,omitempty"`
	// Hops is the number of agent servers the request passed through.
	Hops int `json:"hops,omitempty"`
	// Caller is the opaque ID that the calling agent gives the person a
	// call is for, with Hops above 0. It keeps their agent processes
	// apart, and never decides access.
	Caller string `json:"caller,omitempty"`
}

// identified reports whether the identity holds a verified claim beyond
// the network it came from.
func (id Identity) identified() bool {
	return id.Self || id.Email != "" || id.Node != "" || id.Pairing != "" || id.WebhookNode != "" || id.ChatAccount != ""
}

// PrincipalID is the principal the identity acts as, which owns its tasks,
// approvals and schedules. A call through another agent never acts as the
// user running this one.
func (id Identity) PrincipalID() string {
	switch {
	case id.Self && id.Hops == 0:
		return OwnerID
	case id.Pairing != "":
		return PairPrincipal(id.Pairing)
	case id.WebhookNode != "":
		return NodePrincipal(id.WebhookNode)
	case id.Email != "":
		return EmailPrefix + strings.ToLower(id.Email)
	case id.Node != "":
		return NetworkNodePrefix + id.Provider + ":" + id.Node
	case id.ChatAccount != "":
		return id.ChatAccount
	}
	return ""
}

// Principal prefixes.
const (
	PairPrefix  = "pair:"
	EmailPrefix = "email:"
	// NetworkNodePrefix starts the principals of machines on the private
	// network: node:<provider>:<id>.
	NetworkNodePrefix = "node:"
)

// PairPrincipal is the principal of the pairing name.
func PairPrincipal(name string) string { return PairPrefix + name }

// DisplayPrincipal is a principal's name for people: pair:anna is anna.
func DisplayPrincipal(id string) string {
	if id == "" {
		return "A caller"
	}
	_, name, ok := strings.Cut(id, ":")
	if !ok {
		return id
	}
	return name
}

// IdentityOf rebuilds the identity of a stored principal ID, for
// principals stored before their identity was. It holds what the ID itself
// proves: the user running the agent, a pairing, an email or a machine.
func IdentityOf(id string) Identity {
	switch {
	case id == OwnerID:
		return Identity{Self: true}
	case strings.HasPrefix(id, PairPrefix):
		return Identity{Pairing: strings.TrimPrefix(id, PairPrefix)}
	case strings.HasPrefix(id, EmailPrefix):
		return Identity{Email: strings.TrimPrefix(id, EmailPrefix)}
	}
	if rest, ok := strings.CutPrefix(id, NetworkNodePrefix); ok {
		if provider, node, ok := strings.Cut(rest, ":"); ok {
			return Identity{Network: network.ListenerPrivate, Provider: provider, Node: node}
		}
		return Identity{WebhookNode: rest}
	}
	return Identity{}
}

// Describe names the identity for people, such as "anna@example.com on the
// tailnet", or "an agent of anna@example.com on the relay" for a call from
// another agent.
func (id Identity) Describe() string {
	if id.Hops > 0 {
		return id.describeAgent() + where(id)
	}
	who := id.Email
	switch {
	case id.Self:
		who = "you"
	case id.Pairing != "":
		who = "the pairing " + id.Pairing
	case id.WebhookNode != "":
		who = "the webhook node " + id.WebhookNode
	case who == "" && id.ChatAccount != "":
		who = id.ChatAccount
	case who == "" && id.Node != "":
		who = "the machine " + id.Node
	case who == "":
		who = "an unknown caller"
	}
	switch {
	case id.EmailSource == EmailFromGoogle && who == id.Email:
		return who + " with Google sign-in"
	case id.EmailSource == EmailFromMail && who == id.Email:
		return who + " by email"
	}
	return who + where(id)
}

// describeAgent names the agent server a call came from by what the call
// proves about it.
func (id Identity) describeAgent() string {
	switch {
	case id.Self:
		return "one of your agents"
	case id.Pairing != "":
		return "an agent with the pairing " + id.Pairing
	case id.WebhookNode != "":
		return "the webhook node " + id.WebhookNode
	case id.Email != "":
		return "an agent of " + id.Email
	case id.ChatAccount != "":
		return "an agent of " + id.ChatAccount
	case id.Node != "":
		return "the agent on the machine " + id.Node
	}
	return "an unknown agent"
}

func where(id Identity) string {
	switch id.Network {
	case network.ListenerLocal:
		return " on this machine"
	case network.ListenerPublic:
		return " on the public URL"
	case NetworkChat:
		return " on " + id.Provider
	case network.ListenerPrivate:
		switch id.Provider {
		case network.ProviderTailscale:
			return " on the tailnet"
		case network.ProviderAgentMesh:
			return " on the relay"
		}
		return " on the private network"
	}
	return ""
}

// DomainOf returns the domain of an email address, in lower case.
func DomainOf(email string) string {
	_, domain, _ := strings.Cut(email, "@")
	return strings.ToLower(domain)
}
