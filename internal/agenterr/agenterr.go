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
