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

// Package clierr classifies errors into the a2a-cli error contract,
package clierr

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"syscall"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/a2aproject/a2a-serve/internal/agenterr"
)

// Code is a symbolic error identifier.
type Code string

const (
	// CodeUsage marks invalid arguments, flags, or flag combinations; exit 2.
	CodeUsage = "A2ACLI_ERR_USAGE"
	// CodeProtocol marks a failure the agent reported as an a2a.Error; the
	// specific A2A reason travels in the A2ACode field. Exit 1.
	CodeProtocol = "A2ACLI_ERR_PROTOCOL"
	// CodeIO marks a transport failure reaching or reading from the agent —
	// DNS, connection, TLS, reset, broken pipe. Exit 3.
	CodeIO = "A2ACLI_ERR_IO"
	// CodeTimeout marks a --timeout that expired before a terminal state; exit 5.
	CodeTimeout = "A2ACLI_ERR_TIMEOUT"
	// CodeInternal marks an unexpected CLI-side failure, or any condition with
	// no better code. Exit 1.
	CodeInternal = "A2ACLI_ERR_INTERNAL"

	// CodeNotRunning marks a command which needs the agent server while it
	// doesn't run; exit 6. a2a-cli reserves its own prefix, so codes of
	// a2a-serve use A2ASERVE_ERR_.
	CodeNotRunning = "A2ASERVE_ERR_NOT_RUNNING"
	// CodeNoAgent marks a command run outside an agent directory, or with a
	// --dir that has none; exit 7.
	CodeNoAgent = "A2ASERVE_ERR_NO_AGENT"
	// CodeBusy marks a command run while the agent server starts or stops,
	// and can't take the change; exit 8.
	CodeBusy = "A2ASERVE_ERR_BUSY"
)

// Error is a classified CLI failure.
type Error struct {
	// Code is the symbolic identifier reported to callers.
	Code string
	// Message is the human-readable, single-line failure description.
	Message string
	// Hint is optional remediation advice shown to the user.
	Hint string
	// A2ACode is the A2A protocol error reason, set only for protocol failures.
	A2ACode string
	// Exit is the process exit status for this failure.
	Exit int

	// err is the wrapped cause, exposed via Unwrap.
	err error
	// silent marks a failure the command already reported.
	silent bool
}

// Error returns the failure message, satisfying the error interface.
func (e *Error) Error() string { return e.Message }

// Unwrap returns the wrapped cause so errors.Is and errors.As can inspect it.
func (e *Error) Unwrap() error { return e.err }

// Exit builds an error which only sets the exit status, for a command which
// already reported its failure in its output.
func Exit(code int, msg string) *Error {
	return &Error{Code: CodeInternal, Message: msg, Exit: code, silent: true}
}

// Silent reports whether the failure was already reported.
func (e *Error) Silent() bool { return e.silent }

// exits are the exit statuses of the codes.
var exits = map[string]int{
	CodeUsage:      2,
	CodeProtocol:   1,
	CodeIO:         3,
	CodeTimeout:    5,
	CodeInternal:   1,
	CodeNotRunning: 6,
	CodeNoAgent:    7,
	CodeBusy:       8,
}

// IOHint is remediation advice attached to every I/O error.
const IOHint = "check connectivity or whether the agent is running"

// UsageHint is remediation advice attached to every usage error.
const UsageHint = "run `a2a-serve --help` for the current commands and flags"

// hints are the default hints of the codes.
var hints = map[string]string{
	CodeUsage: UsageHint,
	CodeIO:    IOHint,
}

// Wrap classifies err with code, such as the code of a failure a server
// reported. An unknown code exits 1.
func Wrap(code string, err error) *Error {
	exit, ok := exits[code]
	if !ok {
		exit = 1
	}
	return &Error{Code: code, Message: err.Error(), Hint: hints[code], Exit: exit, err: err}
}

// Timeout classifies err, which the time limit of flag ended, as a timeout.
func Timeout(err error, flag string) *Error {
	e := Wrap(CodeTimeout, err)
	e.Hint = "increase --" + flag
	return e
}

// UsageNoHint builds a usage error whose message says how to fix it, so it
// gets no hint.
func UsageNoHint(msg string) *Error {
	return &Error{Code: CodeUsage, Message: msg, Exit: exits[CodeUsage]}
}

// Usage builds a usage error for invalid arguments, flags, or flag combinations.
func Usage(msg string) *Error {
	return &Error{Code: CodeUsage, Message: msg, Hint: UsageHint, Exit: exits[CodeUsage]}
}

// NotRunning classifies err as the failure of a command which needs the
// agent server while it doesn't run.
func NotRunning(err error) *Error { return Wrap(CodeNotRunning, err) }

// NoAgent classifies err as a lookup which found no agent directory.
func NoAgent(err error) *Error { return Wrap(CodeNoAgent, err) }

// MarshalJSON implements [json.Marshaler].
func (e *Error) MarshalJSON() ([]byte, error) {
	type body struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Hint    string `json:"hint,omitempty"`
		A2ACode string `json:"a2aCode,omitempty"`
	}
	type wrapper struct {
		Error body `json:"error"`
	}
	return json.Marshal(wrapper{Error: body{
		Code:    e.Code,
		Message: e.Message,
		A2ACode: e.A2ACode,
		Hint:    e.Hint,
	}})
}

// usageError is implemented by errors of other packages which report
// invalid input and that callers match with errors.As, such as an invalid
// agent config. Other errors use the constructors of this package.
type usageError interface{ UsageError() bool }

// Classify maps an arbitrary error to a classified CLI Error with a stable code
// and exit status. It returns nil for a nil error and passes an already
// classified *Error (e.g. from Usage or CardResolution) through unchanged; a
// wrapped one keeps the message of err. An [agenterr.Hint] of err becomes
// the hint, worded as the command that fixes it.
func Classify(err error) *Error {
	if err == nil {
		return nil
	}
	ce := classify(err)
	if text := agenterr.HintText(err); text != "" && ce.Hint != text {
		hinted := *ce
		hinted.Hint = text
		return &hinted
	}
	return ce
}

func classify(err error) *Error {
	var classified *Error
	if errors.As(err, &classified) {
		if classified == err {
			return classified
		}
		wrapped := *classified
		wrapped.Message, wrapped.err = err.Error(), err
		return &wrapped
	}

	var ue usageError
	if agenterr.IsRequest(err) || errors.As(err, &ue) && ue.UsageError() {
		return Wrap(CodeUsage, err)
	}

	if errors.Is(err, agenterr.ErrNotRunning) {
		return Wrap(CodeNotRunning, err)
	}

	var busy *agenterr.BusyError
	if errors.As(err, &busy) {
		return Wrap(CodeBusy, err)
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return Timeout(err, "timeout")
	}

	var a2aErr *a2a.Error
	if errors.As(err, &a2aErr) {
		e := Wrap(CodeProtocol, err)
		e.A2ACode = a2a.ErrorReason(a2aErr.Err)
		return e
	}

	if isIOError(err) {
		return Wrap(CodeIO, err)
	}

	e := Wrap(CodeInternal, err)
	if path := deniedPath(err); path != "" {
		e.Hint = "check the permissions of " + path + " and the directories above it"
	}
	return e
}

// deniedPath returns the file that err was denied access to, or its
// directory when the file doesn't exist and so couldn't be created.
func deniedPath(err error) string {
	var pe *fs.PathError
	if !errors.Is(err, fs.ErrPermission) || !errors.As(err, &pe) {
		return ""
	}
	if _, err := os.Lstat(pe.Path); errors.Is(err, fs.ErrNotExist) {
		return filepath.Dir(pe.Path)
	}
	return pe.Path
}

// isIOError reports whether err is a transport-level failure — a network error,
// a broken stream, or a reset/closed connection — identified by type and
// sentinel rather than by matching message text.
func isIOError(err error) bool {
	if errors.Is(err, syscall.EADDRINUSE) {
		return false
	}
	// A bare syscall.Errno also implements net.Error, as file errors do.
	var netErr net.Error
	if errors.As(err, &netErr) {
		if _, ok := netErr.(syscall.Errno); !ok {
			return true
		}
	}
	return errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, io.ErrClosedPipe) ||
		errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE)
}
