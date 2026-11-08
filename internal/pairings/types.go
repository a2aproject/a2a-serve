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

package pairings

import "time"

// InviteRequest asks for a pairing code, for a new pairing or another
// device or account of an existing one.
//
// With Webhook, Name is a new webhook node. A Name which already is a node
// gets a new node code.
type InviteRequest struct {
	Name string
	// Owner makes a new pairing an owner.
	Owner       bool
	Webhook     string
	Headers     map[string]string
	Description string
	// Advertised, when set, advertises the webhook node to the agents of the
	// private network, or stops advertising it.
	Advertised *bool
}

// Code is a pairing code and the agent URL it is redeemed at.
type Code struct {
	Code    string
	Expires time.Time
	URL     string
	// Advertised is set when the request advertised the node.
	Advertised bool
	// Hints tell a person how to link a messenger account with the code.
	Hints []string
	// Node is set for node codes.
	Node *Node
}

// Node describes a webhook node.
type Node struct {
	ID      string
	Name    string
	URL     string
	Webhook string
	Paired  bool
	Created time.Time
}

// Pairing describes someone paired with the agent: a person, a device,
// another agent or a webhook node.
type Pairing struct {
	Name string
	// Kind is one of the pairapi.Pairing* kinds, or empty until the first
	// code is redeemed.
	Kind string
	// Role is authenticated or owner.
	Role      string
	Principal string
	Created   time.Time
	LastUsed  time.Time
	Tokens    []Token
	Accounts  []Account
	// PendingCodes are the expiry times of the codes not used yet. Codes
	// are stored hashed, so they can't be shown again.
	PendingCodes []time.Time
	Node         *NodeDetails
	// Allowed are the tool calls the pairing allowed in all its
	// conversations.
	Allowed []string
}

// Token describes a token issued to a pairing.
type Token struct {
	URL      string
	Created  time.Time
	LastUsed time.Time
}

// Account is a messenger account linked to a pairing.
type Account struct {
	Messenger string
	Account   string
}

// NodeDetails describe the webhook of a node pairing and its mailbox.
type NodeDetails struct {
	URL         string
	Webhook     string
	Description string
	// HeaderNames are the names of the headers sent with every wake.
	HeaderNames []string
	Advertised  bool
	Paired      bool
	// Waiting counts the wakes not delivered yet, and Open the tasks
	// waiting for a reply. Both are zero while the agent is stopped.
	Waiting int
	Open    int
}

// UpdateRequest changes a pairing in place. Unset fields are kept.
type UpdateRequest struct {
	// Owner, when set, makes the pairing an owner or not.
	Owner *bool
	// Kind changes the kind of a pairing other than a webhook node.
	Kind        string
	Webhook     string
	Headers     map[string]string
	Description *string
	Advertised  *bool
	// Rotate revokes every token of the pairing and issues a new code.
	Rotate bool
}

// UpdateResult is the changed pairing, and the new code of a rotation.
type UpdateResult struct {
	Pairing Pairing
	Code    *Code
}

// RemoveResult tells what [Service.Remove] removed.
type RemoveResult struct {
	// Kind is the kind of the pairing.
	Kind string
	// Kept is set when the pairing stays and only its credentials were
	// revoked, as for the local pairing.
	Kept bool
}

// RemovalPreview tells what [Service.Remove] would remove.
type RemovalPreview struct {
	// Kind is the kind of the pairing.
	Kind string
	// Kept is set when the pairing stays and only its credentials would be
	// revoked, as for the local pairing.
	Kept   bool
	Tokens int
	// Accounts are the messenger accounts linked to the pairing.
	Accounts []Account
	// Codes is the number of pairing codes that would be deleted.
	Codes int
	// Node is set when a webhook node would be removed.
	Node      bool
	Schedules int
	Watches   int
	Approvals int
	Tasks     int
	// RunningTasks is the number of Tasks that would be canceled first.
	RunningTasks  int
	Conversations int
	// Allowed is the number of tool calls allowed in all conversations.
	Allowed int
	// AgentHomes are the agent home directories that would be deleted.
	AgentHomes []string
	// Workspaces are the directories of private workspaces, such as an
	// own copy of the workspace, that would be deleted.
	Workspaces []string
}

// SignOutResult tells what [Service.SignOut] revoked.
type SignOutResult struct {
	Email string
	// Tokens is the number of Google sign-in tokens revoked.
	Tokens int
}
