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

// Package a2aerr turns the errors of A2A requests into the errors that
// callers get.
package a2aerr

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// Sanitizer is a call interceptor that sends callers the part of a request
// error written for them, see [Sanitize], and logs the error in full. Add
// it first, so that it also sees the errors of the other interceptors.
type Sanitizer struct {
	a2asrv.PassthroughCallInterceptor
}

var _ a2asrv.CallInterceptor = Sanitizer{}

// After implements [a2asrv.CallInterceptor].
func (Sanitizer) After(_ context.Context, callCtx *a2asrv.CallContext, resp *a2asrv.Response) error {
	if resp.Err == nil {
		return nil
	}
	clean := Sanitize(resp.Err)
	if clean.Error() != resp.Err.Error() {
		slog.Info("request failed", "method", callCtx.Method(), "error", resp.Err)
	}
	resp.Err = clean
	return nil
}

// sentinels are the A2A errors a request error may wrap.
var sentinels = []error{
	a2a.ErrTaskNotFound,
	a2a.ErrTaskNotCancelable,
	a2a.ErrUnsupportedOperation,
	a2a.ErrInvalidParams,
	a2a.ErrInvalidRequest,
	a2a.ErrParseError,
	a2a.ErrMethodNotFound,
	a2a.ErrUnauthenticated,
	a2a.ErrUnauthorized,
	a2a.ErrUnsupportedContentType,
	a2a.ErrPushNotificationNotSupported,
	a2a.ErrExtendedCardNotConfigured,
	a2a.ErrExtensionSupportRequired,
	a2a.ErrVersionNotSupported,
	a2a.ErrInvalidAgentResponse,
	a2a.ErrServerError,
	a2a.ErrInternalError,
}

// Sanitize returns the error callers get for err. a2a-go wraps the errors
// of the task store and the executor in its own context, such as
// "executor setup failed: failed to load exec ctx: ...". Sanitize keeps an
// [a2a.Error], an A2A error with the detail its creator gave it, as
// "permission denied: <detail>" or "<detail>: task not found", or a
// context error. Other errors become an internal error.
func Sanitize(err error) error {
	var a2aErr *a2a.Error
	if errors.As(err, &a2aErr) {
		return a2aErr
	}
	for _, ctxErr := range []error{context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, ctxErr) {
			return ctxErr
		}
	}
	if s := sentinelOf(err); s != nil {
		if e := find(err, func(e error) bool { return strings.HasPrefix(e.Error(), s.Error()+": ") }); e != nil {
			return a2a.NewError(s, e.Error())
		}
		if !internal(s) {
			if e := find(err, func(e error) bool { return wraps(e, s) }); e != nil && s != a2a.ErrTaskNotFound {
				return a2a.NewError(s, e.Error())
			}
			return s
		}
	}
	if known := a2aGoError(leaf(err)); known != nil {
		return known
	}
	return a2a.NewError(a2a.ErrInternalError, "internal error")
}

func sentinelOf(err error) error {
	for _, s := range sentinels {
		if errors.Is(err, s) {
			return s
		}
	}
	return nil
}

func internal(s error) bool {
	return s == a2a.ErrInternalError || s == a2a.ErrServerError
}

// find returns the first error in the tree of err, outermost first, for
// which match is true.
func find(err error, match func(error) bool) error {
	if err == nil {
		return nil
	}
	if match(err) {
		return err
	}
	for _, e := range unwrap(err) {
		if found := find(e, match); found != nil {
			return found
		}
	}
	return nil
}

// wraps reports whether err directly wraps target.
func wraps(err, target error) bool {
	return slices.Contains(unwrap(err), target)
}

func unwrap(err error) []error {
	switch u := err.(type) {
	case interface{ Unwrap() error }:
		if e := u.Unwrap(); e != nil {
			return []error{e}
		}
	case interface{ Unwrap() []error }:
		return u.Unwrap()
	}
	return nil
}

// leaf returns the innermost error of err, following the first error of
// errors that wrap several.
func leaf(err error) error {
	for {
		next := unwrap(err)
		if len(next) == 0 {
			return err
		}
		err = next[0]
	}
}

// executionInProgress is the message of the a2a-go error for a message to
// a task that runs an execution.
const executionInProgress = "task execution is already in progress"

// ExecutionInProgress reports whether err refused a message to a task
// because the task already runs an execution.
func ExecutionInProgress(err error) bool {
	if err == nil {
		return false
	}
	if err.Error() == executionInProgress {
		return true
	}
	for _, e := range unwrap(err) {
		if ExecutionInProgress(e) {
			return true
		}
	}
	return false
}

// a2aGoError returns the A2A error for an error that a2a-go doesn't export
// or type, or nil. a2a-go#552 types them.
func a2aGoError(err error) error {
	msg := err.Error()
	switch {
	case msg == executionInProgress || msg == "task cancelation is in progress":
		return a2a.NewError(a2a.ErrUnsupportedOperation, "the task is already handling a request; try again when it is done")
	case msg == "max concurrency limit reached" || strings.HasPrefix(msg, "max scope concurrency limit reached"):
		return a2a.NewError(a2a.ErrUnauthorized, "too many tasks are running; try again when one finishes")
	}
	return nil
}
