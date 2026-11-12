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

// Package netprovider is the Go SDK of a2a-serve network providers: programs
// that put an agent on a network, such as an Agent Mesh relay or a SLIM node,
// and that the server runs and controls over stdio.
//
// A provider implements [Provider] and calls [Main]. The server sends it
// JSON-RPC 2.0 requests, one per line on stdin, and reads the responses and
// notifications on stdout. Inbound connections reach the server on Unix
// sockets with a PROXY v2 header that names the caller ([Header]); the
// server's outbound calls arrive through a CONNECT tunnel that the SDK serves
// with [Provider.Outbound]. docs/reference/network-providers.md describes the
// protocol for providers in other languages.
package netprovider

import (
	"errors"
	"fmt"
)

// ProtocolVersion is the version of the protocol this SDK speaks.
const ProtocolVersion = 1

// Methods the server calls.
const (
	MethodDescribe = "describe"
	MethodJoin     = "join"
	MethodListen   = "listen"
	MethodMember   = "member"
	MethodPeers    = "peers"
)

// Notifications the provider sends.
const (
	NotifyNotice = "notice"
	NotifyHealth = "health"
)

// Description is what a provider says about itself in reply to describe.
type Description struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	Protocol int    `json:"protocol"`
	// Hosts are the hosts of the URLs the provider gives agents. The server
	// sends its requests to them through the provider's CONNECT tunnel.
	Hosts []string `json:"hosts"`
}

// JoinRequest asks the provider to join the network.
type JoinRequest struct {
	// Agent is the agent's name, Description its card description.
	Agent       string `json:"agent"`
	Description string `json:"description,omitempty"`
	// URL is the --private or --public value, with provider settings in its
	// query.
	URL string `json:"url"`
	// Token is the --network-token, empty without one.
	Token string `json:"token,omitempty"`
	// StateDir is where the provider keeps the agent's identity on the
	// network, so that it keeps its URL across restarts.
	StateDir string `json:"state_dir"`
	// Private and Public select the URLs the agent wants.
	Private bool `json:"private"`
	Public  bool `json:"public"`
}

// JoinResult is the agent's identity on the network.
type JoinResult struct {
	// Node identifies this agent's machine as [Caller.Node] identifies
	// others, so that the server can tell itself apart in peers.
	Node string `json:"node,omitempty"`
	// Owner is the verified email the agent signed in to the network with,
	// empty when it joined with a token. The server treats callers with the
	// same email as the agent's owner.
	Owner string `json:"owner,omitempty"`
}

// ListenRequest tells the provider where to forward inbound connections and
// where to serve the CONNECT tunnel.
type ListenRequest struct {
	// PrivateSocket and PublicSocket are the server's sockets for callers
	// on the private network and on the public URL; empty when the agent
	// doesn't want the URL.
	PrivateSocket string `json:"private_socket,omitempty"`
	PublicSocket  string `json:"public_socket,omitempty"`
	// Token goes into the header of every inbound connection.
	Token string `json:"token"`
	// ConnectSocket is where the provider serves the CONNECT tunnel.
	ConnectSocket string `json:"connect_socket"`
}

// ListenResult are the agent's URLs and the token of the CONNECT tunnel.
type ListenResult struct {
	PrivateURL string `json:"private_url,omitempty"`
	PublicURL  string `json:"public_url,omitempty"`
	// PrivateNote tells people where the private URL works, when that isn't
	// obvious, such as only from other agents on the network.
	PrivateNote string `json:"private_note,omitempty"`
	// ConnectToken authenticates the server's CONNECT requests.
	ConnectToken string `json:"connect_token"`
}

// MemberRequest asks for a machine of the network by its node.
type MemberRequest struct {
	Node string `json:"node"`
}

// PeersResult lists other agents on the network.
type PeersResult struct {
	Peers []PeerInfo `json:"peers"`
}

// PeerInfo is another agent on the network. The server fetches its card.
type PeerInfo struct {
	// Name is the agent's name on the network.
	Name string `json:"name"`
	// URL is the agent's private URL, on one of [Description.Hosts].
	URL         string `json:"url"`
	Description string `json:"description,omitempty"`
	// Node identifies the agent's machine, as [Caller.Node] does.
	Node string `json:"node,omitempty"`
}

// Caller is who a connection comes from, as the network proves it.
type Caller struct {
	// Node identifies the calling machine on the network.
	Node string `json:"node,omitempty"`
	// Email is the verified email of the person the machine belongs to.
	Email string `json:"email,omitempty"`
	// Machine is the machine's name for people.
	Machine string `json:"machine,omitempty"`
	// ClientKey identifies the caller for per-caller limits, such as the
	// address a public request came from. Callers that share it share the
	// limits.
	ClientKey string `json:"client_key,omitempty"`
}

// Identified reports whether c names who is calling, beyond a client key.
func (c Caller) Identified() bool { return c.Node != "" || c.Email != "" || c.Machine != "" }

// Notice is a message for the user, such as a sign-in link.
type Notice struct {
	Message string `json:"message"`
}

// Health states.
const (
	HealthOK           = ""
	HealthDisconnected = "disconnected"
	HealthRemoved      = "removed"
)

// Health says whether the network reaches the agent, and what the user can
// do when it doesn't.
type Health struct {
	State   string `json:"state,omitempty"`
	Problem string `json:"problem,omitempty"`
	Fix     string `json:"fix,omitempty"`
}

// Error kinds, in the data of JSON-RPC errors.
const (
	// KindUnsupported is a method the provider doesn't implement.
	KindUnsupported = "unsupported"
	// KindNotMember is a node the network doesn't have.
	KindNotMember = "not_member"
	// KindToken is a join the network refused because of the network token.
	KindToken = "token"
	// KindRemoved is a join the network refused because it removed the
	// agent.
	KindRemoved = "removed"
)

// Error is an error a provider returns with a kind the server acts on.
type Error struct {
	Kind string
	// Problem says what is wrong; Fix, if set, what the user can do.
	Problem string
	Fix     string
}

func (e *Error) Error() string {
	problem := e.Problem
	if problem == "" {
		problem = kindMessage(e.Kind)
	}
	if e.Fix == "" {
		return problem
	}
	return problem + "; " + e.Fix
}

// Is matches errors of the same kind, so that errors.Is(err,
// ErrUnsupported) holds for any unsupported error.
func (e *Error) Is(target error) bool {
	var t *Error
	return errors.As(target, &t) && t.Kind == e.Kind && t.Problem == "" && t.Fix == ""
}

var (
	// ErrUnsupported is returned for methods a provider doesn't implement.
	ErrUnsupported error = &Error{Kind: KindUnsupported}
	// ErrNotMember is returned by Member for a node the network doesn't
	// have.
	ErrNotMember error = &Error{Kind: KindNotMember}
)

// TokenError returns an error for a join the network refused because of the
// network token.
func TokenError(problem, fix string) error {
	return &Error{Kind: KindToken, Problem: problem, Fix: fix}
}

// RemovedError returns an error for a join the network refused because it
// removed the agent.
func RemovedError(problem, fix string) error {
	return &Error{Kind: KindRemoved, Problem: problem, Fix: fix}
}

func kindMessage(kind string) string {
	switch kind {
	case KindUnsupported:
		return "not supported by this network provider"
	case KindNotMember:
		return "the network has no such machine"
	}
	return fmt.Sprintf("network provider error (%s)", kind)
}
