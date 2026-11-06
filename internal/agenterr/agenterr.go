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

// Package agenterr defines the errors of managing an agent that mean the
// same whether the agent runs or not. The server and its control client map
// them to HTTP statuses and back, and the CLI classifies them.
package agenterr

import (
	"errors"
	"fmt"
)

// NotFoundError is a request that names something the agent doesn't have,
// such as a pairing or a channel.
type NotFoundError struct {
	// Kind is what the request names: pairing, contact, schedule or channel.
	Kind string
	Name string
}

func (e *NotFoundError) Error() string { return "no " + e.Kind + " " + e.Name }

// InvalidError is a request with invalid arguments.
type InvalidError struct{ Err error }

func (e *InvalidError) Error() string { return e.Err.Error() }

func (e *InvalidError) Unwrap() error { return e.Err }

// Invalid marks err as an [InvalidError].
func Invalid(err error) error { return &InvalidError{Err: err} }

// Invalidf returns an [InvalidError] with a formatted message.
func Invalidf(format string, args ...any) error {
	return &InvalidError{Err: fmt.Errorf(format, args...)}
}

// ConflictError is a request that clashes with something the agent already
// has, such as a name taken by a pairing of another kind.
type ConflictError struct{ Err error }

func (e *ConflictError) Error() string { return e.Err.Error() }

func (e *ConflictError) Unwrap() error { return e.Err }

// Conflictf returns a [ConflictError] with a formatted message.
func Conflictf(format string, args ...any) error {
	return &ConflictError{Err: fmt.Errorf(format, args...)}
}

// BusyError is returned while the agent server holds the agent directory
// without serving its control socket, which happens while it starts and
// stops.
type BusyError struct{ Name string }

func (e *BusyError) Error() string {
	return e.Name + " is starting or stopping; try again in a moment"
}

// IsRequest reports whether err is a refused request: not found, invalid
// or in conflict.
func IsRequest(err error) bool {
	var (
		nf *NotFoundError
		in *InvalidError
		co *ConflictError
	)
	return errors.As(err, &nf) || errors.As(err, &in) || errors.As(err, &co)
}

// ErrNotRunning matches the errors of a command that needs the agent
// server while it doesn't run.
var ErrNotRunning error = NotRunning("the server")

// NotRunningError is a command that needs a server, such as the agent
// server, while it doesn't run.
type NotRunningError struct {
	Msg string
	// Hint is how to start the server.
	Hint Hint
}

func (e *NotRunningError) Error() string { return e.Msg }

// Is matches [ErrNotRunning].
func (e *NotRunningError) Is(target error) bool { return target == ErrNotRunning }

// NotRunning returns the error, matching [ErrNotRunning], of the agent
// server called name.
func NotRunning(name string) error {
	return &NotRunningError{Msg: name + " is not running", Hint: HintStart}
}

// Hint names how to fix an error. The control socket carries the code;
// [HintText] words it as a command to run.
type Hint string

// Hints. Arg is the pairing name for the hints of a pairing.
const (
	// HintStart starts the agent server.
	HintStart Hint = "start"
	// HintStartOrURL starts the agent server, or passes the agent's URL.
	HintStartOrURL Hint = "start-or-url"
	// HintRelayRun starts the relay.
	HintRelayRun Hint = "relay-run"
	// HintPairInvite pairs a user first.
	HintPairInvite Hint = "pair-invite"
	// HintLinkChat links a chat account with a pairing code.
	HintLinkChat Hint = "link-chat"
	// HintPairSet changes the pairing Arg.
	HintPairSet Hint = "pair-set"
	// HintPairSetOwner makes the pairing Arg an owner.
	HintPairSetOwner Hint = "pair-set-owner"
	// HintPairRm picks another name, or removes the pairing Arg first.
	HintPairRm Hint = "pair-rm"
	// HintPairRmLocal tells what removing the local pairing Arg does.
	HintPairRmLocal Hint = "pair-rm-local"
	// HintPairInviteWebhook adds a webhook node.
	HintPairInviteWebhook Hint = "pair-invite-webhook"
	// HintRestoreLatest restores the latest snapshot Arg.
	HintRestoreLatest Hint = "restore-latest"
	// HintDamagedDB restores the agent, or moves its damaged database Arg
	// aside.
	HintDamagedDB Hint = "damaged-db"
	// HintPortInUse picks another port for the agent.
	HintPortInUse Hint = "port-in-use"
)

// HintedError is an error with a hint at how to fix it.
type HintedError struct {
	Err  error
	Hint Hint
	// Arg is what the hint applies to, such as a pairing name.
	Arg string
}

func (e *HintedError) Error() string { return e.Err.Error() }

func (e *HintedError) Unwrap() error { return e.Err }

// WithHint attaches the hint h, about arg, to err.
func WithHint(err error, h Hint, arg string) error {
	return &HintedError{Err: err, Hint: h, Arg: arg}
}

// HintOf returns the hint of err and what it applies to, if it has one.
func HintOf(err error) (Hint, string, bool) {
	var he *HintedError
	if errors.As(err, &he) {
		return he.Hint, he.Arg, true
	}
	var nr *NotRunningError
	if errors.As(err, &nr) && nr.Hint != "" {
		return nr.Hint, "", true
	}
	return "", "", false
}
