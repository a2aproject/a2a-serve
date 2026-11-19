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

// Package contact lets a coding agent call other A2A agents, its contacts,
// through the server that runs it. It serves each contact as an A2A agent
// on the agent API, at http://a2a/<name>, or at http://a2a/<scope>/<name>
// when one process serves many conversations. The server calls the contact
// with its own credentials, so none reach the agent.
//
// Calls carry the hop count of the turn, and a chain of agents ends at a
// limit. A conversation reaches only the tasks it started, and approvals
// that contacts ask go to a [Followup] instead of the agent.
// [Proxy.CancelCalls] cancels the tasks that a turn started when the turn
// is canceled.
package contact

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/a2aproject/a2a-serve/core/approval"
	"github.com/a2aproject/a2a-serve/internal/hops"
)

// MaxHops is the longest chain of agents calling agents: a turn that came
// through as many agents can't call another one.
const MaxHops = hops.Max

// HeaderHops is the header that calls to contacts carry, with the number
// of agents a request passed through.
const HeaderHops = hops.Header

// Turn is the running turn of a conversation, which a call to a contact is
// made in.
type Turn struct {
	// ContextID is the A2A context of the conversation, and TaskID the task
	// of the turn, which the calls it starts are recorded for.
	ContextID string
	TaskID    string
	// Hops is the number of agents the turn's message passed through.
	Hops int
	// Metadata is added to the metadata of the requests that send messages
	// to contacts and cancel their tasks, over what the agent set, such as
	// the context ID of the conversation for a host that attributes calls.
	Metadata map[string]any
	// Caller is the embedder's own data about the turn, such as who it runs
	// for. The proxy passes it to the Directory and the Followup untouched.
	Caller any
}

// Turns tells which turn a request on the agent API is made in.
type Turns interface {
	// Turn returns the running turn of the conversation that r comes from.
	// scope is the first path segment of a [Config.Scoped] proxy, and ""
	// otherwise. It returns an error that matches [ErrUnauthenticated] when
	// r proves no agent process, [ErrNotFound] for a scope the process
	// doesn't have, [ErrNoTurn] when no turn runs, and [ErrRefused] when the
	// turn may call no contact. The agent reads the error's message.
	Turn(r *http.Request, scope string) (Turn, error)
}

// Directory resolves the names that the agent calls.
type Directory interface {
	// Card returns the card of the contact name, or nil when it has none,
	// without reaching the contact. The proxy shortens its text before the
	// agent reads it. It returns an error that matches [ErrRefused] for a
	// name that t may not call; the agent reads its message.
	Card(ctx context.Context, t Turn, name string) (*a2a.AgentCard, error)
	// Target returns the contact name for calls in t, and reaches it if it
	// has to.
	Target(ctx context.Context, t Turn, name string) (Target, error)
}

// Target is a contact that the proxy calls. Build one with [Client],
// [Remote] or [Local].
type Target struct {
	handler a2asrv.RequestHandler
	// remote is set for a handler that sends the service params of each
	// request's context, as a2aclient does.
	remote bool
	client *a2aclient.Client
	params a2aclient.ServiceParams
}

// Client calls a remote contact through c, with the service params of
// params on each request, such as headers the contact expects. The hop
// count goes in [HeaderHops].
func Client(c *a2aclient.Client, params a2aclient.ServiceParams) Target {
	return Target{client: c, params: params}
}

// Remote calls a remote contact through h, which sends the service params
// of each request's context as headers, as a2aclient does. The hop count
// goes in [HeaderHops].
func Remote(h a2asrv.RequestHandler) Target {
	return Target{handler: h, remote: true}
}

// Local calls a contact in-process through h, which gets no hop header:
// h learns the hop count of the turn from the Directory that built it.
func Local(h a2asrv.RequestHandler) Target {
	return Target{handler: h}
}

// Calls records the tasks that conversations start at contacts, so that a
// conversation reaches only its own and a canceled turn cancels its own.
type Calls interface {
	// Record notes that the turn t started c.
	Record(ctx context.Context, t Turn, c Call) error
	// Started reports whether the conversation of t started the task id at
	// the contact.
	Started(ctx context.Context, t Turn, contact string, id a2a.TaskID) (bool, error)
	// InContext reports whether the conversation of t started a task in the
	// context contextID of the contact.
	InContext(ctx context.Context, t Turn, contact, contextID string) (bool, error)
	// List returns the IDs of the tasks that the conversation of t started
	// at the contact, newest first; in the contact's context contextID
	// unless it is "".
	List(ctx context.Context, t Turn, contact, contextID string) ([]a2a.TaskID, error)
	// OfTurn returns the calls that the turn t started.
	OfTurn(ctx context.Context, t Turn) ([]Call, error)
}

// Call is a task that a turn started at a contact.
type Call struct {
	Contact string
	TaskID  a2a.TaskID
	// ContextID is the contact's context of the task.
	ContextID string
}

// Followup takes the tasks that outlive the calls that started them: a
// task the agent didn't wait for, one whose stream it dropped, and one that
// waits for an approval. The agent may not answer an approval, so the user
// or the host answers it, with [Dial] and [approval.Answer].
type Followup interface {
	// Follow takes a task of the turn t, once per call that leaves it
	// unfinished. ctx may outlive the call.
	Follow(ctx context.Context, t Turn, o Outstanding)
}

// Outstanding is a contact's task that a call left unfinished.
type Outstanding struct {
	Contact string
	Task    *a2a.Task
	// Approval is what the task waits for an answer to, or nil while it
	// runs.
	Approval *approval.Request
}

// Errors of [Turns] and [Directory], which the proxy answers with. Wrap
// them to word the message that the agent reads.
var (
	// ErrUnauthenticated answers 401: the request proves no agent process.
	ErrUnauthenticated = errors.New("agent key required")
	// ErrNotFound answers 404: the process has no such scope.
	ErrNotFound = errors.New("unknown contacts URL")
	// ErrNoTurn answers 403: no turn runs in the conversation.
	ErrNoTurn = errors.New("this conversation has no running turn")
	// ErrRefused refuses the call: the proxy serves the contact's card with
	// the refusal and fails each call with it.
	ErrRefused = errors.New("call refused")
)

// Refusef returns an error that matches [ErrRefused], with the message
// that the agent reads.
func Refusef(format string, args ...any) error {
	return &refusal{msg: fmt.Sprintf(format, args...)}
}

type refusal struct{ msg string }

func (r *refusal) Error() string { return r.msg }

func (r *refusal) Is(target error) bool { return target == ErrRefused }
